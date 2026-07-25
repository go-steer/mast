# mast memory: design

**Status:** draft, 2026-07-01 (updated 2026-07-25 — two correctness fixes from the design-review pass: budget-critical reads are fail-closed, and reducer idempotency is stated honestly as requiring event cursors or full re-derivation, not assumed of incremental folds). Companion to [`./positioning.md`](./positioning.md) (audit-derived memory is priority #7; single largest unattended differentiator), [`./fork-design.md`](./fork-design.md) (bucket 2 ports `pkg/eventlog/`; bucket 3 builds the mast-side memory subsystem on top), [`./orchestration-design.md`](./orchestration-design.md) (bundle-learning is a memory consumer), [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) (memory reads via state-bound nodes; derivation pipeline is a map-reduce reference-graph instance), [`./durable-execution-design.md`](./durable-execution-design.md) (pause/resume patterns become memory signals), and [`./deployment-design.md`](./deployment-design.md) (tenant scope carries into memory). This doc is the **mast-consumer view** of the shared-memory-stack design that ships in core-agent (PRs #13/14/15) — how mast exposes, consumes, and extends it, not a rehash of the underlying storage design.

## Why this is its own doc

Core-agent's shared-memory stack solves the storage + derivation problem — how session events become persistent typed memory. Mast inherits the storage (via bucket 2 port) and the derivation infrastructure (via bucket 3 port), but the *shape of memory mast consumes* is a mast-specific concern that deserves its own design:

- **Which keys does mast populate.** The memory keyspace is mast's; the storage is generic.
- **Which reducers derive them.** Reducers encode mast-specific knowledge about session shape (workload names, planner decisions, HITL patterns).
- **How memory reaches nodes.** State-bound nodes (`state:"<key>"` tags) are the primary consumer surface.
- **How tenancy composes.** `WithIsolationScope` carries through; mast enforces tenant-scoped reads.
- **How consumers extend.** Library API for programmatic memory access; extension points for custom reducers.

Splitting this from the storage design means the storage can evolve (SQLite → Postgres → Spanner) without touching the consumer contract, and vice versa.

## Substrate: what shared-memory-stack gives us

Assuming core-agent PRs #13/14/15 land per the fork trigger, mast inherits:

- **Event log as source of truth.** Every session event persists; nothing else is authoritative.
- **Reducer registration.** Named reducers subscribe to event streams and produce typed values written to memory under a named key. Reducer execution is idempotent and re-derivable.
- **Typed memory keyspace.** Memory values are typed structures (not opaque blobs). Type schemas registered per key.
- **Time-partitioned reads.** Memory reads are as-of-timestamp — reproducible across replays.
- **Per-scope namespacing.** Memory keys namespace under scope (session ID for per-session; tenant ID for per-tenant; empty for global).

Mast's job:

- Define the mast-owned key catalog.
- Ship the mast-owned reducers.
- Expose read access via state-bound nodes and library API.
- Enforce tenancy on reads.
- Wire consumers (planner, bundle-learning, autonomous loops, observability) to relevant keys.
- Provide extension points for third-party keys + reducers.

## Memory shape

Memory in mast is a key/value store with typed values, namespaced by scope, versioned as-of-timestamp:

```
Scope           Key                                Value type
-----           ---                                ----------
session:S1      turn_count                         int
session:S1      last_plan_shape                    string
session:S1      hitl_requests                      []HITLRequest
tenant:T1       workload_stats.incident-triage     WorkloadStats
tenant:T1       cost_percentile.p95.30d            float64
tenant:T1       specialist_cooccurrence            map[string]map[string]int
global          model_success_rate.gemini-2.5-flash  float64
```

Scopes: `session:<id>`, `tenant:<id>`, `global`.

Reads always specify scope; writes go through named reducers that own specific (scope-pattern, key) tuples.

## Mast-owned key catalog (v0.1 → v0.3)

Keys and their reducers, organized by consumer.

### Per-session keys (populated during session run; readable to session's own nodes)

| Key | Type | Populated by | Consumer |
|---|---|---|---|
| `turn_count` | `int` | incrementing reducer | Any node that wants to know how far into the session it is |
| `elapsed_seconds` | `float64` | derived on read from session-start event | Same |
| `cost_usd_so_far` | `float64` | provider-call reducer | Budget-checking nodes; watchdog cost triggers |
| `tokens_in_total` / `tokens_out_total` | `int` / `int` | provider-call reducer | Same |
| `last_plan_shape` | `string` | planner-decision reducer | Next planner turn (avoid picking the same shape twice) |
| `hitl_requests` | `[]HITLRequest` | HITL-event reducer | Downstream nodes that need to know operator provided input |
| `pause_history` | `[]PauseRecord` | pause-event reducer | Analytical consumers; snapshot exports |
| `specialists_invoked` | `map[string]int` | specialist-invocation reducer | Planner (avoid re-invoking same specialist trivially) |
| `tool_calls_by_name` | `map[string]int` | tool-call reducer | Nodes that want to summarize what happened |
| `errors_by_type` | `map[string]int` | error-event reducer | Recovery nodes; adversarial-verifier's skeptic |

### Per-tenant keys (populated by audit-derived pipeline; readable to any session under the tenant)

| Key | Type | Populated by | Consumer |
|---|---|---|---|
| `workload_stats.<workload>` | `WorkloadStats` | workload-completion reducer (per-workload rollup) | Planner (baseline for cost/latency expectations); bundle-learning |
| `cost_percentile.pXX.<window>` | `float64` | rolling-quantile reducer | Bundle-learning budget refinement; alert-cost-anomaly |
| `specialist_cooccurrence` | `map[string]map[string]int` | specialist-pair reducer | Bundle-learning (which specialists cluster together); planner (which specialists are commonly needed together) |
| `hitl_pattern_stats` | `HITLPatternStats` | HITL-event reducer | Bundle-learning HITL-policy refinement |
| `tool_usage_by_workload.<workload>` | `map[string]int` | tool-usage reducer | Bundle-learning tool-catalog refinement |
| `session_success_rate.<workload>.<window>` | `float64` | outcome-reducer | Alert on drop; bundle-learning signal |
| `mcp_call_stats.<server>.<tool>` | `MCPCallStats` | MCP-event reducer | Observability; MCP-catalog recommendations |
| `active_specialists_last.<window>` | `[]string` | specialist-invocation reducer | Load monitoring; specialist archive candidates |

### Global keys (populated across all tenants; readable to any session with explicit opt-in)

| Key | Type | Populated by | Consumer |
|---|---|---|---|
| `model_success_rate.<provider>.<model>` | `float64` | model-outcome reducer (opt-in per tenant to contribute) | Small-tier-parent guard; model selection heuristics |
| `provider_availability.<provider>` | `ProviderAvailability` | provider-error reducer | Failover heuristics |
| `active_deployment_stats` | `DeploymentStats` | rollup reducer | Cross-deployment operator dashboards (opt-in per deployment) |

Global keys are *narrowly scoped* — nothing tenant-specific bubbles up; only aggregate statistics safe to share. Per-tenant opt-in gates whether a tenant's data contributes. Default opt-out.

## Read paths

Three ways mast consumers access memory:

### 1. State-bound nodes (workflow shape #7 from workflow-scaffolding)

Workflow nodes tag their input params to bind memory keys:

```go
type PlannerParams struct {
    TurnCount        int              `state:"turn_count"`
    CostSoFar        float64          `state:"cost_usd_so_far"`
    WorkloadStats    WorkloadStats    `state:"workload_stats.incident-triage" scope:"tenant"`
    CostP95_30d      float64          `state:"cost_percentile.p95.30d" scope:"tenant"`
}

plannerNode := workflow.NewFunctionNodeFromState("planner", plannerFn, cfg)
```

- Scope defaults to `session`; `tenant` and `global` require explicit tag.
- Tag values resolve at node-invocation time; reads are as-of-that-timestamp.
- Missing keys resolve to zero values (bias: fail-tolerant over fail-fast for missing memory — reducers may not have run yet). **Exception (added 2026-07-25): budget- and policy-critical keys are fail-closed.** A budget gate reading `cost_usd_so_far` that gets a zero value because the reducer lags would silently wave spending through — the exact inversion of what a cap is for. Keys registered with `Critical: true` (budget counters, permission-relevant aggregates) return `ErrMemoryUnavailable` on missing/stale-beyond-bound instead of a zero value, and the caller blocks or escalates per its `hitl_policy`. Note the primary budget path does not depend on memory at all — live enforcement is the event-stream meter per [`./orchestration-design.md`](./orchestration-design.md) budget substrate; memory-derived spend is for cross-session/tenant rollups.

### 2. Library API

Programmatic reads for library-embedded consumers or non-graph code paths:

```go
import "github.com/go-steer/mast/memory"

turnCount, err := memory.Get[int](ctx, memory.Session, sessionID, "turn_count")
stats, err := memory.Get[WorkloadStats](ctx, memory.Tenant, tenantID, "workload_stats.incident-triage")
```

Read scope is enforced against the request context's tenant scope — attempting to read `memory.Tenant(other-tenant)` from within a session scoped to a different tenant returns `memory.ErrScopeMismatch`.

### 3. Direct event log query (for reducers)

Reducer implementations read the event stream directly (that's how they derive their output). Not a general consumer path — reserved for reducer authors extending memory with new keys.

## Write paths

Memory is write-once-per-reducer-invocation. All writes go through registered reducers:

- **Reducer receives a stream of events** matching its subscription (e.g., "all provider-call events for session S1").
- **Reducer emits the current value** for its (scope, key) tuple.
- **Value is versioned by timestamp** — history preserved; reads specify a timestamp (default: latest).

Reducer registration:

```go
memory.RegisterReducer(memory.Reducer{
    Name:       "provider_cost_rollup",
    Key:        "cost_usd_so_far",
    Scope:      memory.Session,
    Subscribes: []string{"provider.call"},   // event type filter
    Reduce: func(events []Event, prev float64) float64 {
        cost := prev
        for _, e := range events {
            cost += e.Cost
        }
        return cost
    },
})
```

Third-party reducers register via the same API; mast-owned reducers register at startup.

## Multi-tenant enforcement

Tenancy composition per [`./deployment-design.md`](./deployment-design.md):

- **Session-scoped memory is inherently isolated** — session ID is unique + carries tenant scope.
- **Tenant-scoped memory** is namespaced by tenant ID; reads specify tenant, writes go through reducers that write under a specific tenant.
- **Global-scoped memory** is unrestricted read; writes require aggregation reducers explicitly opt-in per tenant.
- **`WithIsolationScope(tenantID)` on `RunNode`** propagates through the invocation stack; all downstream reads inherit the scope; cross-tenant reads fail with `memory.ErrScopeMismatch`.
- **Bundle-learning** ([`./orchestration-design.md`](./orchestration-design.md)) respects scope — cross-tenant aggregation only with per-tenant opt-in on both the source data and the target tenant of the learned bundle.

## Reducer execution

Reducers run periodically (not synchronously with events). Cadence per reducer:

| Reducer class | Cadence | Notes |
|---|---|---|
| Per-session counters (`turn_count`, `cost_usd_so_far`) | Event-driven (every event) | Cheap; near-real-time reads |
| Per-session aggregates (`hitl_requests`, `tool_calls_by_name`) | Session-end + every ~5 events | Batched for efficiency |
| Per-tenant rollups (`workload_stats.*`, `cost_percentile.*`) | Periodic (default 5min); manual invoke | Composes with bundle-learning cadence |
| Global rollups (`model_success_rate.*`) | Periodic (default 1h); opt-in contribution | Aggregation across tenants |

Reducer execution is re-derivable — running a reducer over the **full event stream from offset zero** produces the same output every time, which enables re-derivation on schema changes and replay for debugging. *(Corrected 2026-07-25: the earlier phrasing claimed idempotency of the incremental fold itself, which is false under at-least-once delivery — `Reduce(events, prev)` re-applied to already-folded events double-counts. Incremental reducer runs therefore carry an **event cursor**: each (reducer, scope, key) records the last event offset folded, persisted with the derived value in the same write; delivery replays below the cursor are skipped. Multi-instance safety comes from the leaselike claim below plus the cursor — "race but converge" alone was two mechanisms for one problem with neither specified.)*

**Reducer scheduling** in multi-instance deployments: leaselike claim per (reducer, scope) — same claim-based mechanism as timed-pause scheduler per [`./deployment-design.md`](./deployment-design.md). Only one instance runs a given reducer against a given scope at a time.

## Composition with other subsystems

| Subsystem | Memory interaction |
|---|---|
| **Planner** | Reads per-session state (turn count, last shape, cost so far) via state-bound tags. Reads per-tenant baselines (workload stats, cost percentiles) as planning inputs. Planner *decisions* feed reducers that shape future planner inputs — self-improving loop. |
| **Bundle learning** | Primary consumer of per-tenant keys (specialist co-occurrence, tool usage, HITL pattern stats, session success rate). Learning is a workload that runs against memory as input; proposes refinements. |
| **Workflow scaffolding** | State-bound nodes are the primary read path. Every reference-graph shape can bind memory keys as node inputs. |
| **Specialists** | Specialists as agent nodes can read memory via state-bound params — e.g., an `ImagePullBackOff` specialist reads `hitl_requests` to see what previous ambiguities the operator resolved. |
| **Durable execution** | Pause events feed reducers (`pause_history`, `pause_pattern_stats` per-tenant). Snapshot+replay preserves memory state at snapshot time. |
| **Observability** | Some observability signals derive from memory keys (long-window percentiles); most emit directly from the event stream. Correlation IDs cross-reference. |
| **Deployment** | Multi-instance reducer scheduling via claim; tenancy enforcement uniform across all storage backends. |
| **Orchestration (workload bundles)** | Bundle can declare `memory.reducer_overrides` for workload-specific reducer cadence tuning. |
| **Attach mode + mast-web** | mast-web reads memory to surface per-tenant dashboards (workload stats, cost trends, HITL patterns). |
| **Federation** ([`./federation-design.md`](./federation-design.md)) | Session-scoped memory doesn't cross federation boundary by default — child mast has its own session; explicit propagation via workload bundle `federation.state_propagation: [keys]`. Tenant-scoped memory propagates naturally when both instances share tenant scope + memory store. Cross-instance memory writes require shared memory store; per-instance-store deployments use explicit read RPCs (v0.3+). |
| **A2A** ([`./a2a-design.md`](./a2a-design.md)) | A2A boundary is opaque to memory — remote A2A agents have their own memory (or none); cost attribution feeds mast's reducers as remote-cost signals. No cross-runtime memory sharing (Python ADK has its own memory story; interop deferred). |

## Configuration surface

Memory config in the runtime config (populated by env + optional file):

```yaml
memory:
  reducer_cadence:
    per_tenant_rollups_seconds: 300      # 5min default
    global_rollups_seconds: 3600         # 1h default
  retention:
    session_scope_days: 90                # per-session values evicted after N days
    tenant_scope_days: 730                # 2 years
    global_scope_days: 3650               # 10 years
  cross_tenant_contribution: false        # opt-in for global-key contributions
  reducer_overrides:                      # per-reducer cadence overrides
    - name: provider_cost_rollup
      cadence_seconds: 60
```

Per-workload overrides via bundle:

```yaml
# .agents/workloads/incident-triage.yaml (partial)
memory:
  reducer_cadence:
    per_tenant_rollups_seconds: 60      # tighter cadence for this workload
```

## Phasing

| Version | Scope |
|---|---|
| **v0.1** | Substrate: bucket 2 port of `pkg/eventlog/` with v2 event fields. Basic per-session reducers (turn_count, cost_usd_so_far, elapsed_seconds). State-bound node consumer path. Library API read/register surface. No per-tenant reducers yet (needs shared-memory-stack derivation infrastructure). |
| **v0.2** | Per-tenant reducers as shared-memory-stack matures in core-agent (available via port). Workload_stats, cost_percentile, specialist_cooccurrence. Multi-instance reducer scheduling. |
| **v0.3** | Global reducers with per-tenant opt-in. Bundle-learning consumer (per orchestration-design). Full audit-derived memory story end-to-end. Cross-tenant contribution enforcement. |
| **v0.4+** | Custom reducer packaging (plugin-shaped); memory query API for ad-hoc analytics; long-term retention tiering (cold storage for expired scope-day values). |

## Open questions

1. **Reducer failure semantics.** A reducer that errors — retry? Skip event batch? Alert? Bias: retry with backoff; alert on repeated failure; skip event batch only after N retries and log lost derivation.
2. **Schema evolution.** When a memory value's type schema changes, re-derive from source (expensive, correct) or migrate the stored value (cheap, error-prone)? Bias: re-derive on schema version bump; add a version field to reducer registration.
3. **Read consistency across instances.** Multiple mast pods read the same memory; is a read guaranteed to see writes from another pod? Bias: eventually consistent with a bounded staleness (default 30s); strict-consistency reads available via `memory.GetConsistent(...)` for critical decisions.
4. **Retention on tenant deletion.** When a tenant is deleted, memory keys under that tenant scope must be removed. Cascade delete or lazy expiry? Bias: cascade delete; provide tools for retention audit.
5. **Cross-tenant contribution incentive.** Global keys need broad participation to be useful. What's the operator incentive to opt in? Bias: consumers of global keys get better model-selection heuristics + failover behavior; documented as a "network effect" for operators considering opt-in.
6. **Memory export.** Operators want to export per-tenant memory for compliance / migration / analytics. `mast memory export --tenant=T > tenant-memory.jsonl`? Bias: yes v0.3; matches session-store export.
7. **Reducer sandboxing for third-party reducers.** A malicious reducer could exfiltrate cross-tenant data via clever event subscriptions. Bias: reducers registered programmatically only (no runtime plugin loading in v0.1-v0.3); explicit review requirement for third-party reducers.
8. **Memory-as-a-tool for the planner.** Should the planner have `read_memory(key, scope)` as a tool it can invoke mid-turn? Or is state-bound access sufficient? Bias: state-bound sufficient for v0.1-v0.2; add tool if planner authors ask for it.

## Out of scope

- **A general-purpose key-value store.** Memory is a derived-from-events keyspace with reducer discipline; general KV needs go through the operator's existing KV.
- **A vector database for embeddings.** Memory stores typed structured values; embeddings + semantic search are a separate concern (specialists can call MCP-wrapped vector DBs directly).
- **Cross-runtime memory sharing.** Python ADK has its own memory story; interop deferred.
- **Time-travel debugging.** As-of-timestamp reads are useful; a full time-travel debugger is v1.0+.
- **User-facing memory customization.** Operators author reducers programmatically via library API; no `.agents/reducers/*` file convention. If operators consistently ask, revisit.
- **Full-text search over memory values.** Structured typed reads only; text search over event content goes through the event log's own search facilities (v0.3+ concern).

## Related

- [`./positioning.md`](./positioning.md) — audit-derived memory as biggest unattended differentiator (priority #7)
- [`./fork-design.md`](./fork-design.md) — bucket 2 ports eventlog; bucket 3 wires the mast-side memory subsystem
- [`./orchestration-design.md`](./orchestration-design.md) — bundle-learning is the primary v0.3 memory consumer
- [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) — state-bound nodes are the primary read path
- [`./durable-execution-design.md`](./durable-execution-design.md) — pause/resume events feed memory; snapshot preserves memory state
- [`./deployment-design.md`](./deployment-design.md) — multi-instance reducer scheduling; tenancy enforcement across storage backends
- [`./observability-design.md`](./observability-design.md) — some observability signals derive from memory
- [`./library-api-design.md`](./library-api-design.md) — programmatic memory access + reducer registration
- [core-agent's shared-memory-stack design](https://github.com/go-steer/core-agent/blob/main/docs/) — the underlying storage + derivation infrastructure mast rides on
