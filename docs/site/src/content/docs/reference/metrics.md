---
title: Metrics
description: The fixed Prometheus counter families mast exports, and the env-gated OTel trace export.
sidebar:
  order: 4
---

mast's metric registry is **fixed**: every family name lives in
`pkg/observability` and only there. Callers increment pre-declared families
through typed methods and cannot mint new names or labels — that's the
cardinality-control point. Session IDs are never metric labels;
correlation at session grain goes through logs and traces.

`/metrics` is served on the inject listener (same port as `/inject`; a
separate metrics port is revisited in v0.2). All families are materialized
at zero on startup, so `rate()` / `increase()` have a defined origin.

## Counter families

| Family | Labels | Meaning |
|---|---|---|
| `mast_turns_total` | `workload`, `outcome` | Turns driven through the runner. Outcomes: `ok`, `error`, `budget_exceeded`, `watchdog_halt` (stopped by the behavioral watchdog under `--watchdog=enforce`). |
| `mast_model_calls_total` | `workload` | Model calls observed on the event stream (events carrying usage metadata). |
| `mast_tokens_total` | `workload`, `kind` | Provider tokens, by kind: `prompt`, `candidates`. |
| `mast_cost_usd_total` | `workload` | Accumulated cost in USD, derived by the budget meter's pricing model. |
| `mast_hitl_pauses_total` | `workload` | HITL interrupts emitted (durable RequestInput events). |
| `mast_hitl_resumes_total` | `workload` | HITL resumes fed back into paused sessions. |
| `mast_budget_trips_total` | `workload` | Turns aborted because a budget ceiling was crossed. |

The session eventlog is the source of truth; these metrics are a real-time
*view* folded from the same event stream the budget meter observes.

### Durable-execution families (v0.2)

The v0.2 durable-execution surface — pause/abort, planned stop, boot-time
auto-resume — spans five counter families. The `mast_autoresume_total` family
shipped with boot-time auto-resume (#41); the fixed-registry pass (#50) added
the four below it and canonicalized the whole surface. Each advances only when
the durable operation it names actually happened (the pause was recorded, the
boot pass reached a disposition) — except `mast_marker_write_failures_total`,
which is the inverse: it advances only when a marker write *failed*, surfacing
an otherwise-silent loss. So a nonzero value is always evidence of the event
the family names, not just an attempt.

| Family | Labels | Meaning |
|---|---|---|
| `mast_autoresume_total` | `workload`, `outcome` | Boot-pass dispositions per interrupted session. Outcomes: `resumed`, `cleared`, `skipped_stale`, `skipped_ambiguous`, `skipped_loopbreak`, `skipped_superseded`, `skipped_unsupported`, `error`. |
| `mast_marker_write_failures_total` | `workload`, `operation` | Durable marker writes that failed (otherwise silent). Operations: `mark` and `clear` (interruption marker), `pause` (planned-stop gate-pause write). |
| `mast_aborts_total` | `workload` | Terminal aborts whose durable marker landed. |
| `mast_gate_pauses_total` | `workload`, `source` | Out-of-turn gate pauses recorded. Sources: `operator`, `planned_stop`. |
| `mast_timed_pause_fires_total` | `workload`, `outcome` | Timed-pause scheduler fires. Outcomes: `resumed`, `skipped`, `error`. |

### A2A server family (v0.2)

The [A2A server](/mast/reference/cli/#a2a-server) counts task-lifecycle
transitions it drives. The `outcome` label is an A2A task-state value, kept in
lockstep with the wire vocabulary.

| Family | Labels | Meaning |
|---|---|---|
| `mast_a2a_server_tasks_total` | `workload`, `outcome` | A2A server task-lifecycle transitions. Outcomes: `submitted`, `working`, `input-required`, `completed`, `failed`, `canceled`, `rejected`. |

### AG-UI server family (v0.2)

The [AG-UI server](/mast/reference/cli/#ag-ui-server) counts each run it drives
to a terminal frame, plus a duration histogram over runs that reached the turn
(pre-turn refusals — draining, an unaddressable session id — are not timed).
The `outcome` label is kept in lockstep with the server's terminal-frame
vocabulary.

| Family | Labels | Meaning |
|---|---|---|
| `mast_agui_runs_total` | `workload`, `outcome` | AG-UI runs by terminal disposition. Outcomes: `success`, `error`, `aborted`, `interrupted`, `rejected`. |
| `mast_agui_run_duration_seconds` | `workload` | Histogram of executed-run wallclock (a `_bucket`/`_sum`/`_count` triple). |

### Scheduled-trigger family (v0.4)

A workload that declares
[`edge_trigger.scheduled`](/reference/workload-bundle/#scheduled--a-workload-that-wakes-itself)
counts every tick it accounts for, including the ones it deliberately did not
run. `missed` is the one to alert on: it advances once per tick coalesced away
after an outage, so a nonzero rate is the cadence telling you the daemon was
not there — and it is the only place that shows up, because mast does not
catch up on a missed tick.

| Family | Labels | Meaning |
|---|---|---|
| `mast_scheduled_fires_total` | `workload`, `outcome` | Scheduled-trigger ticks by disposition. Outcomes: `ran`, `skipped` (came due during a drain), `error` (the run failed; the tick is spent, the next tick is the retry), `missed` (coalesced away — the daemon was down when it came due). |

## Traces

Trace export is env-gated OTel: a no-op unless `OTEL_EXPORTER_OTLP_*`
endpoints are set. Most of the tree comes from ADK — `invoke_agent`,
`generate_content`, `execute_tool`, `invoke_node` — and mast exports it.
There is no OTel-*metrics* export (Prometheus scrape only).

### `mast.turn`

Every turn opens one span of mast's own, and ADK's tree hangs beneath
it. This is what makes an unattended turn readable: a scheduled fire, an
auto-resume, or a `mast run` has no HTTP request behind it, so without
it ADK's `invoke_agent` is a **trace root** — a trace that starts
nowhere, with nothing on it naming the session. Turns that *do* come
from a request (inject, resume, attach, A2A, AG-UI) already have a
server span, and `mast.turn` takes its place under that one, so the
inject and the turn it caused are one trace.

The span opens **before** the session's turn lock, so a turn refused at
the chokepoint still leaves a record.

| Attribute | Type | Meaning |
|---|---|---|
| `mast.session.id` | string | The session the turn ran on. Deliberately not a metric label — session-grain questions are trace questions. |
| `mast.workload.name` | string | Same workload name the counter families are labelled by. |
| `mast.turn.kind` | string | What drove the turn: `inject`, `attach`, `resume`, `scheduled`, `autoresume`, `a2a`, `agui`, `oneshot`. |
| `mast.turn.detail` | string | The particulars of that one turn — the inject's reason, the interrupt ID a resume answered, the tick a scheduled fire was due at, the A2A method. Absent when the kind has no detail. |
| `mast.turn.outcome` | string | How it ended. The `mast_turns_total` vocabulary (`ok`, `error`, `budget_exceeded`, `watchdog_halt`) plus `refused` — see below. |
| `mast.turn.queued_ms` | int | How long the turn waited for the session's turn lock. One session runs one turn at a time, so on a busy session this is latency that otherwise reads as a slow model. |
| `mast.cost.usd` | float | What *this turn* added, not the session total. Absent on a turn refused before the runner. |

The span status is `Error` on any failing outcome, with the error
recorded as a span event.

`refused` is a span-only outcome. `mast_turns_total` has only ever
counted turns that started, and a turn stopped at the chokepoint — an
aborted session, a gate pause, a watchdog halt on entry — never started
one. Changing that would move every dashboard's denominator, so the
counter is left alone and the span carries the refusal instead. If
you're asking "why didn't my inject run", that is a trace query, not a
metrics one.
