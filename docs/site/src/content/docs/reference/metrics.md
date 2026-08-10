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
| `mast_turns_total` | `workload`, `outcome` | Turns driven through the runner. Outcomes: `ok`, `error`, `budget_exceeded`. |
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

## Traces

Trace export is env-gated OTel: a no-op unless `OTEL_EXPORTER_OTLP_*`
endpoints are set. mast opens no spans of its own in v0.1 — ADK v2's
runner emits the span tree; mast only exports it. There is no
OTel-*metrics* export in v0.1 (Prometheus scrape only).
