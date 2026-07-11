# mast federation: design

**Status:** draft, 2026-07-01. Companion to [`./a2a-design.md`](./a2a-design.md) (A2A as one federation protocol adapter), [`./orchestration-design.md`](./orchestration-design.md) (planner tool vocabulary gains `invoke_remote_agent`), [`./durable-execution-design.md`](./durable-execution-design.md) (cross-instance session-state propagation; cross-boundary pause/resume), [`./deployment-design.md`](./deployment-design.md) (federation topologies map onto deployment topologies), [`./library-api-design.md`](./library-api-design.md) (protocol adapters are extension points), [`./observability-design.md`](./observability-design.md) (distributed tracing across federated agents), [`./memory-design.md`](./memory-design.md) (cross-instance memory scope), and [`./mcp-catalog-design.md`](./mcp-catalog-design.md) (related-but-distinct — MCP for tools, federation for full-agent invocation). This doc covers **federation as a pattern** — one mast instance orchestrating N remote agents across multiple protocols — separate from A2A the protocol.

## Why this is its own doc

A2A is one *protocol* mast supports for agent interoperability. Federation is a *pattern* — the planner treating remote agents as extended tool vocabulary regardless of protocol. Distinguishing:

- **A2A design decisions** are about the A2A ecosystem: agent card, task lifecycle, Google Agent Registry integration, kagent interop. These belong in [`./a2a-design.md`](./a2a-design.md).
- **Federation design decisions** are about the pattern: how does the planner select a remote agent? How does session state propagate across instance boundaries? How do we handle cross-instance HITL? What are the topology options (star, mesh, hierarchical)? These belong here.

Federation composes with A2A (using A2A as one protocol adapter) but also with mast-native inter-instance protocol (richer than A2A, mast-to-mast only) and generic HTTP/RPC (for wrapping arbitrary services as agent-shaped things).

The through-line: **the planner's tool vocabulary gains one class — `invoke_remote_agent(reference, inputs)` — and federation owns the design of that class.**

## The planner's remote-agent tool

Planner tool vocabulary from [`./orchestration-design.md`](./orchestration-design.md) gains one entry:

| Tool | What it does |
|---|---|
| `invoke_remote_agent(reference, inputs)` | Dispatches to a remote agent identified by `reference`; adapter resolved at call time from protocol scheme |

Reference format: `<protocol>://<name-or-endpoint>[?skill=<skill>]`

- `a2a://external-triage` — configured A2A agent named `external-triage` (per [`./a2a-design.md`](./a2a-design.md))
- `a2a://google-agent-registry/production-triage` — dynamically-discovered A2A agent from a registry
- `mast://cluster-2/incident-triage?skill=investigate` — remote mast instance in `cluster-2` fleet
- `mast://tenant-42.regional-supervisor/analyze` — hierarchical federation reference
- `http://internal-scanner.example.com/scan` — bespoke HTTP endpoint wrapped as agent
- `grpc://compliance.svc.cluster.local:9000/CheckPolicy` — bespoke gRPC service wrapped as agent

The adapter dispatches to the appropriate protocol handler. The planner sees uniform tool semantics; the adapter absorbs protocol-specific detail.

## Protocol adapters

Three built-in adapter classes; extension point for more.

### A2A adapter

Covers everything in [`./a2a-design.md`](./a2a-design.md)'s client side. Reference: `a2a://<name>` or `a2a://<registry>/<agent-id>[?skill=<skill>]`.

- Static references resolve against `.agents/a2a/*.yaml` configs.
- Dynamic references resolve via registry discovery.
- Task-based interaction; long-running task support; HITL propagation; streaming (v0.2+).

### Mast-native adapter

Optimized inter-instance protocol between mast instances in a trusted fleet. Reference: `mast://<instance-selector>[/<workload-name>][?...]`.

Why not just A2A between mast instances? Because mast can offer more than A2A when both sides are mast:

- **Session-state propagation.** Remote invocation carries session ID + tenant scope + parent-turn context; remote mast can read parent's session state for context.
- **Native memory propagation.** Remote mast reads audit-derived memory keys from the parent (with permission); no re-derivation.
- **Native HITL routing.** Remote mast's HITL surfaces to the *parent's* attach connection, not a separate operator; unified operator experience.
- **Native cost attribution.** Remote spend charges the parent's `budget.max_cost_usd` accurately (A2A cost attribution requires an extension per a2a-design open Q #4).
- **Native durable-execution correlation.** Parent pause propagates; child pause propagates back; snapshot/replay works across instances.
- **Lower protocol overhead.** Binary encoding (protobuf) instead of JSON-RPC; fewer round trips; native streaming.

For cross-organizational-boundary federation (two different companies' mast fleets) — use A2A. For within-fleet federation (multiple mast instances in one deployment) — use mast-native. The choice is per-reference, made by whoever authored the workload / specialist that carries the reference.

**Instance selectors:**

- `mast://<instance-id>` — specific instance by ID (rare; usually only for debugging).
- `mast://<fleet-name>` — any instance in a named fleet (load-balanced by fleet's own routing).
- `mast://<region>.<fleet-name>` — regional fleet (e.g., `us-east.core-fleet`).
- `mast://<tenant>.<fleet-name>` — tenant-scoped fleet member.
- `mast://<role>@<fleet-name>` — instances filtered by role (e.g., `worker@triage-fleet`, `supervisor@triage-fleet`).

Fleet discovery via one of: Kubernetes service (`Service` + `EndpointSlice`), Consul/etcd (for non-Kubernetes), static config (for standalone fleets), or the mast-web server (which knows the fleet).

**Wire protocol:** gRPC (v0.3+ scope). HTTP/JSON as v0.2 fallback for environments where gRPC isn't practical.

### HTTP/RPC adapter

Wraps arbitrary HTTP or gRPC endpoints as agent-shaped tools. Reference: `http://<url>` or `grpc://<endpoint>/<method>`.

Neither A2A nor mast-native — just a way to call bespoke services from a planner. Config:

```yaml
# .agents/remote/internal-scanner.yaml
name: internal-scanner
reference: http://internal-scanner.example.com/scan
description: |
  Scans a Kubernetes namespace for compliance violations.
  Not an A2A agent; a bespoke HTTP endpoint owned by the security team.
method: POST
headers:
  content-type: application/json
  # auth headers reference env vars
auth:
  header: Authorization
  value_env: SCANNER_TOKEN                # "Bearer ${SCANNER_TOKEN}"
input_schema:
  type: object
  properties:
    namespace: {type: string}
  required: [namespace]
output_schema:
  type: object
  properties:
    violations: {type: array, items: {type: object}}
    scan_id: {type: string}
timeout_seconds: 120
```

The HTTP/RPC adapter effectively lets operators bring their existing infrastructure into the planner's vocabulary without needing to build A2A wrappers first. Useful for:

- **Existing internal services** the operator already runs — the security scanner, the release-notes generator, the compliance checker.
- **Prototyping.** Operator wants to try invoking a service without going full A2A first.
- **Non-Go framework services** where implementing A2A server is high-effort but exposing an HTTP endpoint is trivial.

**Limitations vs. A2A:**
- No standard task lifecycle — HTTP is request-response.
- No native HITL propagation — the endpoint either resolves synchronously or doesn't.
- No agent-card contract — schema must be authored per-service.
- No standard streaming.

For any service reachable via HTTP/RPC that will be repeatedly integrated, migrating to A2A server-side gives everyone (mast + other frameworks) a better story.

### Custom adapters (extension point)

Per [`./library-api-design.md`](./library-api-design.md), the adapter interface is an extension point:

```go
// github.com/go-steer/mast/federation
type Adapter interface {
    Name() string                    // e.g. "a2a", "mast", "http", "grpc", or custom
    Resolve(reference string) (RemoteAgent, error)
    Invoke(ctx context.Context, ref RemoteAgent, inputs any) (Result, error)
}

federation.RegisterAdapter(&myCustomAdapter{})
```

Custom adapters cover: proprietary protocols; corporate internal agent systems; framework-specific adapters (a direct LangGraph adapter, a direct Python-ADK adapter that predates A2A support).

## Federation topologies

Federations have shape. Three archetypes; hybrids common.

### Star

One supervisor mast + N worker masts / A2A agents / HTTP services. Supervisor dispatches; workers execute; supervisor aggregates.

```
                     [supervisor mast]
                    /   |    |    |    \
              [worker A2A] [worker mast] [worker http] [worker mast] ...
```

Use case: incident-response orchestrator (supervisor) dispatching per-affected-cluster investigations (workers).

Composes with: **supervisor+workers reference-graph shape** from [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) — federation is the network-boundary variant.

### Mesh

Peer mast instances; any can call any. No supervisor role.

```
    [mast A] --- [mast B]
       \   \   /   /
        \   \ /   /
         [mast C]
```

Use case: fleet of regional mast instances where each region handles local work but may consult peers for cross-region context (a US mast asks an EU mast for EU-region-specific context on a global incident).

Requires: shared session store OR per-instance stores with a coordination protocol. Complex; typically not v0.1.

### Hierarchical

Tree structure: root supervisor → regional supervisors → workers.

```
          [root supervisor mast]
         /                       \
   [us supervisor]           [eu supervisor]
     /       \                 /       \
  [worker] [worker]        [worker] [worker]
```

Use case: multi-region deployments where regional supervisors own regional workloads but escalate cross-region concerns to root; root aggregates for organization-wide reporting.

Compositional: each supervisor level is a supervisor+workers shape; whole tree is nested supervisor+workers with federation at each edge.

### Hybrid

Real deployments mix shapes. A supervisor mast dispatches to a mesh of regional peer instances; each regional peer runs a star topology over per-cluster workers. Federation supports arbitrary composition; the topology is emergent from the references + dispatch decisions, not statically declared.

## Mast-to-mast handoff

The mast-native adapter's richest feature is cross-instance handoff — genuinely one session that crosses instance boundaries.

### Session-state propagation

When mast A calls `invoke_remote_agent("mast://cluster-B/analyze", input)`:

- Session context propagates: session ID (parent's), tenant scope, workload name (parent's), trace context (`traceparent`).
- Selected session-state keys propagate as *proto-state* — the child mast reads these as state-bound inputs without re-derivation.
- Which keys propagate is governed by workload config: `federation.state_propagation: [key1, key2]`. Default: none (explicit opt-in for cross-instance state).
- Child mast's session is *distinct* (own session ID, own event stream) but *linked* via `parent_session_id` in event metadata.

### Cross-instance HITL

When a child mast's session pauses via HITL:

- Child emits `RequestInputEvent` normally.
- Child's federation adapter *forwards* the event to parent instead of surfacing locally.
- Parent's federation-invoke tool pauses (per [`./durable-execution-design.md`](./durable-execution-design.md) programmatic pause pattern).
- Parent's attach layer surfaces the HITL event through parent's own attach → mast-web.
- Operator responds via parent's mast-web; response propagates back to child; child resumes; parent unpauses.

Net effect: operator sees one unified HITL experience even though the pause originated on a different mast instance.

**Fallback:** if the federation link is broken (parent mast unreachable), child mast fallbacks to surfacing HITL through its own attach layer. Chose-one-operator wins; the other's request is superseded on resume.

### Cross-instance durability

The session-durable pause primitives ([`./durable-execution-design.md`](./durable-execution-design.md)) work across mast-native federation:

- If parent mast crashes while child is running, child continues; on parent restart, parent replays from last durable event; the `invoke_remote_agent` call resumes from the pause it created; polls child for state.
- If child mast crashes, parent's paused state persists; on child restart, child replays from last durable event (matching session_id + parent context); parent's poll succeeds.
- If both crash: both replay from last durable events; parent's poll re-establishes when child is back.

**Session store must be either shared or per-instance-with-linkage.** Two options per [`./deployment-design.md`](./deployment-design.md):
- Shared session store — both parent + child write to same DB with distinct session IDs; cross-instance queries work natively.
- Per-instance stores — each instance owns its own store; federation adapter carries session references + polls remote.

### Cross-instance memory

Per [`./memory-design.md`](./memory-design.md), memory scopes: session / tenant / global. Federation:

- **Session-scoped memory** doesn't propagate cross-instance (child has its own session). Explicit propagation via `federation.state_propagation` field per above.
- **Tenant-scoped memory** propagates naturally if both instances share the tenant scope + memory store.
- **Global-scoped memory** is universally readable.

Cross-instance memory writes require both instances to share the memory store — otherwise child's derivations don't reach parent's readers. In per-instance-store deployments, cross-instance memory access requires an explicit read RPC (v0.3+).

## Cross-tenancy federation

Federation across tenant boundaries is a special case with harder auth + governance requirements.

- **Same-tenant federation** (tenant T's workload dispatches to another mast serving tenant T): auth propagates; tenant scope preserved; memory access as normal.
- **Cross-tenant federation** (tenant T's workload dispatches to another mast serving tenant U): requires explicit opt-in from both sides. Workload bundle field: `federation.cross_tenant.allow: [tenant-list]`. Auth requires token with appropriate cross-tenant scope. Memory access denied by default; explicit opt-in per key.
- **Cross-organizational federation** (tenant T at company A federates to tenant Q at company B): mast-native adapter should not be used across trust boundaries; use A2A adapter with proper auth. Company B's mast is opaque to company A's; only the A2A contract is visible.

## Failure modes

Federated systems fail in more ways than single-instance systems. Named failure modes + mast's response:

| Failure | Detection | Response |
|---|---|---|
| **Remote agent unreachable** (network partition, DNS failure, endpoint down) | Timeout or connection refused | Adapter returns `federation.ErrUnreachable`; planner sees a tool failure; retry policy per `budget.retry_policy` (bundle-configured); after retries, `hitl_policy.on_ambiguity` applies |
| **Remote agent returns malformed response** (schema mismatch) | Response validation | `federation.ErrSchemaViolation`; planner treated as tool failure; observability alert |
| **Remote agent hangs indefinitely** | Timeout expires | Adapter cancels; `federation.ErrTimeout`; retry per policy |
| **Cross-instance HITL response lost** (parent dies during operator response transit) | Child's HITL resume timeout | Child pauses again with same InterruptID; parent replays on restart; operator response idempotent per `ResumeToken` |
| **Federation loop** (A dispatches to B; B dispatches to A; ad infinitum) | Session's parent-chain depth check | Adapter refuses invocation past max-depth (default 5); alert |
| **Cost blowup** (federated agent runs unbounded cost against parent's budget) | Per-invocation `max_cost_usd` on remote agent config + composed with bundle budget | `federation.ErrBudgetExhausted`; parent's `hitl_policy.on_budget_exhaustion` applies |
| **Protocol version mismatch** (parent expects protocol v2; remote speaks v1) | Version negotiation on connection | `federation.ErrProtocolMismatch`; adapter refuses; observability alert; operator resolves via config or upgrade |
| **Auth failure** (token expired, revoked, missing scope) | Remote's 401/403 response | `federation.ErrAuthFailed`; treated as tool failure; may escalate to HITL for token refresh |
| **Partial cross-tenant leakage** (session-state propagation carries a key that shouldn't cross) | Not detected automatically | Rely on `federation.state_propagation` explicit-list discipline; audit log captures propagated keys; post-hoc review catches mistakes |

## Configuration surface

Per-remote-agent configs live under `.agents/remote/*.yaml` (mast-native + HTTP/RPC) or `.agents/a2a/*.yaml` (A2A). Workload bundles enumerate which remote agents are available:

```yaml
# .agents/workloads/incident-triage.yaml (federation-relevant partial)
federation:
  remotes:
    - a2a://external-triage           # A2A agent, statically-configured
    - mast://tenant-B.core-fleet/analyze   # mast-native federation
    - http://internal-scanner         # bespoke HTTP wrapped as agent
  state_propagation: [audit_context]  # session keys to propagate across mast-to-mast calls
  cross_tenant:
    allow: []                         # empty = no cross-tenant federation
  max_federation_depth: 3             # abort federation chains deeper than this
  budget:
    per_remote_max_cost_usd: 1.00     # cap per-remote-call spend
```

Planner sees the `federation.remotes` list as tool vocabulary via `invoke_remote_agent`. Bundle authors control the surface exposed.

## Composition with other subsystems

| Subsystem | Federation interaction |
|---|---|
| **A2A** ([`./a2a-design.md`](./a2a-design.md)) | A2A is one adapter; federation uses it for `a2a://` references. Auth and agent-card discovery live in a2a-design. |
| **Orchestration (planner)** | Planner's `invoke_remote_agent(reference, ...)` tool selects adapter based on reference protocol. Bundle enumerates permitted remotes. |
| **Specialists** | External remote agents (via any adapter) can appear as specialist-shaped tools; the distinction between local specialist + remote agent is where execution happens, not what the planner sees. |
| **Workflow scaffolding** | Federation composes with reference shapes: supervisor+workers where workers are remote; adversarial-verifier where one leg is remote; map-reduce where reducer is remote. |
| **Durable execution** | Cross-instance pause/resume; parent-child session correlation; snapshot/replay across federation. Mast-native adapter has native durability propagation; A2A adapter has long-running-task-with-HITL propagation. |
| **Deployment** | Federation topologies map to deployment topologies. Star ↔ single supervisor + worker fleet. Mesh ↔ multi-region peer fleets. Hierarchical ↔ multi-region-with-regional-supervisors. Load-balancer + service-mesh integration for mast-native routing. |
| **Multi-tenant** | Cross-tenant federation requires explicit opt-in; auth propagates tenant claim; memory access denied by default across tenants. |
| **Observability** | Distributed traces span federation boundary (`traceparent` propagation). Metrics: `mast_federation_invocations_total{adapter, remote}`, `mast_federation_invocation_duration_seconds`, `mast_federation_errors_total{adapter, error_type}`. |
| **Memory** | Cross-instance memory access via shared store (transparent) or explicit RPC (v0.3+). Session-state propagation is explicit-list per workload. |
| **MCP** | Related-but-distinct. MCP = tools; federation = agents. A mast agent invoking `invoke_mcp_tool` is not federation; invoking `invoke_remote_agent` is. Different design surfaces. |
| **Attach + mast-web** | Cross-instance HITL surfaces on parent's attach; mast-web view of a federated session shows both parent + child events (correlated via `parent_session_id`). |
| **Library API** | `federation.Adapter` extension point; programmatic remote-agent registration for library consumers. |
| **Config layout** | `.agents/remote/*.yaml` for HTTP/RPC + mast-native remote configs; per [`./config-layout-design.md`](./config-layout-design.md) discovery + hot-reload rules. |

## Phasing

| Version | Scope |
|---|---|
| **v0.1** | A2A adapter (per [`./a2a-design.md`](./a2a-design.md)) — synchronous A2A calls only. `invoke_remote_agent` tool in planner. Static remote configs. |
| **v0.2** | HTTP/RPC adapter (bespoke service wrapping). Mast-native adapter over HTTP/JSON (basic). Cross-instance session references (parent knows child's session ID; child records parent). Federation observability. Static star topology deployments. |
| **v0.3** | Mast-native adapter over gRPC with session-state propagation, cross-instance HITL, cross-instance durability, shared-store optimizations. Hierarchical topology deployments. Cross-tenant federation with opt-in. |
| **v0.4+** | Mesh topology. Cross-organizational federation via A2A + auth-boundary policies. Federation-native bundle-learning (learn which remotes work well for which task shapes). |

## Open questions

1. **Reference URI scheme finalization.** `a2a://`, `mast://`, `http://`, `grpc://` as I've sketched. Are these compatible with URI parsers, or do we need mast-specific parsing? Bias: use standard URI parsing; scheme becomes the adapter selector; keep it simple.
2. **Fleet discovery mechanism defaults.** Kubernetes native (Service + EndpointSlice) vs. Consul/etcd vs. static config. Bias: Kubernetes native as default for GKE (most common target); static config for standalone; Consul/etcd as v0.3+ opt-in.
3. **Federation loops beyond depth-check.** Depth-5 default is arbitrary. Should the planner emit a "federation-loop-detected" event and let observability alert? Bias: yes, always emit; the check is defense-in-depth, but observability lets operators tune per-workload.
4. **Cross-instance memory write authority.** Can a child mast write to parent's memory scope? Bias: no — child writes to its own scope; parent reads child's scope via federation-mediated read. Simpler auth story.
5. **Federation across ADK versions.** If mast v0.5 talks to mast v0.7, does the mast-native protocol still work? Bias: mast-native protocol is versioned; incompatible versions fall back to A2A (which is standardized).
6. **Load balancing across fleet members.** `mast://fleet-name` load-balances how? Round-robin, least-connections, session-affinity? Bias: least-connections default; per-workload override; session-affinity for follow-up calls within one parent's execution.
7. **Federation registry (as opposed to A2A registry).** For mast-native fleets, is there a lightweight registry pattern separate from A2A's? Bias: Kubernetes Service + Label selector is the registry for GKE deployments; static config for others; no mast-specific registry service.
8. **Federated bundles.** Can a workload bundle *itself* be a federation reference (bundle registered at parent, executed at child)? Bias: not for v0.1; adds bundle-distribution complexity. Bundles stay per-instance.
9. **Cost attribution across mast-native federation.** Child's spend accrues to parent's budget natively (via mast-native protocol richer than A2A). But what if child spins up further federation? Bias: child's federation spend accrues to child's per-invocation budget from parent; total tree-cost visible via observability traces.
10. **Ambient authorization for mast-native.** Kubernetes ServiceAccount + Workload Identity gives implicit auth between mast instances in same cluster. Should mast-native protocol trust ambient auth by default? Bias: yes, for within-cluster; explicit tokens for cross-cluster; explicit strong tokens for cross-organizational.

## Out of scope

- **A2A-specific integrations** (Google Agent Registry, kagent). Those live in [`./a2a-design.md`](./a2a-design.md).
- **Distributed transaction coordination across mast instances.** Federation is agent-shaped, not database-shaped; no two-phase commit across federated agent calls.
- **Automated federation topology optimization.** Manual topology design; observability surfaces bottlenecks; operator restructures.
- **A federation-native protocol contribution back to A2A.** Interesting long-term but not v0.1-v0.3 scope. We build mast-native where mast-native's richness matters; use A2A elsewhere.
- **Cross-runtime federation with Python ADK.** Python ADK does its own federation; interop via A2A only (per a2a-design cross-runtime notes).
- **Federation over non-HTTP transports** for the mast-native adapter (Unix sockets for co-located instances, message queues for async fan-out, etc.). Interesting; v0.4+ if consumer asks.
- **Automatic remote-agent capability discovery** (mast introspecting an arbitrary HTTP endpoint to derive schema). Explicit schema authoring for HTTP/RPC adapter.

## Related

- [`./a2a-design.md`](./a2a-design.md) — A2A adapter's underlying protocol integration
- [`./orchestration-design.md`](./orchestration-design.md) — planner tool vocabulary includes `invoke_remote_agent`
- [`./durable-execution-design.md`](./durable-execution-design.md) — cross-instance pause/resume + snapshot/replay
- [`./deployment-design.md`](./deployment-design.md) — federation topology + deployment topology mapping
- [`./library-api-design.md`](./library-api-design.md) — `federation.Adapter` extension point
- [`./observability-design.md`](./observability-design.md) — distributed tracing + federation metrics
- [`./memory-design.md`](./memory-design.md) — cross-instance memory scope + propagation rules
- [`./mcp-catalog-design.md`](./mcp-catalog-design.md) — sibling pattern (tools vs. agents)
- [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) — reference shapes compose with federation (supervisor+workers with remote workers)
- [`./specialists-design.md`](./specialists-design.md) — remote agents can appear as specialist-shaped tools
- [`./config-layout-design.md`](./config-layout-design.md) — `.agents/remote/*.yaml` file layout
