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
all three dispatch shapes.

## Two limits worth knowing

**1. A ceiling is crossed *by* the call that reports it.** Cost and token
counts arrive with a model response, so mast learns a call was expensive
after it happened. `max_cost_usd: 5.00` means "stop once spend has reached
$5", not "never exceed $5" — the call that carries you over completes and
is billed. Size the cap with one call's worth of headroom, especially with
large-context models.

**2. A crossed specialist ceiling stops the session, not just the
specialist.** When a specialist trips its own cap, the whole run stops —
the coordinator does not route around it and try someone else. A
per-specialist ceiling is a safety limit, not a routing hint; treating it
as "try the next one" would turn a tight cap into a way to burn the
workload budget across the roster.

Both are deliberate, and both are the conservative reading. Neither is
something to discover during an incident.

## Getting unstuck after a trip

A trip is not a one-time event. Enforcement is re-derived every priced
event by comparing the session's accumulated usage against its ceiling —
there is no "tripped" flag to clear — so a session that is past its cap is
past it on the next turn too, and the one after that. Left alone, it
refuses every prompt until the process restarts.

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
which matters because they fail differently: a session stopped by
`max_turns: 40` has spent a fraction of a cent, and a session stopped by
one specialist's `max_cost_usd: 0.25` has plenty of workload budget left.
Both look like "the agent stopped answering".

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
Raising a specialist above the workload's cap still buys nothing — the
workload ceiling is enforced independently, and it is still the outer
bound.

Every reset is logged with the authenticated caller that requested it.

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
