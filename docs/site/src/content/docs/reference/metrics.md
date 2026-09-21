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
separate metrics port is still deferred). All families are materialized
at zero on startup, so `rate()` / `increase()` have a defined origin.

This page is held to a real scrape by a test, in both directions: a
family the registry exports and this page omits fails the build, and so
does a name, a label, or an outcome value on this page that nothing
exports. What is written below is what a `curl /metrics` returns.

## Counter families

| Family | Labels | Meaning |
|---|---|---|
| `mast_turns_total` | `workload`, `outcome` | Turns driven through the runner. Outcomes: `ok`, `error`, `budget_exceeded`, `watchdog_halt` (stopped by the behavioral watchdog under `--watchdog=enforce`), `refusal_loop` (stopped by the [write gate](/concepts/approvals/#a-refusal-the-model-cannot-talk-its-way-past) because the model kept re-proposing a call an operator had refused). |
| `mast_model_calls_total` | `workload` | Model calls observed on the event stream (events carrying usage metadata). |
| `mast_tokens_total` | `workload`, `kind` | Provider tokens, by kind: `prompt`, `candidates`. |
| `mast_cost_usd_total` | `workload` | Accumulated cost in USD, derived by the budget meter's pricing model. |
| `mast_hitl_pauses_total` | `workload` | HITL interrupts emitted (durable RequestInput events). |
| `mast_hitl_resumes_total` | `workload` | HITL resumes fed back into paused sessions. |
| `mast_budget_trips_total` | `workload` | Turns aborted because a budget ceiling was crossed. |

The session eventlog is the source of truth; these metrics are a real-time
*view* folded from the same event stream the budget meter observes.

A token count a provider reports as negative is read as zero everywhere —
these counters, the budget meter's totals, the durable spend ledger and
`GET /usage`. A counter that only goes up cannot take a bad delta back, so
an impossible count is dropped rather than subtracted; the call itself
still counts in `mast_model_calls_total`.

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

The [A2A server](/reference/cli/#a2a-server) counts task-lifecycle
transitions it drives. The `outcome` label is an A2A task-state value, kept in
lockstep with the wire vocabulary.

| Family | Labels | Meaning |
|---|---|---|
| `mast_a2a_server_tasks_total` | `workload`, `outcome` | A2A server task-lifecycle transitions. Outcomes: `submitted`, `working`, `input-required`, `completed`, `failed`, `canceled`, `rejected`. |

### AG-UI server family (v0.2)

The [AG-UI server](/reference/cli/#ag-ui-server) counts each run it drives
to a terminal frame, plus a duration histogram over runs that reached the turn
(pre-turn refusals — draining, an unaddressable session id — are not timed).
The `outcome` label is kept in lockstep with the server's terminal-frame
vocabulary.

| Family | Labels | Meaning |
|---|---|---|
| `mast_agui_runs_total` | `workload`, `outcome` | AG-UI runs by terminal disposition. Outcomes: `success`, `error`, `aborted`, `interrupted`, `rejected`, `queue_full`. `rejected` is a pre-stream refusal of the caller (auth, scope, rate limit, drain, a resume with nothing to resume); `queue_full` is a run shed because its thread already had `agui.run_queue.depth` + 1 runs in flight, which is the one refusal an operator answers with capacity or a bundle key rather than with a caller's credentials. |
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

### Monitoring-notification family (v0.5)

A workload that declares
[`monitor.notify`](/reference/workload-bundle/#notify--speaking-only-when-something-changed)
counts every cycle by what it told the chat ingress — including, and mostly,
the cycles that told it nothing. **A healthy monitor is mostly `quiet`**, so
these two families are read together with `mast_model_calls_total`: quiet
cycles never wake the model, which is the whole point of the feature, and a
`quiet` rate that stops climbing while the cadence keeps firing means
something is now speaking on every cycle.

`error` is the one to alert on. There is no retry and no spool: a send that
failed is gone, and the next cycle reports what is new then. `health` counts
the notices mast writes itself when the monitoring is broken or has
recovered — one on each edge, not one per failing cycle.

| Family | Labels | Meaning |
|---|---|---|
| `mast_monitor_notifications_total` | `workload`, `outcome` | Monitoring cycles by what they told the chat ingress. Outcomes: `posted` (opened a message), `appended` (extended the open one), `replaced` (the ingress had forgotten the body, so the whole text was re-sent), `rolled` (the message was full and the timeline moved to its continuation), `quiet` (nothing changed — no request, no model call), `health` (a mast-authored broken/recovered notice), `error` (the send failed). |
| `mast_monitor_digest_wakes_total` | `workload` | Cycles that spoke because `digest_after` expired rather than because anything changed. Separate from the outcome above because "did anything change" and "did the deadman have to fire" are different questions: a workload whose digest wakes are its *only* notifications is one nothing has happened to, or one whose classifier has quietly stopped classifying. |

### Monitoring-ack family (v0.5)

The other direction: a workload that declares
[`monitor.ack`](/reference/workload-bundle/#ack--taking-an-acknowledgement-back)
counts the operator acknowledgements that arrived on `POST /monitor-ack` and
were forwarded to whoever owns the finding state.

Two outcomes and no more, because mast is doing two things here and neither
has a middle state: it recorded who asked, and it forwarded. `error` covers
both halves failing and is the one to alert on — an operator who pressed the
button and was told nothing believes an alert is muted that is not, and the
next cycle will report the subject again. There is no retry: acking again is
the recovery, and the audit shows both attempts.

This family counts acks, not suppressions. How long one lasts, and whether a
repeat was redundant, is the producer's business; mast forwards a repeat ack
regardless and counts it.

| Family | Labels | Meaning |
|---|---|---|
| `mast_monitor_acks_total` | `workload`, `outcome` | Operator acknowledgements taken on the daemon ingress. Outcomes: `forwarded` (recorded and accepted by the producer's ack tool), `error` (the durable record or the forward failed — the suppression did not take). |

### Planner-dispatch family

A [planner](/concepts/specialists-and-dispatch/) runs each `invoke_specialist` delegation on a
runner of its own, and counts how each one ended.

`rate_limited` is the one to alert on, and it is split out of `failed` for a
reason that is visible nowhere else: a delegation lost to a provider rejection
does **not** fail the turn. The planner is handed the error as an ordinary
tool result and is free to do the specialist's work itself — at full
parent-context cost, with none of the specialist's instruction, on a run that
still ends `ok` in `mast_turns_total`. A climbing `rate_limited` rate beside a
flat turn-error rate is that substitution happening.

`halted` is the system working: a budget ceiling or a watchdog trip stopped
the specialist and the planner was told so in a labelled partial. It is
counted apart from `failed` because nothing broke.

| Family | Labels | Meaning |
|---|---|---|
| `mast_dispatches_total` | `workload`, `outcome` | Planner `invoke_specialist` dispatches by how each one ended. Outcomes: `ok` (the specialist returned a result), `halted` (a ceiling or a watchdog trip stopped it early; the planner got a labelled partial), `rate_limited` (the provider rejected a model call under the specialist), `failed` (any other error under the specialist). |

Counted per dispatch and not per specialist: the specialist name is
workload-authored and unbounded, and a label with a workload's vocabulary in
it is a cardinality bill the operator did not agree to. The specialist is in
the `WARN` log line the same failure writes, alongside the session id.

### Provider-retry family

One tier below the dispatch family. That one counts what a provider rejection
did to a *delegation*; this one counts the rejection itself, for every model
call the process makes — planner, specialist, graph node, plain turn.

A rejection is retryable when the provider says HTTP 429 or 503, read off the
provider's own error type and never off the message text. (A model grading a
corpus about exhausted quotas would otherwise retry on its own output.) The
schedule is deliberately short — one retry, after two seconds — and behind a
process-wide cooldown of one minute: an unattended turn that waits forty
seconds has stopped being late and started being wedged, and a fan-out of
eight calls meeting one shed at the same instant should produce one retry, not
eight.

| Family | Labels | Meaning |
|---|---|---|
| `mast_provider_retries_total` | `workload`, `outcome` | Model calls that met a transient provider rejection, by what the retry did about it. Outcomes: `recovered` (the second attempt returned content), `exhausted` (the schedule ran out; the provider's error is what the caller got), `declined` (the cooldown refused — another call had just taken the window). |

`recovered` is the series worth a dashboard even though nothing went wrong.
It is the only trace a shed-and-recovered call leaves anywhere: the turn
completes, returns content, and looks unremarkable except for being two
seconds slower. A `recovered` rate climbing week over week is a provider under
worsening pressure, visible before it stops being transient.

`declined` is mast reporting on mast rather than on the provider. Sustained,
beside `mast_dispatches_total{outcome="rate_limited"}`, it says the fan-out is
wider than the quota — which no amount of retrying fixes.

A call that met no rejection increments nothing, and neither does one that
failed some other way: a 400 is not provider pressure and does not belong in
this family's denominator. The model name is in the `WARN` line rather than in
a label, for the same cardinality reason as the specialist name above.

Only a running workload retries. mast's own eval harness wraps its model calls
in a longer, cooldown-free schedule of its own and reports nothing here: a
measurement that waits four times is still measuring, and a workload that does
is wedged.

## Park announcements

A durable [approval park](/concepts/approvals/) is discoverable four ways, and
three of them require somebody to look first. `--park-notify <conversation>`
is the fourth: it announces the park to the chat ingress `--notify-url`
configures, once, when the park opens.

This family only moves on a daemon that set that flag. A workload with no
park egress increments nothing, and that silence is configuration rather than
a fault — so the denominator here is parks *announced*, not parks *raised*.

| Family | Labels | Meaning |
|---|---|---|
| `mast_park_notifications_total` | `workload`, `outcome` | Parks by whether the operator was told about them out of band. Outcomes: `sent` (the ingress took it), `throttled` (this workload is parking faster than the announcement budget of 3 then 1 per 5 minutes allows), `error` (the send failed). |

`error` is the one to alert on: it is the daemon reporting that it stopped to
ask a question and could not reach anybody to ask. Nothing is queued and
nothing replays — the park is unaffected and still answerable, so the recovery
is an operator reading `GET /parks`.

`throttled` is a different alert. It rarely means one announcement went
missing and usually means a workload has started parking in a loop, which ends
with an operator muting the channel — and a muted channel costs them the park
that mattered too. Neither outcome is ever an error on the turn's books.

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
| `mast.turn.outcome` | string | How it ended. The `mast_turns_total` vocabulary (`ok`, `error`, `budget_exceeded`, `watchdog_halt`, `refusal_loop`) plus `refused` — see below. |
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

### `mcp.tool_call`

Every MCP tool call that goes through the [digest
wrap](/reference/mcp-servers/#digesting-large-tool-responses) opens one,
carrying `mast.mcp.tool_name`. Its child, `digest.process`, is where the
pruning happens and carries the method chosen and the reduction
achieved.

Two limits worth knowing. The span covers the *wrapped* call, so a
server with `no_digest: true` and a daemon running `--mcp-digest=false`
emit nothing here. And it does not yet parent the upstream HTTP round
trip — mast has no `otelhttp` on the MCP transport, so a hosted server's
request time shows as span duration rather than as a child.

## What has no metric

Config drift does not. The daemon checks once a minute whether the
configuration on disk still matches what it loaded and logs a `WARN`
when it stops matching, but nothing here counts it and there is no
alert behind it — if you want to be paged when an edit fails to take,
alert on that log line. See [config
drift](/install/#config-drift-diagnosed-not-reconciled) for what it
looks like and what to do about it.
