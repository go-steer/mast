---
title: Interop surfaces
description: Five ways into a running mast daemon — inject, attach, A2A, AG-UI, and federation — aimed at five different callers, each its own trust boundary.
sidebar:
  order: 7
---

An unattended agent is only useful if something can reach it. mast exposes
several surfaces rather than one, because "an alertmanager webhook", "an
operator watching a live incident", "another team's agent", and "a chat UI"
are genuinely different callers with different auth models — and collapsing
them into one endpoint means the weakest one sets the bar.

Every surface is **off by default except inject**, and each carries its own
token. That is deliberate: they are separate trust boundaries, not four
views of one.

## What none of them publish

**A model's thinking is not part of any answer mast gives.** Frontier models
return their reasoning as a distinct kind of content — Anthropic calls it a
thinking block, Gemini a thought part — and it comes back in the same field
as the answer, marked rather than separated. mast reads the mark. Reasoning
is replayed to the provider on the next turn, because a signed thinking block
has to come back intact for the model to continue, and it goes nowhere else:
not into the A2A result artifact, not into an AG-UI `TextMessage` frame or
`RunFinished.result`, not into the `/parks` projection, not into a specialist
result the planner reads, and not into the daemon's own operator log.

This is a floor, not a policy, and one surface has already asked: an AG-UI
workload that sets [`agui.emit_reasoning`](/reference/workload-bundle/)
streams the thinking as its own `REASONING_*` frames. That does not weaken
the floor — it is a *deliberate read* of the thought parts, by name, rather
than the removal of a filter, so the answer stream is unchanged whether the
opt-in is on or off and "reasoning reached a browser" stays a grep for one
symbol. Off by default, and off publishes nothing at all rather than an
empty placeholder. What is ruled out is publishing reasoning by forgetting
to check.

The one thing no opt-in reaches is the **thought signature** — the opaque
blob a provider wants back to continue. It is a replay credential, not a
thought, so it is replayed to the provider and modeled nowhere on any wire.

## Inject — the machine trigger

The daemon's own HTTP endpoint (`--listen`, default `:7777`): `/inject`,
`/resume`, `/abort`, `/parks`, `/metrics`. This is how an incident gets in — an
Alertmanager webhook, a Cloud Scheduler job, a CI step, a `curl` in a
runbook. Bearer auth via `MAST_INJECT_TOKEN`; unset means unauthenticated,
which is a dev-only posture and is warned about at startup.

If you are wiring monitoring to mast, this is the surface. The others are
additions to it.

A Cloud Scheduler job or a Kubernetes CronJob posting here still works, and
it remains the right answer when you already run one — your schedules live
in one place and mast is just another target. What changed in v0.4 is that
you no longer need one to run something periodically: see [no caller at
all](#no-caller-at-all--the-workloads-own-clock) below.

### Reading what is parked

`POST /resume` has been able to *answer* a parked approval since v0.2. What
no out-of-process caller could do until v0.9 was find out what it was
answering — so a chat bot or a web UI wiring up an Approve button could
only offer "approve the thing, whatever it is". That is uninformed consent
with an audit trail, which is worse than the CLI it replaces, because it
looks like the opposite.

Two read-only routes close that:

| route | returns |
|---|---|
| `GET /parks` | every **parked** session, each in full, oldest question first |
| `GET /parks/{session}` | the named session whether it is parked or not — 200 with an empty `parks` list when it is not, 404 only when no such session exists |

Both need `--session-db`; a daemon without a session store answers 404
rather than an empty list, because "nothing to approve" and "I cannot see
parks" are different answers and only one of them means it is safe to stop
looking. Auth is the same rule as `/resume`: the shared token, or a user
from the token table. A caller who may answer a park may read it, and a
caller who may not read it has no business answering one.

The body is always an object with a `sessions` array — for the
single-session route too, and when nothing is parked. Each session carries
its `state`, an `awaiting` (`approval` or `input`, an approval outranking a
question, the same ranking [`turn_state`](#what-turn_state-says) uses), the
open `parks`, an operator `hold` if one is active, and the `applied` calls
the gate [captured a revert for](/reference/write-gate/#what-the-change-overwrote).

An approval park carries a `change`: the tool, the arguments, the rendered
`key`, the gate `policy` that parked it, the proposing specialist, the
`verdict_format` the gate will accept, a `stale` line when a grant the
operator already gave no longer covers the call — and the `change_set` the
call belongs to, when it belongs to one, because an operator cannot
sensibly trade `scope: change_set` for a set they have not been shown. A
question park carries none of that: a question is not a change.

What it does **not** carry is as much of the contract as what it does. No
model output — no reasoning, no narration, none of the assistant text
around the call. No tool results, including the result of the read that
produced a capture: only that read's name, its digest, and the declared
field names travel, because the values are cluster state and can be
anything. And nothing the write gate did not park on. The projection is
written out field by field in `cmd/mast/parks.go`, so a field added to the
transcript cannot reach this wire without someone deciding it should.

A `hold` is reported beside the parks and never as one. Nothing resolves by
answering a hold, so a client that rendered it as a question would put a
prompt in front of somebody with nothing to say back.

## Attach — the operator's live view

`--attach-listen` serves the mast-native attach protocol (HTTP + SSE) that
[mast-web](https://github.com/go-steer/mast-web) speaks: live-tail a
running turn, read a session's transcript, inject an operator message,
answer a parked approval. It needs `--session-db`, since the live tail
pumps from the event log.

### What `turn_state` says

Every `status-update` frame carries a `turn_state`, and a client that
renders "is this session busy, or does it need me?" reads that and nothing
else. It has four values:

| `turn_state` | means |
|---|---|
| `streaming` | a turn is running right now |
| `awaiting_permission` | the [write gate](/concepts/approvals/) parked a mutating call; somebody has to approve or reject it |
| `awaiting_elicit` | the session asked a question — `request_operator_input` — and is waiting for the answer |
| `idle` | none of the above |

The distinction matters because a parked session **finishes its turn**.
The gate does not block inside the call; it records the question durably
and returns, so from the daemon's point of view nothing is running. Before
v0.9 that is exactly what the wire said: a session waiting on a human
reported `idle`, the same string a session that finished its work reports,
and a gateway rendering an approval card had no frame to hang it on.

Two things follow from where the answer comes from. It is read from the
**transcript**, not from in-memory turn state, so it survives a daemon
restart — a session parked yesterday still reports `awaiting_permission`
today. And it is in the **boot snapshot**, the `status-update` every client
receives on connect, so a client that attaches an hour after the park sees
it without having been present for the frame.

Two cases are deliberately not `awaiting_*`. A session paused by
`pause_session` reports `idle`: that is a hold an operator placed, not a
question anyone is waiting on an answer to. And a session resuming from a
park reports `streaming` for the whole resume turn, even though the
interrupt it is answering stays open on the transcript until mid-turn —
a live turn outranks the park it is resolving, because reporting otherwise
would freeze the session on an operator's screen while it works.

One ordering guarantee comes with this. `turn-complete` and `turn-error`
are now held until the turn's own event-log frames have reached every
subscriber. They previously raced: the terminal frame went straight to
subscribers the moment the turn returned, while the answer travelled
through the event log and the pump, so a client that finalized its render
on `turn-complete` could drop the last message. If the log cannot be
queried, or does not catch up within two seconds, the terminal frame is
released anyway and the delay is logged — a late frame is a correctness
bug, a missing one is worse.

### Answering a park over attach

`awaiting_permission` tells a client there is a question; two routes let it
carry the answer back without leaving the attach protocol:

| route | does |
|---|---|
| `GET /sessions/{app}/{id}/perms/stream` | SSE; one `prompt` event per open approval park |
| `POST /sessions/{app}/{id}/perms/respond` | `{"id": "<interrupt id>", "decision": "allow-once"}` |
| `GET /sessions/{app}/{id}/perms` | the rules this session runs under, and what it has already decided |

This is a **second door onto `POST /resume`**, not a second approval model.
A press resolves the same durable park, is attributed to the same
authenticated caller, and is subject to the same rules — including the ones
that make this surface narrower than the protocol it inherited:

- `decision` takes `deny` and `allow-once`. The four wider values the
  protocol defines are refused **400 by name** rather than narrowed to
  allow-once, the same refusal `scope: session` gets on `/resume`. A button
  that means less than it says is a bad button; one that means more is
  worse.
- Answering a park that is already resolved is a **404**. Durability cuts
  both ways: a question that outlives the process can also be answered by
  someone else first.
- A `200` is `{"acknowledged": true, "approver": "<identity>"}`. When
  `approver` is absent, mast recorded nobody — do not render a name.
- `perms_stream` in the capability report is true exactly when these routes
  will answer. A daemon that cannot serve them answers `501`.

Before v0.9 the routes existed and every mast daemon answered `501`: they
came with a ported package built for a synchronous prompt at a keyboard,
and mast has no keyboard. They now sit on the mechanism mast does have.
A park carries `kind: control_plane_write`, which is the field a client
should switch its button set on.

The **read** beside them, `GET /perms`, has its own capability flag —
`perms`, not `perms_stream` — because they are separate wirings. A daemon
can say what its rules are and what it has adjudicated without ever
streaming a live question, and mast is that daemon. The read answers
`mode` (the permissions gate, absent when there is none), `on_mutation`
(the bundle's write-gate policy, on its own axis), and the session's
durable approval log. Through v0.8 it answered `200` with the zero value
on every mast daemon, which is a description of something that gates
nothing; it now refuses instead, and
[the write-gate reference](/reference/write-gate/#reading-what-governs-a-session-over-http)
spells out the fields.

`GET /sessions/{id}/tools` lists the tools the daemon actually holds, each
with a `source`, the MCP `server` it came from if it has one, and a
`gate_state` — what the [write gate](/concepts/approvals/) would do to a
call of it: `allowed`, `prompted` (it parks for approval), or `denied`
(`on_mutation: dry_run`, so it will never run).

Two sources appear. `builtin` is the daemon's own control plane — under
planner dispatch, `invoke_specialist`, `run_shape_llm_router`,
`run_shape_fan_out_fan_in`, `request_operator_input`, and
`pause_session`. `mcp` is everything a server
declared. Builtins sort ahead of the servers and are always listed: unlike
a server, they cannot fail to answer or vanish between polls. The MCP half
is read from the live servers rather than from the bundle's declaration,
and refreshed at most every 30 seconds; a server that fails to answer is
left out and logged rather than blanking the rest — including the
builtins, which stay listed when every server is down.

What is still missing is the handful of tools ADK installs itself:
`finish_task`, and a coordinator's per-specialist transfer tools. mast
never wires those, so it cannot enumerate them without keeping a
hand-written list — and a catalog naming tools that do not exist is worse
than one omitting tools that do.

`GET /sessions/{id}/subagents` lists the roster the daemon **loaded** —
what this thing can do. (`/agents` lists what has been *spawned*, which for
mast is always empty: every dispatch shape resolves its specialists inside
the turn.) Each entry carries the specialist's description, its `model:`
override if it declared one, its declared `capability` (`read_only` or
`change_executor`), its `agent_mode` (`Task` or `SingleTurn`), and an
`invocation` — how the composed root actually reaches it:

| `invocation` | means |
|---|---|
| `parent_tool` | the planner calls it via `invoke_specialist` |
| `transfer` | coordinator dispatch hands the turn to it |
| `graph_node` | a node in a graph, or fan-out's synthesis merger |
| `fanout_branch` | one of fan-out's concurrent analysts |
| *(empty)* | **nothing in this shape reaches it** |

That last row is the useful one. A roster can carry a member the composed
shape never routes to — a `_fallback` under fan-out dispatch, a second
`SingleTurn` spec under graph dispatch — and an empty `invocation` is how
you see it without reading the composition code.

Each entry also carries a `tools` grant, which is the other half of *what
can this thing do*: `capability` says whether the specialist is allowed to
change anything, `tools` says what it can reach.

```json
"tools": {
  "mcp_grant": "listed",
  "mcp": [
    {"server": "gke", "tools": ["get_pod", "get_events"], "whole_server": false},
    {"server": "slack", "whole_server": true}
  ],
  "builtin_declared": ["apply_manifest"]
}
```

`mcp_grant` is the field to read first, because the underlying declaration
means opposite things one character apart. A spec with **no** `mcp:` key
inherits *every* MCP toolset the workload has (`"all"`); a spec that writes
`mcp: []` is denied *all* of them (`"none"`); a non-empty list is a
whitelist (`"listed"`, and only then is `mcp` present). Both of the first
two spellings are an empty list on the wire, so a catalog that just
transcribed the declaration would show the deny-all specialist and the
reach-the-whole-cluster one identically. `whole_server` draws the same
distinction per entry: a listed server with no `tools:` of its own passes
whole.

`builtin_declared` is named for what it is: nothing in the declaration
installs a tool, so the list grants and narrows nothing. What reads it is
the capability-split check, which refuses a `read_only` specialist that
lists a mutating name, the fan-out branch check, which does the same for an
analyst, and the startup log that reports a workload's declared write
surface. Read it as a claim the spec makes about itself and is held to, not
as a set of tools the specialist holds.

The one built-in mast does install on a Task specialist is
[`retrieve_raw`](/reference/mcp-servers/#digesting-large-tool-responses),
and it deliberately does not appear here: it is a property of the daemon's
`--mcp-digest` setting rather than of the roster, and it returns bytes the
specialist has already received, so it widens no grant this catalog
describes.

`GET /sessions/{id}/guardrails` answers the question an operator actually
has when a session stops responding: *what stopped it, and what do I do?*
It reports the budget ceilings in force and the usage against them across
all three dimensions plus each specialist's own, and the watchdog's posture
(`advisory: true` under `--watchdog=warn` and under the default
`--watchdog=feedback`, which corrects but never stops; `false` under
`--watchdog=enforce`, whether or not it has fired yet).

#### The signals

The watchdog raises three signals by default, and each names a different
way an unattended run goes wrong:

| signal | severity | fires when |
|---|---|---|
| `repeated-tool-call` | Critical | the same call, five times running — path spellings (`./main.go`, `/workspace/main.go`) count as the same file |
| `alternating-tool-cycle` | Critical | a short loop repeating, e.g. `list_agents → check_agent` three times over — the shape a consecutive-repeat check is structurally blind to |
| `tool-failure-streak` | Warn | three calls in a row all returned errors, so nothing the agent ran has verified the state of anything |

The last one is the one to read carefully in a report. It does not mean the
agent was stuck; it means the agent's conclusions rest on tools that all
failed. It stays Warn deliberately: three denials into a legitimate RBAC
probe is not a runaway, and halting there would make the backstop the
outage.

A fourth signal, **`dominant-tool-call`**, ships and is *not* wired by
default. It covers the shape between the first two — one call repeated
with occasional others wedged in (`a a a b a a a c a a a`), which resets
the repeat detector's run and shows the cycle detector no repeating
block. Density does not care where the interleaves fall, so it reaches
the verdict inside one twelve-call window instead of waiting for the
interleaves to stop; when they are the whole delay, that is most of a
loop's cost. It is off by default because a third Critical detector
changes what every unattended workload is told about itself under
`feedback`, and a polling workload with any variation in it is exactly a
dominant call with interleaves. A [library
embedder](/quickstart/library-embed/) wires it by constructing
`watchdog.DefaultWatchdog` with its own signal list including
`watchdog.NewDominantToolCallSignal(12, 8)`; alongside the other two it
stands down wherever one of them already owns the shape, so one loop
still produces one alert.

Severity is a property of the pattern, not of the posture. What changes
between postures is the reaction, and the three of them are a ladder —
each rung includes the one below it:

- **`--watchdog=warn`** logs every alert and lets the turn run.
- **`--watchdog=feedback`** (default) also tells the model. Each alert carries a
  model-facing sentence alongside the operator-facing one, and on the
  session's next turn those are prepended to the prompt as a `[watchdog]`
  block: *an automated observation about your own previous turn — this is
  not a message from the user*. Every posture below this one routes the
  observation to a reader who may not be there; the model about to repeat
  the call is the party that can decide not to. It is a correction, not a
  backstop — nothing stops a model that reads the block and loops anyway,
  which is why a workload with a bounded tool loop still wants `enforce`.
- **`--watchdog=enforce`** also cancels the turn in flight on a
  **Critical** alert and refuses the session's every subsequent turn —
  auto-resume, a scheduled fire, and an attach inject all included — until
  an operator resets. The cancel happens *during* the turn, not at its
  boundary: the loop the watchdog catches usually lives inside a single
  turn, and a reaction that waits for the turn to end waits for the thing
  it is supposed to stop.

A turn cancelled that way — and a turn an operator stops with
`POST /sessions/{id}/interrupt` — comes back on the stream as a `turn-error`
of kind `canceled`, with `retryable: false`. That is the flag a client keys
its "run it again" affordance off, and both of these are somebody deciding
the turn should stop; offering to re-drive the loop the watchdog just halted
would be the wrong end of the decision. A turn that ran out of *time* is
still `transient_network` and still retryable, because nobody asked for that
one.

The halt itself — the refusal the tripping turn returns, and the one every
turn after it gets until a reset — comes back as kind **`watchdog_halt`**,
also `retryable: false`, with a hint naming the reset endpoint. It is a
separate kind from `canceled` because it describes a standing state rather
than one stopped turn: retrying a `canceled` turn is arguably the operator's
call, while retrying a `watchdog_halt` fails identically until somebody
clears it. Both are separate from `cost_ceiling`, whose remedy is a bigger
budget rather than a fixed loop.

`enforce` including `feedback` is deliberate. An enforce halt is cleared
by an operator reset, and a reset resumes a model whose context still ends
in the loop it was halted for; without the injected observation the very
next turn re-issues the same call, and the reset is a treadmill. For the
same reason, a reset clears the halt but **keeps** the queued observation.

The `[watchdog]` block is steering, not a trust boundary — nothing
downstream grants authority based on it, and a user prompt may contain the
literal string.

#### Where the posture comes from

Three sources, in order: **`--watchdog` beats the bundle's
`safety.watchdog` beats mast's default.** The daemon logs which one won at
startup (`watchdog posture resolved mode=… source=…`), because a posture
nobody can see is a posture nobody audits — and `enforce`, the one an
operator most needs to know is armed, otherwise announces itself only by
refusing a turn.

```yaml
# workload.yaml
safety:
  watchdog: enforce
```

The bundle is where a workload ships its own backstop: mast's deployment
unit is the bundle, and without this every invocation and every deploy
manifest had to carry the flag by hand. The flag sits above it so an
operator debugging a halted workload can drop the posture for one run
without editing — and later forgetting to revert — the deployed manifest.

**The default is `feedback`.** Not `warn`: every mast run is unattended,
so warn routes the alert to a log nobody is tailing, which is
indistinguishable from off. Not `enforce` either: `alternating-tool-cycle`
has a workload-shaped false positive — a scheduler-driven daemon watching
a rollout settle calls the same tool with the same arguments on purpose —
and on an unattended deployment a false halt is an outage that waits for
the morning. A false `feedback` costs one paragraph the model is free to
disregard. Recoverable beats unrecoverable when nobody is watching. Set
`safety.watchdog: enforce` on a workload whose tool loop is bounded by
construction — a triage run that reads, concludes, and stops.

The library-embedded surface (`mast.RunWorkload`) reads the same
`safety.watchdog` field and taps the same signals, with the rungs bounded
by what that surface holds: `enforce` abandons the runaway turn, but there
is no cross-call session state for the "refuse every later turn" half, and
no next turn for `feedback` to inject into.

#### It watches inside a planner dispatch too

Under `planner` dispatch, `invoke_specialist` runs each specialist on a
runner of its own, and a private runner is a private event stream. Through
v0.4 that meant a specialist spinning inside one dispatch emitted its calls
where the watchdog could not see them — `--watchdog=enforce` could not halt
a loop it could not observe, which is the exact shape it exists to catch.
Since v0.5 the dispatch feeds those events to the **session's** watchdog,
the same one the outer stream feeds.

The session's, deliberately, and not one minted per dispatch: the signals
count repetition, so a per-dispatch watchdog would reset the count every
time a dispatch ended and a specialist making three identical calls in each
of ten dispatches would never reach a threshold of five. The cost is worth
knowing — the planner's own calls and its specialists' now land in one
signal set, so an `invoke_specialist` between two dispatches breaks a
consecutive run the repeat detector was building. That is the interleaved
shape `alternating-tool-cycle` and the opt-in dominant-tool signal exist
for, and it is the same trade-off the outer stream already makes between a
coordinator and its sub-agents. The loop this catches — a specialist
spinning inside *one* dispatch — has no interleaver at all.

**A trip here halts the session, not just the dispatch.** This is the
deliberate difference from a crossed budget cap, which stops only the
dispatch and hands the planner the reason
([budgets](/concepts/budgets/#a-spent-specialist-closes-one-path-not-the-session)).
A ceiling is
cumulative, so stopping the sub-run is the whole remedy. A watchdog trip is
a latch meaning *this session is behaving pathologically and an operator
must reset it*, and every other door to it refuses the whole session; a
halt that stopped only the dispatch would let the planner re-dispatch the
same specialist immediately, which is the treadmill `enforce` exists to
break. So both fire: the dispatch stops with a labelled partial, and the
turn is cancelled. Under `warn` and `feedback` nothing is cancelled — the
alert is retained and reportable, and the model-facing paragraph goes to
the **planner**, since the specialist's sub-session is already gone and the
planner is the one that would otherwise dispatch again.

Library embeds that compose the planner themselves get this by wiring
`compose.RootConfig.SubRunObserver`, the same seam the meter uses;
`mast.RunWorkload` and the daemon already do.

#### A halt outlives the process that observed it

An `enforce` halt is written to the session database, and a daemon that
restarts adopts it on the halted session's next turn — before any model
call, whichever surface drives that turn. A halt a restart cleared would
not be a halt: mast's restarts are automatic and unattended, so the loop →
halt → crash → restart cycle `enforce` exists to break would simply
resume, each restart handing the loop a clean backstop. The reset is
durable in the same place, so clearing a halt clears it for good, and the
row records who cleared it and what runway they added.

Two things to know about it:

- **It needs `--attach-listen`** (which already requires `--session-db`).
  The reset endpoint is attach-only, so persisting a halt on a daemon with
  no attach surface would leave an operator no way to clear it. Starting
  with `--watchdog=enforce` and no attach listener logs a warning saying
  exactly that.
- **The posture still wins over the history.** A deployment dialed back
  from `enforce` does not inherit a halt it would no longer produce.

Restore fails open: an unreadable guardrail table logs a warning and the
turn runs. A storage fault must not halt every session in the deployment
with no trip behind it, and the per-turn backstops are all still armed.

Budget *spend* is durable in the same place and fails open the same way —
a ledger of priced calls, folded back before the first turn after a
restart — but **not on the same terms**: it needs `--session-db` and not
`--attach-listen`. A ledger is not a latch, so the reset-endpoint argument
above does not reach it, and there is nothing an operator has to be able
to clear. Through v0.6.0 it inherited that argument anyway by riding the
same database handle, which denied a durable ceiling to unattended daemons
— the deployment least likely to bind an operator socket and most likely
to crash-loop (issue #274). See
[spend survives a restart](/concepts/budgets/#spend-survives-a-restart).

`POST /sessions/{id}/guardrails/reset` is the way out: a budget trip is
otherwise permanent, since enforcement is re-derived from usage against the
ceiling on every priced event. See
[getting unstuck after a trip](/concepts/budgets/#getting-unstuck-after-a-trip)
for what a reset does and the three things it deliberately refuses to do.

Attach can read transcripts and drive turns, so it gets its own token
(`MAST_ATTACH_TOKEN`) and a hard rule: a non-loopback bind without auth is
**refused**, not warned about. It also stays up through a shutdown drain,
so an operator watching a finishing turn sees its last events rather than a
dropped connection.

### Who can see a session

A session created over attach is owned by the caller who created it, and
that ownership is what the per-session authorization matrix is built on:
the owner reads and writes, *viewers* read, *contributors* read and write,
and everyone else gets a 404 — not a 403, so an unauthorized caller cannot
enumerate which sessions exist by the shape of the refusal.

`GET /sessions/{id}/acl` reads that list back and `PUT` replaces it:

```json
{"owner": "alice@example.com",
 "viewers": ["carol@example.com"],
 "contributors": ["bob@example.com"],
 "enforced": true, "persisted": true}
```

The `PUT` is a whole-document replace, not a patch. A list you omit is a
list you cleared — which is the only sane reading, because the alternative
(omission means "leave it alone") would leave no way to *remove* the last
viewer, and revocation is half of why the endpoint exists. Identities are
trimmed and de-duplicated; a blank one is a 400 rather than a stored grant
that could never match a caller.

`GET` sits at the read bar, not the admin bar. Anyone the ACL already
admits can read the whole transcript, so the membership list is not the
sensitive part — and a viewer who wants write access needs to know whom to
ask. `PUT` is admin: the owner, or a daemon admin.

Two things the response tells you that the ACL itself cannot:

- **`enforced`** is false when the daemon is not running multi-session, in
  which case the ACL governs nothing and every authenticated caller gets
  in. A `PUT` against such a daemon is **refused with 501** rather than
  accepted: an amendment nothing consults would report success for an
  access restriction that does not exist.
- **`persisted`** is false when the amendment lives only in this process —
  no ACL store wired, or a session registered without an owner (the
  daemon's own bootstrap session is one). The grant works until the daemon
  restarts, and saying so is the difference between a durable decision and
  one that quietly evaporates overnight.

When `persisted` is true the grant outlives the session's stay in memory.
Idle sessions are evicted and rebuilt from disk on the next request, and a
restart evicts all of them; either way the ACL that comes back is the one
on disk, not one reconstructed by whatever rebuilt the session. That
matters most for the grant you just made: a viewer added at 4pm is still a
viewer after the overnight restart, and an owner whose session was swept
out is not locked out of it by the act of asking for it again.

Ownership **transfer** — sending a different `owner` — is daemon-admin
only. It is there for the case where the owner left, and it fails with a
403 for anyone else rather than being silently dropped, because an ignored
field in an accepted request reads as a completed transfer. An owner
cannot be cleared at all: a session with no owner is reachable by admins
alone, which is a lockout rather than an edit.

## A2A — other agents

`--a2a-listen` publishes an [A2A](https://a2a-protocol.org) agent card and
a JSON-RPC endpoint (`message/send`, `tasks/get`, `tasks/cancel`,
`message/stream`) for workloads that opt in with `a2a.expose: true`.

The opt-in is per workload and never automatic, because publishing an agent
card is an *external contract*: you are telling other systems this skill
exists and is callable. `MAST_A2A_TOKEN` enables per-skill scope
enforcement; a non-loopback bind without a token is refused, since
`tasks/cancel` is destructive.

Use it when another team's agent — or another mast deployment — should be
able to hand you work.

## AG-UI — user-facing clients

`--agui-listen` serves an [AG-UI](https://docs.ag-ui.com/introduction)
run endpoint per opted-in workload, plus a discovery descriptor, for
CopilotKit apps and chat-platform bots. Also opt-in
(`agui.expose: true`), also its own token (`MAST_AGUI_TOKEN`), with rate
limiting, since a run drives a budgeted turn.

The one concept worth deciding up front is `agui.session_model`:
`per_thread` (default) maps one continuing mast session to each AG-UI
thread, which is what chat UX expects; `per_run` gives every run a fresh
session, which is right for stateless one-shots. Either way the daemon
derives and namespaces the session id — a client never supplies a raw one.

Two publication surfaces sit behind their own bundle keys, both empty-or-off
by default and both silent rather than redacted when off:
`agui.state_projection` names the session-state keys a run may publish as
`StateDelta` patches, and `agui.emit_reasoning` lets the workload stream the
model's thinking as `REASONING_*` frames. Neither is a display preference —
the client on the other end is a browser — which is why the defaults publish
nothing and the daemon logs what a running workload has enabled.

Because both are per-bundle, a client cannot infer either from the protocol
version, and inferring them from a finished run is hopeless: a stream with no
reasoning in it looks the same whether the workload publishes none or the
model simply didn't think. So the public discovery descriptor states them up
front, as a
[`capabilities` object](/reference/cli/#ag-ui-server) per workload. It is
read from the same value the run's emitter is built from rather than
recomputed from the bundle — a capability claim that restates a config
instead of reading what it describes is how `/tools` and `/perms` each spent
releases advertising something untrue.

## Federation — calling out

The surfaces above are inbound. `invoke_remote_agent` is the outbound
counterpart: a specialist calling another agent over A2A.

It classifies as **mutating**, unconditionally. Effects on the far side of
a federation call are invisible from here, and a call whose consequences
you cannot see is not one to fire unattended — so it goes through [the
write gate](/concepts/approvals/) like any other change.

## No caller at all — the workload's own clock

Every surface above answers "what reached the daemon". A workload that
declares
[`edge_trigger.scheduled`](/reference/workload-bundle/#scheduled--a-workload-that-wakes-itself)
answers nothing: it wakes on its own interval, and the run has no caller to
take an identity from. That is why it runs as `mast:scheduler` — a
namespaced identity no human login can produce — and why a mutating call
inside a scheduled run still parks for a real approver. The trigger is a
bundle field rather than a surface: nothing is listening, so there is no
token and no trust boundary to configure.

Which to use is a question about where your schedules should live, not
about capability. An external scheduler posting to `/inject` gives you one
place to see every job your org runs and one place to change them; the
bundle's own cadence gives you a workload that is still periodic after it
is copied to a cluster where that scheduler does not exist.

## Picking one

| The caller is… | Surface |
|---|---|
| Alertmanager, CI, a runbook `curl` | **inject** |
| A scheduler you already run — Cloud Scheduler, a CronJob | **inject** (it is just another caller) |
| Nothing — the workload should run periodically on its own | `edge_trigger.scheduled`, no surface |
| A human operator watching or steering an incident | **attach** (mast-web) |
| Another agent, in or out of your org | **A2A** |
| A chat UI or CopilotKit app with end users in it | **AG-UI** |
| mast calling *out* to another agent | `invoke_remote_agent` |

Wire shapes, flags, and auth details for all of them: [CLI
reference](/reference/cli/). Opt-in fields and per-skill policy:
[workload bundle](/reference/workload-bundle/).
