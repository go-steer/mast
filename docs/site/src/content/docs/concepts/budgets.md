---
title: Budgets and cost
description: Turn, wallclock, and dollar ceilings at the workload and specialist level — how spend is attributed, how the caps compose, and the two limits to understand before you rely on one.
sidebar:
  order: 4
---

Nobody is watching. That is the premise of the whole product, and it is why
a ceiling is not a nice-to-have: the failure mode of an unattended agent is
not a wrong answer, it is a loop that runs all night and bills for it.

Three ceilings, declared in the bundle:

```yaml
budget:
  max_turns: 40                # model calls, not tool calls
  max_wallclock_seconds: 900
  max_cost_usd: 5.00
```

Any of the three may be omitted; what you set is what is enforced.

- **`max_turns`** counts *model calls*. Tool calls are free — a specialist
  reading twelve pods costs one turn if it batched them into one model
  step. This is the cap that catches a genuine loop.
- **`max_wallclock_seconds`** bounds a turn's lifetime. It doubles as the
  daemon's drain bound on shutdown, so a finishing turn is never cut
  shorter than its own budget allows
  ([durability](/concepts/durability/#shutdown-is-accounted-for-not-just-survivable)).
- **`max_cost_usd`** is computed from provider token accounting for the
  models actually used, which is why a cross-provider roster still gets one
  meaningful number.

## Per-specialist ceilings

A workload-wide cap is blunt: it cannot tell "the classifier is looping"
from "the whole incident is expensive". So a specialist can declare its own:

```yaml
# specialists/OOMKilled.tmpl
budget:
  max_turns: 6
  max_cost_usd: 0.25
```

**Composition is tightest-cap-wins, by construction.** A specialist cannot
raise its own ceiling above the workload's — a `max_cost_usd: 50` in a
`.tmpl` under a `max_cost_usd: 5` workload buys nothing. The bundle is the
outer bound; a specialist may only tighten it.

That makes the classifier's one cheap turn budgetable as one cheap turn,
and it lets you give the expensive diagnoser room without giving it to
everyone.

## Attribution

Spend is attributed by the **event author** — which specialist emitted the
event — rather than by execution branch. That is not a detail you would
guess: under `coordinator` dispatch the branch field is empty, so
branch-based attribution silently attributes everything to the root and
per-specialist ceilings never trip. Author-based attribution works across
every dispatch shape.

Seeing the spend in the first place takes one extra seam under `planner`
dispatch, and it is worth knowing why. A planner runs each
`invoke_specialist` call on a runner of its own, so those events never
appear on the stream the meter watches; through v0.4 a specialist
dispatched that way was billed to nobody, and a workload could spend past
its ceiling as long as it spent through the planner's door. Since v0.5
the dispatch tool hands those events back to the same meter, under the
same session — coordinator, graph and fan-out dispatch never had the gap.
If you embed mast as a library and compose the planner yourself, wire
`compose.RootConfig.SubRunObserver`; the top-level `mast.RunWorkload`
already does.

## Where the dollar figure comes from

Tokens are counted from what the provider reports; the price per token
comes from a built-in rate table generated from
[LiteLLM's catalog](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json)
and refreshed weekly by an automated pull request, so a `max_cost_usd`
does not slowly become a different number of dollars as vendors move
their prices. Prompt-caching is priced with the three buckets Anthropic
bills separately — uncached input, cache reads at a tenth of it, and
cache *writes* at 1.25x — rather than charging every cached token the
read rate, which under-reports a cache-heavy turn.

Rates are overridable, which is what you want for negotiated enterprise
pricing or a model mast has never heard of: an operator can drop a
`pricing.json` next to the workload in `.agents/`, and a library embedder
can pass rows directly (`pricing.Options.CfgOverride`, highest
precedence). A model with no row anywhere is metered at a flat fallback
rate and counted as unpriced rather than billed at zero — a ceiling still
trips on it, just less precisely.

### Which name gets looked up

The rate table is keyed on the model the **provider says it ran**, not the
one you asked for. Those normally agree, and where they don't the server's
answer is the one that was billed: an alias resolves server-side to a dated
snapshot, and pricing the snapshot is more accurate than pricing the alias.
Lookup is by exact match and then by longest prefix anchored at the start of
the string, so a Vertex-style `claude-opus-4-5@20251101` finds its
`claude-opus-4-5` row.

One shape can't be looked up: a backend that names the model as a full
resource path (`projects/.../publishers/anthropic/models/claude-opus-4-5`)
has no key that is a prefix of it. mast falls back to the model ID the
request asked for rather than let a whole session drop to the flat rate —
which would keep the ceiling honest but lose every per-model rate including
cache reads, the largest term on a cache-warm agent.

### The same model on two backends is two prices

Claude is sold first-party by Anthropic and resold on Vertex AI; Gemini is
sold through the Developer API and through Vertex. Those are four billing
relationships, and nothing guarantees the two prices for a model stay equal
— so the rate table is keyed on the **pair**, `<backend>/<model>`, with the
bare model ID as a fallback. The four backend names are `anthropic`,
`anthropic-vertex`, `gemini` and `vertex`.

(As of this writing, every model mast ships costs the same on both of its
backends. The pair is how the table is built anyway: the alternative is
being correct by coincidence, and finding out otherwise from a bill.)

The backend is **not** whatever you passed to `--provider`. That flag is an
alias, and the environment can override where it lands:
`GOOGLE_GENAI_USE_VERTEXAI=true` sends a plain `gemini` run to Vertex, and
with no alias at all Anthropic picks first-party when `ANTHROPIC_API_KEY` is
set and Vertex when only a GCP project is. mast resolves the backend once,
with the same code that builds the client, so the row that prices a call is
the row for the backend that will bill it.

For an override this means you can price one backend without touching the
other. A negotiated first-party rate goes in as `anthropic/claude-opus-5`
and leaves your Vertex spend on the catalog rate; a bare `claude-opus-5` row
still applies to both, which is usually what you want:

```json
{
  "version": 1,
  "models": {
    "anthropic/claude-opus-5": { "input_per_mtok": 12.0, "output_per_mtok": 60.0 },
    "claude-haiku-4-5":        { "input_per_mtok":  0.8, "output_per_mtok":  4.0 }
  }
}
```

Precedence and prefix matching work the same on qualified keys, so
`anthropic-vertex/claude-opus-4-5@20251101` finds an
`anthropic-vertex/claude-opus-4-5` row.

### Some rates have an expiry date, and the catalog cannot say so

A launch price is often introductory, and a `max_cost_usd` sized against
one buys fewer tokens once it lapses — without anything looking wrong,
because the number in the table is still the number the vendor is
currently charging. LiteLLM's catalog carries no expiry field on any row,
so mast records the known ones itself and fails its own build if a lapsed
rate is still in the table.

The two that matter as of 2026-08-20:

| Model | Rate now | Changes to | On |
|---|---|---|---|
| `gemini-3.7-flash` (the gemini/vertex frontier default) | $0.75 / $3.75 per MTok | $1.50 / $7.50 | 2027-01-01 |
| `gemini-3.6-flash` | $0.75 / $3.75 per MTok | $1.50 / $7.50 | 2027-01-01 |

Cache reads double alongside. If you are sizing a ceiling that will still
be in force in 2027 on either model, size it against the later number.
(`claude-sonnet-5`'s introductory $2 / $10 was scheduled to rise on
2026-09-01 and will not — Anthropic made it the standard price.)

## The ceiling is checked before the call, and after it

Two checks, asking different questions.

**Before each model call**, mast asks whether the ceiling can still be
respected — and refuses the call if the arithmetic says it cannot. That
makes `max_turns` **exact**: `max_turns: 40` means the workload makes 40
model calls, not 41. On tokens and dollars it means a workload sitting *at*
its cap stops there rather than buying one more call to confirm what it
already knows.

**After each call**, the same ceilings are re-derived from what the
provider actually reported. This is the ledger, and it is the number
everything else reads.

The pre-call check will not guess. It refuses only where the arithmetic is
a proof — a call is one turn, and every real call adds tokens and cost — so
it never estimates how large the *next* call will be. The consequence is
the one number to size against:

**A call that starts with headroom is always allowed to finish, and is
billed for whatever it turns out to cost.** `max_cost_usd: 5.00` at $4.90
spent permits the next call, and that call may be a $2 one. So the cap is
still "stop at $5", not "never exceed $5" — but the overshoot is bounded by
one call, and only ever a call there was room for. A cap you land exactly
on costs nothing further.

The alternative — predicting a call's cost and refusing on the estimate —
would refuse affordable work on a bad guess and permit unaffordable work on
a worse one, and nothing in the transcript would tell you which had
happened.

### What a refusal looks like from outside

A refused call is not a crash and not a silence. The agent gets a
synthesized answer saying it was stopped by a ceiling and why, so the
reason is in the transcript where anyone reading the session finds it, and
a coordinator reading its specialist's report can tell "found nothing" from
"was never allowed to look".

The turn still fails, though — a turn whose work did not happen must not
report OK:

- the **turn stops at the first refusal** and returns an error naming the
  ceiling and the arithmetic. It stops rather than carrying on for the same
  reason a crossed ceiling stops it: anything above the model that reacts to
  a bad answer by asking for another one — a contract handing a report back
  to be fixed, say — was bounded by the budget it was burning, and a refused
  call burns nothing. Two sentinels, and they mean different things:
  `budget.ErrRefused` is a call that did not happen, `budget.ErrExceeded` is
  a call that happened and went over. Match both if you only care that the
  budget stopped the run.
- over attach it arrives as a `turn-error` of kind **`cost_ceiling`**,
  `retryable: false`, with the reset endpoint in the hint — the same kind a
  crossed ceiling gets, because from outside this is one event. The `code`
  is what separates them: `BUDGET_REFUSED` against `BUDGET_EXCEEDED`. Key
  your UI off the kind; read the code only if you need to say whether the
  money was actually spent.
- the **next turn is refused at the door** with `409` and the reset
  endpoint, the same as a session that had crossed a ceiling. A scheduler
  retrying every minute buys nothing.
- `mast_budget_trips_total` **increments either way**, so an alert written
  against the old behaviour keeps firing.

## A spent specialist closes one path, not the session

When a specialist reaches its own cap, that specialist is refused and the
session carries on. The refusal goes back to whoever dispatched it as an
answer — a `finish_task` report saying it was stopped by a ceiling, whose
budget, and by how much — and the coordinator routes around it the same
way it handles a specialist that declines. The workload's *own* ceiling
still stops the turn, because there is nothing left to route to.

This changed in v0.6. Through v0.5 a specialist's crossed cap cancelled
the whole run, and that was never really a decision: cancelling was the
only lever a turn driver had for stopping a specialist from calling again.
The [pre-call gate](#the-ceiling-is-checked-before-the-call-and-after-it)
is a narrower one — it stops the *call*, so the session no longer has to
be collateral.

A per-specialist cap is still a safety limit and not a routing hint. What
makes routing around one safe is that the cap is enforced where it is
declared: the refused specialist cannot make another call whoever asks it
to, so a coordinator retrying it gets the same refusal rather than a
second budget. The workload cap is what bounds the roster as a whole.

The thing to watch for is the quiet version. A workload that lost half its
roster finishes and reports success, so mast makes the loss loud in four
places:

- `mast_budget_trips_total` **increments**, the same counter a crossed
  ceiling moves. If you alert on budget trips, you already see this.
- the daemon logs `BUDGET CEILING — a specialist was refused; the turn
  routed on` at **WARN**, with the reason and the count.
- `GET /sessions/{id}/guardrails` reports each specialist's own state
  under `cost_ceiling.scopes[]`. The session's `tripped` stays `false`,
  because the session is not the thing that stopped — check the scopes.
- library callers get `mast.Result.Exhausted`, the specialists whose
  ceilings can no longer admit a call, with the arithmetic behind each.

Raising the session budget does **not** clear a specialist's cap, and
`POST /guardrails/reset` says so rather than reporting a cleared ceiling
it did not clear. Name the specialist to raise its own:

```bash
curl -X POST $MAST/sessions/incident-abc/guardrails/reset \
  -H 'Content-Type: application/json' \
  -d '{"scope": "OOMKilled", "additional_budget_usd": 0.50}'
```

### Letting a stopped specialist file what it found

A refusal ends the specialist with nothing attached, and for a specialist
refused on its first call that is the honest answer: nothing was looked at,
and mast will not invent a finding to fill the gap.

It is the wrong answer for the other case. A diagnoser stopped on its
twelfth turn, after six log queries and a quarter of a million tokens, had
established a great deal — and the incident got an unresolved delegation
anyway. The tokens were spent either way.

`budget.final_report` buys that specialist exactly one more model call, with
**every tool but its report tool withdrawn**, and an instruction to report
what it can already support and to say plainly what it did not get to:

```yaml
budget:
  max_cost_usd: 5.00
  final_report: true
```

mast synthesizes nothing. The report is written by the model, from the
evidence in its own context, in its own output schema — including "I
established nothing", if that is the truth, which is still a far more useful
artifact than silence. Stripping the tools is what makes it a report rather
than one more query: there is no move left but to file.

Three bounds, and they are why this is safe to turn on:

- **Once per specialist per session.** A grant that could be re-taken would
  feed the invalid-report retry loop a model call at a time. The second ask
  gets the ordinary refusal.
- **Opt-in.** It is off by default, because it is a deliberate overshoot:
  one call, on a specialist already at its ceiling. A cap that is a hard
  spending limit should stay a hard spending limit.
- **Never granted to a specialist that has spent nothing.** There is no
  partial finding to salvage from an agent that never ran.

Each grant is logged at WARN — `BUDGET CEILING — a stopped specialist bought
its final report` — so the overshoot is announced rather than arriving
unexplained in a total.

One thing to expect: the grant does not change *which* ceiling stopped what.
A specialist's own cap closes one path and the turn routes on, so the report
comes back to the coordinator as an answer it can use. The workload's cap
still ends the turn — the report is written and lands in the transcript, but
there is no turn result left to carry it. If your only ceiling is
session-wide, this flag buys you an artifact in the session, not a better
answer.

### Inside a planner dispatch

A specialist that reaches its cap inside a planner dispatch has always
stopped only that dispatch: the planner gets its result back marked
`"status": "halted"` with the reason, and its turn continues. That used to
be the one exception to a harsher rule; it is now the same rule, reached
by a different route.

The **watchdog** deliberately does not work this way. It watches the same
dispatches, but a trip there halts the session as well as the dispatch,
because a watchdog trip is a latch an operator has to clear rather than a
cumulative total that stopping the sub-run already settles. See
[it watches inside a planner dispatch too](/concepts/interop/#it-watches-inside-a-planner-dispatch-too).

## Sizing one

A ceiling is a number an operator has to choose, and the honest way to
choose it is to measure rather than to estimate. Two things make estimating
worse than it sounds.

**The same workload does not cost the same thing twice.** Five runs of one
GKE triage bundle against a live cluster — identical incident text,
identical roster, identical model, nothing changed between them:

| Run | Model calls | Cost |
| --- | --- | --- |
| 1 | 11 | $0.140 |
| 2 | 10 | $0.152 |
| 3 | 14 | $0.201 |
| 4 | 13 | $0.198 |
| 5 | 18 | $0.267 |

That is a 90% spread on an unchanged envelope, and it is not noise to be
averaged away — it is the model choosing how many things to look at. Run 4
died on a turn cap that run 1 cleared with one turn to spare. A cap sized
on the median is a coin flip, and the side it lands on is *the incident got
no answer*, having paid for most of one.

So size on the **worst run you have observed, plus headroom**, and treat
the median as information about the bill rather than about the cap.

**What actually drives the spread is tool output, not model verbosity.**
The same three reads, projected and unprojected, from that workload:

| Read | Unprojected | Projected |
| --- | --- | --- |
| one Deployment | 19,080 B | 600 B |
| every pod in a namespace | 138,609 B | 588 B |
| `list_k8s_events`, unbounded | 19,976 B | 3,736 B |

A single unprojected namespace listing is larger than every other message
in a short investigation put together, it is charged again on every
subsequent turn of that agent's loop, and it buys nothing a projection
would not have. Before raising a ceiling, check whether the workload is
paying to re-read the same 138 kB — narrowing what a tool returns is the
cheaper fix, and the only one that also makes the run *faster*. See [tools
and MCP](/concepts/tools-and-mcp/) for what a server will let you narrow.

Two consequences for which knob to reach for:

- **Prefer `max_cost_usd` to `max_turns` as the outer bound.** Cost is what
  you actually care about bounding, and it degrades honestly: a run that is
  more expensive than usual because it read something large trips on the
  thing that made it expensive. A turn cap trips on the count instead, so
  the run that read three cheap things and the run that read one enormous
  one are treated identically.
- **Keep `max_turns` as the runaway stop, not the budget.** Set it above
  the worst observed turn count with room, and let it catch a loop rather
  than a thorough investigation. The [watchdog](/concepts/interop/) is the
  better instrument for the loop case anyway; it can tell the difference.

None of this can be inferred from the bundle, which is why mast declines to
pick numbers for you. It will, however, tell you what the last run cost:
`Result.Usage`, the `session_cost_usd` log field, the `mast.cost.usd` span
attribute and `mast_cost_usd_total` all report it. Five runs of a new
workload behind a generous ceiling is a cheaper way to find the right
ceiling than one run behind a guessed one.

## Getting unstuck after a trip

A trip is not a one-time event. Enforcement is re-derived from the
session's accumulated usage against its ceiling — there is no "tripped"
flag to clear — so a session that is out of budget is out of it on the
next turn too, and the one after that. *Out of budget* includes sitting
exactly on a cap: the pre-call check refuses a turn that cannot make its
first call, so such a session is refused at the door rather than answering
"I am out of budget" once per prompt forever. Left alone it
refuses every prompt, and a restart does not clear it either: the spend
is [durable](#spend-survives-a-restart), so the arithmetic comes back with
it. An operator is the way out.

Two attach endpoints are the way out:

```bash
# what is armed, what tripped, why
curl $MAST/sessions/incident-abc/guardrails

# hand it more runway
curl -X POST $MAST/sessions/incident-abc/guardrails/reset \
  -H 'Content-Type: application/json' \
  -d '{"additional_turns": 20}'
```

The read reports all three dimensions and each specialist's own ceilings,
which matters because they fail differently — and because only one of them
stops the session. A session stopped by `max_turns: 40` has spent a
fraction of a cent and answers nothing at all; a workload that spent one
specialist's `max_cost_usd: 0.25` is still answering, just with a path
missing, and `cost_ceiling.tripped` on the session is `false`. Read
`cost_ceiling.scopes[]` for that one.

Three things the reset deliberately does *not* do:

- **It does not zero the accumulator.** "Reset" means raising the ceiling.
  A session that has spent $40 still reports $40 afterwards, so `/usage`,
  the eventlog, and the post-incident review all agree on one number.
- **It does not clear a trip it cannot clear.** Ask for $50 on a session
  stopped by a turn cap and you get `409` with the crossed dimension named,
  not a `200` that re-trips on your next prompt. The check runs before
  anything changes, so a refusal costs nothing — re-issue with the right
  dimension.
- **It does not impose a ceiling that wasn't there.** `additional_turns` on
  a workload that declared no `max_turns` is a no-op, not a new cap.

`scope` targets one specialist's ceilings; omit it for the session's own.
The two are judged separately, in both directions: a session grant is not
refused because some specialist is still spent, and it does not report
that specialist cleared either. What it does do is name it, so the reply
to a session reset on a workload short a path reads *"raised the session
budget (+$50.0000); nothing was tripped; specialist "OOMKilled" can admit
no further call (raise with scope="OOMKilled")"*. Raising a specialist
above the workload's cap still buys nothing — the workload ceiling is
enforced independently, and it is still the outer bound.

Every reset is logged with the authenticated caller that requested it, and
recorded durably alongside the guardrail state when the daemon runs with
`--attach-listen` — so "who cleared this, when, and what did they hand
over" survives the log rotation and the process. The runway itself is
replayed after a restart, against the scope it was granted to.

## Spend survives a restart

A ceiling a restart resets is a ceiling on what a workload spends *per
process*, which is not what anyone bought. mast's restarts are automatic
and unattended, so an in-memory accumulator would let a crash loop spend
the cap once per restart, indefinitely.

With `--session-db` set, each priced model call is written to a ledger
mast owns, and a session's first turn after a restart folds it back before
anything runs: a workload stopped by `max_cost_usd: 5.00` after $5.02
comes back at $5.02, not at zero. So does each specialist's own accumulator, so per-specialist
ceilings survive too — and so do the grants an operator handed over, which
means a session rescued at $5.02 comes back rescued rather than wedged by
a restart nobody made.

Three properties worth knowing:

- **A restored over-ceiling session is refused at the door, not one call
  in.** Without durable spend, a wedged session bought a model call per
  turn and the overshoot was bounded only by the process lifetime. Now
  the turn is refused up front with `409` naming the reset endpoint — a
  whole-turn refusal, above the per-call check, so a scheduler retrying
  every minute gets an HTTP error rather than a session that starts and
  immediately declines.
- **What a call cost is recorded when the call is made**, not
  re-derived later. Rates move weekly; a session's spend is money that has
  already left the account, and re-pricing history at today's catalog
  would quietly rewrite it.
- **Restore fails open**, the way the watchdog's does: an unreadable
  ledger logs and the turn runs, rather than a storage fault stopping
  every session in the deployment. The read is retried on the next turn
  rather than remembered as "restored to nothing".

The ledger needs a database and nothing else — an attach surface is not
required, and through v0.6.0 it wrongly was (issue #274), which denied a
durable ceiling to unattended daemons, the deployment least likely to bind
an operator socket and most likely to crash-loop. Without `--session-db`
sessions are in-memory, there is no durable connection to write to, and
the accumulator is per-process as it was before; a daemon with budget
ceilings and no session database says so at startup. Alert on
`mast_budget_trips_total` either way.

The watchdog's halt is durable on a narrower condition — it needs
`--attach-listen` as well — because `POST /guardrails/reset` is the only
thing that clears one, and a halt nobody can clear is worse than a halt
that a restart forgets. A ledger has no such latch, which is why the two
differ.

## Watching it

```
mast_budget_trips_total{workload,specialist,limit}
```

is the metric to alert on. A trip is not automatically a bug — a tight cap
doing its job looks identical to a runaway being caught — but a *rising*
trip rate on one specialist is a prompt or a model that stopped converging.
See [metrics](/reference/metrics/).

## Overrides at deploy time

Two environment variables override the bundle without editing it:

```bash
MAST_BUDGET_MAX_COST_USD=1.00
MAST_BUDGET_MAX_WALLCLOCK_SECONDS=300
```

The intended use is environment differentiation from one bundle — a staging
deployment on a fraction of production's ceiling — not routine tuning.
Tuning belongs in the bundle, where it shows up in a diff and someone
reviews it.

Full field semantics: [workload bundle
reference](/reference/workload-bundle/).
