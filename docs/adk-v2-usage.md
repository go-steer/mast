# mast ADK v2 usage: inventory

**Status:** draft, 2026-07-16 (updated 2026-07-25 — spike-2 verification pass against `google.golang.org/adk/v2 v2.1.0`; API corrections, `session/database`, resume contract, v2.1.0 additions. Everything marked *verified* below was exercised by running code in the `mast-prototype` repo — see its `FINDINGS.md` — not inferred from docs. Updated 2026-07-26 — Phase-1 build-wave findings folded in: `workflowagent` option pass-through, `NewJoinNode` signature, `ParallelWorker` retry/ordering/sub-branch semantics, unconditional-edge idiom, `finish_task` output shape, OTel-API structural import; verified against the shipped code under `examples/workflows/` and `pkg/`). Consolidated cross-reference for the ADK v2 constructs mast leans on. Companion to [`./fork-design.md`](./fork-design.md) (resolves ADK v2 from day one; the "Recommended approach" and 2026-07-01 resolved blocks are the densest v2 material), [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) (reference-graph library expressed on v2 primitives), [`./specialists-design.md`](./specialists-design.md) (subagent-as-tool via `agenttool` + agent modes), [`./durable-execution-design.md`](./durable-execution-design.md) (session-durable pause/resume substrate), and [`./orchestration-design.md`](./orchestration-design.md) (planner tool vocabulary on top of the same primitives). Not a strategy doc — this is a reference: which v2 surface areas mast consumes, where each surface shows up, and why.

## Why this doc exists

Someone bootstrapping Phase 1 bucket 1 of the fork ([`./fork-design.md`](./fork-design.md) P1.2) needs to know the whole ADK v2 surface mast depends on before writing the first `go.mod`. That surface is currently scattered across five design docs — each mentions the constructs it needs in the context of its own subsystem. This doc gathers the mentions into one inventory, grouped by v2 subsystem, with the mast-side consumer and rationale for each.

It is deliberately non-authoritative: each companion doc's resolved-decisions section stays the source of truth. If this doc and a companion doc conflict, the companion doc wins.

## The v2 substrate at a glance

The single package mast pins is `google.golang.org/adk/v2` (see [`./fork-design.md`](./fork-design.md) P1.1). The two subpackages that appear most in the design corpus:

- `google.golang.org/adk/v2/workflow` — graph engine, node types, routing, run options ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) line 9).
- ADK v2 `agenttool` package (signature unchanged from v1; see [`./specialists-design.md`](./specialists-design.md) Related section).

The v2 gain that shapes almost every downstream decision: **agent execution, node execution, tool calls, specialist invocation, and sub-workflows all share one span tree and one event stream** ([`./observability-design.md`](./observability-design.md) "Substrate: v2's unified span tree"; [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "Interaction with attach mode + mast-web"). Everything below composes off that unification.

## Runner + agent modes

**What v2 provides.** A single runner that drives `LlmAgent`s in one of three modes:

- `Chat` — conversational coordinator.
- `Task` — opinionated goal-directed execution; auto-installs a `finish_task` helper tool the agent calls to return its focused final.
- `SingleTurn` — one call, one output; no `finish_task`.

Per [`./fork-design.md`](./fork-design.md) 2026-07-01 resolved block, "auto-installed helper tools (`finish_task`, `single_turn`, `task`) replace prompt-engineering we would otherwise have to do to signal completion."

**Where mast uses it.**

- **Task-class profiles** ([`./positioning.md`](./positioning.md) "Default tool catalog"; [`./fork-design.md`](./fork-design.md) resolved-decisions): `--task=chat` → `Chat`; `--task=debug|implement|research|review|orchestrate` → `Task`; `SingleTurn` is internal only ([`./orchestration-design.md`](./orchestration-design.md) "SingleTurn is internal, not user-facing").
- **Specialists** ([`./specialists-design.md`](./specialists-design.md) schema `mode:` field): `Task` (default; `finish_task` argument is the return value) or `SingleTurn` (single output). `Chat` deliberately not exposed — specialists are sub-agents, not coordinators.
- **Planner** ([`./orchestration-design.md`](./orchestration-design.md) "Recommended: C with light D flavor"): a Task-mode `LlmAgent` whose canonical exit is `finish_task`; hitting `budget.max_turns` without `finish_task` triggers `hitl_policy.on_budget_exhaustion`.
- **Classifiers** ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #7; [`./orchestration-design.md`](./orchestration-design.md) "Classifier-first dispatch"): `SingleTurn` `LlmAgent` on the cheap tier — the shape behind LLM-as-router and the substring-matcher's replacement.
- **Bucket 1 core** ([`./fork-design.md`](./fork-design.md) P1.2): "Chat / Task / SingleTurn mode wiring; helper-tool auto-installation surfaces exposed to bucket 2's config layer."

**Rationale.** The auto-installed exit tool preserves the digest pattern by construction — specialists return focused output through `finish_task`'s argument, not raw last-assistant-text ([`./specialists-design.md`](./specialists-design.md) open Q #1, resolved 2026-07-01). No wrapper needed. `Task` vs. `SingleTurn` is the primary schema knob for whether a callable subroutine reasons in a loop or classifies in one shot.

## Unified `agent.Context`

**What v2 provides.** One context type flowing through providers, nodes, agents, and tools ([`./fork-design.md`](./fork-design.md) line 29: "unified `agent.Context`"). Replaces v1's split between agent-side and node-side context types.

**Where mast uses it.**

- **Bucket 2 adapter ports** ([`./fork-design.md`](./fork-design.md) P1.3): providers, MCP client, attach transport, event log — all take the unified context. Explicitly called out as an ADK-touching change per the port table.
- **Cross-cutting concerns inherited by workflow shapes** ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) design-principle section): "cost ceilings, permission gates, watchdog integration — stay as first-class runtime concerns configured on the parent agent (the shapes inherit them via `agent.Context`)."
- **Custom `InvocationContext` implementations** ([`./durable-execution-design.md`](./durable-execution-design.md) "Substrate"; [`./fork-design.md`](./fork-design.md) "ADK-version-boundary flag"): must supply `IsolationScope()` and `ResumedInput(id string)` — the primitives that make replay work.

**Rationale.** One context means one place for permission gates, isolation scopes, cost interceptors, watchdog hooks. It also anchors the ADK-boundary flag between mast (v2) and core-agent (v1) — anywhere the context type reaches through a ported package, the port is not a straight cherry-pick.

## Graph engine and node types

**What v2 provides.** The `workflow` subpackage delivers node runtime, graph scheduler, cyclic graphs, branch isolation, and dynamic-list orchestration as native primitives ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "Why this is a real subsystem"). Node constructors and edge/route helpers appear across the reference-graph sketches:

| Construct | Kind | First mention |
|---|---|---|
| `workflow.NewFunctionNode` | Plain function node | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shapes #1, #2, #4, #5, #6, #7 |
| `workflow.NewFunctionNodeFromState` | State-bound function node (pulls session state values as typed inputs via `state:"<key>"` struct tags) | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #2; [`./memory-design.md`](./memory-design.md) example |
| `workflow.NewEmittingFunctionNode` | Function node that can emit `session.Event`s during execution | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #5; [`./specialists-design.md`](./specialists-design.md) "streaming partial results" |
| `workflow.NewDynamicNode` | Dynamic-orchestration node; body is plain Go calling `RunNode` per work item | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #3 |
| `workflow.NewJoinNode` | Fan-in aggregation (fan-in to a non-`JoinNode` fails validation: `ErrUnsupportedFanIn`). *Corrected 2026-07-26 (build-wave verification): the v2.1.0 signature is `NewJoinNode(name)` — no `NodeConfig` argument (a schema-validating variant `NewJoinNodeWithSchema(name, schema)` also exists). The join's input — and its verbatim output — is a `map[string]any` keyed by predecessor node name, **even with a single predecessor**; the successor unwraps by key.* | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shapes #1, #5, #6 |
| `workflow.NewParallelWorker(name, wrapped Node, maxConcurrency int, cfg NodeConfig)` — or `NodeConfig{ParallelWorker: true}` | Parallel workers over homogeneous input. *Corrected 2026-07-25: earlier drafts named `NewParallelWorkerNode` / `NewParallelWorkers`, which do not exist in v2.1.0.* *Verified 2026-07-26 (v2.1.0 source + `examples/workflows/fan-out-fan-in`): the constructor **rejects** a wrapped node carrying `RetryConfig` — retry is hoisted to the worker's own `NodeConfig` and applied per item independently; aggregated outputs preserve input order; each item runs in a derived sub-branch `<wrapped-name>@1..N`; intermediate non-output events from the wrapped node are suppressed (only the single aggregated output event surfaces — which reinforces the no-HITL-inside-parallel constraint below).* | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shapes #1, #6 |
| `workflow.NewToolNode` | Tool node | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #2 |
| `workflow.NewWorkflowNode` | Sub-workflow embedded as a single node (composition) | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "Composition + reuse" |
| `workflow.NewEdgeBuilder` with `AddFanOut`, `AddFanIn`, `AddRoutes` (plus `Add`, `AddRoute`, `Build`); `workflow.Chain` / `workflow.Concat` for linear wiring | Edge/route wiring. *Corrected 2026-07-25: `AddFanOutDynamic` does not exist in v2.1.0 — dynamic per-item parallelism is `ParallelWorker`; dynamic orchestration is `NewDynamicNode` + `RunNode`.* *Corrected 2026-07-26: an **unconditional** edge is plain `EdgeBuilder.Add(from, to)` (a nil `Route`). The earlier doc-sketch idiom `AddRoutes(x, map[string]Node{"": y})` is wrong — `AddRoutes` maps every key through `StringRoute`, so the empty key creates `StringRoute("")`, which never matches anything.* | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shapes #1, #2, #4, #6, #7 |
| `workflow.RunNode[OUT]` | Dynamic sub-run invocation — generic on the *output* type only: `RunNode[OUT any](ctx agent.Context, child Node, input any, opts ...RunNodeOption) (OUT, error)`. *Verified 2026-07-26: when the child is a Task-mode specialist (agent node), the `RunNode` output is the **whole `finish_task` arguments map** (e.g. `map[result:...]`), not a bare string — callers unwrap the key they want.* | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #3; [`./fork-design.md`](./fork-design.md) 2026-07-01 note ("`task`-mode agents can't be used as static graph nodes — workflow-scaffolding examples use dynamic nodes (`RunNode`) for Task-mode sub-agents") |
| `workflow.StringRoute`, `workflow.Default` (also `IntRoute`, `BoolRoute`, `MultiRoute[T]`) | Routing primitives | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #7 |
| `workflowagent.New(Config{Name, Description, Edges, SubAgents})` | Wraps a graph as a runnable `agent.Agent`. **Verified 2026-07-25: a workflowagent-wrapped graph runs as the runner's root agent directly** — the runner's Chat-mode restriction applies only to `LlmAgent` roots. Register wrapped agents in `SubAgents` so the runner resolves event authorship. *Caveat (verified 2026-07-26): `workflowagent.Config` does **not** pass `workflow.Option`s through — it calls `workflow.New(name, edges)` bare — so graph-wide run options like `WithMaxConcurrency` are unreachable from a workflowagent root; see the run-options list below.* | `mast-prototype` `pkg/graph`; upstream `examples/workflow/routing/llm` |

**Run options** (passed to `RunNode`, parallel workers, etc.):

- `WithIsolationScope(scopeID)` — segregates history per scope. Multi-tenant supervisor+workers is the primary use case ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #3 and "With isolation"); [`./orchestration-design.md`](./orchestration-design.md) `isolation.scope` bundle field maps here; [`./memory-design.md`](./memory-design.md), [`./deployment-design.md`](./deployment-design.md), [`./a2a-design.md`](./a2a-design.md), [`./skills-design.md`](./skills-design.md), [`./ag-ui-design.md`](./ag-ui-design.md) all propagate tenant scope through it.
- `WithUseSubBranch(true)` — isolates a child's context from its parent (the specialist-isolation primitive when a specialist is invoked as an agent node; see [`./specialists-design.md`](./specialists-design.md) composition table and [`./orchestration-design.md`](./orchestration-design.md) `invoke_specialist` tool row).
- `WithMaxConcurrency(n)` — graph-wide concurrency cap; critical for fan-out-fan-in and map-reduce ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "With watchdog + cost ceilings"). *Caveat (verified against v2.1.0 docs): does NOT govern dynamic children invoked via `RunNode` — supervisor+workers needs its own limiter for dynamic fan-out.* *Second caveat (verified 2026-07-26, build wave): `workflowagent.Config` does not pass `workflow.Option`s through (`workflow.New(name, edges)` bare), so a graph run as a workflowagent root **cannot set** `WithMaxConcurrency` at all — the per-`NewParallelWorker` `maxConcurrency` argument is the only concurrency cap that binds there. `examples/workflows/fan-out-fan-in` documents the three distinct knobs.*

**Parallel-branch HITL constraint (added 2026-07-25).** v2.1.0 exposes `ErrParallelHITLUnsupported`: a HITL interrupt cannot be raised from inside parallel branches. Shapes that combine fan-out with mid-branch operator escalation (fan-out-fan-in, adversarial verifier, map-reduce in [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)) must hoist escalation to a post-join node or route the escalating item out of the parallel section first.

**Per-node knobs:**

- `Timeout` — maps to specialists' `max_wallclock_seconds` ([`./specialists-design.md`](./specialists-design.md) open Q #2; [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "With watchdog + cost ceilings").
- `RetryConfig` — bounds transient failures. v2's `DefaultRetryConfig` = 5 attempts, 1s→60s, 2× backoff, full jitter ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) same section).

**Branch isolation.** v2 defaults to isolating parallel branches; fan-out-fan-in and map-reduce don't need extra care to prevent history leakage across siblings ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "With isolation").

**Cyclic graphs.** v2 makes cycles first-class ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #4). Autonomous loops (replacing today's `pkg/agent/autonomous.go`), inbox loops, and long-running monitors express as cyclic graphs with a router deciding continue-vs-exit.

**Rationale.** The whole design principle in [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) — "reference graphs, not helper packages" — depends on these primitives being expressive enough that mast's contribution is domain wiring, not engine wrapping. The presence of dynamic nodes + `RunNode[T]` is what makes supervisor+workers, multi-tenant scoping, and the planner's supervisor-body shape ([`./orchestration-design.md`](./orchestration-design.md) shape "C") land without helper layers.

## `agenttool` — subagent-as-tool

**What v2 provides.** The `agenttool` package (signature unchanged from v1 per [`./specialists-design.md`](./specialists-design.md) Related section) wraps an `LlmAgent` as a `tool.Tool` the parent can invoke. Task-mode auto-installed `finish_task` is what the wrapped subagent returns through.

**Where mast uses it.**

- **Specialists loader** ([`./specialists-design.md`](./specialists-design.md) implementation shape; [`./fork-design.md`](./fork-design.md) P1.4): `pkg/specialists/Register` wires each Spec via `agenttool.New` for `agenttool`-shaped invocation from the parent's own reasoning.
- **Skills invoked as callable tools** ([`./skills-design.md`](./skills-design.md); [`./orchestration-design.md`](./orchestration-design.md) `invoke_skill` planner tool): skills surface uniformly through the same wrapping mechanism.
- **A2A client-side agent mapping** ([`./a2a-design.md`](./a2a-design.md) v0.1 scope): "mapping `invoke_a2a_agent` to `agenttool`-shaped tool."
- **The `agenttool`-as-alternative to agent-node** ([`./specialists-design.md`](./specialists-design.md) composition table; [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "With specialists"): parent decides mid-turn to invoke the specialist — non-graph-position-bound. Contrast with agent-node composition where the specialist sits at a fixed graph position.

**Rationale.** The proven shape from `mastersingh24/gke-agent` ([`./specialists-design.md`](./specialists-design.md) line 17). Same mechanism gives mast a uniform way to expose specialists, skills, and remote A2A agents to the parent's reasoning without inventing three separate tool contracts.

## HITL primitives

**What v2 provides.** HITL is delivered via session events, not workflow wrapping. Emit an interrupt event; the runtime pauses; a resume call feeds a validated response back into the same session.

Constructs the design corpus names:

- `RequestInputEvent` — the interrupt event shape ([`./durable-execution-design.md`](./durable-execution-design.md) "Substrate": "one interrupt shape; the underlying pause primitive is more general").
- `workflow.NewRequestInputEvent` — constructor used by specialists ([`./specialists-design.md`](./specialists-design.md) composition-table HITL row).
- `session.RequestInput{InterruptID, Message, ResponseSchema}` — the payload ([`./specialists-design.md`](./specialists-design.md) same row).
- `ResponseSchema` — drives `mast-web` form generation ([`./fork-design.md`](./fork-design.md) 2026-07-01 resolved HITL block; [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "With attach mode + mast-web"). *Resolved 2026-07-25 (was open Q #5): the type is `*jsonschema.Schema` from `github.com/google/jsonschema-go` — real JSON Schema, not a Go struct or opaque payload. The engine validates the resume payload against it.*
- Typed errors on resume: `ErrInvalidResumeResponse`, `ErrNothingToResume` ([`./durable-execution-design.md`](./durable-execution-design.md) "Substrate"; [`./library-api-design.md`](./library-api-design.md) open Q #2 wraps them in `mast.*` errors that `errors.Is` correctly).
- `workflow.ResumeOrRequestInput(ctx, emit, req)` — collapses the re-entry pattern into one call: returns the human's reply on re-run, otherwise emits the request and returns `ErrNodeInterrupted`.
- `ctx.ResumedInput(interruptID)` — the re-entry lookup on `agent.Context`.

**Resume wire shape + node re-entry contract (verified 2026-07-25, spike 2).** A resume is a user turn whose `genai.FunctionResponse.ID` equals the pending `InterruptID`, payload under `Response["response"]` (see runner `buildResumeResponses` / `decodeResumeResponse`); the runner reuses the paused run's invocation ID for the resume turn. On resume, dynamic-node bodies **re-execute** (`NodeConfig.RerunOnResume` defaults to true) — and `RunNode` does *not* return cached child results across the pause turn, because dynamic children aren't part of the static graph that `ReconstructRunState` rehydrates. The required body shape is therefore **ResumedInput-first**: check `ctx.ResumedInput(id)` before invoking any child, and stash anything the resume pass needs (e.g. the specialist result awaiting approval) into session state via a `StateDelta` event before interrupting. An unguarded child re-run is worse than wasted cost: the re-invoked LlmAgent fails assembling its request against the resume turn's orphan `FunctionResponse` ("no function call event found for function responses ids"). Verified across process restart: pause → `kill -9` → fresh process on the same SQLite DB → resume completes with stashed state + schema-validated verdict, without re-invoking the specialist.

**Where mast uses it.**

- **First-class on both plain `LlmAgent`s and workflows** ([`./fork-design.md`](./fork-design.md) 2026-07-01 resolved HITL block; [`./specialists-design.md`](./specialists-design.md) composition-table HITL row): "v2 gain: HITL works on plain `LlmAgent`s, not just workflow-wrapped ones." Not delivered by wrapping — delivered by attach mode + `mast-web`.
- **Specialists** ([`./specialists-design.md`](./specialists-design.md) HITL row): change-safety specialist escalates ambiguous approvals to an operator.
- **Reference graphs** ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shapes #4, #5): autonomous loops escalate on ambiguity; adversarial verifier escalates when neither proposer nor skeptic can decide.
- **Planner** ([`./orchestration-design.md`](./orchestration-design.md) `request_operator_input` tool row): planner tool vocabulary exposes HITL directly.
- **Durable execution** ([`./durable-execution-design.md`](./durable-execution-design.md) "HITL pause"): treated as a special case of external-signal pause; `ResponseSchema`-driven form; resume validates against schema.
- **A2A + AG-UI mapping** ([`./a2a-design.md`](./a2a-design.md) task-state table maps `input-required` to `RequestInputEvent`; [`./ag-ui-design.md`](./ag-ui-design.md) protocol-mapping table maps `RequestInputEvent` to `RunFinished{outcome: interrupt}`): same primitive; different wire protocols.
- **Cross-runtime resume future-preserved** ([`./durable-execution-design.md`](./durable-execution-design.md) "Cross-runtime resume"; [`./fork-design.md`](./fork-design.md) 2026-07-01 HITL block): shared v2 interrupt shape with Python ADK; mast v0.1 doesn't build cross-runtime dispatch but must not break the wire compat.

**Rationale.** HITL as a session event (rather than a workflow-only construct) is what makes it composable with plain `LlmAgent`s, `agenttool`-wrapped specialists, and reference-graph nodes uniformly — one plumbing surface, four consumers.

## Session events and durability

**What v2 provides.** Session state is derived from the event log ([`./durable-execution-design.md`](./durable-execution-design.md) "Session storage requirements"). Paused workflows are reconstructed by scanning session history — "the session event stream *is* the state" (same section). `session.NewEvent(ctx, invocationID)` is the constructor; the event schema carries v2-new fields.

**Persistent session services (verified 2026-07-25 — this changes bucket-2 scope).** v2.1.0 ships `session/database.NewSessionService(gorm.Dialector, ...)` — a GORM-backed `session.Service` covering SQLite (`github.com/glebarez/sqlite`, pure Go, no cgo), Postgres, and anything else GORM dials — plus `session/vertexai`. Call `database.AutoMigrate(svc)` at startup; the service does not self-migrate. Because the runner calls `Workflow.ReconstructRunState` from session history on *every* turn (no in-memory workflow state between turns), restart-survival is free once the store is durable. Implications: the [`./fork-design.md`](./fork-design.md) P1.3 `pkg/eventlog/` port re-scopes to the audit/query/retention surface on top of (or beside) `session/database`, not a bespoke store; and [`./deployment-design.md`](./deployment-design.md)'s Postgres-for-Cloud-Run story is the same `NewSessionService` call with a different dialector, not a v0.2-sized work item.

**Usage metering substrate (verified 2026-07-25).** `session.Event` embeds `model.LLMResponse`, so every model call's `UsageMetadata` (prompt / candidates / total token counts) is visible to anything consuming the runner's event stream, and events carry `Branch` + `NodeInfo` for per-branch attribution. What ADK does **not** provide: pricing and enforcement — both mast-side ([`./orchestration-design.md`](./orchestration-design.md) budget composition). Event-stream metering is enforcement-after-the-call; pre-call gating composes as a `model.LLM` wrapper and/or the v2.1.0 TaskRunner seam for tool fan-out.

**Event fields called out in the fork port table** ([`./fork-design.md`](./fork-design.md) P1.3 `pkg/eventlog/` row):

- `IsolationScope`
- `Output`
- `Routes`
- `RequestedInput`
- `NodeInfo`

**Where mast uses it.**

- **Bucket 2 `pkg/eventlog/` port** ([`./fork-design.md`](./fork-design.md) P1.3): new event fields must persist; `session.NewEvent` signature is an ADK-touching change.
- **Attach mode** ([`./fork-design.md`](./fork-design.md) P1.3 `pkg/attach/` row): emits v2 event shape; exposes `RequestInputEvent` schema to `mast-web`. TUI compat considerations for the enriched fields spelled out in the "core-agent-tui disposition" resolved block.
- **Bucket 1 lean core** ([`./fork-design.md`](./fork-design.md) P1.2): "Event-stream plumbing: session events emitted uniformly regardless of node vs. agent execution."
- **Durable execution substrate** ([`./durable-execution-design.md`](./durable-execution-design.md) "Substrate"): reconstructable pause is what makes session-durable pause/resume work; `IsolationScope()` and `ResumedInput(id string)` are the custom-context primitives that support replay.
- **Cross-runtime compat** ([`./durable-execution-design.md`](./durable-execution-design.md) "Cross-runtime resume"): any mast-added event field must be Python-ADK-deserialize-tolerant.

**Rationale.** Event-log-primary storage means durability is a substrate property, not a mast subsystem. Mast rides on it; the pause/resume surface in [`./durable-execution-design.md`](./durable-execution-design.md) is a thin operator-facing layer over what v2 already persists.

## Unified telemetry span tree

**What v2 provides.** "Node/agent execution shows up in one consistent telemetry span tree" ([`./observability-design.md`](./observability-design.md) "Substrate"). Every operation — `LlmAgent` turn, node execution, tool call, specialist invocation, sub-workflow, HITL request — is a span with the same attribute vocabulary.

**Where mast uses it.**

- **Observability export** ([`./observability-design.md`](./observability-design.md) full doc): mast does not design its own trace shape — v2's spans are exported via OTel with mast-specific attributes (`mast.session.id`, `mast.workload.name`, `mast.tenant.scope`, etc.) layered on top.
- **Specialist visibility in `mast-web`** ([`./specialists-design.md`](./specialists-design.md) open Q #5, updated 2026-07-01): reduces to "which attribute do we filter on to render specialists distinctly" — no correlation between separate span shapes needed.
- **Session-view rendering** ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "With attach mode + mast-web"): `mast-web` can render graph, plain-agent, and specialist execution uniformly.

**Rationale.** One trace shape is the reason mast can defer building a mast-native observability format entirely — the substrate is already export-shaped.

**Dependency-graph note (verified 2026-07-26, slim-embed build):** ADK's model path depends on `google.golang.org/genai`, which **structurally imports the OTel API packages** (`go.opentelemetry.io/otel`, `/trace`, `/metric`, plus the `otelhttp` instrumentation) — that API surface is no-op stubs unless an SDK is installed, and mast cannot shed it without shedding ADK itself. Only the OTel **SDK and exporters** are heavy, and they enter the module graph solely via `pkg/observability`. Consequence: the slim-embed denylist ([`./library-api-design.md`](./library-api-design.md), enforced by `scripts/check-slim-deps.sh`) denies OTel at the SDK/exporter level, not wholesale.

## Cross-cutting: what the ADK-boundary flag catches

Per [`./fork-design.md`](./fork-design.md) sync-discipline section, the concrete shared-infrastructure surface that sits on the v1/v2 boundary (and therefore ports across at adaptation time, not straight cherry-pick):

- Session store — event schema change (`IsolationScope`, `Output`, `Routes`, `RequestedInput`, `NodeInfo`; `session.NewEvent` signature).
- Provider adapters — context type change (unified `agent.Context`).
- Watchdog + signal routing — event-emission surface (watchdog signals reach the model via v2's emitting-function-node pattern per [`./positioning.md`](./positioning.md) priority #2 and [`./durable-execution-design.md`](./durable-execution-design.md) "Watchdog" composition row).
- Custom `InvocationContext` implementations — must add `IsolationScope()` / `ResumedInput()` on the v2 side.

This is the only surface where v1/v2 divergence matters operationally; the ADK-independent shared code (`pkg/permissions/`, `pkg/pricing/`, `pkg/digest/`, most of `pkg/config/`) is unaffected.

## Open questions

Things that surfaced during grep as load-bearing but not fully specified in the corpus. Not to be re-litigated here — flagged so the next pass through the affected companion doc can settle them.

1. ~~**`single_turn` and `task` auto-installed helper tools.**~~ *Resolved 2026-07-25 (spike observations): in the SubAgents dispatch pattern, ADK auto-installs one `task` tool per Task-mode sub-agent and one `single_turn` tool per SingleTurn-mode sub-agent on the coordinator — the coordinator's LLM invokes a sub-agent by calling its tool. `finish_task` is auto-installed on the Task-mode agent itself and appears in `model.LLMRequest.Tools` (a fake/scripted model can key on its presence); the argument passed to `finish_task` becomes the agent's output, delivered as a `finish_task` function_call + function_response pair in the event stream.*
2. **Concrete `agenttool.New` signature under v2.** [`./specialists-design.md`](./specialists-design.md) Related section asserts "signature unchanged from v1" but does not spell it out; the sketch in the implementation-shape section shows only `Register(specs []Spec, parent *agent.Agent) ([]tool.Tool, error)`. Bucket 2's specialist-loader port needs the exact wrapping call. *(2026-07-25 note: the spike used the SubAgents auto-tool pattern and `AgentNode`/`RunNode` composition instead; `agenttool` remains unverified.)*
3. ~~**How `Timeout` and `RetryConfig` are set per node.**~~ *Resolved 2026-07-25: `workflow.NodeConfig{Timeout time.Duration, RetryConfig *RetryConfig, ParallelWorker bool, RerunOnResume *bool, WaitForOutput *bool, EmitsOwnSpan bool}` is the per-node config passed to every node constructor; `DefaultRetryConfig()` returns 5 attempts / 1s→60s / 2× backoff / full jitter; nil `RetryConfig` = no retries.*
4. ~~**`session.Event` vs. `session.NewEvent` distinction.**~~ *Resolved 2026-07-25: `session.Event` is the struct (embeds `model.LLMResponse`; carries `InvocationID`, `Branch`, `IsolationScope`, `Author`, `Actions` (with `StateDelta`), `LongRunningToolIDs`, `Routes`, `RequestedInput`, `Output`, `NodeInfo`); `session.NewEvent(ctx, invocationID)` is the constructor.*
5. ~~**`ResponseSchema` shape.**~~ *Resolved 2026-07-25: `*jsonschema.Schema` (github.com/google/jsonschema-go); the engine validates resume payloads against it. See the HITL section above.*
6. **`agent.Pause` / `agent.Resume` / `PauseSpec`.** Defined in [`./durable-execution-design.md`](./durable-execution-design.md) as mast primitives *on top of* the v2 substrate — deliberately not included in the inventory above because they are mast surface, not v2 surface. Flagged here to prevent future readers from re-cataloging them as v2 constructs.

## v2.1.0 additions relevant to mast (2026-07-25)

v2.1.0 (released 2026-07-23) is the pin for bucket 1; the spike validated against it. Additions that intersect mast's design corpus — each needs an evaluate-before-build pass in its subsystem doc:

- **Name-based model registry** (`Register` / `NewLLM`) + OpenAI support — intersects the [`./positioning.md`](./positioning.md) multi-provider pillar and the bucket-2 `pkg/providers/` port shape: decide whether mast providers register into ADK's registry or wrap around it.
- **Agent-registry package** (REST transport; discovery of agents + MCP servers; `RemoteAgent` / `McpToolset` factories) — overlaps `pkg/a2a/` + federation-adapter plans ([`./a2a-design.md`](./a2a-design.md), [`./federation-design.md`](./federation-design.md)); evaluate before hand-building registry clients.
- **Auth/credential provider package** + per-request MCP auth via `Config.Auth` — candidate substrate for the per-MCP credential-resolution requirement ([`./positioning.md`](./positioning.md) keep-list; [`./mcp-catalog-design.md`](./mcp-catalog-design.md)).
- **TaskRunner seam** (caller-controlled tool fan-out) — candidate interception point for the permission gate and pre-call cost metering.
- Also: `runner.NewInMemory` convenience; parallel-worker items emit `invoke_node` spans (observability); tool-confirmation ordering fixes (HITL-adjacent). The v1 branch remains actively maintained (v1.5.1 shipped 2026-07-22) — relevant to the ADK-boundary sync discipline with core-agent.

Related tooling note: `tool.FilterToolset(ts, tool.AllowedToolsPredicate(names))` narrows a toolset per consumer — per-tool MCP filtering is stock (see [`./specialists-design.md`](./specialists-design.md) allowlists; verified in spike 2).

## Related

- [`./fork-design.md`](./fork-design.md) — the mechanics: bucket 1 rebuilds on v2 primitives fresh; bucket 2 ports adapters with minimal v2-compat changes; the ADK-boundary flag in sibling-sync discipline
- [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) — the reference-graph library that instantiates most of the graph-engine + node-type inventory
- [`./specialists-design.md`](./specialists-design.md) — `agenttool` composition + agent-mode schema field
- [`./durable-execution-design.md`](./durable-execution-design.md) — session-durable pause/resume; custom-context primitives; typed resume errors
- [`./orchestration-design.md`](./orchestration-design.md) — planner as Task-mode `LlmAgent` with reference-graph shapes as tool vocabulary
- [`./observability-design.md`](./observability-design.md) — the unified span tree as the export substrate
- [`./memory-design.md`](./memory-design.md) — state-bound nodes and `state:"<key>"` struct tags
- [`./positioning.md`](./positioning.md) — the strategic frame: v2 changes what mast has to build
- ADK v2 workflow package: `google.golang.org/adk/v2/workflow` (announced 2026, see [Google Developers Blog: Announcing ADK-Go 2.0](https://developers.googleblog.com/announcing-adk-go-20/))
