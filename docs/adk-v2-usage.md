# mast ADK v2 usage: inventory

**Status:** draft, 2026-07-16. Consolidated cross-reference for the ADK v2 constructs mast leans on. Companion to [`./fork-design.md`](./fork-design.md) (resolves ADK v2 from day one; the "Recommended approach" and 2026-07-01 resolved blocks are the densest v2 material), [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) (reference-graph library expressed on v2 primitives), [`./specialists-design.md`](./specialists-design.md) (subagent-as-tool via `agenttool` + agent modes), [`./durable-execution-design.md`](./durable-execution-design.md) (session-durable pause/resume substrate), and [`./orchestration-design.md`](./orchestration-design.md) (planner tool vocabulary on top of the same primitives). Not a strategy doc — this is a reference: which v2 surface areas mast consumes, where each surface shows up, and why.

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
| `workflow.NewJoinNode` | Fan-in aggregation | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shapes #1, #5, #6 |
| `workflow.NewParallelWorkerNode` / `NewParallelWorkers` | Parallel workers over homogeneous input | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shapes #1, #6 |
| `workflow.NewToolNode` | Tool node | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #2 |
| `workflow.NewWorkflowNode` | Sub-workflow embedded as a single node (composition) | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "Composition + reuse" |
| `workflow.NewEdgeBuilder` with `AddFanOut`, `AddFanOutDynamic`, `AddFanIn`, `AddRoutes` | Edge/route wiring | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shapes #1, #2, #4, #6, #7 |
| `workflow.RunNode[T]` | Dynamic sub-run invocation (generic on result type) | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #3; [`./fork-design.md`](./fork-design.md) 2026-07-01 note ("`task`-mode agents can't be used as static graph nodes — workflow-scaffolding examples use dynamic nodes (`RunNode[T]`) for Task-mode sub-agents") |
| `workflow.StringRoute`, `workflow.Default` | Routing primitives | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #7 |

**Run options** (passed to `RunNode`, parallel workers, etc.):

- `WithIsolationScope(scopeID)` — segregates history per scope. Multi-tenant supervisor+workers is the primary use case ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #3 and "With isolation"); [`./orchestration-design.md`](./orchestration-design.md) `isolation.scope` bundle field maps here; [`./memory-design.md`](./memory-design.md), [`./deployment-design.md`](./deployment-design.md), [`./a2a-design.md`](./a2a-design.md), [`./skills-design.md`](./skills-design.md), [`./ag-ui-design.md`](./ag-ui-design.md) all propagate tenant scope through it.
- `WithUseSubBranch(true)` — isolates a child's context from its parent (the specialist-isolation primitive when a specialist is invoked as an agent node; see [`./specialists-design.md`](./specialists-design.md) composition table and [`./orchestration-design.md`](./orchestration-design.md) `invoke_specialist` tool row).
- `WithMaxConcurrency(n)` — graph-wide concurrency cap; critical for fan-out-fan-in and map-reduce ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "With watchdog + cost ceilings").

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
- `ResponseSchema` — drives `mast-web` form generation ([`./fork-design.md`](./fork-design.md) 2026-07-01 resolved HITL block; [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "With attach mode + mast-web").
- Typed errors on resume: `ErrInvalidResumeResponse`, `ErrNothingToResume` ([`./durable-execution-design.md`](./durable-execution-design.md) "Substrate"; [`./library-api-design.md`](./library-api-design.md) open Q #2 wraps them in `mast.*` errors that `errors.Is` correctly).

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

**What v2 provides.** Session state is derived from the event log ([`./durable-execution-design.md`](./durable-execution-design.md) "Session storage requirements"). Paused workflows are reconstructed by scanning session history — "the session event stream *is* the state" (same section). `session.NewEvent` is the constructor; the event schema carries v2-new fields.

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

## Cross-cutting: what the ADK-boundary flag catches

Per [`./fork-design.md`](./fork-design.md) sync-discipline section, the concrete shared-infrastructure surface that sits on the v1/v2 boundary (and therefore ports across at adaptation time, not straight cherry-pick):

- Session store — event schema change (`IsolationScope`, `Output`, `Routes`, `RequestedInput`, `NodeInfo`; `session.NewEvent` signature).
- Provider adapters — context type change (unified `agent.Context`).
- Watchdog + signal routing — event-emission surface (watchdog signals reach the model via v2's emitting-function-node pattern per [`./positioning.md`](./positioning.md) priority #2 and [`./durable-execution-design.md`](./durable-execution-design.md) "Watchdog" composition row).
- Custom `InvocationContext` implementations — must add `IsolationScope()` / `ResumedInput()` on the v2 side.

This is the only surface where v1/v2 divergence matters operationally; the ADK-independent shared code (`pkg/permissions/`, `pkg/pricing/`, `pkg/digest/`, most of `pkg/config/`) is unaffected.

## Open questions

Things that surfaced during grep as load-bearing but not fully specified in the corpus. Not to be re-litigated here — flagged so the next pass through the affected companion doc can settle them.

1. **`single_turn` and `task` auto-installed helper tools.** [`./fork-design.md`](./fork-design.md) 2026-07-01 resolved block names them alongside `finish_task` ("`finish_task`, `single_turn`, `task` replace prompt-engineering we would otherwise have to do to signal completion") but the corpus doesn't explain their contract — when they're auto-installed, what arguments they take, how they interact with Task vs. SingleTurn modes. Bucket 1's mode-wiring code needs a concrete answer.
2. **Concrete `agenttool.New` signature under v2.** [`./specialists-design.md`](./specialists-design.md) Related section asserts "signature unchanged from v1" but does not spell it out; the sketch in the implementation-shape section shows only `Register(specs []Spec, parent *agent.Agent) ([]tool.Tool, error)`. Bucket 2's specialist-loader port needs the exact wrapping call.
3. **How `Timeout` and `RetryConfig` are set per node.** [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "With watchdog + cost ceilings" says per-node `Timeout` and `RetryConfig` bound cost — the shape sketches use a `cfg` argument to `NewFunctionNode(...)` but no doc names the config type or its fields.
4. **`session.Event` vs. `session.NewEvent` distinction.** Constructor is named in [`./fork-design.md`](./fork-design.md) P1.3 port notes; the event struct type is referenced by field name in the same table and used in emitting-node function signatures across [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md), but the corpus doesn't consolidate the two. Likely obvious from the v2 code once bucket 1 starts; note here so the first port doesn't stall on nomenclature.
5. **`ResponseSchema` shape.** Corpus mentions it drives form generation but doesn't specify whether it's JSON Schema, a Go struct type, a v2-specific schema type, or an interface. [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #5 hints at typed shapes (`{approved: bool, note?: string}`); [`./ag-ui-design.md`](./ag-ui-design.md) protocol-mapping table treats it as opaque payload. Bucket 1's HITL plumbing needs a concrete answer before bucket 2's `mast-web` interoperability can be verified.
6. **`agent.Pause` / `agent.Resume` / `PauseSpec`.** Defined in [`./durable-execution-design.md`](./durable-execution-design.md) as mast primitives *on top of* the v2 substrate — deliberately not included in the inventory above because they are mast surface, not v2 surface. Flagged here to prevent future readers from re-cataloging them as v2 constructs.

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
