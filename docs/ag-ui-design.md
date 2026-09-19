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
- **Reasoning** — `ReasoningStart`, `ReasoningMessageStart/Content/End`, `ReasoningEncryptedValue`. *mast emits the first five behind `agui.emit_reasoning` (Stage 4) and never the sixth: `ReasoningEncryptedValue` carries a provider replay credential, not a thought.*
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

Mast exposes its workloads to AG-UI clients via HTTP+SSE endpoints. Multiple workloads can be exposed; each is one AG-UI-callable agent.

#### Implementation status (v0.2, Stages 1–2)

AG-UI ships in stages under umbrella #84, mirroring the A2A cadence.

- **Stage 1 (shipped).** Server core. IN: the bundle `agui:` section (`expose`, `endpoint_path`, `description`, `input_schema`, `session_model`, `auth.scopes`); a per-workload HTTP+SSE run endpoint plus the `/agui/agents.json` discovery descriptor; `RunAgentInput` acceptance; shared auth + rate limiting (see [Auth](#auth-1)); the happy-path event stream (`RunStarted` → a `StateSnapshot` echoing the client's input state → the model's answer as a `TextMessage` triad, with `ToolCallStart/Args/End` + `ToolCallResult` for tool activity → a terminal `RunFinished` / `RunError`); and the `mast_agui_runs_total{workload,outcome}` + `mast_agui_run_duration_seconds{workload}` metrics. Every run drives the same `runTurnPre` chokepoint every other turn kind funnels through (turn-lock, abort / gate-pause refusal, budget meter, watchdog, effects outbox by construction), and the session id is always daemon-derived and namespaced under `agui-` with an ownership fence against the reserved ops-row namespace — a client never supplies a raw session id.
- **Stage 2 (shipped).** The HITL interrupt/resume lifecycle. A turn that parks on a HITL primitive (a `request_operator_input`-class long-running tool, or a programmatic/external-signal pause) now closes the stream with a terminal `RunFinished{outcome: {type: "interrupt", interrupts: [{id, message, responseSchema?, expiresAt?}]}}` — projected from the durable session's pending-interrupt state, not a fabricated success and no longer the honest-placeholder `RunError{interrupt}` Stage 1 emitted. The client resumes by starting a **new run** whose `RunAgentInput.Resume` carries one `ResumeEntry` per interrupt (`Status: "resolved"` or `"cancelled"`, optional payload); the daemon reconciles each entry against the session's open interrupt ids, builds the resume function-response, and drives the resume turn through the same `runTurnPre` chokepoint — a resume against no open interrupt (or an unknown id) is refused with `ErrNotResumable` (HTTP 409) rather than silently forking a fresh turn. Because the resume is a *new* run (new `runId`), reaching the parked session under `session_model: per_run` (which keys the session on `runId`) requires the resume to carry `RunAgentInput.parentRunId` naming the run that parked; under the default `per_thread` the shared `threadId` reaches it with no extra field. The terminal interrupt frame records the `interrupted` outcome on `mast_agui_runs_total`.
- **Stage 3 (shipped, 2026-09-14, #98).** Per-key `StateDelta` projection, resolving [OQ #7](#open-questions). A workload's bundle declares `agui.state_projection: [key, …]`; a runtime state write (ADK's `session.EventActions.StateDelta`, which is where `pkg/graph` and `pkg/approval` put theirs) whose key the list names is published as an RFC 6902 patch, keys sorted, one `add` op each. Everything else about it follows from the default being empty:
  - **Allowlist, and a filter rather than a redaction.** An unlisted key produces no op at all, so a client cannot learn that it changed. Session state is whatever the runtime put there, including grants and change sets, and the AG-UI client is a browser — a denylist would make every future state key an exfiltration decision taken by whoever added it.
  - **`add` is the only op mast emits**, and that is a reading of RFC 6902 rather than a shortcut: for an object member `add` replaces an existing value and creates a missing one, while `replace` fails against a target that lacks the member. The opening `StateSnapshot` echoes the *client's* state document, so the daemon does not know which keys that document already carries; `add` is correct either way and makes the stream idempotent under a client that reconnects and replays.
  - **The emission sits above the content check** in the event handler, because state rides on `Actions` and not on `Content`: `pkg/graph` stashes a node result on an event carrying no content at all.
  - **Refused at startup, not at the first write:** an empty entry (it matches no key, forever) or a duplicate. A key naming state the workload never writes is deliberately accepted — which keys a roster produces depends on the dispatch shape and on runtime-resolved tools, so refusing one would be a guess. The enabled allowlist is logged at startup for the same reason the builtin-tools summary is: an operator should be able to read what the daemon publishes off the log rather than off the bundle they believe is mounted.
- **Stage 4 (shipped, 2026-09-16, #98).** Reasoning events, resolving [OQ #5](#open-questions). A workload's bundle sets `agui.emit_reasoning: true` and the model's thinking streams to its AG-UI clients as a five-frame phase — `REASONING_START`, a `REASONING_MESSAGE_START/CONTENT/END` triad, `REASONING_END` — emitted **ahead of** the answer's own `TextMessage` triad, because that is the order the model produced them in and a client opens a collapsed "thinking" region on the phase bracket. It is the second per-bundle publication surface and it copies Stage 3's shape on purpose; what is specific to reasoning is the rest:
  - **Off by default, and off means silent.** A workload that does not set the key emits no reasoning frame of any kind — not an empty bracket, not a redaction marker. The existence of a thought can itself be the disclosure, so this is the same filter-not-redaction rule the state projection follows.
  - **The opt-in is a deliberate read, not a removed filter.** [#370](https://github.com/go-steer/mast/issues/370) built `internal/modeltext.Text`, the predicate every caller-facing surface reads model text through; publishing reasoning calls its named counterpart `modeltext.Thought`. The two are disjoint and the disjointness is pinned by a test, so a thought never becomes answer text or `RunFinished.result` under either setting, and "can reasoning reach a browser?" is a grep for one symbol rather than an audit of every `part.Text`.
  - **A thinking block's signature has no frame at all.** The spec's `ReasoningEncryptedValue` is deliberately not modeled. That value is a provider replay credential — Anthropic requires it back verbatim on the assistant turn preceding a `tool_result` — and a client holding it can reconstruct an assistant turn the model accepts as its own. Publishing reasoning prose is a decision an operator can take; handing out the signature is a different decision, and mast does not offer it.
  - **Which means an opted-in workload usually still publishes nothing, correctly.** Under the request mast sends today `claude-opus-5` returns a *signed block with an empty body*, and a block with no prose is not a phase. That is the same vendor default #370 found, seen from the other side: it was the reason nothing had leaked, and it is now the reason the flag looks inert until a workload asks for reasoning text.
  - **Said out loud at startup, at `WARN` where the projection's line is `INFO`.** A projection publishes keys the operator chose one at a time; this publishes whatever the model happened to think, which on an injected turn includes the injection's own working. It is a legitimate setting, not a misconfiguration — but an operator scanning a running daemon should not have to already suspect it.
- **Discovery capabilities (shipped, 2026-09-16, [#377](https://github.com/go-steer/mast/issues/377)).** Not a #98 stage — a gap the last two of them opened. Stages 3 and 4 each added a per-bundle publication surface, and `/agui/agents.json` described neither, so a client had to start a run and infer from what did not arrive: a stream with no reasoning in it is identical whether the workload publishes none or the model did not think. Each descriptor now carries `capabilities: {state_delta, state_keys?, reasoning}`, and the bits come from an optional `agui.CapabilityReporter` on the **Backend** — the object that builds the emitter — rather than a field on `ExposedWorkload` that whoever assembles the config would fill in a second time. `cmd/mast` reads both switches exactly once, into an `aguiPublication` the emitter and the reporter share; a test drives a real run per bundle shape and requires the advertised bits to match the frames that actually appeared. The booleans are never elided when false (an absent key means an older server; an explicit `false` is a promise), `state_keys` is cleared by the handler whenever `state_delta` is false so the document cannot contradict itself, and the endpoint stays public under the rule recorded at [OQ #8](#open-questions).
- **Step events (shipped, 2026-09-16, [#98](https://github.com/go-steer/mast/issues/98)).** The `STEP_STARTED` / `STEP_FINISHED` half of the activity row. **A step is the stretch of a run authored by one agent, and the step name is the agent's name** — the bracket opens when `session.Event.Author` changes on a model event, closes the previous one first, and the last open bracket is closed when the turn returns, under every disposition including an abort and a HITL pause. Four things are worth recording:
  - **`turn-N` was the wrong unit, and this doc proposed it.** An AG-UI run drives exactly one turn through `runTurnPre`, so a turn-numbered step would have said `turn-1` on every run of every workload. Authorship is the thing that actually varies inside a run, and it is where the interesting boundary is: a coordinator handing off to a specialist, or a graph node's agent taking over.
  - **`Author` and deliberately not `Branch`.** `Author` is the attribution seam the rest of mast already trusts — `pkg/budget` buckets spend by it, `pkg/effects` classifies dangling calls by it, and [`pkg/specialists`](../pkg/specialists/spec.go) records that it carries the agent's name on every dispatch shape mast builds while `Branch` is empty in the coordinator/sub-agent-tool shape. A mutation swapping the read to `Branch` emits no step at all, and there is a test that says so.
  - **No bundle key, unlike Stages 3 and 4.** The two publication surfaces before this one are opt-in because they disclose something a permitted client could not otherwise see. This discloses nothing new: a handoff already reaches the same stream as a `transfer_to_agent` / `invoke_specialist` `ToolCallStart` naming the very same agent. That is [OQ #8](#open-questions)'s rule for the public descriptor, applied to a frame family — and with nothing to switch off, a key would only add a bundle field that can be set wrong and, since [#302](https://github.com/go-steer/mast/issues/302), a spelling that is fatal at load. It is for the same reason **not** a `capabilities` bit: #377 added that object because both keys were *per workload*; a frame family every workload emits has nothing per-workload to report, and the honest home for a server-wide claim is `protocol_version`, which does not move because `STEP_*` is core AG-UI vocabulary a `0.1` client already tolerates.
  - **Flat, and one name can bracket twice.** Events arrive serialized on one stream, so the sequence reports arrival order rather than a nesting mast cannot observe; under a parallel fan-out two workers' events interleave and their brackets alternate. Reporting that is honest; collapsing it would invent a structure the runtime did not produce.
- **Deferred (documented, follow-on stages).** Sequenced 2026-09-17 (#98) against the [v1.0 API freeze](../DESIGN.md#the-v10-stability-promise), because the deciding question for each is not "how big is it" but **"does it need a `workload.Bundle` key, and does it hold a defect today"**. A key is freeze-exposed; a defect is not a feature and does not wait for one.

  | Slice | Where it sits | Why |
  |---|---|---|
  | **Client-disconnect cancellation** ([#383](https://github.com/go-steer/mast/issues/383)) | **SHIPPED 2026-09-17** (v0.9) | Was never a feature. A transport blip destroyed the run, and a retry mid-tool re-fired a mutating call that already happened — measured. Was mostly the removal of a coupling; see [Long-running runs](#long-running-runs). |
  | **Concurrent-run policy** ([#384](https://github.com/go-steer/mast/issues/384), [OQ #4](#open-questions)) | **SHIPPED 2026-09-17** (v1.0) | Landed whole rather than key-first: the key is additive after the freeze, but *bounding a queue that is unbounded today* changes what an existing bundle does, which is the breaking category [`./compatibility-policy.md`](./compatibility-policy.md) calls invisible to a signature. That half could not slip. |
  | **Client-declared tools** ([OQ #6](#open-questions)) | **Gate + key shape pre-v1.0**, implementation **v1.1** | The recorded intersection cannot be built (see OQ #6); the gate has to be designed, and its opt-in key is freeze-exposed. Parsed and dropped today, which is the safe behaviour. |
  | **Reconnect proper** (replay cursor + re-subscription) | **v1.1** | A genuine mast extension, and deferrable *because* #383 lands first: after it a blip costs the tail of the stream, not the run. |
  | **`ACTIVITY_*` (planner half)** | **Blocked, not scheduled** | Needs a publication decision about a specialist's interior reaching a browser — see below. The graph half is already served by step events. |
  | **Webhook push** | **Unscheduled** | No consumer has asked. Every mast deployment that has wanted out-of-band delivery has had somewhere to hold an SSE connection. |
  | **`agui://` federation client** | **Past v1.0** | Unbuilt for want of a consumer, not a blocker; outbound is governed already. See [Mast as AG-UI client](#mast-as-ag-ui-client). |

  Only the `ACTIVITY_*` row is blocked on something mast does not control. Everything above it is sequenced by the freeze; everything below it by demand.

  On the activity family specifically, now that the step half has shipped and the seam has been read rather than guessed at: **the graph half of the promise is already served by steps** (a reference-graph node execution is a stretch authored by that node's agent, so it brackets like any other handoff, and inventing a second vocabulary for it would have published the same fact twice). What is left is the **planner half**, and it is blocked on a real boundary, not on scheduling. `pkg/planner/dispatch.go` runs each `invoke_specialist` under a **private runner**, and its own package doc states the consequence: "a private runner is a private event stream: nothing the sub-run emits reaches the OUTER runner's consumer" — which is the consumer the AG-UI emitter rides. Coordinator and graph dispatch never had that gap; planner dispatch does, and the only door through it is `planner.Config.SubRunObserver`, whose daemon implementation (`cmd/mast/subrun.go`) is keyed per **session** and feeds the accounting consumers (meter, watchdog, intent recorder), not a per-run SSE stream. So shipping `ActivitySnapshot{activityType: "PLAN"}` needs two decisions, and both are larger than a wiring change: (1) whether a run-scoped observer may be attached to a session-scoped sink, and (2) whether a planner-dispatched specialist's interior may reach a browser at all — which is a publication question of exactly the class Stages 3 and 4 answered with an opt-in, and would need its own. Until then, a client is not blind to the dispatch: the coordinator's `invoke_specialist` call is an ordinary `ToolCallStart` on the outer stream. It cannot see inside it.

##### Build-vs-buy: hand-rolled, zero-dependency (overrides the July SDK-wrap guidance)

The wire vocabulary, HTTP+SSE surface, auth, and rate limiter live in a hand-rolled `pkg/agui` (+ the shared `pkg/serverauth`) with **zero new external dependencies** — *not* the community AG-UI Go SDK. This **supersedes** this doc's earlier guidance ([Mast as AG-UI client](#mast-as-ag-ui-client), the "Reimplementing the AG-UI Go SDK" [out-of-scope](#out-of-scope) line, and OQ #1's version-pin bias, all of which assumed wrapping `github.com/ag-ui-protocol/ag-ui/sdks/community/go`). Same call, and for the same reasons, as the A2A server ([`./a2a-design.md`](./a2a-design.md) "Implementation status"): (1) it keeps every AG-UI turn on the `runTurnPre` chokepoint rather than letting an SDK executor drive the runner directly and bypass the budget/pause/abort/outbox seams; and (2) the deployment slim-graph gate (`dev/ci/presubmits/slim-deps.sh`) denylists heavy transitive deps, and pulling the SDK would trip it. The wire subset mast emits is small and stable; when the client direction lands it will hand-roll the SSE client the same way. `pkg/agui` remains the isolation boundary the doc always wanted — the encoding just isn't the community SDK's.

##### Deviations from the design text

- **Dedicated `--agui-listen` listener** (not "on the mast HTTP listener", as the section intro originally read). mast has no single shared HTTP root — the inject, attach, and A2A surfaces each own a listener — so a dedicated `--agui-listen` bind address (empty disables) is the consistent choice, matching `--a2a-listen`.
- **Fixed `session_model` default** (`per_thread`, with an explicit `per_run` override), rather than OQ #3's "default keyed on the workload's task class". `workload.Bundle` carries no task-class field today, so a fixed default + per-workload override is what ships; the task-class-aware default can arrive with a task-class field.

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
  state_projection: [plan, phase]                 # allowlist: which state keys
                                                  # reach the client as StateDelta
                                                  # patches. Empty = none.
  emit_reasoning: false                           # publish the model's thinking
                                                  # as REASONING_* frames. Default
                                                  # (and shown here) is off.
  run_queue:
    depth: 3                                      # runs that may WAIT on one
                                                  # thread. 3 (the default, shown
                                                  # here) admits four at once: one
                                                  # executing plus three queued.
                                                  # The fifth gets 409 + Retry-After.
```

> **Correction, 2026-09-14 (#98).** This example previously ended with two more
> keys, `streaming: true` and `activity_events: true`. Neither field exists on
> `workload.AGUI`, and since [#302](https://github.com/go-steer/mast/issues/302)
> made an unrecognised bundle key a load error rather than a warning, a bundle
> copied from this example would not have started the daemon at all. They are
> removed rather than implemented: incremental streaming is the whole-message
> emission Stage 1 chose deliberately, and activity events were deferred at the
> time. **Follow-up, 2026-09-16 (#98):** the step half of that family has since
> shipped, and it took no key — `STEP_STARTED` / `STEP_FINISHED` are emitted
> unconditionally, because unlike the state projection and reasoning they
> disclose nothing a permitted client could not already read off the stream
> (see [Implementation status](#implementation-status)). So `activity_events:`
> was not a key waiting to be implemented; it was a key that should never have
> existed. The `ACTIVITY_*` half that remains is blocked on a seam, not on a
> switch. Every key shown above now exists.

Workloads without an `agui` section are not exposed via AG-UI — same conservative default as A2A. Deliberate: AG-UI exposure has real ops implications (auth setup, external client contract stability, user-facing UX considerations).

### Endpoint layout

- **`<endpoint_path>`** per workload — the POST endpoint clients hit with a `RunAgentInput`.
- **`/agui/agents.json`** (mast-specific, optional) — lists the endpoints, input schemas, required scopes, and per-workload `capabilities` of all exposed workloads. Not required by the AG-UI protocol but useful for CopilotKit consumers who want to discover-then-connect. Compatible with anything that expects a directory-style endpoint listing. Unauthenticated, deliberately: see [open question 8](#open-questions) for the rule that keeps it so.

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
| `tools` | client-declared tools. **Parsed and dropped** — mast never offers them to the model. The intersection this row used to describe cannot be built out of `tool_catalog`, which is a policy-override table rather than an allowlist; see [OQ #6](#open-questions) for the re-bias and the gate it owes |
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
| a model event whose `Author` differs from the open step's | `StepFinished{stepName: <previous author>}` (if one was open) + `StepStarted{stepName: <author>}`. **Shipped** (#98) — unconditional, no bundle key. Supersedes this row's original `StepStarted{stepName: "turn-N"}` (if `activity_events` enabled), which was degenerate: an AG-UI run drives exactly one turn through `runTurnPre`, so every run would have reported `turn-1`. |
| turn end, any disposition | `StepFinished` for the open step, ahead of the terminal frame. **Shipped** (#98) |
| assistant text **message** | `TextMessageStart/Content(delta)/End` — one triad per message, the `delta` carrying the whole text. This row said "token"; see the correction below. |
| tool call begin | `ToolCallStart{toolCallId, toolCallName}` |
| tool **call arguments** | `ToolCallArgs{delta}` — one frame carrying the whole argument object. This row said "tool arg streaming"; see the correction below. |
| tool call end | `ToolCallEnd{toolCallId}` + `ToolCallResult{content}` |
| specialist / sub-workflow invocation | `ToolCallStart` (nested) + child span visibility via `parentMessageId`; a coordinator/graph handoff additionally brackets as a `StepStarted` named after the agent that takes over |
| planner step | `ActivitySnapshot{activityType: "PLAN"}` — **not shipped, and blocked rather than unscheduled**: a planner dispatch runs under a private runner, so none of its events reach the outer consumer the AG-UI emitter rides (`pkg/planner/dispatch.go`, "a private runner is a private event stream"). See the deferred bullet in [Implementation status](#implementation-status) for what deciding it needs. |
| state write to a key named in `agui.state_projection` | `StateDelta{delta}` — one `{"op": "add", "path": "/<key>", "value": …}` per allowlisted key that changed, keys sorted. **Shipped** (#98). |
| state write to any other key | *nothing* — a filter, not a redaction: an unlisted key produces no op, so a client cannot learn that it changed |
| `RequestInputEvent` (HITL pause) | `RunFinished{outcome: {type: "interrupt", interrupts: [{id, message, responseSchema, expiresAt}]}}` |
| session finish (`finish_task`) | `RunFinished{outcome: {type: "success"}, result}` |
| session error | `RunError{message, code}` |
| session abort | `RunError{message: "cancelled", code: "aborted"}` |
| model thinking parts, workload sets `agui.emit_reasoning` | `REASONING_START` → `REASONING_MESSAGE_START/CONTENT/END` → `REASONING_END`, ahead of the answer's own triad; one event's thinking parts concatenate into one reasoning message. **Shipped** (#98). |
| model thinking parts, workload does not set it (the default) | *nothing* — not an empty bracket and not a marker, so a client cannot learn the model reasoned |
| a thinking block whose payload is only its provider signature | *nothing*, under either setting. The signature is a replay credential, not a thought; `ReasoningEncryptedValue` is deliberately unmodeled |

> **Correction, 2026-09-19 ([#400](https://github.com/go-steer/mast/issues/400)).**
> The left column of this table named *tokens* — "assistant text token", "tool
> arg streaming" — and the emitter has never worked that way. It mints one
> frame per **runner event**, and every runner site in the module passes
> `StreamingModeNone`, so one runner event is one whole model response and the
> `delta` fields carry whole values. That is the same whole-message choice the
> 2026-09-14 (#98) correction above records for the removed `streaming:`
> bundle key, which was struck for naming a field that never existed and a
> behaviour never chosen; the rows simply kept the earlier vocabulary. They
> are reworded rather than marked unimplemented, because the frames *are*
> shipped — it is the granularity the table overstated.
>
> The correction is not cosmetic, because the wording described a shape the
> emitter would have handled wrongly. Under `StreamingModeSSE` ADK yields each
> provider chunk as its own partial event, and since every frame was minted
> per event, a streamed tool call would have produced a **complete**
> `ToolCallStart`/`Args`/`End` triple per chunk with empty arguments —
> dispatched as several distinct calls by any client acting on
> `ToolCallEnd` — plus the real one on the aggregate. `onEvent` now drops
> partials, the same guard `pkg/watchdog/bridge.go` took for the same reason
> in [#331](https://github.com/go-steer/mast/issues/331), and
> `cmd/mast/agui.go` moves into the safe set named by
> `TestEveryRunnerSiteIsNonStreaming`'s failure message.
>
> Filling the delta frames with actual deltas — the feature the old wording
> implied — is [#407](https://github.com/go-steer/mast/issues/407), and it is
> deferred on purpose: with nothing in the module streaming, the only evidence
> it worked would be a fixture written by the same change, so it waits for a
> runner site that can produce real chunks. Until then the contract a client
> may rely on is the one stated in the rows above: each frame arrives once and
> carries the whole value.

### Auth

Pluggable `TokenValidator` interface, shared with the A2A implementation (single validator can authorize both surfaces). See [`./a2a-design.md`](./a2a-design.md) for the built-in validators (JWT, Google IAM Workload Identity, static bearer, OAuth 2.0 introspection). AG-UI-specific:

- Bearer token in `Authorization` header validated against the same set of validators.
- Scopes checked per workload: token must carry the `agui.auth.scopes` from the bundle.
- **The scope check is real; the *discrimination* is not, under the validator the daemon builds** *(added 2026-09-17, [#389](https://github.com/go-steer/mast/issues/389))*. `MAST_AGUI_TOKEN` is one token, resolving to one principal (`Subject: "mast-agui-static"`) carrying the **union of every exposed workload's scopes** — necessarily, since one token has to drive all of them. So `authorize`'s loop runs and has no reachable `false` branch: an operator cannot express "this token runs the reporting workload and not the deploying one", because there is only one token. Two consequences to state rather than let a reader discover. **`auth.scopes` on a discovery descriptor is the workload's *requirement*, not a property of any token mast issues** — true as written, and read by most people as the other thing. And the caller-in-the-session-id below separates *deployments*, not users, because the subject it hashes is a constant here; that is this issue seen from the other side, not a defect in [#382](https://github.com/go-steer/mast/issues/382). What makes scopes discriminate is a host-supplied `pkg/serverauth.TokenValidator`, which is a real seam and needs no change to either server: supply one and the scopes, the per-principal rate-limit buckets and the thread ownership all become per-user at once. A multi-token daemon-side form is [#389](https://github.com/go-steer/mast/issues/389) option 2 and takes no freeze deadline with it — both tokens are environment variables, which the v1.0 promise does not cover.
- Rate limiting per authenticated principal (per-caller QPS + concurrent-run caps); shared implementation with A2A rate limiting.
- **A thread belongs to the caller who opened it** *(added 2026-09-17, [#382](https://github.com/go-steer/mast/issues/382))*. Carrying the endpoint's scopes answers "may this caller run this workload"; it is not an answer to "is this conversation yours", and `threadId` is the client's own correlation string, not a secret. So on an authenticated endpoint the caller's identity — tenant and subject, hashed to a fixed-width tag — is part of the derived session id, and two principals naming the same `threadId` address two different sessions. The separation is **structural, not a verdict**: a second caller is never refused, so there is no 403-vs-404 disclosure question to settle, and no owner record to store, migrate or clear. This is the same reasoning that already makes the session id daemon-derived rather than client-supplied (see [Session mapping](#session-mapping)) — a property of the id is stronger than a check against it. An endpoint with **no** validator has no subject and therefore nothing to own: its session ids are unchanged, mirroring `pkg/attach`'s `enforceACL` posture. The identity is hashed rather than interpolated so that a variable-width subject next to an attacker-chosen `threadId` cannot be made to collide across the delimiter.

### Long-running runs

AG-UI supports long-running runs natively via the streamed-event model — the SSE connection stays open until `RunFinished`. But mast workloads often exceed practical SSE-connection lifetimes (planner running for 20 minutes with an intervening HITL pause). Mast handles this via:

- **Native pause/resume**: HITL interrupts (per protocol) close the stream cleanly via `RunFinished{interrupt}`; client resumes with a new run.
- **Client disconnect resilience** *(mast extension — AG-UI defines no run-reattach semantics)*: ~~if the SSE stream disconnects mid-run, mast's session persists (per [`./durable-execution-design.md`](./durable-execution-design.md)); mast allows reconnect via the same `threadID` + `runID` and resumes streaming from the last durable event.~~ **Corrected 2026-09-17 (#98): a disconnect used to do the opposite — destroy the run.** The stream ctx descended from `r.Context()`, so a TCP reset cancelled the turn. The written rationale for cancelling covers a *stalled* consumer — `emit` runs on the turn goroutine holding the per-session lock and the drain bracket, so a vanished reader must not pin the turn — and that hazard is real, but the remedy conflated *this consumer is gone* with *this work should stop*, and nobody recorded the second as a decision; it fell out of which context was in scope.

  It was measurably harmful and not only untidy: if the disconnect landed while a mutating tool was executing — after the effect, before its `FunctionResponse` persists — the session kept no trace of the call, so a **well-behaved** client retry re-applied the change. Measured at 1 call vs. 2 on identical harnesses differing only in cancel timing.

  [`pkg/attach`](../pkg/attach) already solves the same hazard the other way, in this repo and ported from core-agent: `GET /events` is a *subscription*, a slow or dead consumer drops **the subscriber rather than the publisher**, and stopping a turn is an explicit `POST /interrupt`. So this splits in two, and the split is the useful part:

  - **The correctness half — [#383](https://github.com/go-steer/mast/issues/383), SHIPPED 2026-09-17.** Subscriber-scoped cancellation: the turn runs on `context.WithoutCancel(ctx)`, so it keeps the request's *values* (trace context) and loses only its cancellation, and a failed frame write retires the writer instead of the turn. The stalled-consumer hazard the old coupling was reaching for is handled where it actually lives — the per-frame write deadline in `writeSSEEvent` — so no emit can pin the turn for longer than that deadline, and a retired stream stops arming new ones. The consequence to state plainly: **nothing transport-side cancels a turn any more.** A disconnected run is bounded by `budget.max_wallclock_seconds`, the watchdog, and `mast sessions pause --cancel-turn`, which is the same set that bounds a daemon-started turn with no client at all. There is deliberately no AG-UI `POST /interrupt` yet; when a client needs to *mean* "stop", that is the shape to add, per `pkg/attach`.
  - **Reconnect proper — v1.1.** A replay cursor plus a re-subscription endpoint, so a client can rejoin a stream it dropped. This is the genuine mast extension, and it is deferrable precisely *because* the correctness half lands first: after it, a blip costs the tail of the stream, not the run.

  Standard AG-UI clients won't know to reconnect without mast-specific client code; document it as such when it ships.
- **Webhook event push** (v0.2+) *(mast extension — corrected 2026-07-25: there is no "AG-UI push-notification pattern" in the spec; that concept is A2A's)*: for clients that can't hold a persistent SSE connection, mast can POST events to a client-provided webhook URL as a mast-defined extension, clearly flagged as non-portable.

### Concurrent runs on the same thread

A `threadID` may have multiple in-flight `runID`s (branched exploration, retries, HITL-abandoned + new-attempt). Mast serializes execution per-thread by default (one active run per thread; queued if a new run arrives while another is active) — ~~configurable per bundle if the workload supports genuine concurrency~~.

**Corrected 2026-09-17 (#98).** The serialization is real, but it was a side effect rather than a policy: a second run blocks on the shared per-session turn lock inside `runTurnPre`. There was **no bundle key**, no queue depth, no refusal and no metric. The queue was unbounded and the only ceiling was `budget.max_wallclock_seconds`, so a caller queued behind a long turn got a timeout that said nothing about why it waited.

**Bounded 2026-09-17 ([#384](https://github.com/go-steer/mast/issues/384)).** The serialization is now a policy, and the wait has a ceiling that is not the budget:

- **`agui.run_queue.depth` counts runs that may *wait*.** The default is 3, so a thread admits four runs at once — one executing plus three queued. `depth: 0` is legal and means no concurrency at all: the thread runs one run and refuses the rest. The key is absent from almost every bundle, which is why it is a `*int` — `0` and *unset* must not be the same value.
- **There is deliberately no way to say "unbounded".** Unbounded is what the key exists to end, so a negative depth is refused at daemon start rather than clamped: it can only be a typo or a misremembered `-1`, and clamping would hand that author the default they did not ask for.
- **The surplus run gets HTTP 409 with `Retry-After`, decided before the SSE upgrade** — not the in-stream `RunError` the bias recorded, which allocates a stream, writes one frame and closes it for a request already decided against. Not 429 either: `Server.rateLimit` already returns that and means *arrival rate*, so a client that cannot tell "back off everywhere" from "this one conversation is busy" will pick the wrong remedy. A full thread is a conflict on that thread, and 409 is what says so.
- **It is counted**, as `mast_agui_runs_total{outcome="queue_full"}`, separately from the `rejected` the other pre-stream refusals share. The distinction is operational: `rejected` is answered with the caller's credentials or the rate limit, `queue_full` with capacity or a bundle key.

Two things the bound is *not*. It is not the turn lock: `sessionTurnLocks` stays unbounded by design, because inject, resume, scheduled fires and auto-resume have no client standing there to be refused. And it is not a global concurrency limit — admission is per derived session, so a busy thread never refuses a run addressed to another one.

## Mast as AG-UI client

Mast agents (specialists, planner, workflow nodes) can call *other* AG-UI servers when it makes sense. Rare compared to server-side; the primary AG-UI use case is being called, not calling. But the client side matters for a few scenarios:

- **CopilotKit-hosted agents** that mast wants to invoke as sub-tasks.
- **Framework peers** (e.g. a LangGraph agent hosted by CopilotKit Runtime) that don't yet speak A2A but do speak AG-UI.
- **User-driven sub-agent chains** where an AG-UI-user-shaped remote agent is the right composition.

Under [`./federation-design.md`](./federation-design.md), this is another protocol adapter alongside A2A / mast-native / HTTP/RPC. Reference format: `agui://<name>` or `agui://<endpoint-url>[?thread=<threadID>]`.

The federation `invoke_remote_agent("agui://external-triage", inputs)` tool wraps the AG-UI call — mast constructs a `RunAgentInput`, streams the response, and returns the aggregated result (or propagates HITL interrupts back to mast's own operator via [`./durable-execution-design.md`](./durable-execution-design.md)).

~~Community Go SDK (`github.com/ag-ui-protocol/ag-ui/sdks/community/go`) is the AG-UI client implementation mast uses; SDK is pre-1.0 but functional (types, events, SSE client). Mast wraps it in a thin `pkg/agui/client.go` for consistent context propagation, observability spans, and cancellation semantics.~~ Superseded by [Build-vs-buy](#build-vs-buy-hand-rolled-zero-dependency-overrides-the-july-sdk-wrap-guidance): the SDK is not a dependency and `slim-deps.sh` refuses one, so a client would be hand-rolled like the server.

**Deferred past v1.0 (decided 2026-09-17, #98).** Nothing here is blocked and nothing about it is hard; it is unbuilt because no consumer has asked, and the three scenarios above are all "a peer that speaks AG-UI but not A2A" — a shape [`./federation-design.md`](./federation-design.md)'s A2A adapter already covers for every peer mast has actually met. Two things make deferring cheap rather than merely convenient. The outbound direction is governed already: `invoke_remote_agent` classifies as **mutating unconditionally**, so an AG-UI adapter would inherit the write gate rather than need a new decision. And the server half is the load-bearing half — mast's job on this surface is being called by a browser, not calling one. Two things make it worth revisiting rather than cutting: the phrase "not yet" is doing real work in "peers that don't yet speak A2A", and a **cross-runtime** case (a Python-ADK AG-UI client calling mast, or the reverse) would be the first consumer that is not hypothetical. Filed against the umbrella rather than a milestone; it is the only remaining `#98` slice with no defect and no freeze exposure behind it.

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
| **Specialists** ([`./specialists-design.md`](./specialists-design.md)) | Specialists execute normally; their invocations emit as nested `ToolCallStart/Args/End/Result` events in the AG-UI stream. Users see specialists as tool calls in the UI. Under **coordinator or graph** dispatch the specialist's own events also reach the outer stream, so its work brackets as a `STEP_STARTED` named after it (#98). Under **planner** dispatch they do not — that runs on a private runner — so the client sees the `invoke_specialist` tool call and nothing inside it. |
| **Skills** ([`./skills-design.md`](./skills-design.md)) | Skill `allowed_tools` intersected with AG-UI client's `RunAgentInput.tools` — client-declared tools further narrow the allowlist. Skill invocations surface as tool calls in AG-UI. |
| **Workflow scaffolding** ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)) | Reference-graph node executions surface as the `STEP_STARTED` / `STEP_FINISHED` bracket, named after the node's agent (#98) — graph dispatch funnels sub-agent events up to the outer stream, so the emitter sees the author change. Users see the workflow shape in the UI. `ActivitySnapshot` / `ActivityDelta` are **not** used for this: a node execution is already a step, and a second vocabulary would publish the same fact twice. |
| **Durable execution** ([`./durable-execution-design.md`](./durable-execution-design.md)) | AG-UI interrupt lifecycle *is* mast's durable pause/resume. `RunFinished{interrupt}` = pause; `RunAgentInput.resume` = resume. Cross-boundary state persistence works out of the box. ~~Client disconnect + reconnect resumes streaming from last durable event.~~ *(Corrected 2026-09-17: since [#383](https://github.com/go-steer/mast/issues/383) a disconnect leaves the turn running to completion and durably recorded, which is the half that mattered; **re-attaching to the stream** is unbuilt and is v1.1 — see [Long-running runs](#long-running-runs).)* |
| **Multi-tenant** ([`./deployment-design.md`](./deployment-design.md)) | AG-UI auth token can carry tenant claim; maps to `WithIsolationScope`. Per-tenant `agui.expose` policies possible via bundle isolation scope. |
| **Observability** ([`./observability-design.md`](./observability-design.md)) | AG-UI-specific span types (`agui.server.run`, `agui.client.call`); metrics (`mast_agui_runs_total{workload, outcome}`, `mast_agui_run_duration_seconds{workload}`, `mast_agui_active_threads{workload}`, `mast_agui_interrupts_total{workload}`). Distributed tracing across `traceparent` propagation to AG-UI client. |
| **Memory** ([`./memory-design.md`](./memory-design.md)) | AG-UI `state` field maps to bundle-scoped state-bound reads. AG-UI's `StateDelta` events emit when bundle state changes visible to the workload. |
| **Library API** ([`./library-api-design.md`](./library-api-design.md)) | `github.com/go-steer/mast/agui` package: `agui.Server` + `agui.Client` + `agui.TokenValidator` (shares interface with A2A). Programmatic exposure via `ServerConfig.AGUI` (analog to `ServerConfig.Attach`). |
| **Deployment** ([`./deployment-design.md`](./deployment-design.md)) | AG-UI endpoint typically fronted by Ingress + TLS. Chat-platform bots (CopilotKit-based) deploy as sidecars — same pod, same cluster, or external. Deployment starters ship for each. |
| **MCP catalog** ([`./mcp-catalog-design.md`](./mcp-catalog-design.md)) | Sibling to the surface comparison — the fourth interop surface (agent↔user protocol); mcp-catalog-design enumerates all four and owns the framing. |
| **Config layout** ([`./config-layout-design.md`](./config-layout-design.md)) | No new `.agents/agui/` directory required — AG-UI exposure is bundle-level config (`agui.*` fields on workload bundles). |

## Phasing

**The version column below lapsed and is struck through (2026-09-17, #98).** It was written before the fork and never re-cut: v0.2 lists work that shipped across v0.2 and v0.9, work that has since been cancelled outright (the SDK pin, [OQ #1](#open-questions)), and work still open; v0.3 and v0.4+ are a schedule for releases that came and went. Struck rather than re-dated, because a lapsed schedule quietly re-read as a commitment is [#300](https://github.com/go-steer/mast/issues/300)'s failure shape and the same call [#291](https://github.com/go-steer/mast/issues/291) made for `deployment-design`. **The live sequence is [Implementation status](#implementation-status-v02-stages-12) for what shipped and the checklist on [#98](https://github.com/go-steer/mast/issues/98) for what has not**; where the open items now sit relative to the v1.0 freeze is recorded in the individual open questions above. The rows are kept as the record of what was once planned.

| ~~Version~~ | Scope |
|---|---|
| **v0.1** | **Nothing ships** (re-cut 2026-07-25 per [`./fork-design.md`](./fork-design.md) — AG-UI server + client both move to v0.2; this also resolves the earlier contradiction where this doc put an `agui://` federation adapter in v0.1 while [`./federation-design.md`](./federation-design.md) said A2A-only). Design-time obligation only: keep the attach protocol + durable pause/resume shaped so the v0.2 AG-UI mapping stays a projection, not a rework. |
| **v0.2** | AG-UI server: per-workload endpoints (`agui.expose: true`); RunAgentInput acceptance; event streaming for lifecycle + text messages + tool calls + state; HITL via the draft interrupt extension (`RunFinished{outcome: interrupt}` + resume) — SDK version-pinned, encoding isolated in `pkg/agui`. Auth via shared `TokenValidator`. Basic client (`invoke_remote_agent("agui://...")` federation adapter). SSE-only. Then: activity events (workflow-shape visibility); reasoning events (opt-in); mast-extension webhook push; `/agui/agents.json`; `examples/deploy/slack-via-copilotkit/` starter **once the channels-* packages stabilize on npm**; client-disconnect + reconnect resumption (mast extension). |
| **v0.3** | Multi-thread concurrency support (opt-in per bundle); per-tenant AG-UI policy; observability + bundle-learning integration (AG-UI patterns feed learning). `examples/deploy/{teams,discord,telegram,whatsapp}-via-copilotkit/` starters. CopilotKit React reference example (`examples/copilotkit-react/`) showing a full stack. |
| **v0.4+** | AG-UI protocol version negotiation once the spec matures (interrupts/activity/reasoning finalized). Cross-runtime AG-UI federation (mast AG-UI server called by Python-ADK AG-UI client). |

## Open questions

1. ~~**AG-UI protocol version pinning.** SDK is pre-1.0; specs evolve. Bias: test against pinned SDK version per mast release; document tested-against version in `pkg/agui/VERSION.md`; upgrade cadence separate from mast release cadence.~~ **Struck 2026-09-17 (#98): the question's premise is gone.** Every clause of the bias names an SDK, and [Build-vs-buy](#build-vs-buy-hand-rolled-zero-dependency-overrides-the-july-sdk-wrap-guidance) removed the SDK — `pkg/agui` is hand-rolled, the community SDK is not a dependency, and `dev/ci/presubmits/slim-deps.sh` refuses one. There is nothing to pin, no upgrade cadence to decouple, and `pkg/agui/VERSION.md` was never written and should not be: the file would record the version of a dependency mast does not have. What replaced it is narrower and already ships — a `protocol_version` string on each discovery descriptor, which states the wire version mast *speaks* rather than the version of a library it builds against. The live question this leaves is version *negotiation* (what a server does when a client speaks a different `protocol_version`), which is listed under [Phasing](#phasing) and is not this question; struck rather than deleted because the reversal is the record.
2. ~~**`/.well-known/` metadata endpoint for AG-UI.** Not in the standard but potentially useful for CopilotKit discovery. Bias: ship an optional aggregation endpoint (`/agui/agents.json`) but don't require clients to use it — many will be configured with direct endpoint URLs.~~ *Resolved 2026-09-17 (#98), by what Stage 1 built and [#377](https://github.com/go-steer/mast/issues/377) finished: the bias shipped exactly as written. `/agui/agents.json` exists, nothing requires a client to read it, and no `/.well-known/` path was added — inventing a well-known URI for a protocol whose spec defines none is a claim on a registry namespace mast does not own. The content model was the part that actually needed deciding, and it is [OQ #8](#open-questions), settled separately.*
3. **Thread-to-session mapping bundle default.** `per_thread` (long-lived session) or `per_run` (one session per run)? Bias: `per_thread` for `Chat`-mode workloads (matches chat UX); `per_run` for `orchestrate`/`debug`/`research`/`review` (matches task-runner UX). Explicit override always available. *Resolved 2026-08-08 (Stage 1): a **fixed** `per_thread` default with an explicit `per_run` override ships now — `workload.Bundle` has no task-class field yet, so the task-class-aware default waits on one. See [Implementation status](#implementation-status-v02-stages-12).*
4. **Concurrent-run policy.** Default: one active run per thread; queue if another arrives. Configurable per bundle. What's the queue depth? Bias: 3; reject with `RunError` if exceeded; observable via metric. *Still open, and re-stated 2026-09-17 (#98) because the recorded bias was being read as a description of the code. **None of it is implemented** — no depth limit, no `RunError`, no metric, no bundle key.* What actually happens: a second run on a thread blocks on the shared per-session turn lock inside `runTurnPre`, unbounded, and the only ceiling is `budget.max_wallclock_seconds`, so a caller that queues behind a long turn eventually gets a timeout carrying no information about why it waited. `Server.rateLimit` is not this — it bounds *arrival rate* per `(subject, tenant, workload, method)`, so one client can stack N in-flight runs on a thread as long as they arrive slowly enough. The bias itself still looks right: a bounded queue with an explicit refusal gives an unattended caller something to act on, where an unbounded wait ending in a wallclock timeout does not. *Resolved 2026-09-17 (#98), and the bias is implemented rather than replaced — both sub-questions answered, and the deadline restated because the version recorded here was the weaker one.* **Depth is a per-bundle nested key, `agui.run_queue: {depth: 3}`.** Nested rather than a flat `max_queued_runs` for a reason specific to this moment: adding a key to `workload.Bundle` after v1.0 is additive and permitted, while changing a key's *shape* is a `schema_version` bump, and nothing has consumed this yet — so an object that absorbs a later `policy:` or `wait_timeout:` is worth the extra nesting. **The refusal is HTTP 409 with `Retry-After`, before the SSE upgrade.** Not the in-stream `RunError` the bias assumed, which allocates a stream, writes one frame and closes it for a request already decided against; and not 429, because `Server.rateLimit` already returns that and means something else by it — arrival rate for a `(subject, tenant, workload, method)` bucket — so a client that cannot tell *back off globally* from *this one thread is busy* will pick the wrong remedy. 409 says conflict on this resource, which is what a full per-thread queue is. **And the freeze pressure is the behaviour, not the key**, which is the correction: this question recorded the deadline as the bundle key landing on a frozen `workload.Bundle`, and that is the half that turns out to be permitted later. What is not permitted later is bounding a queue that is unbounded today, because that changes what an existing bundle does — one of the two breaking categories [`./compatibility-policy.md`](./compatibility-policy.md) names as invisible to any signature, owing two released minors and 90 days after v1.0. So the bounded queue is the pre-freeze item with or without the key. Tracked as [#384](https://github.com/go-steer/mast/issues/384). ***Shipped 2026-09-17 (#384) as resolved, with one number the resolution did not state: `depth` counts the runs that may **wait**, so the admitted total is `depth + 1` and the default of 3 admits four. Naming it "queue depth" and then counting the executing run in it would make `depth: 0` mean "refuse everything", which is not a setting anyone wants; this way `depth: 0` means "no concurrency", which is. A negative depth is refused at startup rather than clamped, `Retry-After` is a fixed 5 seconds — a backoff floor, not a prediction, since mast does not know how long the turn ahead will take — and the refusal is counted as `queue_full`, distinct from `rejected`. See [Concurrent runs on the same thread](#concurrent-runs-on-the-same-thread).***
5. **Reasoning event exposure.** Some model reasoning tokens are sensitive (chain-of-thought reveals prompt-injection surface). Bias: default off; opt-in per bundle (`agui.emit_reasoning: true`); document the tradeoff. *Resolved 2026-09-16 (Stage 4, #98): the bias shipped as written, with the shape the 2026-09-15 annotation demanded. The floor it opts in FROM was built first (#370): "default off" had not been a default at all — eight surfaces read model text with `part.Text != ""`, which does not exclude a thinking block, and the only reason nothing leaked was that `claude-opus-5` under the request mast sends returns a thinking block with an empty body. So `emit_reasoning: true` is a deliberate read of `Thought` parts through `internal/modeltext.Thought`, never the removal of a filter, and the two predicates are disjoint by test. Three things the bias did not say are settled with it: off emits **nothing at all**, not an empty bracket, because the existence of a thought can be the disclosure; a thinking block's provider **signature has no frame** (`ReasoningEncryptedValue` is deliberately unmodeled — it is a replay credential, not a thought); and the enabled flag logs at `WARN` rather than `INFO`, unlike the state projection, because what it publishes is the one text on this surface nobody wrote for an audience. See [Implementation status](#implementation-status-v02-stages-12).*
6. **Tool declarations from AG-UI clients.** `RunAgentInput.tools` lets clients declare tools they expose *to* the agent (frontend tool calls). Mast can support this (client-side tools count as another tool class the planner can invoke); need to reconcile with bundle `tool_catalog` allowlist. ~~Bias: client-declared tools require `agui.accept_client_tools: true` opt-in per bundle; intersected with bundle allowlist same as skills.~~ **Re-biased 2026-09-17 (#98). The opt-in half stands; the intersection half names an object that cannot do the job, and shipping it would have produced a gate that is empty by construction.** The half that survives: acceptance is a per-bundle opt-in, default off, because a client-declared tool is a *caller* telling the agent what it may call, which is the one direction the rest of this surface never allows.

   The half that does not: "intersected with bundle allowlist same as skills" is wrong twice. `tool_catalog.tools` is **not an allowlist** — it is a per-tool *policy override* table (mutation class, capture/revert), keyed by registered name; nothing in it grants or denies. And the real allowlist, the per-specialist one in [`./specialists-design.md`](./specialists-design.md), narrows **wired** toolsets: built-in tools the daemon constructed and MCP servers it dialed. A browser-executed tool is in neither axis and never can be, because mast has no implementation of it — that is the entire point of the feature. So the intersection is the empty set for every input, and a bundle that opted in would find that no client tool is ever callable.

   That is not a naming slip to patch, it is the design question arriving properly: **what does it mean to allow a tool whose behaviour lives on the other side of the wire?** Everything mast's governance layer does to a tool call — the mutation predicate, the write gate, the effect outbox, capture/revert — reads a tool mast can describe. A client tool is a name and a JSON schema supplied by the caller, per run, and nothing more. So the gate has to be built rather than borrowed, and it owes answers to at least: whether the bundle names permitted client tools (making them operator-declared, and the run's declaration a match rather than a grant) or merely permits the class; what the **default mutation classification** is, given the defaults elsewhere treat an unknown tool as mutating and treating a browser tool that way would park every run at the write gate; whether a client tool can appear inside a **specialist's** allowlist at all or only at the root; and what the **effect record** says about a call whose outcome mast only knows by being told.

   **Sequencing** (settled 2026-09-17): the gate is designed and the bundle key shape decided **before v1.0**, because any key lands on the frozen `workload.Bundle`; the implementation ships in **v1.1**. Until then `RunAgentInput.tools` is parsed and dropped, which is the correct behaviour for a declaration mast cannot honour safely — and is silent today, which the design work should fix.

   ***Gate resolved 2026-09-17 (#98). All four answers below, and the premise the first attempt rested on turned out to be false — which is the part worth reading.***

   **Identity cannot be the trust boundary here, and finding out why produced [#389](https://github.com/go-steer/mast/issues/389).** The natural gate is *who may declare* — a per-workload scope, on machinery that already ships: `principal.HasScope` in `pkg/agui/server.go`, and the `auth.scopes` every discovery descriptor already publishes. It does not work. The daemon's validator (`cmd/mast/agui.go`) is a static bearer map holding **one** token, resolving to `Subject: "mast-agui-static"`, carrying the **union of every exposed workload's scopes** — its own comment says why: *"so it can drive any of them."* So there is exactly one caller, it holds every scope the config asks for, `authorize`'s loop has no reachable `false` branch, and with `MAST_AGUI_TOKEN` unset there is no principal at all. An `agui:declare_tools` scope would be minted into the only principal that exists: a second spelling of *the feature is on*, wearing the costume of a capability grant. That is the [#364](https://github.com/go-steer/mast/issues/364) / [#375](https://github.com/go-steer/mast/issues/375) shape, and it was very nearly committed in the act of avoiding it. **The trust boundary on this surface is therefore the deployment, not the person** — a constraint rather than a preference, and one that relaxes on its own the day a host supplies a real validator through the `pkg/serverauth.TokenValidator` seam.

   **(1) The bundle bounds the class; it does not name its members.** An operator cannot know the frontend's tool names — they live in the UI's source, shipped on its own clock — so a name list means transcribing another team's identifiers into YAML and redeploying on every UI release, and the predictable end state is a wildcard, which is worse than no gate because it looks like one. The key is an envelope — `agui.client_tools: {accept, max_declared, during_hitl_park, …}` — nested for [#384](https://github.com/go-steer/mast/issues/384)'s reason rather than flat. A bound on the declared JSON schema's size belongs in it too: that text is caller-supplied and enters the model's context, so it is prompt-injection surface before it is anything else.

   **(2) The class is mutating by default**, with `mutating: false` for a frontend whose tools genuinely only read. This reverses the drafting position. Non-mutating-by-default was argued on the grounds that the write gate would park every run — but that describes an *unattended* daemon, and this is the one surface that is not one. A browser has a human in front of it, and the round-trip already ships: a parked run emits `RunFinished{outcome: interrupt}` listing the open interrupts, and the client answers with a new run carrying `Resume []ResumeEntry` keyed on `ParentRunID`. Parking here is the **cheapest** park in the product. What survives of the objection is narrow — approving a confirm dialog is circular — and that is an argument for classing one tool non-mutating, which is impossible without names. So it is an escape hatch, not a default.

   **(3) Root only.** Never inside a specialist. The per-specialist allowlist narrows **wired** toolsets and a browser tool is in neither axis, so stretching it over a third would make one construct mean two things; and a specialist is a delegation mast runs on the operator's behalf, so routing a caller-supplied tool into one crosses two trust boundaries in a single hop. **The honest cost, recorded rather than argued away:** most real bundles are multi-specialist, so this may make the feature useless in practice — which is the signal to revisit, and it needs a consumer to produce it rather than a guess.

   **(4) The effect record exists, says it was attested rather than observed, and closes the revert door explicitly** — `attested_by: client`, `observed: false`, `revert: {available: false, reason: "client-executed"}`. Omitting the row was the alternative, on the ground that an unverifiable entry pollutes a ledger whose value is that everything in it was observed. Rejected: an outbox that is silently an incomplete account of the run is v0.9's failure shape exactly — a surface answering confidently with nothing in it, where nothing downstream can tell the difference. An explicit `false` with a reason is the same call [#377](https://github.com/go-steer/mast/issues/377) made for `capabilities`, and for the same reason.
7. **State delta authorship.** AG-UI `StateDelta` events publish state changes to the client. Which mast state keys emit? Bias: bundle declares `agui.state_projection: [key1, key2]` — explicit allowlist of state keys projected to the client; default empty (nothing projected without explicit config). *Resolved 2026-09-14 (Stage 3, #98): the bias shipped as written, and two things the bias did not say are settled with it. An unlisted key emits **nothing** rather than a redacted op, so the projection is a filter and a client cannot learn that an unlisted key changed; and mast emits only `add` ops, because the opening `StateSnapshot` echoes the client's own state document and the daemon therefore cannot know which members that document already has. See [Implementation status](#implementation-status-v02-stages-12).*
8. **Aggregation endpoint content model.** `/agui/agents.json` is mast-defined; format-shape TBD. Bias: JSON array of `{name, endpoint, description, input_schema, auth: {scopes}}` per exposed workload. Keep it simple; align with CopilotKit conventions once they publish one. *Resolved 2026-09-16 (#377): the bias shipped in Stage 1 and then went stale twice, because Stages 3 and 4 added two per-bundle publication surfaces the descriptor said nothing about. A client cannot infer either from `protocol_version` — they are per workload, so two workloads on one daemon differ — and cannot infer them from a finished run either, since a stream with no reasoning in it looks identical whether the workload publishes none or the model did not think. So each descriptor now carries a **`capabilities`** object: `state_delta`, `state_keys` (the declared keys in declared order, omitted when `state_delta` is false), and `reasoning`. Three things settle with it. The booleans are **never elided when false**, because an absent key means an older server and an explicit `false` is a promise, and a client has to tell those apart. The values are read from the **same `aguiPublication` the run's emitter is built from**, via an optional `agui.CapabilityReporter` on the Backend rather than a field on `ExposedWorkload` — a second reader of the same config is exactly how [#364](https://github.com/go-steer/mast/issues/364) and [#375](https://github.com/go-steer/mast/issues/375) each advertised something untrue for releases. And the endpoint **stays public**, under a stated rule: the descriptor may say what a client permitted to run would observe in the stream anyway, and may not say anything about the governance around the stream — which is why there is no HITL bit, since an unauthenticated "mutations here fire unattended" is observable to nobody who cannot already run. See [Implementation status](#implementation-status-v02-stages-12).*
9. **CopilotKit-hosted vs. self-hosted-bot deployment guidance.** CopilotKit sells a managed platform; self-hosting the `@copilotkit/channels` bot process alongside mast is also viable. Bias: document both; recommend self-hosted for platform teams with existing GKE/Cloud Run infra; recommend managed for teams without.
10. **AG-UI-native workload authoring UX.** Some workloads are natively chat-shaped and want UI hints in their bundle (starter messages, quick-reply chips, avatar). AG-UI protocol supports these via `Custom` events. Bias: pass through as-is; provide helper functions in `pkg/agui/` for common patterns; don't add mast-specific extensions.

## Out of scope

- ~~**Reimplementing the AG-UI Go SDK.** We use the community SDK (`github.com/ag-ui-protocol/ag-ui/sdks/community/go`) as-is; contribute upstream when we hit bugs; wrap in `pkg/agui/` for mast-specific integration.~~ **Superseded** — see [Build-vs-buy](#build-vs-buy-hand-rolled-zero-dependency-overrides-the-july-sdk-wrap-guidance). `pkg/agui` is hand-rolled with zero new external dependencies; the SDK is not a dependency and pulling it would trip `dev/ci/presubmits/slim-deps.sh`. Struck through rather than deleted because the reversal is the record: this line said the opposite of what ships for three releases after the decision was taken elsewhere in this same file.
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
