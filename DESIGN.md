# mast — architecture (v0.4)

**Status:** current as of v0.4.0 (2026-08-17). This is the map of what
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
 envelopes ────────▶│  inject HTTP (dispatch / resume / abort,       │
 (webhooks, queues) │  pause / stop / ack-effects)                   │
                    │                                                │
 schedules ────────▶│  scheduled fires (durable anchor; a missed     │
 (bundle scheduled) │  tick is skipped, one session per fire)        │
                    │                                                │
 operators ────────▶│   workload bundle ──▶ root agent               │
 (mast-web, curl,   │   (.agents/ or        (coordinator / graph /   │
  attach clients)   │    programmatic)       fanout / bounded;       │
                    │                        specialists as tools)   │
                    │                                                │
                    │   write gate ──▶ operator verdict              │
                    │   (a mutating call parks; approve / reject /   │
                    │    edit, durable across a restart)             │
                    │                                                │
                    │   ADK v2 runner ──▶ session store (SQLite/     │
                    │   (span tree,        Postgres via ADK          │
                    │    HITL pause)       session/database)         │
                    │                        └─ eventlog overlay     │
                    │                           (seq + Watch/Since)  │
                    │                                                │
                    │   attach HTTP/SSE · A2A · AG-UI (list, tail,   │
                    │   inject, wake, interrupt, guardrail reset)    │
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
  `mast.ListSessions` / `mast.ResumeSession` / `mast.ResumeByToken`
  (operator surface), `mast.Pause` and `mast.AckEffects` (durability
  controls — the programmatic pause and the ambiguous-effect ack). A
  CI-enforced **slim-embed guarantee** (reference consumer
  `examples/deploy/slim` + denylist script) keeps the minimal import
  path free of heavyweight deps — pay for what you import.
- **Binary.** `cmd/mast`: serve mode (workload daemon with inject +
  attach + A2A + AG-UI + metrics listeners), one-shot mode, and the
  `mast sessions` / `mast stop` operator CLIs. Serve and one-shot are
  the *same* invocation distinguished by a positional prompt — `mast
  --workload=<name> …` serves, `mast --task=<class> "<prompt>"` runs
  one turn and exits. There is no `serve` subcommand; only `sessions`
  and `stop` are subcommands (`cmd/mast/main.go`).

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
| `pkg/graph` | Workflow-graph dispatch (LLM-as-router over ADK's workflow engine) and the `fanout` shape — concurrent read-only branches on `parallelagent` (never `ParallelWorker`, whose branch events the log suppresses) with one `_synthesis` merge. |
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
| `pkg/transcript` | Operator surface over the ADK session store: list/show summaries, pending-interrupt scan, durable abort/pause markers, resume-token records, and the durable decision records (`approve`/`reject`/`edit`) that `mast sessions export-decisions` writes out as JSONL. (Named `session` pre-v0.1.0; renamed to end the alias collision with ADK's `session`.) |
| `pkg/eventlog` | Seq-overlay + `Since`/`Watch` stream + audit metadata sidecar layered **on** ADK `session/database` (ADK owns the tables), plus two mast-owned append-only logs folded forward across restarts: `GuardrailStore` (trips and resets, so an `enforce` halt outlives the process that observed it) and `SpendStore` (one row per priced model call, so a cost ceiling does too). Ported from core-agent. |
| `pkg/budget` | Turn/cost metering folded from event usage; trips cancel the run context. Metering stays in memory and database-free; durability is a two-part seam — `Config.OnSpend` writes each priced call out, `Meter.Restore` folds a previous process's spend back in — which `cmd/mast` wires to `eventlog.SpendStore`. |
| `pkg/effects` | The recorded-effect outbox: the session event log **is** the outbox (durable `FunctionCall` = intent, paired `FunctionResponse` = completion), read once per turn in an ADK runner plugin's `BeforeRun`. A dangling mutating intent puts the turn in fail-closed ambiguous-effect mode until an operator acks. |
| `pkg/approval` | The write gate: the runner-plugin seam where a mutating call parks for an operator, the three-valued verdict (`approve`/`reject`/`edit`), the typed change-set producer, and exact-`(tool, arguments)`-signature grants with their freshness re-read. Policy stays in `pkg/permissions`; the durable pause is ADK's tool-confirmation flow. |
| `pkg/permissions` | Permission gate + prompt contract (ported). Runtime-wired since v0.3 through `pkg/approval`'s plugin — it decides policy (proceed / ask / refuse) and stays ADK-independent. |
| `pkg/auth` | Caller identity, session ACL types, bearer/mTLS config (ported). Approvals and edits are recorded against the authenticated approver it resolves. |
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
| `pkg/attach` | The mast-native operator transport (HTTP/SSE): session registry + resume gating, seq'd replay + live tail, inject/wake/interrupt, capabilities frames, agent card, prompt broker, peer registry (optionally durable across hub restarts), rate limiting, and the guardrail surface (`GET`/`POST /sessions/{id}/guardrails[/reset]`) an `enforce` halt is cleared through. Ported from core-agent; wire-compatible with it (mast-web serves both). |
| `pkg/attachadapter` | Bridges the runner-driven daemon into attach's `Registrant` contract: one injected message = one serialized turn; typed operator events in wire order; interrupt cancels the turn context. |
| `pkg/inject` | The unattended entry point: `POST /inject`, `/resume`, `/abort`, `/pause`, `/extend-token`, `/stop`, `/ack-effects`, plus `/metrics`. |
| `pkg/observability` | Fixed Prometheus counter registry + env-gated OTel trace export ([`docs/observability-design.md`](./docs/observability-design.md)). |
| `pkg/a2a` / `pkg/federation` | A2A v0.3 both ways: the synchronous client, and the server (agent card, `message/send`·`tasks/get`·`tasks/cancel`·`message/stream` over SSE) exposing workloads that opt in via the bundle's `a2a.expose`. Plus the frozen `federation.Adapter`/`Handle` interface + `invoke_remote_agent` ([`docs/a2a-design.md`](./docs/a2a-design.md), [`docs/federation-design.md`](./docs/federation-design.md)). |
| `pkg/agui` | The AG-UI server surface (agent↔user) for CopilotKit apps and chat-platform bots: hand-rolled zero-dep wire types, an HTTP+SSE run endpoint, `/agui/agents.json` discovery, and the HITL interrupt/resume lifecycle. Runtime-free, like `pkg/a2a` ([`docs/ag-ui-design.md`](./docs/ag-ui-design.md)). |
| `pkg/serverauth` | The request-admission seams both network servers share: pluggable bearer auth (`TokenValidator` → `Principal`, per-surface scope checks) and rate limiting. Stdlib + `golang.org/x/time` only, so it stays slim-embed-safe. |
| `pkg/mcp` | MCP toolset wiring + per-specialist tool allowlists. HTTP servers get their transport wrapped so a 4xx/5xx carries the server's own error text (an IAM permission name, a quota metric) rather than a bare status line. |

**Internal:** `internal/compose` (model/backend dispatch, shared
one-shot construction, the `bounded` single-node build),
`internal/evals` (the deterministic eval suite and the judged
nightly's scoring, including the tiered-cost check),
`internal/version` (ldflags-injected build identity, reported by
`--version` and the attach capabilities frame), `internal/toolcatalog`
(the tool declarations a real turn puts in front of a model, captured
by driving two agent rigs through an ADK runner, plus the shared
invariant every provider adapter's wire test is held to — see
`docs/model-support-design.md` R2/R8). The scheduled-trigger
loop is daemon-side in `cmd/mast/schedtrigger.go`, reading the
bundle's `scheduled:` section.

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
- **Two runner plugins bracket every tool call, in this order.**
  `pkg/effects` (the outbox) registers first, `pkg/approval` (the
  write gate) second, so a call replayed after a crash is answered
  from the outbox instead of asking an operator to re-approve a
  mutation that already fired. Reordering them is a correctness bug,
  not a preference.
- **Unknown tools count as mutating.** The mutation predicate is
  default-deny: a tool nothing has classified is gated, so a bundle
  cannot get write access by omission. Un-gating is an audited
  per-tool `tool_catalog.tools[].mutating` override.
- **Nothing mutating runs in a parallel branch.** Fan-out branches all
  run *before* the single post-synthesis approval gate, and a branch's
  `Output` payload is its only durable record — so mutating tools (and
  `request_operator_input`) are refused in a branch's allowlist at
  construction.
- **Attach is wire-compatible with core-agent.** The protocol shape is
  the contract; mast-web and any attach client work against both.
  Divergence in *shape* is a bug on whichever side left the documented
  form. The version *numbers* are a different matter and have already
  diverged — mast is on v1.5.0, core-agent on v1.8.0, and the same
  number does not name the same feature set on both. Feature-detect
  against the capabilities frame's `event_types` / `features`, never
  against the version alone.
- **Ports carry provenance.** Adapter packages derive from
  core-agent at per-stage pinned SHAs (`83ec0713` / `b8dd225e` /
  `25d8531c`), one derivation header per file. Shared-infrastructure
  fixes land wherever found first, then port within a week
  ([`docs/fork-design.md`](./docs/fork-design.md) sync discipline).

## Deliberately not in v0.4

Deferrals are decisions ([`AGENTS.md`](./AGENTS.md) house rule #7);
the owning doc names the version that lifts each one, and the
[roadmap](https://go-steer.github.io/mast/roadmap/) is the
user-facing view. Highlights: cross-run finding state and the chat
egress half of unattended monitoring (v0.5, and deliberately owned by
[`k8s-lookout`](https://github.com/go-steer/k8s-lookout) and
switchboard rather than by mast); pre-call budget gating (today a
ceiling is crossed by the call that reports it); the remaining AG-UI
slices (`agui://` federation client, per-key `StateDelta`, webhook
push, client-declared tools,
[`docs/ag-ui-design.md`](./docs/ag-ui-design.md)); the `run_shape_*`
planner vocabulary wired to the reference-graph library (it returns
`not_implemented` in the shipped scaffold); multi-session attach (ACL
store, per-caller auth, operator session creation) and `mode:
multi_session` bundles; skills consumption
([`docs/skills-design.md`](./docs/skills-design.md)); audit-derived
memory ([`docs/memory-design.md`](./docs/memory-design.md), gated on
core-agent's shared-memory stack); OTel *metrics* export (Prometheus
scrape + OTel traces only). Providers beyond Gemini and Claude are a
proposal, not a plan —
[`docs/model-support-design.md`](./docs/model-support-design.md)
targets v0.5+ and nothing in it is settled.
