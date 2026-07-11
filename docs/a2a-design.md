# mast A2A support: design

**Status:** draft, 2026-07-01. Companion to [`./positioning.md`](./positioning.md) (agent-interop is table stakes for the platform-substrate positioning; agent-card publishing is already on the keep list), [`./federation-design.md`](./federation-design.md) (federation as pattern; A2A as one protocol adapter within it), [`./specialists-design.md`](./specialists-design.md) (external A2A agents can appear as specialist-shaped tools), [`./orchestration-design.md`](./orchestration-design.md) (workload bundles can reference A2A servers + expose workloads as A2A agents), [`./library-api-design.md`](./library-api-design.md) (A2A client + server APIs are extension points), [`./mcp-catalog-design.md`](./mcp-catalog-design.md) (protocol sibling — MCP for tools, A2A for agents), [`./deployment-design.md`](./deployment-design.md) (A2A discovery in-cluster), [`./durable-execution-design.md`](./durable-execution-design.md) (A2A calls that pause survive restart), and [`./observability-design.md`](./observability-design.md) (A2A calls get first-class trace/metric coverage). Covers **A2A as a protocol integration** — the ecosystem contract mast supports for interoperability with Google Agent Registry / Runtime, kagent, and similar frameworks.

## Why A2A as first-class

Mast as a substrate for agent workloads is only complete if the agents built on it can interoperate with the broader agent ecosystem. Without A2A, mast is an island — great inside itself, invisible to platform teams standardizing on cross-framework agent registries.

Concrete scenarios that require A2A:

- **Platform team standardizes on Google Agent Registry.** All agents (whatever framework) get registered; discoverable + callable via A2A. Mast agents that can't publish A2A endpoints don't appear in the registry — operationally invisible to the platform.
- **kagent-based cluster runs mixed agent frameworks.** Mast agents alongside Python ADK agents alongside LangGraph agents. Cross-framework composition (mast planner dispatching to a Python ADK specialist; a Python ADK coordinator invoking a mast workload) requires a common protocol. A2A is that protocol.
- **Multi-vendor agent workflows.** A customer standardizes on a mixed stack — mast for platform-team unattended workloads, framework X for their data team's exploratory analysis. Both need to talk to a shared incident-response agent living somewhere else. A2A gives them one integration story.
- **Federated deployments across organizational boundaries.** Two organizations run mast internally; one wants to expose specific agent capabilities to the other; A2A + auth is the boundary.

The alternative — mast-native protocol only — locks operators into a mast-only fleet, which contradicts positioning's "substrate for platform teams" framing (platform teams don't standardize on single-vendor stacks).

## A2A protocol overview

Brief. Assumes the reader can consult the [A2A spec](https://a2aproject.github.io/A2A/) for wire-level detail; this doc is about mast's *integration* with it.

Core A2A concepts:

- **Agent card.** Public metadata document (`/.well-known/agent-card.json` conventionally) describing an agent's capabilities, endpoints, supported input/output modes, auth requirements, and skills. Discovery-time contract.
- **Tasks.** A2A interactions are structured as tasks — the client sends a task; the server processes; task progresses through states (submitted → working → input-required / completed / failed / canceled). Long-running tasks are natively supported.
- **Skills.** Named capabilities an agent exposes ("investigate incident", "generate report", "review config"). Each skill has structured input/output schema.
- **Messages + parts.** Task communication is via messages (bidirectional); each message contains parts (text, file, structured data). Similar shape to LLM tool-call schemas.
- **Streaming.** Servers can stream intermediate task state via SSE or similar transport.
- **Authentication.** Token-based (bearer tokens; OAuth 2.0 patterns); auth requirements advertised in agent card.
- **Push notifications.** For long-running tasks where the client polls too infrequently, servers can push updates to a client-provided webhook.

Mast's role: implement both sides of this protocol correctly — server (mast agents callable via A2A) + client (mast agents calling A2A) — and layer mast-specific composition on top.

## Mast as A2A server

Mast exposes its agents to the A2A ecosystem via a standard endpoint. Multiple agents can be exposed from one mast instance; each corresponds to a *workload* (with A2A-specific config).

### Which agents get exposed

Only workloads that opt in are exposed via A2A. Bundle field:

```yaml
# .agents/workloads/incident-triage.yaml (A2A section)
a2a:
  expose: true
  skill_name: incident-triage
  skill_description: |
    Investigate GKE pod-failure incidents. Given a pod reference and
    a symptom description, returns root cause + concrete remediation.
  input_schema:                    # A2A skill input schema
    type: object
    properties:
      pod: {type: string, description: "namespace/pod format"}
      symptom: {type: string, description: "operator observation"}
    required: [pod, symptom]
  output_schema:                   # A2A skill output schema
    type: object
    properties:
      root_cause: {type: string}
      remediation: {type: string}
      confidence: {type: number}
  auth:
    required: true
    scopes: [incident-triage, incident-triage.read]
```

Workloads without an `a2a` section are not exposed via A2A. This is deliberate: A2A exposure has real ops implications (auth setup, external contract stability, cross-org discoverability); operators opt in per workload.

### Endpoint layout

Standard A2A endpoints on the mast HTTP listener (or on a separate port, configurable via [`./config-layout-design.md`](./config-layout-design.md)):

| Path | Purpose | v0.X |
|---|---|---|
| `/.well-known/agent-card.json` | Aggregated agent card (all exposed workloads as skills) | v0.1 |
| `/a2a/tasks/send` | Submit a task (JSON-RPC over HTTP) | v0.1 |
| `/a2a/tasks/{id}` | Get task state | v0.1 |
| `/a2a/tasks/{id}/cancel` | Cancel a task | v0.1 |
| `/a2a/tasks/subscribe` | Subscribe to task updates (SSE) | v0.2 |
| `/a2a/tasks/pushNotification/set` | Configure push notifications | v0.2 |

The agent card at `/.well-known/agent-card.json` aggregates all exposed workloads as skills — one card per mast instance, N skills within it. If operators need per-workload cards (some registries require distinct endpoints per agent), mast can also serve per-workload cards at `/.well-known/agent-card/<workload-name>.json` — configurable.

### Task lifecycle mapping

A2A task states map onto mast session states:

| A2A state | Mast session state | Notes |
|---|---|---|
| `submitted` | Session created; not yet started | Server-side acknowledgment |
| `working` | Session running (any agent turn / node execution) | Progress events surface as task update messages |
| `input-required` | Session paused via HITL (`RequestInputEvent`) | A2A input-required maps to mast HITL — same primitive |
| `completed` | Session terminated via `finish_task` | Response is the `finish_task` argument |
| `failed` | Session errored | Error surface passes through structured |
| `canceled` | Session aborted (via `mast sessions abort` or A2A cancel) | Idempotent |

**Long-running tasks** are supported natively via mast's durable execution ([`./durable-execution-design.md`](./durable-execution-design.md)) — a task can run for hours, pause across process restart, resume, and complete without the A2A client re-submitting. This is arguably where mast + A2A shine — most agent frameworks don't survive infrastructure churn during long tasks.

### Auth model

Bearer tokens per A2A convention. Token resolution:

- **Per-workload scopes** declared in bundle's `a2a.auth.scopes` field.
- **Token validator** pluggable via [`./library-api-design.md`](./library-api-design.md) extension point (`a2a.TokenValidator` interface). Built-in validators: JWT (via configured issuer + JWKS), static bearer tokens (for simple deployments), Google-signed IAM tokens (for Google Cloud–hosted deployments), OAuth 2.0 introspection.
- **Scope check per skill call.** Token must carry the skill's required scopes; missing scope → 403.
- **Audit trail.** Every A2A task carries the authenticated principal in the session eventlog; audit-derived memory + observability can attribute costs per caller.

For Google Agent Registry integration specifically, the registry's own auth flow (typically OAuth 2.0 with Google-issued tokens; Workload Identity in-cluster) is the source of tokens; mast validates against Google's JWKS.

### Request → session mapping

An A2A task submission → mast session:

1. Task arrives at `/a2a/tasks/send` with skill name + inputs + auth token.
2. Mast validates token; checks scope against skill's required scopes.
3. Mast maps skill name to workload bundle (`a2a.skill_name` field).
4. Mast starts a session with the resolved bundle; task inputs become the session input.
5. Task ID is generated (also serves as session ID; content-addressable UUID v7 per [`./durable-execution-design.md`](./durable-execution-design.md)).
6. Task state transitions emit A2A task update messages; consumers can subscribe (SSE) or poll.
7. Session `finish_task` produces the A2A completed state with the finish output as the response.

**Tenant scope propagation.** A2A token can carry a tenant claim (`tenant_id` or similar). Mast maps this to `WithIsolationScope(tenantID)` on the session per [`./deployment-design.md`](./deployment-design.md).

## Mast as A2A client

Mast agents (specialists, planner, workflow nodes) can call external A2A agents as tools. Two shapes:

### Static registration

External A2A agent configured statically via `.agents/a2a/`:

```
.agents/a2a/
  external-triage-agent.yaml
  compliance-scanner.yaml
  ...
```

```yaml
# .agents/a2a/external-triage-agent.yaml
name: external-triage
agent_card_url: https://triage-service.example.com/.well-known/agent-card.json
skills: [investigate-incident]         # subset of agent's skills mast may invoke; empty = all
auth:
  type: bearer
  token_env: EXTERNAL_TRIAGE_TOKEN     # env-var reference; never file-embedded
  # or:
  # type: google-iam
  # target_audience: https://triage-service.example.com
timeout_seconds: 300
```

Referenced from workload bundles + specialists like MCP servers:

```yaml
# .agents/workloads/incident-triage.yaml (partial)
a2a_agents:                           # A2A clients this bundle can invoke
  - external-triage
tool_catalog:
  # ... existing tool catalog config
```

The planner treats external A2A agents as another class in its tool vocabulary — `invoke_a2a_agent(name, skill, inputs)`. Composes with `invoke_specialist` and `run_shape_*` uniformly (see [`./federation-design.md`](./federation-design.md) for the unified `invoke_remote_agent` tool).

### Dynamic discovery via registry

For deployments integrated with an A2A registry (Google Agent Registry, kagent's registry, custom):

```yaml
# .agents/a2a/registry.yaml
registries:
  - name: google-agent-registry
    endpoint: https://agentregistry.googleapis.com/v1
    auth:
      type: google-iam
    filters:
      # Restrict which agents mast may discover + invoke
      required_labels: {environment: production}
      skill_prefixes: [platform-, security-]
```

At startup (and on `SIGHUP`), mast queries the registry, fetches agent cards for matching agents, and registers them the same way as statically-configured ones. Discovered agents appear with names derived from the registry (`registry-name/agent-id`).

### Call lifecycle

Client-side A2A call from within a mast agent:

1. Planner (or specialist, or tool node) invokes `invoke_a2a_agent(name, skill, inputs)`.
2. Mast resolves `name` to a configured A2A agent (static or discovered).
3. Mast constructs the A2A task submission — validates inputs against the skill schema from the agent card; attaches auth token from the configured resolver; propagates `traceparent` for distributed tracing.
4. Task submitted via HTTP; task ID returned.
5. **If the skill is short-running** (agent card advertises or task completes within a threshold): mast waits synchronously; result returns to the caller.
6. **If the skill is long-running** (task takes longer than threshold, or agent card indicates streaming): mast pauses the calling session per [`./durable-execution-design.md`](./durable-execution-design.md) — programmatic pause with `Reason: a2a_task_pending`; resumes when the A2A task reaches a terminal state (completed / failed / canceled) via either subscription or push-notification callback.
7. On resume, task output surfaces as the tool result to the caller.

**HITL propagation.** If the remote A2A agent's task hits `input-required` state (remote HITL), mast propagates: the calling session sees an `input-required` sub-state, surfaces it via attach mode as if it were mast's own HITL. Operator responds via mast-web; response is forwarded to the remote A2A agent's task; remote task resumes.

### Cost tracking

External A2A calls have their own cost implications:

- The remote agent's cost is opaque to mast (mast doesn't have visibility into the remote agent's provider spend).
- Mast tracks *its own* time-and-tokens (network overhead, wait time) as spans.
- Remote agents can optionally report cost back to the client via a well-known A2A extension (per open question 4). If they do, mast attributes it as remote cost in the bundle budget.

## Agent card publishing

Positioning already mentions "agent-card publishing" as a keep item. A2A gives that a concrete standard shape.

The agent card at `/.well-known/agent-card.json` contains:

```json
{
  "name": "mast-instance-name",
  "description": "GKE platform-team incident-triage agent hosted by mast",
  "url": "https://mast-instance.example.com/a2a",
  "version": "0.1.0",
  "provider": {
    "organization": "example.com platform team",
    "url": "https://mast-instance.example.com"
  },
  "capabilities": {
    "streaming": true,
    "pushNotifications": true
  },
  "authentication": {
    "schemes": ["bearer", "oauth2"]
  },
  "defaultInputModes": ["text/plain", "application/json"],
  "defaultOutputModes": ["text/plain", "application/json"],
  "skills": [
    {
      "id": "incident-triage",
      "name": "Incident triage",
      "description": "...",
      "tags": ["incident-response", "gke"],
      "inputModes": ["application/json"],
      "outputModes": ["application/json"]
    }
    // ... one per exposed workload
  ]
}
```

**Skills correspond to A2A-exposed workloads.** Bundle authors write the workload; mast generates the skill entry from the bundle's `a2a` section. Auto-refreshes on config reload (see [`./config-layout-design.md`](./config-layout-design.md) `SIGHUP` behavior in v0.2).

**Provider metadata + version** derived from mast build metadata + operator-configured deployment info.

**Capabilities negotiation** — mast declares what it supports (streaming from v0.2; push notifications from v0.2; long-running tasks always).

## Registry integrations

### Google Agent Registry / Runtime

Google Agent Registry is Google's directory of A2A-reachable agents; Google Agent Runtime is a managed runtime for hosting A2A agents. Mast agents integrate as:

- **Registry publication.** Mast has a first-class command `mast a2a publish --registry=google` that submits the agent card to Google Agent Registry. Requires GCP credentials + agent-publisher IAM role.
- **Discovery.** `.agents/a2a/registry.yaml` names Google Agent Registry as a source (per above); mast discovers other Google Registry–listed agents for planner invocation.
- **Runtime targeting.** Mast agents deployable *to* Google Agent Runtime (as a managed hosting option) — deployment starter ships under `examples/deploy/gcp-agent-runtime/`.

**Skill discovery.** Google Agent Registry also catalogs SKILL.md bundles alongside A2A agents (see [`./skills-design.md`](./skills-design.md)). Mast's skill loader uses the same registry endpoint + Google IAM auth as A2A discovery — one credential set covers both surfaces. Skill entries and agent entries are distinguished by resource-type in the registry response.

### kagent

kagent is a Kubernetes-native agent framework with its own A2A-compatible registry:

- **Registry integration.** kagent's registry is A2A-compatible; same discovery mechanism as Google's (different endpoint URL).
- **Deployment integration.** Mast agents can register with kagent's registry on startup; `examples/deploy/kagent/` shows the pattern.
- **Peer interoperation.** A mast planner in a kagent cluster can invoke kagent-registered non-mast agents; kagent-native agents can invoke mast workloads.

### Other registries

The A2A spec allows any registry that supports the standard discovery contract. Additional registries can be added:

- LangGraph's registry (if/when they publish an A2A adapter).
- Corporate internal registries (many organizations build their own agent catalogs).
- Static registry (mast can consume a static YAML file listing agents — useful for air-gapped deployments without a live registry).

## Composition with other subsystems

| Subsystem | A2A interaction |
|---|---|
| **Specialists** | External A2A agents can appear as *A2A-shaped specialists* — an operator adds `.agents/a2a/*.yaml` and references them in bundles / specialist tool allowlists. Compare/contrast: local specialists = in-process, budget-composed, direct tool access; A2A specialists = remote, opaque budget, protocol-mediated. Both are valid; choice depends on locality + trust. |
| **Orchestration (workloads)** | Bundle `a2a.expose` field opts a workload into A2A publication; `a2a_agents` field enumerates A2A clients available in the bundle. Planner sees both as extended vocabulary. |
| **Federation** | A2A is one protocol adapter; mast-native + HTTP/RPC are others. See [`./federation-design.md`](./federation-design.md) for the pattern. `invoke_remote_agent(reference, ...)` selects the appropriate adapter based on reference. |
| **MCP** | Sibling protocol. MCP for **tools** (structured calls to defined-schema functions); A2A for **agents** (task-based interactions with negotiable state, streaming, HITL). Not overlapping. Can coexist: a mast agent might use MCP for tool calls + A2A to invoke other agents in the same session. |
| **Attach mode + mast-web** | Distinct from A2A. Attach is *operator-facing* (human at a browser); A2A is *agent-facing* (another agent or framework). mast-web renders A2A-hosted skills in a directory view; can trigger A2A tasks manually (operator-initiated). |
| **Durable execution** | A2A calls to long-running skills pause the calling session; resume on remote-task-terminal-state. A2A tasks mast hosts as server survive process restart. Session ID + A2A task ID are the same ID for consistency. |
| **Multi-tenant** | A2A auth token can carry tenant claim; maps to `WithIsolationScope`. Cross-tenant A2A calls require explicit opt-in (bundle field). |
| **Observability** | A2A spans emit alongside other spans; distributed tracing across mast→A2A→remote-agent (with `traceparent` propagation). Metrics: `mast_a2a_client_calls_total`, `mast_a2a_server_tasks_total`, `mast_a2a_task_duration_seconds`. |
| **Library API** | `a2a.Server` + `a2a.Client` interfaces exposed for library-embedded consumers. Custom `a2a.TokenValidator` as extension point. Programmatic agent-card generation for consumers publishing A2A alongside their own service. |
| **Deployment** | A2A server endpoint typically fronted by Ingress + TLS; auth via IAM (GCP) / service mesh (Istio) / bearer tokens. Per-topology deployment starters ship A2A wiring examples. |
| **Config layout** | `.agents/a2a/` for A2A client configs; agent-card generation from `.agents/workloads/*` `a2a` sections. Precedence + discovery follow config-layout doc. |

## Phasing

| Version | Scope |
|---|---|
| **v0.1** | Server: agent card publication (`/.well-known/agent-card.json`), synchronous task submission (`/a2a/tasks/send`), task-state polling (`/a2a/tasks/{id}`), task cancel, bearer-token auth with a pluggable validator (`a2a.TokenValidator`). Client: static A2A agent configs in `.agents/a2a/`, synchronous invocation, mapping `invoke_a2a_agent` to `agenttool`-shaped tool. Google Agent Registry publishing command (`mast a2a publish --registry=google`). |
| **v0.2** | Streaming (SSE) for both server + client. Push notifications for long-running tasks. Dynamic registry discovery (Google Agent Registry, kagent, static). HITL propagation across A2A (mast-hosted A2A skill's HITL surfaces to mast-web; remote A2A HITL surfaces to mast client's operator). Per-workload agent-card endpoints. |
| **v0.3** | Cost attribution across A2A boundary (opt-in extension). Federation patterns fully wired (per [`./federation-design.md`](./federation-design.md)). Multi-registry composition. Auth: OAuth 2.0 flows end-to-end; Google IAM Workload Identity as default in GKE deployments. |
| **v0.4+** | Cross-runtime resume via A2A + Python ADK (per durable-execution's cross-runtime-resume commitment). A2A-specific bundle-learning: learn which external agents work well for which task shapes. Advanced agent-card capability negotiation (input-mode preferences, streaming preferences). |

## Open questions

1. **A2A protocol version pinning.** A2A spec evolves; mast should support a specific version range. Bias: `>= 1.0, < 2.0` initially; version-negotiate in agent-card exchange; document tested-against versions per mast release.
2. **Agent-card refresh cadence for discovered agents.** Cached agent cards go stale (endpoint changes, skill schema changes, auth requirements change). Bias: refresh on `SIGHUP` (per [`./config-layout-design.md`](./config-layout-design.md)) + on-demand refresh via CLI (`mast a2a refresh <agent-name>`) + optional TTL-based refresh (opt-in per agent).
3. **Streaming task output → planner reasoning.** When mast is A2A client and remote skill streams updates, does the calling planner see updates mid-tool-call or only the terminal state? Bias: only terminal state for v0.2 (matches Task-mode's `finish_task` shape); streaming updates surface as observability spans, not as planner reasoning inputs.
4. **Cost attribution extension.** No standard A2A extension for cost reporting yet. Bias: propose one to the spec community; ship mast-side as opt-in header (`Mast-Cost-USD`) until standardized.
5. **A2A vs. federation-native protocol between mast instances.** When two mast instances federate, should they use A2A (standard, works with everything) or a mast-native protocol (richer — cross-instance session state, native memory propagation)? Discussed in [`./federation-design.md`](./federation-design.md); default is A2A for cross-organization boundaries, mast-native within a trusted fleet.
6. **Rate limiting on the A2A server side.** External callers can consume mast's provider budget via A2A calls. Bias: per-caller (token subject) rate limits; per-workload concurrent-task caps; budget-per-caller as an extension.
7. **Agent-card discovery from operators.** How does an operator find A2A agents to add to `.agents/a2a/`? Bias: registry-based (see registry section); manual (paste URL). No mast-native agent directory.
8. **A2A tasks that need external tool access.** A mast-hosted A2A skill can invoke local specialists + MCP tools; but can it invoke *other A2A agents* transitively? Bias: yes; A2A composition depth is bounded only by budget + cycle detection. Watch for federation-loop-anti-patterns.
9. **Skill schema authoring ergonomics.** JSON Schema in YAML is awkward. Consider a friendlier authoring surface? Bias: v0.1 uses YAML-embedded JSON Schema (matches specialists doc's pattern); revisit if operators complain.

## Out of scope

- **Building an A2A registry service.** We publish to registries + consume from registries; we don't run one. Google Agent Registry / kagent / customer-internal registries are the substrates.
- **A2A protocol design contributions.** Spec evolution happens upstream; mast follows.
- **A2A-native workflow orchestration.** Multi-step workflows composed *entirely* of A2A-hosted agents are the federation doc's territory, not this one.
- **Automatic agent-card promotion to registries.** Operators run `mast a2a publish` explicitly; no auto-publication on config change (safety).
- **A2A-to-MCP proxy.** Exposing MCP tools as A2A skills or vice versa is possible but not in scope — different protocol shapes serving different purposes.
- **A2A-based fine-tuning / model updates.** Not what A2A is for.
- **A2A over non-HTTP transports.** HTTP + SSE + optionally WebSockets. gRPC or QUIC transports are v1.0+.

## Related

- [A2A protocol spec](https://a2aproject.github.io/A2A/) — upstream protocol
- [Google Agent Registry / Runtime](https://cloud.google.com/agent-registry) — Google's A2A-based registry + runtime
- [kagent](https://github.com/kagent-dev/kagent) — Kubernetes-native agent framework with A2A support
- [`./federation-design.md`](./federation-design.md) — pattern that uses A2A as one adapter
- [`./positioning.md`](./positioning.md) — agent-card publishing keep item; A2A is the concrete standard
- [`./specialists-design.md`](./specialists-design.md) — A2A-shaped specialists
- [`./orchestration-design.md`](./orchestration-design.md) — workload bundle `a2a.*` fields
- [`./durable-execution-design.md`](./durable-execution-design.md) — long-running A2A tasks + cross-boundary pause/resume
- [`./observability-design.md`](./observability-design.md) — A2A trace propagation + metrics
- [`./deployment-design.md`](./deployment-design.md) — A2A endpoint deployment topology
- [`./mcp-catalog-design.md`](./mcp-catalog-design.md) — sibling protocol comparison (MCP for tools, A2A for agents)
- [`./library-api-design.md`](./library-api-design.md) — `a2a.Server`, `a2a.Client`, `a2a.TokenValidator` interfaces
- [`./config-layout-design.md`](./config-layout-design.md) — `.agents/a2a/` file layout
