# mast AG-UI support: design

**Status:** draft, 2026-07-01 (updated 2026-07-25 — corrections pass: the interrupt lifecycle + Activity/Reasoning events are **draft AG-UI spec extensions**, explicitly changeable before finalization — building v0.1... v0.2 HITL on them is a deliberate, labeled bet (mitigation: version-pin the community Go SDK, isolate interrupt encoding behind `pkg/agui`); CopilotKit package names corrected to the published `@copilotkit/channels-*` line (`bot-*` names never shipped; naming still in flux); "push notifications" and "reconnect-and-resume-stream" are **mast extensions**, not AG-UI-spec patterns (the spec defines neither a webhook push pattern nor run-reattach semantics); the unverified Bedrock-AgentCore-speaks-AG-UI claim is cut (AgentCore documents an A2A contract); AG-UI moves wholesale to v0.2 per [`./fork-design.md`](./fork-design.md)'s 2026-07-25 re-cut, which also resolves the v0.1-adapter contradiction with [`./federation-design.md`](./federation-design.md)). Companion to [`./positioning.md`](./positioning.md) (attach mode remains the mast-native operator transport; AG-UI added as the ecosystem-standard user-facing protocol), [`./a2a-design.md`](./a2a-design.md) (protocol sibling: A2A = agent↔agent, AG-UI = agent↔user, MCP = agent↔tool — the fourth corner of the interop surface), [`./specialists-design.md`](./specialists-design.md) (specialists render into AG-UI as tool-call events uniformly), [`./orchestration-design.md`](./orchestration-design.md) (workload bundles can opt in to AG-UI exposure similar to `a2a.expose`), [`./library-api-design.md`](./library-api-design.md) (`github.com/go-steer/mast/agui` package), [`./deployment-design.md`](./deployment-design.md) (AG-UI endpoint deployment topology; chat-platform bots as sidecar workloads), [`./durable-execution-design.md`](./durable-execution-design.md) (AG-UI interrupt lifecycle maps directly onto mast's durable pause/resume), [`./federation-design.md`](./federation-design.md) (mast can be AG-UI client of other AG-UI servers when useful), [`./observability-design.md`](./observability-design.md) (AG-UI spans + metrics), and [`./config-layout-design.md`](./config-layout-design.md) (`.agents/agui/` if needed). Covers **AG-UI as a protocol integration** — the ecosystem contract mast supports for interoperability with CopilotKit-built UIs, chat-platform bots (Slack / Teams / Discord / Telegram / WhatsApp), and any other AG-UI-compatible client.

## Why AG-UI as first-class

The three-cornered interop surface enumerated in [`./mcp-catalog-design.md`](./mcp-catalog-design.md) (MCP tools, A2A agents, skill templates) has been implicitly assuming one client for mast: `mast-web`, our first-party UI that speaks the mast-native attach-mode protocol. That assumption breaks when platform teams standardize on **AG-UI** — the emerging cross-framework standard for agent↔user interaction — as the way user-facing surfaces (chat platforms, embedded assistants, mobile apps) connect to agent backends. Without AG-UI, mast is reachable only via mast-web, and mast agents are invisible to:

- **CopilotKit-built React applications** (any React app using `@copilotkit/react-core` to embed an agent).
- **Chat-platform bots** built on CopilotKit's bot SDK — Slack (via CopilotKit + Slack Bolt), Discord, Teams, Telegram, WhatsApp. Ecosystem SDKs handle the platform-specific plumbing; the agent backend just needs to speak AG-UI.
- **Other AG-UI-compatible clients** (Microsoft Agent Framework's Go integration and Pydantic AI implement the protocol, including the draft interrupt extension; more framework-adjacent clients are landing).

The competitive framing is the same as A2A: *"speaking the standard buys you the ecosystem's velocity; not speaking it means you have to build every integration yourself."* CopilotKit having a first-party Slack bot SDK means an AG-UI-compatible mast gets platform-team incident-triage-in-Slack for free — one of the highest-value integrations for mast's audience.

The four interop surfaces (framing harmonized 2026-07-25 with [`./mcp-catalog-design.md`](./mcp-catalog-design.md) — "surfaces" is canonical; attach mode is mast's native transport, not a fifth surface or a competing "corner"):

| Direction | Protocol | Doc |
|---|---|---|
| agent → tool | MCP | [`./mcp-catalog-design.md`](./mcp-catalog-design.md) |
| agent → agent | A2A | [`./a2a-design.md`](./a2a-design.md) |
| agent → user (ecosystem-standard) | **AG-UI** | this doc |
| operator → agent (mast-native, richer) | Attach mode | [`./positioning.md`](./positioning.md) keep list |

Attach mode stays as the mast-native transport (richer features: full session-state visibility, workflow-node visualization, planner turn detail, federation cross-instance spans, snapshot/replay controls); AG-UI is the *ecosystem* protocol for third-party clients. Same pattern as A2A + mast-native federation.

## AG-UI protocol overview

Brief; consult the [AG-UI docs](https://docs.ag-ui.com/introduction) and the [community Go SDK](https://github.com/ag-ui-protocol/ag-ui/tree/main/sdks/community/go) for wire-level detail. This section captures what's load-bearing for mast's integration.

### Transport

- **HTTP POST** with JSON request body; **SSE response** (`text/event-stream`) for the streamed event sequence.
- Endpoint is agent-application-defined (e.g. `/agentic`, `/chat`, `/mast/run`) — no standard well-known path like A2A's `/.well-known/agent-card.json`. Discovery is out-of-band; the client is told the URL to hit.
- WebSockets are also part of the AG-UI-supported transport set for bidirectional streaming; SSE is the common case.

### Request envelope: `RunAgentInput`

```go
type RunAgentInput struct {
    ThreadID       string            // conversation ID (persists across runs)
    RunID          string            // this run's ID
    ParentRunID    *string           // optional; for branched runs
    State          any               // client-managed state snapshot
    Messages       []Message         // full message history for the run
    Tools          []Tool            // tools the client says are available
    Context        []Context         // client-supplied context entries
    ForwardedProps any               // opaque bag of extras
    Resume         []ResumeEntry     // present when resuming from an interrupt
}

type ResumeEntry struct {
    InterruptID string
    Status      ResumeStatus         // "resolved" or "cancelled"
    Payload     any
}
```

### Response: streamed events

Base envelope: every event carries `type`, `timestamp?`, `rawEvent?`. Event categories:

- **Lifecycle** — `RunStarted`, `RunFinished`, `RunError`, `StepStarted`, `StepFinished`.
- **Text messages** (streaming triad) — `TextMessageStart`, `TextMessageContent(delta)`, `TextMessageEnd`; convenience `TextMessageChunk`.
- **Tool calls** — `ToolCallStart`, `ToolCallArgs(delta)`, `ToolCallEnd`, `ToolCallResult`, convenience `ToolCallChunk`.
- **State management** — `StateSnapshot` (full), `StateDelta` (RFC 6902 JSON Patch), `MessagesSnapshot`.
- **Activity** — `ActivitySnapshot`, `ActivityDelta` for arbitrary in-flight activity indicators (e.g. `"PLAN"`, `"SEARCH"`).
- **Reasoning** — `ReasoningStart`, `ReasoningMessageStart/Content/End`, `ReasoningEncryptedValue`.
- **Special** — `Raw` (passthrough), `Custom` (application-defined).

Correlation IDs: `threadId`, `runId`, `parentRunId`, `messageId`, `toolCallId`, `parentMessageId`, `entityId`.

### HITL / interrupts

Not a distinct event family. HITL is surfaced via the *run lifecycle*:

- Server emits `RunFinished` with `outcome: {type: "interrupt", interrupts: [{id, message, responseSchema, expiresAt?, ...}]}`.
- Run pauses. Client renders the interrupt to the user (form generation from `responseSchema` is expected).
- Client resumes by starting a **new run** whose `RunAgentInput.Resume` array carries one `ResumeEntry` per interrupt (with `Status: "resolved"` or `"cancelled"` and optional payload).
- Server resumes execution from the interrupt point.

This maps *directly* onto mast's existing HITL primitives ([`./durable-execution-design.md`](./durable-execution-design.md) programmatic-pause and external-signal-pause) — same shape, different wire protocol.

### Auth

Not specified by the protocol beyond convention: bearer token in `Authorization: Bearer <token>` header (default), or custom header (e.g. `X-API-Key: <token>`). Server-side auth model is application-defined. Same pattern mast already uses for A2A (see [`./a2a-design.md`](./a2a-design.md) `TokenValidator`).

### Discovery + versioning

- **No standardized discovery** in-protocol (unlike A2A's `/.well-known/agent-card.json`). Clients are configured with endpoint URLs directly; some ecosystems (CopilotKit) provide their own catalog. Mast should still publish agent-card-like metadata for AG-UI clients that expect it, mirroring the A2A pattern.
- **No wire-level version negotiation** yet; the community SDK is pre-1.0 and evolves. Mast tests against pinned versions and documents supported version ranges.

## Mast as AG-UI server

Mast exposes its workloads to AG-UI clients via HTTP+SSE endpoints on the mast HTTP listener. Multiple workloads can be exposed; each is one AG-UI-callable agent.

### Which workloads get exposed

Only workloads that opt in are exposed via AG-UI (mirrors the `a2a.expose` pattern from [`./a2a-design.md`](./a2a-design.md)). Bundle field:

```yaml
# .agents/workloads/incident-triage.yaml (AG-UI section)
agui:
  expose: true
  endpoint_path: /agui/incident-triage           # relative to mast's HTTP root
  description: |
    Investigate GKE pod-failure incidents. Send a run with the pod
    reference + observed symptoms; receive streamed diagnosis and
    proposed remediation with HITL approval for mutating actions.
  input_schema:                                   # applied to Messages[0].content
    type: object
    properties:
      pod: {type: string}
      symptom: {type: string}
    required: [pod, symptom]
  auth:
    required: true
    scopes: [incident-triage.read, incident-triage.write]
  streaming: true                                 # emit incremental events
  activity_events: true                           # emit ActivitySnapshot for planner steps
```

Workloads without an `agui` section are not exposed via AG-UI — same conservative default as A2A. Deliberate: AG-UI exposure has real ops implications (auth setup, external client contract stability, user-facing UX considerations).

### Endpoint layout

- **`<endpoint_path>`** per workload — the POST endpoint clients hit with a `RunAgentInput`.
- **`/agui/agents.json`** (mast-specific, optional) — lists the endpoints + input schemas of all exposed workloads. Not required by the AG-UI protocol but useful for CopilotKit consumers who want to discover-then-connect. Compatible with anything that expects a directory-style endpoint listing.

The AG-UI standard doesn't prescribe an aggregation endpoint, so we ship both patterns: per-workload endpoints (mandatory) + optional aggregation endpoint (nice-to-have for CopilotKit dashboards).

### Session mapping

AG-UI has `threadID` (conversation) + `runID` (turn). Mast has session ID (durable, per-workload-invocation) + turn / step. The mapping:

| AG-UI concept | Mast concept |
|---|---|
| `threadID` | mast conversation ID — parent-of-many-sessions (one session per `runID`) OR one long-lived session with many turns, depending on `agui.session_model` bundle config (`per_thread` or `per_run`) |
| `runID` | mast session ID (with `per_run`) or mast turn correlator (with `per_thread`) |
| `parentRunID` | previous session's ID (for lineage; used for snapshot+replay parenting) |
| `state` | forwarded to bundle context as a state-bound input (per [`./memory-design.md`](./memory-design.md) state-bound-node pattern) |
| `messages` | fed as conversation history to the agent |
| `tools` | client-declared tools; intersected with bundle `tool_catalog` (skill-shaped policy layering per [`./skills-design.md`](./skills-design.md)) — client can't grant tools the bundle doesn't allow |
| `context` | forwarded to bundle context as environment info |
| `forwardedProps` | preserved; available to workload but not interpreted by mast |
| `resume` | maps to `mast.Resume(sessionID, resumeToken, payload)` per [`./durable-execution-design.md`](./durable-execution-design.md) |

The two `session_model` options let operators pick per workload:

- **`per_thread`** — one mast session per AG-UI thread; each `runID` is a turn within that session. Better for long-lived conversations with continuous state; matches CopilotKit chat UX.
- **`per_run`** — one mast session per AG-UI run; `threadID` is metadata for correlation across sessions. Better for stateless task-runner workloads (invoke, get result, done); matches classifier / one-shot patterns.

Default: `per_thread` for workloads with `Chat` task class; `per_run` for `orchestrate` / `debug` / etc.

### Event emission mapping

Mast's internal event stream emits AG-UI events uniformly:

| Mast internal event | AG-UI event(s) |
|---|---|
| session start | `RunStarted{ThreadID, RunID}` |
| turn begin (LlmAgent turn) | `StepStarted{stepName: "turn-N"}` (if `activity_events` enabled) |
| assistant text token | `TextMessageStart/Content(delta)/End` |
| tool call begin | `ToolCallStart{toolCallId, toolCallName}` |
| tool arg streaming | `ToolCallArgs{delta}` |
| tool call end | `ToolCallEnd{toolCallId}` + `ToolCallResult{content}` |
| specialist / sub-workflow invocation | `ToolCallStart` (nested) + child span visibility via `parentMessageId` |
| planner step | `ActivitySnapshot{activityType: "PLAN"}` |
| state write (bundle-configured state key) | `StateDelta{delta}` (RFC 6902 patches) |
| `RequestInputEvent` (HITL pause) | `RunFinished{outcome: {type: "interrupt", interrupts: [{id, message, responseSchema, expiresAt}]}}` |
| session finish (`finish_task`) | `RunFinished{outcome: {type: "success"}, result}` |
| session error | `RunError{message, code}` |
| session abort | `RunError{message: "cancelled", code: "aborted"}` |
| reasoning stream (frontier-model reasoning tokens) | `ReasoningStart/MessageContent/End` (opt-in per workload — reasoning is sensitive) |

### Auth

Pluggable `TokenValidator` interface, shared with the A2A implementation (single validator can authorize both surfaces). See [`./a2a-design.md`](./a2a-design.md) for the built-in validators (JWT, Google IAM Workload Identity, static bearer, OAuth 2.0 introspection). AG-UI-specific:

- Bearer token in `Authorization` header validated against the same set of validators.
- Scopes checked per workload: token must carry the `agui.auth.scopes` from the bundle.
- Rate limiting per authenticated principal (per-caller QPS + concurrent-run caps); shared implementation with A2A rate limiting.

### Long-running runs

AG-UI supports long-running runs natively via the streamed-event model — the SSE connection stays open until `RunFinished`. But mast workloads often exceed practical SSE-connection lifetimes (planner running for 20 minutes with an intervening HITL pause). Mast handles this via:

- **Native pause/resume**: HITL interrupts (per protocol) close the stream cleanly via `RunFinished{interrupt}`; client resumes with a new run.
- **Client disconnect resilience** *(mast extension — AG-UI defines no run-reattach semantics)*: if the SSE stream disconnects mid-run, mast's session persists (per [`./durable-execution-design.md`](./durable-execution-design.md)); mast allows reconnect via the same `threadID` + `runID` and resumes streaming from the last durable event. Standard AG-UI clients won't know to do this without mast-specific client code; document it as such.
- **Webhook event push** (v0.2+) *(mast extension — corrected 2026-07-25: there is no "AG-UI push-notification pattern" in the spec; that concept is A2A's)*: for clients that can't hold a persistent SSE connection, mast can POST events to a client-provided webhook URL as a mast-defined extension, clearly flagged as non-portable.

### Concurrent runs on the same thread

A `threadID` may have multiple in-flight `runID`s (branched exploration, retries, HITL-abandoned + new-attempt). Mast serializes execution per-thread by default (one active run per thread; queued if a new run arrives while another is active) — configurable per bundle if the workload supports genuine concurrency.

## Mast as AG-UI client

Mast agents (specialists, planner, workflow nodes) can call *other* AG-UI servers when it makes sense. Rare compared to server-side; the primary AG-UI use case is being called, not calling. But the client side matters for a few scenarios:

- **CopilotKit-hosted agents** that mast wants to invoke as sub-tasks.
- **Framework peers** (e.g. a LangGraph agent hosted by CopilotKit Runtime) that don't yet speak A2A but do speak AG-UI.
- **User-driven sub-agent chains** where an AG-UI-user-shaped remote agent is the right composition.

Under [`./federation-design.md`](./federation-design.md), this is another protocol adapter alongside A2A / mast-native / HTTP/RPC. Reference format: `agui://<name>` or `agui://<endpoint-url>[?thread=<threadID>]`.

The federation `invoke_remote_agent("agui://external-triage", inputs)` tool wraps the AG-UI call — mast constructs a `RunAgentInput`, streams the response, and returns the aggregated result (or propagates HITL interrupts back to mast's own operator via [`./durable-execution-design.md`](./durable-execution-design.md)).

Community Go SDK (`github.com/ag-ui-protocol/ag-ui/sdks/community/go`) is the AG-UI client implementation mast uses; SDK is pre-1.0 but functional (types, events, SSE client). Mast wraps it in a thin `pkg/agui/client.go` for consistent context propagation, observability spans, and cancellation semantics.

## CopilotKit as reference consumer

CopilotKit (`github.com/CopilotKit/CopilotKit`) is the largest AG-UI ecosystem consumer and the one that unlocks the highest-value integrations for mast's audience. Overview:

- **React frontend stack** — `@copilotkit/react-core`, `@copilotkit/react-ui`; prebuilt chat surfaces (`CopilotChat`, `CopilotSidebar`, `CopilotPopup`); headless UI for custom rendering.
- **Runtime server** — the AG-UI server-side implementation for the CopilotKit hosted agent path. Mast doesn't use CopilotKit Runtime (we're the runtime); but we speak the same protocol so CopilotKit clients don't care.
- **Chat-platform channels SDK** — published as `@copilotkit/channels` + platform adapters (`@copilotkit/channels-slack` shipped; further platforms in flight) *(corrected 2026-07-25: the `@copilotkit/bot-*` names from earlier drafts never shipped to npm; naming is still in flux — treat the whole package line as pre-stable and re-verify names before any deployment starter is written)*. Bot connects to any AG-UI backend; the backend just needs to speak AG-UI.
- **Cross-platform JSX** — `@copilotkit/channels-ui` renders once, adapts to Slack Block Kit / Discord Components V2 / Telegram HTML per platform.

For mast, the composition is:

```
[Slack workspace]
     ↓
[@copilotkit/channels-slack] (CopilotKit Slack adapter — Bolt SDK, Socket Mode or HTTP)
     ↓
[@copilotkit/channels] (platform-agnostic bot engine — threads, tool calls, HITL gate)
     ↓ (AG-UI over HTTP+SSE)
[mast --workload=incident-triage] (AG-UI server; exposes workload as an AG-UI agent)
     ↓
[mast planner + specialists + MCP servers + reference graphs + durable execution]
```

The bot process (Node.js, running CopilotKit `@copilotkit/channels-*`) is a *sidecar* to mast — could deploy in the same pod, same cluster, or as an external service depending on operator preference. Communication is via AG-UI over standard HTTP. Auth is bearer token; mast's `TokenValidator` handles the check.

**OpenTag** (`github.com/CopilotKit/OpenTag`) — the reference Slack bot from the CopilotKit team ("open-source alternative to Claude in Slack"). Shows the wiring pattern end-to-end. Mast operators wanting Slack-as-mast-UX can start from OpenTag's setup and repoint the agent backend at mast's AG-UI endpoint. Two-process deployment: agent (mast in this case) + bot (CopilotKit's `@copilotkit/channels` + Slack adapter).

## Slack (and Teams / Discord / Telegram / WhatsApp) via CopilotKit

The chat-platform bot SDK is the highest-value AG-UI-derived capability for mast's audience. Concrete integrations:

### Slack (primary; via `@copilotkit/channels-slack`)

- **Socket Mode** (default) — outbound WebSocket only; no public URL needed. Fits GKE deployments behind private ingress.
- **HTTP mode** — for operators who prefer Slack webhook-based ingress; needs `signingSecret` + public path.
- Auth via `SLACK_BOT_TOKEN` + `SLACK_APP_TOKEN`; mast operators wire these as Kubernetes Secrets.
- Response routing: DMs conversational; app mentions in-thread; plain replies require another mention. All configurable.
- Rich rendering via Block Kit (from JSX authored once in `@copilotkit/channels-ui`).
- HITL: user's Slack response resumes the mast session's paused interrupt. Approval buttons render as Block Kit interactive components.
- Ships a `mast-slack-bot` deployment starter (v0.2+): `examples/deploy/slack-via-copilotkit/` with Terraform / Kustomize configs + Slack app manifest.

### Teams (via CopilotKit channels adapter, when published)

Same shape; Teams-native rendering (Adaptive Cards from JSX). Auth via Microsoft Bot Framework tokens. Deployment starter `examples/deploy/teams-via-copilotkit/`.

### Discord / Telegram / WhatsApp

Same shape; platform-native rendering. Included as deployment options; documentation notes when each fits mast's audience (Discord for community platform teams; Telegram/WhatsApp for regional operator communities).

### Cost model

CopilotKit's chat-platform bot SDK is open-source (no per-seat cost). Operators deploy the bot themselves alongside mast. CopilotKit sells a managed Enterprise Intelligence Platform on top for those who want hosted mast-web-alternative + hosted chat integrations, but that's optional.

## Composition with other subsystems

| Subsystem | AG-UI interaction |
|---|---|
| **Attach mode + `mast-web`** | Distinct transport; mast-web stays as mast-native operator UI (richer features). AG-UI is the ecosystem-facing UI protocol. Both coexist; different consumer. Operators pick per need. |
| **A2A** ([`./a2a-design.md`](./a2a-design.md)) | Sibling protocol (different direction: A2A = agent↔agent). Shared `TokenValidator` interface; shared auth path (Google IAM Workload Identity, JWT, etc.). Mast can expose the same workload via both A2A + AG-UI simultaneously — A2A skill for cross-framework agent calls; AG-UI endpoint for user-facing clients. |
| **Federation** ([`./federation-design.md`](./federation-design.md)) | AG-UI adapter is one of the federation protocols (`agui://` reference). Planner `invoke_remote_agent` treats AG-UI remote agents uniformly with A2A / mast-native / HTTP-RPC. |
| **Orchestration (workloads)** ([`./orchestration-design.md`](./orchestration-design.md)) | Bundle `agui.expose` field opts a workload into AG-UI exposure. `session_model` controls per-thread vs. per-run mapping. Planner's `invoke_remote_agent` can dispatch to AG-UI remotes. |
| **Specialists** ([`./specialists-design.md`](./specialists-design.md)) | Specialists execute normally; their invocations emit as nested `ToolCallStart/Args/End/Result` events in the AG-UI stream. Users see specialists as tool calls in the UI. |
| **Skills** ([`./skills-design.md`](./skills-design.md)) | Skill `allowed_tools` intersected with AG-UI client's `RunAgentInput.tools` — client-declared tools further narrow the allowlist. Skill invocations surface as tool calls in AG-UI. |
| **Workflow scaffolding** ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)) | Reference-graph node executions emit as `ActivitySnapshot` / `ActivityDelta` in AG-UI (e.g., fan-out shape reports each worker's progress). Users see the workflow shape in the UI. |
| **Durable execution** ([`./durable-execution-design.md`](./durable-execution-design.md)) | AG-UI interrupt lifecycle *is* mast's durable pause/resume. `RunFinished{interrupt}` = pause; `RunAgentInput.resume` = resume. Cross-boundary state persistence works out of the box. Client disconnect + reconnect resumes streaming from last durable event. |
| **Multi-tenant** ([`./deployment-design.md`](./deployment-design.md)) | AG-UI auth token can carry tenant claim; maps to `WithIsolationScope`. Per-tenant `agui.expose` policies possible via bundle isolation scope. |
| **Observability** ([`./observability-design.md`](./observability-design.md)) | AG-UI-specific span types (`agui.server.run`, `agui.client.call`); metrics (`mast_agui_runs_total{workload, outcome}`, `mast_agui_run_duration_seconds{workload}`, `mast_agui_active_threads{workload}`, `mast_agui_interrupts_total{workload}`). Distributed tracing across `traceparent` propagation to AG-UI client. |
| **Memory** ([`./memory-design.md`](./memory-design.md)) | AG-UI `state` field maps to bundle-scoped state-bound reads. AG-UI's `StateDelta` events emit when bundle state changes visible to the workload. |
| **Library API** ([`./library-api-design.md`](./library-api-design.md)) | `github.com/go-steer/mast/agui` package: `agui.Server` + `agui.Client` + `agui.TokenValidator` (shares interface with A2A). Programmatic exposure via `ServerConfig.AGUI` (analog to `ServerConfig.Attach`). |
| **Deployment** ([`./deployment-design.md`](./deployment-design.md)) | AG-UI endpoint typically fronted by Ingress + TLS. Chat-platform bots (CopilotKit-based) deploy as sidecars — same pod, same cluster, or external. Deployment starters ship for each. |
| **MCP catalog** ([`./mcp-catalog-design.md`](./mcp-catalog-design.md)) | Sibling to the surface comparison — the fourth interop surface (agent↔user protocol); mcp-catalog-design enumerates all four and owns the framing. |
| **Config layout** ([`./config-layout-design.md`](./config-layout-design.md)) | No new `.agents/agui/` directory required — AG-UI exposure is bundle-level config (`agui.*` fields on workload bundles). |

## Phasing

| Version | Scope |
|---|---|
| **v0.1** | **Nothing ships** (re-cut 2026-07-25 per [`./fork-design.md`](./fork-design.md) — AG-UI server + client both move to v0.2; this also resolves the earlier contradiction where this doc put an `agui://` federation adapter in v0.1 while [`./federation-design.md`](./federation-design.md) said A2A-only). Design-time obligation only: keep the attach protocol + durable pause/resume shaped so the v0.2 AG-UI mapping stays a projection, not a rework. |
| **v0.2** | AG-UI server: per-workload endpoints (`agui.expose: true`); RunAgentInput acceptance; event streaming for lifecycle + text messages + tool calls + state; HITL via the draft interrupt extension (`RunFinished{outcome: interrupt}` + resume) — SDK version-pinned, encoding isolated in `pkg/agui`. Auth via shared `TokenValidator`. Basic client (`invoke_remote_agent("agui://...")` federation adapter). SSE-only. Then: activity events (workflow-shape visibility); reasoning events (opt-in); mast-extension webhook push; `/agui/agents.json`; `examples/deploy/slack-via-copilotkit/` starter **once the channels-* packages stabilize on npm**; client-disconnect + reconnect resumption (mast extension). |
| **v0.3** | Multi-thread concurrency support (opt-in per bundle); per-tenant AG-UI policy; observability + bundle-learning integration (AG-UI patterns feed learning). `examples/deploy/{teams,discord,telegram,whatsapp}-via-copilotkit/` starters. CopilotKit React reference example (`examples/copilotkit-react/`) showing a full stack. |
| **v0.4+** | AG-UI protocol version negotiation once the spec matures (interrupts/activity/reasoning finalized). Cross-runtime AG-UI federation (mast AG-UI server called by Python-ADK AG-UI client). |

## Open questions

1. **AG-UI protocol version pinning.** SDK is pre-1.0; specs evolve. Bias: test against pinned SDK version per mast release; document tested-against version in `pkg/agui/VERSION.md`; upgrade cadence separate from mast release cadence.
2. **`/.well-known/` metadata endpoint for AG-UI.** Not in the standard but potentially useful for CopilotKit discovery. Bias: ship an optional aggregation endpoint (`/agui/agents.json`) but don't require clients to use it — many will be configured with direct endpoint URLs.
3. **Thread-to-session mapping bundle default.** `per_thread` (long-lived session) or `per_run` (one session per run)? Bias: `per_thread` for `Chat`-mode workloads (matches chat UX); `per_run` for `orchestrate`/`debug`/`research`/`review` (matches task-runner UX). Explicit override always available.
4. **Concurrent-run policy.** Default: one active run per thread; queue if another arrives. Configurable per bundle. What's the queue depth? Bias: 3; reject with `RunError` if exceeded; observable via metric.
5. **Reasoning event exposure.** Some model reasoning tokens are sensitive (chain-of-thought reveals prompt-injection surface). Bias: default off; opt-in per bundle (`agui.emit_reasoning: true`); document the tradeoff.
6. **Tool declarations from AG-UI clients.** `RunAgentInput.tools` lets clients declare tools they expose *to* the agent (frontend tool calls). Mast can support this (client-side tools count as another tool class the planner can invoke); need to reconcile with bundle `tool_catalog` allowlist. Bias: client-declared tools require `agui.accept_client_tools: true` opt-in per bundle; intersected with bundle allowlist same as skills.
7. **State delta authorship.** AG-UI `StateDelta` events publish state changes to the client. Which mast state keys emit? Bias: bundle declares `agui.state_projection: [key1, key2]` — explicit allowlist of state keys projected to the client; default empty (nothing projected without explicit config).
8. **Aggregation endpoint content model.** `/agui/agents.json` is mast-defined; format-shape TBD. Bias: JSON array of `{name, endpoint, description, input_schema, auth: {scopes}}` per exposed workload. Keep it simple; align with CopilotKit conventions once they publish one.
9. **CopilotKit-hosted vs. self-hosted-bot deployment guidance.** CopilotKit sells a managed platform; self-hosting the `@copilotkit/channels` bot process alongside mast is also viable. Bias: document both; recommend self-hosted for platform teams with existing GKE/Cloud Run infra; recommend managed for teams without.
10. **AG-UI-native workload authoring UX.** Some workloads are natively chat-shaped and want UI hints in their bundle (starter messages, quick-reply chips, avatar). AG-UI protocol supports these via `Custom` events. Bias: pass through as-is; provide helper functions in `pkg/agui/` for common patterns; don't add mast-specific extensions.

## Out of scope

- **Reimplementing the AG-UI Go SDK.** We use the community SDK (`github.com/ag-ui-protocol/ag-ui/sdks/community/go`) as-is; contribute upstream when we hit bugs; wrap in `pkg/agui/` for mast-specific integration.
- **Owning CopilotKit's frontend.** We don't ship a React library. CopilotKit's `@copilotkit/react-core` + `react-ui` are the ecosystem's React answer; mast is the backend.
- **Owning the chat-platform bots.** We don't ship a Slack bot. CopilotKit's `@copilotkit/channels-*` packages are the ecosystem's chat-platform answer; mast provides the deployment starters + auth wiring.
- **AG-UI protocol design contributions beyond feedback.** Spec evolution happens at ag-ui-protocol/ag-ui; mast follows.
- **A mast-branded AG-UI client.** We already have mast-web (mast-native); CopilotKit React apps are the AG-UI-native alternative. No third client.
- **Replacing attach mode with AG-UI.** Attach mode's richer feature set (workflow-node visualization, planner turn detail, federation cross-instance spans, snapshot/replay controls) doesn't fit AG-UI's user-facing scope. Both coexist; different consumer surfaces.
- **AG-UI-driven bundle authoring.** Bundle files are operator-authored; no in-band AG-UI mechanism to author new workloads.

## Related

- [AG-UI protocol](https://docs.ag-ui.com/introduction) — upstream spec
- [AG-UI Go SDK (community)](https://github.com/ag-ui-protocol/ag-ui/tree/main/sdks/community/go) — the client + types library mast wraps
- [CopilotKit](https://github.com/CopilotKit/CopilotKit) — React frontend + Runtime + chat-platform bot SDK
- [OpenTag](https://github.com/CopilotKit/OpenTag) — reference open-source Slack bot ("open-source alternative to Claude in Slack") built on CopilotKit
- [`./a2a-design.md`](./a2a-design.md) — sibling protocol (agent↔agent); shared `TokenValidator`
- [`./federation-design.md`](./federation-design.md) — AG-UI as one federation protocol adapter
- [`./durable-execution-design.md`](./durable-execution-design.md) — AG-UI interrupts = mast pause/resume
- [`./orchestration-design.md`](./orchestration-design.md) — bundle `agui.expose` field; planner integration
- [`./observability-design.md`](./observability-design.md) — AG-UI span types + metric families
- [`./deployment-design.md`](./deployment-design.md) — deployment starters incl. Slack-via-CopilotKit
- [`./library-api-design.md`](./library-api-design.md) — `github.com/go-steer/mast/agui` package
- [`./mcp-catalog-design.md`](./mcp-catalog-design.md) — four-corner interop-surface framing
- [`./positioning.md`](./positioning.md) — attach + AG-UI both keep-list; different consumer surfaces
