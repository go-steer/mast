# mast: design docs

Design documentation for `mast` — the agent-infrastructure substrate for unattended, library-embedded, multi-provider workloads. These docs describe the project as it's *currently designed*; the code lives in [`core-agent`](https://github.com/go-steer/core-agent) until the fork executes (per [`./fork-design.md`](./fork-design.md)'s trigger conditions).

## Reading order for someone landing cold

1. **[`./positioning.md`](./positioning.md)** — the thesis. What `mast` is, what it isn't, what gets kept / cut / reshaped from core-agent's surface. Strategy, not implementation.
2. **[`./fork-design.md`](./fork-design.md)** — the mechanics. How the fork actually happens: phasing, trigger conditions, sync discipline under (E)-sibling-products motivation, resolved decisions.
3. **[mast-web's `web-design.md`](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md)** — the operator-facing UI. Why web (not terminal TUI); what we reuse from cogo-wasm2 and what we don't; stack decisions; deployment options.
4. **[`./specialists-design.md`](./specialists-design.md)** — the subagent-as-tool subsystem replacing core-agent's skills. Schema, loader shape, composition with existing patterns.
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

Per [`./fork-design.md`](./fork-design.md), the code fork executes after these in-flight items land in core-agent:

1. Issues [#158-#161](https://github.com/go-steer/core-agent/issues?q=is%3Aissue+158+OR+159+OR+160+OR+161) (bash search-gate, watchdog→model routing, `--task=debug` profile extensions, gemini-3.5-flash probe).
2. The shared-memory stack (PRs #13/14/15 against core-agent).

When both are done, phase 1 of the fork begins (hard-fork-then-prune, single squash commit, per the design doc).

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
| Phase 1 trigger = after #158-#161 + shared-memory stack | [`./fork-design.md`](./fork-design.md) |
| CI/release infra = independent at start | [`./fork-design.md`](./fork-design.md) |
| Interactive UI = web, not terminal | [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) + [`./positioning.md`](./positioning.md) |
| Skills replaced by specialists subsystem | [`./specialists-design.md`](./specialists-design.md) + [`./fork-design.md`](./fork-design.md) |
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
| Web UI = thin client over attach mode, not WASM-as-agent | [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) |
| `mast-web` phases A+B+C don't gate on the fork trigger | [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) |
| Docs split: mast-design here, mast-web design in mast-web repo | This README + redirect stubs in core-agent's `docs/mast/` |

## Open questions still on the table

Each doc has its own open-questions section. The strategic ones (cross-doc impact):

- **AX boundary audit** ([`./positioning.md`](./positioning.md), [`./fork-design.md`](./fork-design.md)). Some of what core-agent does today (background agents? cross-process inbox?) may belong up at AX. Needs a dedicated audit.
- **core-agent's own positioning rewrite** ([`./fork-design.md`](./fork-design.md)). Under (E), core-agent needs its own positioning doc — sibling to [`./positioning.md`](./positioning.md) — written for its actual audience (experimentation/integration/cogo).
- ~~**Small-tier-parent classifier aging**~~ *Resolved 2026-07-01: LLM-as-router with a `SingleTurn` LlmAgent replaces the substring matcher and ages gracefully. See [`./positioning.md`](./positioning.md) open Q #4 update and [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)'s LLM-as-router section.*
- **Canonical positioning name** ([`./positioning.md`](./positioning.md)). "Agent infrastructure" / "platform agent runtime" / "agent substrate" — pick one and use consistently in the README sweep.
- ~~**Task-class name for `SingleTurn` mode**~~ *Resolved 2026-07-01: SingleTurn is internal, not user-facing. Public task classes remain chat/debug/implement/research/review + new `orchestrate` (planner-enabled workloads). See [`./orchestration-design.md`](./orchestration-design.md) task-class-resolution section.*
- **Plan-first gate interaction with workflow graphs and planner** ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) open Q #5; [`./orchestration-design.md`](./orchestration-design.md) treats the planner case). Prose plan (`plan_it_out`) satisfies gate + composes with `plan_review_required` HITL; broader "does plan-first reach into pre-authored graphs" still open.

The doc-local open questions (specialist-API details for `agenttool`, sync discipline edge cases, per-shape testing conventions, etc.) live in each doc and don't affect the others.
