# mast observability: design

**Status:** draft, 2026-07-01. Companion to [`./positioning.md`](./positioning.md) (unattended-first positioning makes telemetry non-optional), [`./fork-design.md`](./fork-design.md) (bucket 1's lean core exposes the emission hooks; bucket 2's `pkg/eventlog/` port supplies the base signal), [`./durable-execution-design.md`](./durable-execution-design.md) (pause/resume events surface as metrics), and [`./orchestration-design.md`](./orchestration-design.md) (workload bundles are the natural aggregation dimension). Uses ADK v2's unified span tree as substrate — every agent, node, tool, and specialist invocation surfaces uniformly in one trace shape.

## Why this is not optional

Positioning says mast is unattended-first. Unattended means *no operator is watching in real time* — telemetry IS the observation. An agent that finishes a workload silently and never reports what it did is not observable; an agent that reports through the event log alone requires an operator to open the eventlog to see anything.

The audit-log-as-governance-moat framing (positioning.md keep list) is necessary but not sufficient. Governance is retrospective; operations is present-tense. A platform team running mast on GKE needs:

- **Real-time cost visibility** — is bundle X's p95 cost climbing?
- **Real-time latency visibility** — is specialist Y responding 3× slower than yesterday?
- **Real-time failure visibility** — are HITL escalations spiking for bundle Z?
- **Distributed traces** — where did the 8s spent by "incident-triage" go? MCP calls? Provider latency? Specialist reasoning?
- **Alert integration** — a Prometheus alert firing should be able to trigger a mast bundle, not just page a human.

None of this is delivered by the eventlog alone. All of it is table-stakes for a production platform substrate.

## Substrate: v2's unified span tree

Prior to v2, agent execution and workflow execution had separate telemetry shapes. Under v2, the runner drives everything through the same node runtime and "node/agent execution shows up in one consistent telemetry span tree." Every operation — LlmAgent turn, node execution, tool call, specialist invocation, sub-workflow, HITL request — surfaces as a span with the same attribute vocabulary.

This changes what mast has to build. We do not design our own trace shape; we *export* v2's spans through standard mechanisms (OTel primarily) and layer mast-specific attributes (workload name, tenant scope, cost) on top.

Three layers of the observability stack, each with a distinct consumer:

| Layer | Data | Primary consumer | Export shape |
|---|---|---|---|
| **Traces** | Per-span, distributed across MCP hops | Operator debugging a specific session | OTel → Jaeger / Tempo / Cloud Trace |
| **Metrics** | Aggregate counters, gauges, histograms | Platform team dashboards, alerting | Prometheus scrape endpoint; OTel metrics |
| **Logs** | Per-event, structured | Post-hoc analysis, incident response, audit | OTel logs; structured stdout (JSON) |

Session eventlog persists to storage regardless of export configuration — it is the source of truth. Observability exports are the *view* onto that source, shaped for real-time consumption.

## Traces

### Span coverage

Every level of execution emits a span:

- **Session** — top-level span from session-start to session-end (or pause).
- **Turn** — one LlmAgent turn (Chat mode conversation exchange; Task mode planning step; SingleTurn call).
- **Node** — one workflow-graph node execution.
- **Tool call** — one tool invocation (built-in tools, MCP tools, agenttool-wrapped specialists).
- **Specialist invocation** — one specialist run (as a Task-mode agent or a SingleTurn call).
- **Sub-workflow** — one reference-graph shape instantiation.
- **HITL** — one `RequestInputEvent` from emit to resume.
- **Pause** — one durable-execution pause from `Pause()` call to resume trigger.
- **MCP call** — one MCP tool invocation, with the underlying protocol call as a child span.
- **Provider call** — one LLM call to Gemini / Claude / Vertex (spanning the raw HTTP request).
- **A2A server task** — one A2A task hosted by mast, from submission to terminal state (see [`./a2a-design.md`](./a2a-design.md)).
- **A2A client call** — one outbound A2A invocation to a remote agent, with `traceparent` propagation.
- **AG-UI server run** — one AG-UI run hosted by mast (from `RunAgentInput` receipt to `RunFinished` / interrupt / error), with per-event child spans for text messages / tool calls / state deltas (see [`./ag-ui-design.md`](./ag-ui-design.md)).
- **AG-UI client call** — one outbound AG-UI invocation, with `traceparent` propagation.
- **Federation call** — one `invoke_remote_agent` invocation (spans the underlying protocol call whether A2A / mast-native / HTTP/RPC / AG-UI; see [`./federation-design.md`](./federation-design.md)).

### Standard attribute vocabulary

Every span carries a common attribute set:

| Attribute | Type | Example |
|---|---|---|
| `mast.session.id` | string | UUID v7 |
| `mast.workload.name` | string | `incident-triage` |
| `mast.workload.task_class` | string | `orchestrate` |
| `mast.tenant.scope` | string | `tenant-42` (from `WithIsolationScope`) |
| `mast.agent.mode` | string | `Chat` / `Task` / `SingleTurn` |
| `mast.specialist.name` | string (spans under a specialist) | `ImagePullBackOff` |
| `mast.provider.name` | string | `gemini` / `claude` / `vertex` |
| `mast.provider.model` | string | `gemini-2.5-flash` |
| `mast.node.type` | string | `function` / `agent` / `tool` / `join` / `dynamic` |
| `mast.shape.name` | string (sub-workflow spans) | `fan-out-fan-in` |
| `mast.cost.usd` | float | Cumulative for the span |
| `mast.cost.tokens.in` | int | Provider tokens in |
| `mast.cost.tokens.out` | int | Provider tokens out |
| `mast.hitl.reason` | string (HITL spans) | `ambiguity` / `mutation_approval` / `budget_exhaustion` |
| `mast.pause.reason` | string (pause spans) | `watchdog_anomaly` / `cost_cool_down` / `hitl` / `timed` / `external_signal` |
| `mast.pause.duration_ms` | int (pause spans, on resume) | 3600000 for a 1h pause |

Additional per-node attributes as appropriate (`mast.tool.name`, `mast.tool.args_digest`, etc.). Values that could contain secrets (tool args, provider responses) go through the same redaction pipeline as snapshot export (see [`./durable-execution-design.md`](./durable-execution-design.md) open question #7).

### Trace propagation across MCP

MCP calls carry standard W3C `traceparent` / `tracestate` headers if the MCP server supports them. Servers that don't propagate context surface as leaf spans (opaque duration); servers that do propagate surface as parent spans over the underlying protocol call (kubectl request, Prometheus query, etc.). Distributed traces across mast → MCP → downstream infrastructure work out of the box for OTel-instrumented consumers.

### OTel export

Trace export via OTel SDK; configured via standard OTel env vars (`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, etc.). Sampling configurable per workload (`observability.trace_sample_rate` in workload bundle; global default via env).

No mast-specific export protocol. We're not in the trace-storage business.

## Metrics

Metrics surface aggregates that don't fit trace-shape queries. Prometheus scrape endpoint (`/metrics`) exposes them; also available via OTel metrics for consumers preferring that path.

### Metric families

**Session lifecycle:**
- `mast_sessions_started_total{workload, task_class, tenant}` — counter
- `mast_sessions_completed_total{workload, task_class, tenant, outcome}` — counter; outcome ∈ `finish_task` / `error` / `budget_exhausted` / `hitl_abandoned` / `aborted`
- `mast_sessions_active` — gauge (currently running + paused)
- `mast_sessions_paused{reason}` — gauge (currently paused, by reason)
- `mast_session_duration_seconds{workload}` — histogram

**Cost:**
- `mast_session_cost_usd{workload, tenant, provider, model}` — histogram (per-session cost distribution)
- `mast_provider_tokens_in_total{provider, model}` — counter
- `mast_provider_tokens_out_total{provider, model}` — counter
- `mast_workload_cost_usd_rate{workload, tenant}` — gauge (rolling 5m rate)

**Turns / nodes:**
- `mast_turns_total{workload, agent_mode}` — counter
- `mast_turn_duration_seconds{workload, agent_mode}` — histogram
- `mast_nodes_executed_total{shape, node_type}` — counter
- `mast_node_duration_seconds{shape, node_type}` — histogram

**Tools / MCP:**
- `mast_tool_calls_total{tool, workload}` — counter
- `mast_tool_call_duration_seconds{tool}` — histogram
- `mast_tool_call_errors_total{tool, error_type}` — counter
- `mast_mcp_calls_total{server, tool}` — counter
- `mast_mcp_call_duration_seconds{server, tool}` — histogram

**Specialists:**
- `mast_specialist_invocations_total{specialist, workload}` — counter
- `mast_specialist_duration_seconds{specialist}` — histogram
- `mast_specialist_budget_exhausted_total{specialist}` — counter

**HITL:**
- `mast_hitl_requests_total{workload, reason}` — counter
- `mast_hitl_wait_seconds{workload, reason}` — histogram (time from emit to resume)
- `mast_hitl_abandoned_total{workload, reason}` — counter (never resumed within TTL)

**Pause / resume:**
- `mast_pauses_total{reason}` — counter
- `mast_pause_duration_seconds{reason}` — histogram
- `mast_resumes_total{reason, outcome}` — counter; outcome ∈ `resumed` / `aborted` / `expired`

**Planner:**
- `mast_planner_invocations_total{workload}` — counter
- `mast_planner_turns{workload}` — histogram (per-invocation turn count)
- `mast_planner_shape_choices_total{workload, shape}` — counter (which shapes the planner picked)

**Watchdog:**
- `mast_watchdog_signals_total{signal_type, workload}` — counter
- `mast_watchdog_signal_to_action_seconds` — histogram (signal-to-effect latency)

**A2A:**
- `mast_a2a_server_tasks_total{skill, outcome}` — counter (tasks accepted by mast's A2A server)
- `mast_a2a_server_task_duration_seconds{skill}` — histogram
- `mast_a2a_server_auth_failures_total{reason}` — counter
- `mast_a2a_client_calls_total{remote, skill, outcome}` — counter (outbound A2A calls)
- `mast_a2a_client_call_duration_seconds{remote, skill}` — histogram

**AG-UI:**
- `mast_agui_runs_total{workload, outcome}` — counter (`outcome` ∈ `success` / `interrupt` / `error` / `cancelled`)
- `mast_agui_run_duration_seconds{workload}` — histogram
- `mast_agui_active_threads{workload}` — gauge
- `mast_agui_interrupts_total{workload, reason}` — counter (per HITL / durability pause)
- `mast_agui_reconnects_total{workload}` — counter (client disconnect + reconnect resume)
- `mast_agui_client_calls_total{remote, outcome}` — counter (outbound AG-UI calls when mast is client)
- `mast_agui_client_call_duration_seconds{remote}` — histogram
- `mast_agui_server_auth_failures_total{reason}` — counter

**Federation:**
- `mast_federation_invocations_total{adapter, remote, outcome}` — counter
- `mast_federation_invocation_duration_seconds{adapter, remote}` — histogram
- `mast_federation_errors_total{adapter, error_type}` — counter (`ErrUnreachable` / `ErrSchemaViolation` / `ErrTimeout` / `ErrProtocolMismatch` / `ErrAuthFailed` / `ErrBudgetExhausted` per [`./federation-design.md`](./federation-design.md))
- `mast_federation_depth` — histogram (federation chain depth per session; per federation-loop-detection)

### Cardinality management

Prometheus is cardinality-sensitive. Guidance:

- **Tenant is a high-cardinality dimension** and is *not* on every metric by default. Metrics that include `tenant` are opt-in per deployment (env var `MAST_METRICS_INCLUDE_TENANT=1`) and are expected to be shipped to a store with sufficient scale (Cortex, Thanos, VictoriaMetrics).
- **Session ID is never a metric label.** Session-shaped queries go through traces.
- **Tool name / MCP server name** are bounded per deployment (finite tools + finite MCP servers configured); safe as labels.
- **Specialist name** is bounded by `.agents/specialists/*.tmpl`; safe as a label.
- **Workload name** is bounded by `.agents/workloads/*.yaml`; safe as a label.

### Prometheus scrape endpoint

`/metrics` on the same HTTP listener as attach mode (or on a separate port; configurable). Standard OpenMetrics format. Multi-instance deployments: each mast pod exposes its own `/metrics`; Prometheus scrapes each; aggregation happens Prometheus-side.

## Logs

Structured logs (JSON) for post-hoc analysis. Distinct from the audit event log (per-session, persisted) — logs here are process-level and OS-standard-stream.

### Log content

- **Startup / shutdown** — version, config summary, connected MCP servers, loaded specialists + workloads.
- **Session boundary events** — session start / end / pause / resume, with session ID, workload, tenant, outcome.
- **Error events** — provider errors, MCP errors, permission gate denials, invariant violations.
- **Warning events** — budget-threshold-crossings before exhaustion, retry exhaustion, deprecated config usage.
- **Debug events** — turn-by-turn planner decisions, per-tool call arguments (debug-level only), per-provider raw request/response headers (debug-level only, redacted).

Log level default `info`; per-package override via env (`MAST_LOG_LEVEL_pkg_planner=debug`).

### Correlation

Every log line carries `mast.session.id`, `mast.workload.name`, `mast.tenant.scope` where applicable. Log-to-trace correlation via `trace_id` and `span_id` fields (standard OTel-log convention).

### OTel logs

For consumers using OTel-native log collection, mast emits logs via the OTel SDK in addition to stdout. Same content; different transport.

## Alert integration back into the agent loop

Positioning priority #2 (bash search-gate, watchdog→model routing, task=debug extensions, gemini-3.5-flash probe) includes core-agent issue #159: watchdog signals should reach the model's next-turn context, not just operator UI. V2's emitting-function-node pattern is the mechanism.

Alerts external to mast (Prometheus firing rules, Cloud Monitoring alerts, Grafana alert rules) should be able to reach the agent loop too — that closes the operational loop for unattended workloads. Two paths:

### Alert-as-workload

An external alert triggers a mast workload via the standard entry-point mechanism (HTTP webhook, queue message). The alert payload becomes the workload input; the bundle picks the appropriate task class + specialists + planner. This is the direct path: alert → workload → response.

Example: Prometheus AlertManager configured to POST to `mast-instance:8080/webhook/alerts`; entry-point config binds `/webhook/alerts` to the `alert-triage` workload bundle.

### Alert-as-signal-into-running-session

An external alert becomes a signal an already-running session (autonomous loop, long-running planner) can react to. Emitted via `mast.EmitSignal(sessionID, signal)`; consumed by the session's next relevant node.

Example: an autonomous cost-monitor loop is running; an external cost alert fires; the alert is emitted as a signal to the monitor loop's next iteration; the loop escalates via HITL.

Both paths use the same emit / consume mechanism internally — the difference is whether a session already exists.

## Composition with other subsystems

| Subsystem | Interaction |
|---|---|
| **Durable execution** | Pause + resume events emit metrics + spans; snapshot exports include trace-friendly correlation IDs. Alert-triggered pauses (from Prometheus firing) surface via metrics that pre-declare the pause reason. |
| **Orchestration (workload bundles)** | Workload name is the primary metric aggregation dimension; bundles can declare `observability.trace_sample_rate` and `observability.log_level` overrides. Planner decisions emit per-shape counters (`mast_planner_shape_choices_total`). |
| **Specialists** | Per-specialist invocation counters + duration histograms; SingleTurn specialists (classifier-first dispatch, LLM-as-router classifiers) surface as their own metric class. |
| **Workflow scaffolding** | Per-shape metrics; per-node metrics; graph-shape-specific spans (fan-out-fan-in, adversarial-verifier, etc.). |
| **Memory** | Audit-derived-memory pipeline runs emit their own workload metrics — it's a workload, not a special case. |
| **Deployment** | Multi-instance deployments: each mast pod exposes its own `/metrics`; central aggregation via Prometheus scrape. Session ID uniqueness across instances is a metric-labeling requirement. |
| **Attach mode + mast-web** | Attach connections and mast-web sessions emit their own low-cardinality metrics (`mast_attach_connections`, `mast_mastweb_operators_active`). |
| **MCP** | MCP calls emit per-server + per-tool spans; trace propagation via `traceparent` where servers support it. |

## Configuration surface

Global observability config lives in `pkg/config/` (populated by env + optional config file):

```yaml
# example config
observability:
  otel:
    enabled: true
    endpoint: otlp-collector.observability.svc:4317
    protocol: grpc                 # grpc | http/protobuf | http/json
    headers: {authorization: "Bearer ${OTEL_TOKEN}"}
  prometheus:
    enabled: true
    listen: :9100                  # or "" to reuse attach-mode port
    path: /metrics
  logs:
    level: info                    # per-package overrides via env
    format: json                   # json | text (for local dev)
  trace_sample_rate: 1.0           # 0.0 to 1.0; overridable per-workload
  include_tenant_in_metrics: false # cardinality guardrail
```

Workload-level overrides via bundle:

```yaml
# .agents/workloads/incident-triage.yaml (partial)
observability:
  trace_sample_rate: 1.0           # always sample incident triage
  log_level: debug                 # verbose for on-call review
```

## Deployment-target starters

Ship starter configs for the common consumption stacks:

- `examples/observability/prometheus-grafana/` — Prometheus scrape config; Grafana dashboard JSON (per-workload latency + cost + HITL panels).
- `examples/observability/gcp/` — Cloud Trace + Cloud Monitoring; export via OTel Collector to Google Cloud Ops backends.
- `examples/observability/otel-collector/` — vanilla OTel Collector routing to any downstream.
- `examples/observability/loki/` — Loki-shaped log queries for Grafana consumers.

Not shipping in v0.1 — parallel to `docs/deployment-design.md` starter configs. Land as the deployment stories mature.

## Phasing

| Version | Scope |
|---|---|
| **v0.1** | Prometheus `/metrics` endpoint with the base metric families (session lifecycle, cost, turns, tools, specialists, HITL, pause). Structured JSON logs to stdout. Basic OTel trace export configured via env. Config-file plumbing per above. |
| **v0.2** | Alert-as-signal-into-running-session (`mast.EmitSignal`). OTel logs (in addition to stdout). Sampling per-workload. Starter Grafana dashboards. |
| **v0.3** | Full trace propagation across MCP hops (requires MCP-server-side context propagation; some servers may not support). Distributed traces including sub-agent spans. Per-tenant metric cardinality (opt-in). |
| **v0.4+** | Cost-anomaly detection as a first-class alert source (workloads can subscribe to their own cost anomaly). Trace-driven bundle-learning input. |

## Open questions

1. **`/metrics` port sharing.** Should Prometheus scrape share the attach-mode HTTP port or run on its own? Bias: separate port by default (standard Prometheus convention; some scrape configs are strict about not seeing attach paths). Shared port as an opt-in for constrained environments.
2. **Log content in unattended pods.** Debug logs are useful for triage but noisy on hot paths. Should log level auto-elevate around HITL escalations and errors? Bias: yes, "auto-debug on error" is a common pattern — buffer info-level logs for the last N seconds and dump on error.
3. **Trace sampling on high-volume workloads.** A cost-monitor loop running every 30s at 100 replicas is 8.6M sessions/month; 100% sampling is expensive. Per-workload sampling defaults? Bias: 100% for orchestrate task class, 10% for research/review, 1% for chat, per-workload override always available.
4. **Log-to-trace correlation ID persistence.** Should the correlation ID be part of the session record so log-only reads (post-hoc audit review) can still cross-reference to traces even after trace storage TTL expires? Bias: yes; correlation ID is a session field.
5. **Metric-name pollution risk.** Custom specialists and workloads shouldn't be able to introduce arbitrary metric names (that's a cardinality DoS). All metrics defined in `pkg/observability/`; specialists can emit *events* (surfaced via traces) but not metrics. Enforce at API level.
6. **Cost telemetry accuracy for streaming responses.** Provider-side streaming means token counts arrive incrementally; when do we emit `mast_provider_tokens_out_total`? Bias: on stream end (accurate); with an interim emit at HITL boundary if the session pauses mid-stream.
7. **Correlation with core-agent traces during dual-run periods.** During any period when a workload runs on core-agent and mast (comparison, migration), can traces be correlated? Both should carry a common `deployment.id` or similar; deferred but capture the design constraint.

## Out of scope

- **Custom metric backend implementations.** We support Prometheus + OTel; we don't build adapters for proprietary metric systems. Consumers use OTel Collector to bridge.
- **Trace storage.** We export; storage is Jaeger/Tempo/Cloud Trace/etc.'s job.
- **Log storage.** Same. We emit; storage is Loki/CloudWatch/Cloud Logging/etc.'s job.
- **A mast-native dashboard.** mast-web has session views + workload views but is not a metrics dashboard. Grafana / Cloud Monitoring / operator's preferred tool.
- **Automatic alert authoring.** Operators write PromQL alerts against mast metrics; we ship examples, not managed alerts.
- **Log shipping.** Kubernetes DaemonSet loggers, sidecar log shippers — infrastructure concern, not mast's job. We emit to stdout; consumers ship.

## Related

- [`./positioning.md`](./positioning.md) — governance moat is retrospective; observability is present-tense
- [`./fork-design.md`](./fork-design.md) — `pkg/observability/` is a new package in bucket 1's design surface
- [`./durable-execution-design.md`](./durable-execution-design.md) — pause/resume metrics + trace propagation across pause boundaries
- [`./orchestration-design.md`](./orchestration-design.md) — workload bundles carry per-workload observability overrides
- [`./deployment-design.md`](./deployment-design.md) — multi-instance metric aggregation
- [`./memory-design.md`](./memory-design.md) — audit-derived-memory pipeline emits standard workload metrics
- [`./library-api-design.md`](./library-api-design.md) — library consumers configure observability programmatically
- OpenTelemetry Go SDK (`go.opentelemetry.io/otel`) — trace + metric + log export
- ADK v2 unified span tree — the substrate we export
