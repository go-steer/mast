# mast: design docs

Design documentation for `mast` — the agent-infrastructure substrate for unattended, library-embedded, multi-provider workloads. These docs describe the project as it's *currently designed*; the code lives in [`core-agent`](https://github.com/go-steer/core-agent) until the fork executes (per [`./fork-design.md`](./fork-design.md)'s trigger conditions).

## Reading order for someone landing cold

1. **[`./positioning.md`](./positioning.md)** — the thesis. What `mast` is, what it isn't, what gets kept / cut / reshaped from core-agent's surface. Strategy, not implementation.
2. **[`./fork-design.md`](./fork-design.md)** — the mechanics. How the fork actually happens: phasing, trigger conditions, sync discipline under (E)-sibling-products motivation, resolved decisions.
3. **[mast-web's `web-design.md`](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md)** — the operator-facing UI. Why web (not terminal TUI); what we reuse from cogo-wasm2 and what we don't; stack decisions; deployment options.
4. **[`./specialists-design.md`](./specialists-design.md)** — the subagent-as-tool subsystem for mast-authored subagents. Schema, loader shape, composition with existing patterns. Coexists with skills as complementary authoring model (see #16).
5. **[`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)** — the reference-graph library on ADK v2 primitives. Seven canonical shapes (fan-out-fan-in, sequential pipeline, supervisor+workers, autonomous loop, adversarial verifier, map-reduce, LLM-as-router) with domain wiring for mast's audience.
6. **[`./orchestration-design.md`](./orchestration-design.md)** — the unattended orchestration story: workload bundles (declarative operational profiles under `.agents/workloads/*.yaml`), the planner (supervisor-body agent with reference-graph vocabulary), bundle learning (audit-derived refinement), and evaluation + regression harness. One doc, four subsystems, one story arc.
7. **[`./durable-execution-design.md`](./durable-execution-design.md)** — the fourth pillar (unattended + library + multi-provider + durable). Pause/resume beyond HITL: programmatic pause, timed pause, external-signal pause, snapshot+replay, cross-runtime Python-ADK compat constraints.
8. **[`./observability-design.md`](./observability-design.md)** — telemetry as first-class operational surface (traces via OTel, metrics via Prometheus, structured logs, distributed tracing across MCP hops, alert integration back into the agent loop).
9. **[`./library-api-design.md`](./library-api-design.md)** — the public Go import surface for library-embedded consumers; extension points (custom providers, session stores, permission checkers, tools, specialist sources, workload sources, attach transports, observability sinks).
10. **[`./deployment-design.md`](./deployment-design.md)** — production topologies (Cloud Run, GKE, library-embedded, standalone); multi-instance coordination; multi-tenant tenancy; packaging.
11. **[`./memory-design.md`](./memory-design.md)** — mast-consumer view of the shared-memory / audit-derived-memory substrate; keyspace, reducers, state-bound reads, tenancy enforcement.
12. **[`./mcp-catalog-design.md`](./mcp-catalog-design.md)** — position doc + curated wiring templates for the MCP servers mast validates against (gke, prometheus, github, cloud-logging, slack + v0.2 expansion).
13. **[`./config-layout-design.md`](./config-layout-design.md)** — ties together `.agents/` file layout across specialists, workloads, MCP; discovery order; precedence rules; env-var overrides; hot-reload semantics.
14. **[`./a2a-design.md`](./a2a-design.md)** — Agent-to-Agent protocol integration: mast as A2A server (expose workloads as A2A skills) and client (call external A2A agents). Framework integration with Google Agent Registry / Runtime, kagent.
15. **[`./federation-design.md`](./federation-design.md)** — federation as pattern: one mast instance orchestrating N remote agents via multiple protocols (A2A, mast-native, HTTP/RPC). Planner's `invoke_remote_agent` vocabulary; topology archetypes (star / mesh / hierarchical); mast-to-mast handoff with cross-instance session state, HITL, durability.
16. **[`./skills-design.md`](./skills-design.md)** — SKILL.md format support (reinstated 2026-07-01 after GKE + Google teams began publishing skills as first-class artifacts). Coexists with specialists as complementary authoring model — specialists for mast-authored subagents; skills for consumed published templates. Publisher/consumer split, Google Agent Registry integration, policy layering (allowlist intersection, budget caps).
17. **[`./ag-ui-design.md`](./ag-ui-design.md)** — AG-UI protocol integration (agent↔user); the fourth of the four interop surfaces alongside MCP (tools), A2A (agents), and skills (task templates). Mast as AG-UI server for CopilotKit React apps + chat-platform bots (Slack et al. via `@copilotkit/channels-*`); mast as AG-UI client via federation. Interrupt lifecycle (a draft AG-UI extension — labeled bet) maps onto mast's durable pause/resume. All AG-UI ships v0.2 per the 2026-07-25 re-cut.
18. **[`./adk-v2-usage.md`](./adk-v2-usage.md)** — consolidated inventory of the ADK v2 constructs mast leans on (runner + agent modes, unified `agent.Context`, graph engine + node types, `agenttool`, HITL primitives, session events, unified span tree). Cross-references the companion docs where each surface area is decided; reference doc, not a strategy doc. Read before starting Phase 1 bucket 1.
19. **[`./triage-demo-plan.md`](./triage-demo-plan.md)** — v0.1 anchor use case: mast-native reshape of core-agent's GKE triage recipe. Workload bundle + thirteen specialists (eleven per-failure-mode + `SingleTurn` classifier + `change-safety-gate` HITL) + LLM-as-router workflow graph, tying substrate and subsystems together end-to-end against a real platform-team problem. Sanctioned pre-trigger prototyping scope per [`./fork-design.md`](./fork-design.md).

Each doc has a Resolved-decisions section at the bottom listing what's been settled in conversation; the rest is open for discussion.

## Status (2026-07-01)

This repo holds the design corpus for the future `mast` project. The thesis (E) — *sibling products with divergent agendas* — is the current resolved framing: mast = platform-agent product; core-agent = experimentation/integration substrate (cogo-shaped consumers). Both maintained indefinitely.

Current state:

| Repo | Status |
|---|---|
| [`go-steer/mast`](https://github.com/go-steer/mast) (this repo) | Design-corpus-only, pre-fork. Docs land here as drafts evolve. |
| [`go-steer/mast-web`](https://github.com/go-steer/mast-web) | Initialized 2026-06-12 with main scaffolding + four stacked PRs (A/B/C/C+/docs). Operator UI ships independently of the code fork. |
| [`go-steer/core-agent`](https://github.com/go-steer/core-agent) | Holds mast's code until the fork executes. Continues as the experimentation/integration substrate under (E). |

## Fork trigger

Per [`./fork-design.md`](./fork-design.md) (trigger revised 2026-07-26): **Phase 1's rebuild work (P1.1, P1.2, bucket-3 minimum) starts immediately** — it shares no code with core-agent, and the spike-validated `mast-prototype` graduates into it. Only **P1.3 (adapter ports) gates**, on core-agent's three code cleanup milestones closing (*Correctness & durability*, *Security hardening*, *Substrate & API structure* — they churn the exact packages being ported). Issues #158-#160 land in core-agent independently and no longer gate; the shared-memory stack is re-homed as the gate on mast's v0.2+ memory work.

## Resolved decisions cross-reference

A consolidated view (each doc's local resolved section is authoritative for its own scope):

| Decision | Where it's resolved |
|---|---|
| Strategic motivation = (E) sibling products | [`./fork-design.md`](./fork-design.md) |
| Project name = `mast`, repo = `github.com/go-steer/mast` | [`./fork-design.md`](./fork-design.md) |
| ADK dependency stays | [`./fork-design.md`](./fork-design.md) |
| ADK v2 from day one | [`./fork-design.md`](./fork-design.md) |
| Trigger *not* extended for core-agent's v2 migration | [`./fork-design.md`](./fork-design.md) |
| Phase-1 mechanic = rebuild-lean-core, not prune-in-place | [`./fork-design.md`](./fork-design.md) |
| Provenance via per-file attribution headers on bucket-2 ports | [`./fork-design.md`](./fork-design.md) |
| Phase 1 = P1.1-P1.6 (bootstrap, lean core, adapter ports, specialists, smoke, tag) | [`./fork-design.md`](./fork-design.md) |
| In-flight work lands in core-agent first | [`./fork-design.md`](./fork-design.md) |
| ~~Phase 1 trigger = after #158-#161 + shared-memory stack~~ *Revised 2026-07-26: P1.1/P1.2/bucket-3 start immediately; only P1.3 ports gate, on core-agent's three code cleanup milestones; #158-#160 demoted (land independently); shared-memory re-homed to mast's v0.2 memory gate.* | [`./fork-design.md`](./fork-design.md) |
| CI/release infra = independent at start | [`./fork-design.md`](./fork-design.md) |
| Interactive UI = web, not terminal | [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) + [`./positioning.md`](./positioning.md) |
| ~~Skills replaced by specialists subsystem~~ *Reversed 2026-07-01: skills reinstated as first-class consumable; coexist with specialists as complementary authoring model. Rationale: GKE + Google teams publishing skills as first-class artifacts inverted the audience-fit assumption behind the cut.* | [`./skills-design.md`](./skills-design.md) + [`./specialists-design.md`](./specialists-design.md) |
| Task-class profiles shaped by v2 agent modes (Chat/Task/SingleTurn) | [`./fork-design.md`](./fork-design.md) + [`./positioning.md`](./positioning.md) |
| HITL is first-class on both plain `LlmAgent`s and workflows | [`./fork-design.md`](./fork-design.md) + [`./specialists-design.md`](./specialists-design.md) |
| Workflow scaffolding = reference graphs on v2 primitives, not helper packages | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) |
| Small-tier-parent classifier → LLM-as-router with `SingleTurn` LlmAgent | [`./positioning.md`](./positioning.md) + [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) |
| ADK-boundary flag in sibling-sync discipline (v1/v2 API split) | [`./fork-design.md`](./fork-design.md) |
| Unattended dispatch is bundle-driven, not CLI-flag-driven; four resolution paths (explicit / envelope / bundle-selection / classifier-first) | [`./orchestration-design.md`](./orchestration-design.md) |
| Workload bundles under `.agents/workloads/*.yaml` are the operator-authored operational profile | [`./orchestration-design.md`](./orchestration-design.md) |
| Planner shape = supervisor-body with reference-graph shapes as tool vocabulary | [`./orchestration-design.md`](./orchestration-design.md) |
| Public task classes: chat, debug, implement, research, review, orchestrate; SingleTurn is internal, not user-facing | [`./orchestration-design.md`](./orchestration-design.md) |
| Bundle learning is v0.3+; requires audit-derived memory as substrate | [`./orchestration-design.md`](./orchestration-design.md) |
| Durable execution is the fourth pillar (unattended + library + multi-provider + durable) | [`./positioning.md`](./positioning.md) + [`./durable-execution-design.md`](./durable-execution-design.md) |
| Pause/resume beyond HITL: programmatic / timed / external-signal / snapshot+replay | [`./durable-execution-design.md`](./durable-execution-design.md) |
| Python ADK cross-runtime resume is future-preserved, not v0.1 scope | [`./durable-execution-design.md`](./durable-execution-design.md) |
| Observability = OTel traces + Prometheus metrics + structured logs; unified via v2 span tree | [`./observability-design.md`](./observability-design.md) |
| Library API stable-marked packages under `github.com/go-steer/mast/`; extension points at every seam | [`./library-api-design.md`](./library-api-design.md) |
| Four production topologies: Cloud Run, GKE, library-embedded, standalone | [`./deployment-design.md`](./deployment-design.md) |
| Multi-instance coordination via claim-based session ownership + timed-pause scheduler | [`./deployment-design.md`](./deployment-design.md) |
| Memory keyspace: session-scoped, tenant-scoped, global-scoped (with per-tenant opt-in) | [`./memory-design.md`](./memory-design.md) |
| MCP catalog = consume, not build; ship wiring templates + reference workloads | [`./mcp-catalog-design.md`](./mcp-catalog-design.md) |
| `.agents/` discovery order: env > project > user > system; first-match-wins, no cross-location merging | [`./config-layout-design.md`](./config-layout-design.md) |
| A2A support is first-class; mast is both A2A server (expose workloads as skills) and client | [`./a2a-design.md`](./a2a-design.md) |
| A2A integration includes Google Agent Registry / Runtime and kagent as first-order registries | [`./a2a-design.md`](./a2a-design.md) |
| Federation is a pattern distinct from A2A; planner tool vocabulary gains `invoke_remote_agent` | [`./federation-design.md`](./federation-design.md) |
| Three federation protocol adapters: A2A, mast-native, HTTP/RPC | [`./federation-design.md`](./federation-design.md) |
| Mast-native inter-instance protocol offers richer semantics than A2A within trusted fleets (session-state propagation, cross-instance HITL, native durability) | [`./federation-design.md`](./federation-design.md) |
| Federation topologies: star, mesh, hierarchical; hybrids common | [`./federation-design.md`](./federation-design.md) |
| Skills reinstated as first-class consumable (SKILL.md format) — coexist with specialists, complementary authoring models | [`./skills-design.md`](./skills-design.md) + [`./positioning.md`](./positioning.md) + [`./fork-design.md`](./fork-design.md) |
| Specialists vs. skills = authoring model choice (mast-authored subagents vs. consumed published templates); both surface uniformly to planner | [`./skills-design.md`](./skills-design.md) + [`./specialists-design.md`](./specialists-design.md) |
| Skill discovery via A2A registries (Google Agent Registry catalogs both agents + skills) | [`./skills-design.md`](./skills-design.md) + [`./a2a-design.md`](./a2a-design.md) |
| ~~Three curated surfaces: MCP tools, A2A agents, skill templates — complementary not competing~~ *Extended 2026-07-01: four surfaces after AG-UI reinstated the agent↔user corner. MCP (tools), A2A (agents), AG-UI (user-facing surfaces), skills (task templates).* | [`./mcp-catalog-design.md`](./mcp-catalog-design.md) + [`./ag-ui-design.md`](./ag-ui-design.md) |
| AG-UI is first-class alongside attach mode; attach = mast-native rich operator UX (mast-web), AG-UI = ecosystem-standard user-facing (CopilotKit apps + chat-platform bots) | [`./ag-ui-design.md`](./ag-ui-design.md) + [`./positioning.md`](./positioning.md) |
| CopilotKit `@copilotkit/channels-*` gives mast chat-platform integration *when those packages stabilize* (revised 2026-07-25 from "for free" — the earlier `bot-*` names never shipped; Slack adapter published, others in flight); OpenTag is the reference Slack bot pattern | [`./ag-ui-design.md`](./ag-ui-design.md) |
| AG-UI interrupt lifecycle (`RunFinished{interrupt}` + `RunAgentInput.resume`) maps directly onto mast's durable pause/resume — same primitive, different wire protocol | [`./ag-ui-design.md`](./ag-ui-design.md) + [`./durable-execution-design.md`](./durable-execution-design.md) |
| Web UI = thin client over attach mode, not WASM-as-agent | [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) |
| `mast-web` phases A+B+C don't gate on the fork trigger | [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) |
| Docs split: mast-design here, mast-web design in mast-web repo | This README + redirect stubs in core-agent's `docs/mast/` |
| Pre-trigger prototype lives in the standalone `mast-prototype` git repo (tags `spike1`/`spike2`), not an uncommitted scratch worktree | [`./triage-demo-plan.md`](./triage-demo-plan.md) + [`./fork-design.md`](./fork-design.md) |
| Bucket-1 ADK pin = `google.golang.org/adk/v2 v2.1.0` (spike-2 verified) | [`./fork-design.md`](./fork-design.md) + [`./adk-v2-usage.md`](./adk-v2-usage.md) |
| Graphs run as the runner root via `workflowagent.New` — no coordinator required (runner's Chat-mode rule applies to LlmAgent roots only; spike-1 conclusion reversed) | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) + [`./adk-v2-usage.md`](./adk-v2-usage.md) |
| Resume model = reconstruct-and-re-execute (not deterministic replay); node bodies are ResumedInput-first with session-state stash; mutating side effects are at-least-once with declared guards | [`./durable-execution-design.md`](./durable-execution-design.md) + [`./adk-v2-usage.md`](./adk-v2-usage.md) |
| Session store = ADK `session/database` (SQLite pure-Go v0.1; Postgres = same service, pulled forward where topology demands); `pkg/eventlog/` port re-scoped to audit/query surface; shared-FS SQLite dropped | [`./durable-execution-design.md`](./durable-execution-design.md) + [`./fork-design.md`](./fork-design.md) |
| v0.1 reference-graph subset = LLM-as-router + fan-out-fan-in; HITL cannot originate inside parallel branches (`ErrParallelHITLUnsupported`) | [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) |
| Tool-allowlist algebra = per-field presence (absent = inherit, empty = deny, non-empty = whitelist), one normative table; per-tool MCP filtering is stock ADK `FilterToolset` | [`./specialists-design.md`](./specialists-design.md) |
| Budget substrate = event-stream `UsageMetadata` (+ `Branch`/`NodeInfo` attribution); pricing + enforcement are mast-side; post-call meter v0.1, pre-call gate follow-on | [`./orchestration-design.md`](./orchestration-design.md) + [`./adk-v2-usage.md`](./adk-v2-usage.md) |
| Triage-demo sessions are per-incident (`incident-<uid>`), not shared-session | [`./triage-demo-plan.md`](./triage-demo-plan.md) |
| v0.1 scope re-cut (2026-07-25): A2A server, AG-UI (server + client), and registry publishing move to v0.2; v0.1 keeps MCP templates, `federation.Adapter` interface + `invoke_remote_agent` stub, synchronous A2A client; Phase 1 is honestly ~3-4 weeks | [`./fork-design.md`](./fork-design.md) |
| A2A integration targets the v0.3/1.0 spec (JSON-RPC `message/send` surface; no skill schemas in `AgentSkill`; bundle I/O schemas are a mast-side convention) | [`./a2a-design.md`](./a2a-design.md) |
| One planner tool for remote agents: `invoke_remote_agent(reference, inputs)`; `invoke_a2a_agent` retired | [`./federation-design.md`](./federation-design.md) + [`./a2a-design.md`](./a2a-design.md) |
| AG-UI interrupts/activity/reasoning are draft spec extensions — a labeled bet (SDK pinned, encoding isolated in `pkg/agui`); chat-platform packages are `@copilotkit/channels-*`; push + reconnect-resume are mast extensions | [`./ag-ui-design.md`](./ag-ui-design.md) |
| "Four interop surfaces" is the canonical framing (MCP / A2A / AG-UI / skills); attach mode is mast-native transport, not a surface | [`./mcp-catalog-design.md`](./mcp-catalog-design.md) + [`./ag-ui-design.md`](./ag-ui-design.md) |
| v0.1 MCP catalog entries require one pinned upstream each (gke = official Google GKE MCP endpoint); unpinnable entries demote to v0.2-candidate | [`./mcp-catalog-design.md`](./mcp-catalog-design.md) |
| Cloud Run v0.1 uses Postgres via ADK `session/database`; GKE-with-SQLite is StatefulSet+PVC | [`./deployment-design.md`](./deployment-design.md) |
| Library API v0.1 semver freeze = five pillar packages (`mast`, `agent`, `session`, `provider`, `tool`); all else experimental with named stabilization versions; `memory` experimental v0.1 → stable v0.3 | [`./library-api-design.md`](./library-api-design.md) |
| Budget/policy-critical memory keys are fail-closed; incremental reducers carry persisted event cursors (at-least-once safety) | [`./memory-design.md`](./memory-design.md) |
| Classifier-first dispatch is a privilege boundary: code-enforced per-entry-point bundle allowlists; envelope selection validated + transport-authenticated; resolutions audit-logged | [`./orchestration-design.md`](./orchestration-design.md) |
| Mutation predicate for `on_mutation`: built-in `Mutating` annotation + MCP `readOnlyHint`, default-deny-unknown, per-tool audited override | [`./orchestration-design.md`](./orchestration-design.md) |
| Resume tokens: tenant-scope-bound, permission gate re-runs on resume, TTL 7d with audited override | [`./durable-execution-design.md`](./durable-execution-design.md) |
| Smaller-agents strategy: forkable standalone starters + tested slim-embed guarantee + honest routing to raw ADK — no product tier below mast, no prebuilt single-purpose binaries | [`./positioning.md`](./positioning.md) + [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) + [`./library-api-design.md`](./library-api-design.md) |
| v0.1 anchor use case = mast-native reshape of GKE triage (specialists + LLM-as-router + workflow graph + in-band HITL); showcase mast differentiators against a real platform-team problem, not prove parity with the core-agent recipe | [`./triage-demo-plan.md`](./triage-demo-plan.md) |

## Open questions still on the table

Each doc has its own open-questions section. The strategic ones (cross-doc impact):

- **AX boundary audit** ([`./positioning.md`](./positioning.md), [`./fork-design.md`](./fork-design.md)). Some of what core-agent does today (background agents? cross-process inbox?) may belong up at AX. Needs a dedicated audit.
- **core-agent's own positioning rewrite** ([`./fork-design.md`](./fork-design.md)). Under (E), core-agent needs its own positioning doc — sibling to [`./positioning.md`](./positioning.md) — written for its actual audience (experimentation/integration/cogo).
- ~~**Small-tier-parent classifier aging**~~ *Resolved 2026-07-01: LLM-as-router with a `SingleTurn` LlmAgent replaces the substring matcher and ages gracefully. See [`./positioning.md`](./positioning.md) open Q #4 update and [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)'s LLM-as-router section.*
- **Canonical positioning name** ([`./positioning.md`](./positioning.md)). "Agent infrastructure" / "platform agent runtime" / "agent substrate" — pick one and use consistently in the README sweep.
- ~~**Task-class name for `SingleTurn` mode**~~ *Resolved 2026-07-01: SingleTurn is internal, not user-facing. Public task classes remain chat/debug/implement/research/review + new `orchestrate` (planner-enabled workloads). See [`./orchestration-design.md`](./orchestration-design.md) task-class-resolution section.*
- **Plan-first gate interaction with workflow graphs and planner** ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) open Q #5; [`./orchestration-design.md`](./orchestration-design.md) treats the planner case). Prose plan (`plan_it_out`) satisfies gate + composes with `plan_review_required` HITL; broader "does plan-first reach into pre-authored graphs" still open.

The doc-local open questions (specialist-API details for `agenttool`, sync discipline edge cases, per-shape testing conventions, etc.) live in each doc and don't affect the others.
