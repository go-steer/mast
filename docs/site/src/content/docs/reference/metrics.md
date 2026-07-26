---
title: Metrics
description: The seven fixed Prometheus counter families mast exports, and the env-gated OTel trace export.
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

## Traces

Trace export is env-gated OTel: a no-op unless `OTEL_EXPORTER_OTLP_*`
endpoints are set. mast opens no spans of its own in v0.1 — ADK v2's
runner emits the span tree; mast only exports it. There is no
OTel-*metrics* export in v0.1 (Prometheus scrape only).
