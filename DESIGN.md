# mast — architecture (v0.1)

**Status:** current as of v0.1.0 (2026-07-30). This is the map of what
actually ships — the working architecture for contributors and
embedders. The *why* behind each subsystem lives in the design corpus
under [`docs/`](./docs/README.md) (start with
[`docs/positioning.md`](./docs/positioning.md) for the thesis and
[`docs/fork-design.md`](./docs/fork-design.md) for the fork mechanics);
the resolved-decisions table in [`docs/README.md`](./docs/README.md)
is the index of settled questions.

## Shape of the system

mast is a substrate for agent workloads that run **unattended**: the
same engine is consumable as a Go library and as a standalone binary,
and every subsystem assumes nobody is watching in real time —
durability, budgets, audit, and the operator surface are load-bearing,
not add-ons.

```
                    ┌────────────────────────────────────────────────┐
 envelopes ────────▶│  inject HTTP (dispatch / resume / abort)       │
 (webhooks, queues) │                                                │
                    │   workload bundle ──▶ root agent               │
 operators ────────▶│   (.agents/ or       (graph or SubAgents       │
 (mast-web, curl,   │    programmatic)      dispatch, specialists     │
  attach clients)   │                       as tools)                │
                    │                                                │
                    │   ADK v2 runner ──▶ session store (SQLite/     │
                    │   (span tree,       Postgres via ADK           │
                    │    HITL pause)      session/database)          │
                    │                       └─ eventlog overlay      │
                    │                          (seq + Watch/Since)   │
                    │                                                │
                    │   attach HTTP/SSE (list, tail, inject, wake,   │
                    │   interrupt, capabilities, agent card)         │
                    └────────────────────────────────────────────────┘
```

The substrate is **ADK v2** (`google.golang.org/adk/v2`): mast uses its
agent modes (Task / SingleTurn / Chat), workflow graph engine, runner,
unified span tree, and `session/database` persistence directly rather
than wrapping them. [`docs/adk-v2-usage.md`](./docs/adk-v2-usage.md)
records the verified substrate behavior mast relies on;
[`docs/spike-findings.md`](./docs/spike-findings.md) records the
resume contract and allowlist semantics that are verified behavior,
not suggestions.

## Consumer shapes

Two first-class shapes, same subsystems ([`docs/library-api-design.md`](./docs/library-api-design.md)):

- **Library.** The root package: `mast.Run` (instruction + input),
  `mast.RunWorkload` (programmatic bundle + specialists),
  `mast.ListSessions` / `mast.ResumeSession` (operator surface). A
  CI-enforced **slim-embed guarantee** (reference consumer
  `examples/deploy/slim` + denylist script) keeps the minimal import
  path free of heavyweight deps — pay for what you import.
- **Binary.** `cmd/mast`: serve mode (workload daemon with inject +
  attach + metrics listeners), one-shot mode (`mast --task=<class>
  "<prompt>"`), and the `mast sessions` operator CLI.

Semver stability from v0.1 is reserved for the five packages the
pillars stand on (the root `mast` package, `agent`, `transcript`, and the
provider and tool interfaces); everything else is experimental until
the version named in the library API design's import-surface table.

## Package map

**Dispatch + orchestration** —
[`docs/workflow-scaffolding-design.md`](./docs/workflow-scaffolding-design.md),
[`docs/orchestration-design.md`](./docs/orchestration-design.md)

| Package | Role |
|---|---|
| `pkg/agent` | Agent-mode constructors over ADK (coordinator, Task, SingleTurn) + per-mode default instructions; echo/scripted fake models for offline smoke. |
| `pkg/graph` | Workflow-graph dispatch (LLM-as-router over ADK's workflow engine). |
| `pkg/router` | LLM-as-router classifier (SingleTurn) used by graph dispatch. |
| `pkg/specialists` | Subagent-as-tool: `.tmpl` files (YAML frontmatter) with budgets, model overrides, tool allowlists ([`docs/specialists-design.md`](./docs/specialists-design.md)). |
| `pkg/workload` | Workload bundles: declarative YAML naming specialists, tool catalog, budgets, HITL policy. |
| `pkg/planner` | Supervisor-body planner scaffold (`plan`/`finish_plan`; `invoke_remote_agent` composes here). |
| `pkg/envelope` | Inject payloads — the unattended entry-point contract. |
| `pkg/config` | `.agents/` discovery (workloads, specialists, MCP refs, A2A registrations) ([`docs/config-layout-design.md`](./docs/config-layout-design.md)). |

**Durability + governance** —
[`docs/durable-execution-design.md`](./docs/durable-execution-design.md)

| Package | Role |
|---|---|
| `pkg/transcript` | Operator surface over the ADK session store: list/show summaries, pending-interrupt scan, durable abort markers. (Named `session` pre-v0.1.0; renamed to end the alias collision with ADK's `session`.) |
| `pkg/eventlog` | Seq-overlay + `Since`/`Watch` stream + audit metadata sidecar layered **on** ADK `session/database` (ADK owns the tables), plus `GuardrailStore` — a mast-owned append-only log of guardrail trips and resets, folded forward so an `enforce` halt outlives the process that observed it. Ported from core-agent. |
| `pkg/budget` | Turn/cost metering folded from event usage; trips cancel the run context. |
| `pkg/permissions` | Permission gate + prompt contract (ported; deliberately not runtime-wired in v0.1 — the package doc records the wiring-time inputs). |
| `pkg/auth` | Caller identity, session ACL types, bearer/mTLS config (ported). |
| `pkg/watchdog` | Loop signals (repeated call, alternating cycle, tool-failure streak) + session-event bridge + the `warn`/`feedback`/`enforce` posture ladder; alerts are logged, projected onto the guardrail surface, and — from `feedback` up — routed into the model's own next prompt. The posture resolves `--watchdog` > the bundle's `safety.watchdog` > `watchdog.DefaultMode` (`feedback`), and every turn-driving surface taps it, the library embed included. Under `--attach-listen` a halt is persisted through `eventlog.GuardrailStore` and adopted on the next turn after a restart — configuration still wins, so a posture dialed back below `enforce` inherits nothing. |

**Providers** — reshaped at port time (per-provider Options structs,
no registry; dispatch is an explicit switch in `internal/compose`)

| Package | Role |
|---|---|
| `pkg/providers/gemini` | Built-in-tool wrapper (search grounding, URL context), Vertex context-cache stamping, per-request built-in gating for models that reject mixed tools. |
| `pkg/providers/anthropic` | First-party + Vertex backends; thinking-block round-trip, prompt-cache usage fold, draft-2020-12 schema normalization. |
| `pkg/providers/vertexcache` | Vertex context-cache manager (public so compose and embedders can wire hooks). |
| `pkg/providers/mock` | Scripted JSONL replay for tests and offline demos. |
| `pkg/taskclass` / `pkg/modeltier` / `pkg/pricing` | Task-class profiles → model-tier defaults → catalog pricing for the budget meter. |
| `pkg/instruction` / `pkg/digest` | Instruction assembly; transcript digesting (eventlog-store variant descoped at port). |

**Operator + interop surfaces**

| Package | Role |
|---|---|
| `pkg/attach` | The mast-native operator transport (HTTP/SSE): session registry + resume gating, seq'd replay + live tail, inject/wake/interrupt, capabilities frames, agent card, prompt broker, peer registry, rate limiting. Ported from core-agent; wire-compatible with it (mast-web serves both). |
| `pkg/attachadapter` | Bridges the runner-driven daemon into attach's `Registrant` contract: one injected message = one serialized turn; typed operator events in wire order; interrupt cancels the turn context. |
| `pkg/inject` | The unattended entry point: dispatch/resume/abort HTTP + `/metrics`. |
| `pkg/observability` | Fixed Prometheus counter registry + env-gated OTel trace export ([`docs/observability-design.md`](./docs/observability-design.md)). |
| `pkg/a2a` / `pkg/federation` | Synchronous A2A v0.3 client; frozen `federation.Adapter`/`Handle` interface + `invoke_remote_agent` ([`docs/a2a-design.md`](./docs/a2a-design.md), [`docs/federation-design.md`](./docs/federation-design.md)). |
| `pkg/mcp` | MCP toolset wiring + per-specialist tool allowlists. |

**Internal:** `internal/compose` (model/backend dispatch, shared
one-shot construction), `internal/version` (ldflags-injected build
identity, reported by `--version` and the attach capabilities frame).

## Key contracts worth knowing before changing anything

- **Sessions are event logs.** State derives from the append-only
  event history (ADK reconstructs run state from it every turn), which
  is why restart-survival is free once the store is durable, and why
  the eventlog overlay (seq + watch) is the audit/tail surface rather
  than a second store.
- **One session service instance per store.** ADK's `AppendEvent`
  type-asserts its own session type — every writer must go through the
  same service instance (the daemon owns it; the abort path routes
  through the daemon for exactly this reason).
- **Budgets act by cancellation.** The meter folds usage from the
  event stream and trips by canceling the run context — subsystems
  must tolerate mid-turn cancellation.
- **Attach is wire-compatible with core-agent.** The protocol
  (v1.4.0) is the contract; mast-web and any attach client work
  against both. Divergence is a bug on whichever side left the
  documented shape.
- **Ports carry provenance.** Adapter packages derive from
  core-agent at per-stage pinned SHAs (`83ec0713` / `b8dd225e` /
  `25d8531c`), one derivation header per file. Shared-infrastructure
  fixes land wherever found first, then port within a week
  ([`docs/fork-design.md`](./docs/fork-design.md) sync discipline).

## Deliberately not in v0.1

Deferrals are decisions ([`AGENTS.md`](./AGENTS.md) house rule #7);
the owning doc names the version that lifts each one. Highlights:
A2A **server** + registry publishing, AG-UI (both v0.2,
[`docs/a2a-design.md`](./docs/a2a-design.md) /
[`docs/ag-ui-design.md`](./docs/ag-ui-design.md)); skills consumption
([`docs/skills-design.md`](./docs/skills-design.md)); audit-derived
memory ([`docs/memory-design.md`](./docs/memory-design.md), gated on
core-agent's shared-memory stack); multi-session attach (ACL store,
per-caller auth, operator session creation); permission-gate runtime
wiring; programmatic pause / resume tokens
([`docs/durable-execution-design.md`](./docs/durable-execution-design.md));
OTel metrics export (Prometheus scrape only in v0.1).
