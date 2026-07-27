# mast: fork design

**Status:** draft, 2026-06-11 (updated 2026-07-01 — ADK v2 disposition + fork mechanic revised from prune-in-place to rebuild-lean-core). Companion to [`./positioning.md`](./positioning.md) (the thesis), [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) (the operator-facing UI), [`./specialists-design.md`](./specialists-design.md) (the subagent-as-tool subsystem), and [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) (the canonical workflow shapes on top of ADK v2's graph engine). This doc covers the *mechanics* of extracting `mast` from current core-agent. Reading the positioning doc first is assumed — this doc takes the keep/cut/change-shape decisions as given and answers *how* to make them happen.

## The strategic question that determines everything

**Why fork instead of refactoring core-agent in place?**

The mechanical work is similar — same set of code gets deleted either way. The right shape of the answer is wildly different depending on which of these motivates the move:

| Motivation | Fork shape |
|---|---|
| **(A)** Backward compat for existing users. core-agent stays for current consumers; lean fork is a new product for a new audience. Both maintained indefinitely. | Two repos, two release tracks, ongoing sync discipline. Highest maintenance burden. |
| **(B)** Branding / market reset. core-agent name is too generic or too associated with kitchen-sink; the fork is the same team's sharper-branded successor. Eventual deprecation of core-agent. | One repo becomes primary, the other enters maintenance-only mode. Migration story for existing consumers. |
| **(C)** License / governance / ownership reset. Something about the current project (ADK dependency, contributor agreement, org membership, etc.) is a constraint the fork escapes. | Hard cutoff. No sync back. Existing core-agent likely continues with original constraints; fork lives independently. |
| **(D)** Refactor-in-place would touch too much at once and risk breaking everyone simultaneously. Fork is the "staging area" for the cleanup; once stable, it replaces core-agent. | Temporary two-repo state. Fork lands, soak period, then `core-agent` repo gets archived/redirected. |
| **(E)** Sibling products with divergent agendas. The lean fork targets unattended/platform agents. core-agent stays alive as the experimentation / integration substrate (embedded/experimentation consumers — embedded use, richer interactivity, kitchen-sink acceptable). Different jobs, not different cohorts. Both maintained indefinitely. | Two repos, each with its own positioning, README, audience, and roadmap. Shared maintenance discipline on the *intersection* (bug fixes, security); deliberately divergent on features. |

**Resolved (2026-06-11): the motivation is (E) — sibling products with divergent agendas.** The lean fork is the platform-agent product (per `./positioning.md`); core-agent stays alive as the experimentation/integration substrate for embedded/experimentation consumers. Both have a future. This is meaningfully different from (A) — both maintained — because the two have *different jobs*, not just different user cohorts for the same job. That distinction reduces the sync burden (less overlap to keep in lockstep) but raises the bar for clear positioning so users know which to reach for.

## Recommended approach: rebuild lean core around ADK v2, port adapters

**Revised 2026-07-01.** Three mechanics were considered pre-v2:

1. **Hard fork.** `git clone` the repo, rename, start deleting on a fresh branch. History preserved verbatim. Pro: full provenance. Con: every git log entry references files that no longer exist; the diff to land the prune is huge.
2. **Code extraction.** Create empty repo, copy in the keep list, write a fresh initial commit. Pro: clean history matching the new scope. Con: loses provenance entirely; tests/CI/coverage start from zero.
3. **Hard-fork-then-prune in one squash commit** (originally recommended). Hard-fork to preserve history, then land all cuts in a single squash commit titled `chore: prune to lean scope (forked from go-steer/core-agent@<SHA>)`. Pro: provenance preserved, but history *after* the fork point reads cleanly against the new scope. Con: the squash commit is enormous.

**ADK v2 changes the calculation.** The v2 workflow package delivers node runtime, graph engine, Chat/Task/SingleTurn agent modes, durable HITL, unified `agent.Context`, and one telemetry span tree. Under v1, the core agent loop (`pkg/agent/{agent,runner,loop,scheduler,checkpointer,compactor,autonomous,inbox}.go`) was substantial value-add code worth preserving through a prune. Under v2, most of what that code did *is what v2 provides natively* — porting it would be porting code whose value has evaporated. Prune-in-place is the wrong shape for code that we would rewrite anyway.

**The revised recommended mechanic: rebuild the lean core around v2 primitives, port the adapter packages unchanged, build the mast-specific subsystems v2-natively.** Three buckets of work; different provenance treatment per bucket:

- **Bucket 1 (rebuild).** The lean core — a small (~500-1500 LOC) shim over v2's runner + node runtime + Chat/Task/SingleTurn wiring + HITL propagation + event-stream plumbing + watchdog signal emission + plan-first gate as a graph-entry pattern. Written fresh against v2; no v1 code carried forward. This is where mast's positioning gets encoded in the loop itself, not negotiated against inherited v1 patterns.
- **Bucket 2 (port).** Adapter packages that constitute value-add substrate — providers, permissions, eventlog, attach, MCP, config, instruction, modeltier, taskclass, usage, pricing, digest, tools + agentic wrappers. Carry forward with minimal changes for v2 compat (unified `agent.Context`, `session.NewEvent` signature, event fields in eventlog). Attribution headers per-file: `// Originally derived from go-steer/core-agent@<SHA>`.
- **Bucket 3 (build v2-native).** Specialists loader, workload-bundle loader + resolution paths + classifier-first dispatch (see [`./orchestration-design.md`](./orchestration-design.md)), planner (scaffolded in v0.1, complete in v0.2), reference-graph library, autonomous+inbox as cyclic graphs, shared memory + audit-derived memory ([`./memory-design.md`](./memory-design.md)), bundle learning (v0.3), evaluation + regression harness (v0.3), watchdog integration, small-tier-parent classifier, durable-execution primitives beyond HITL (programmatic/timed/external-signal pause per [`./durable-execution-design.md`](./durable-execution-design.md)), observability instrumentation ([`./observability-design.md`](./observability-design.md)), library-API surface ([`./library-api-design.md`](./library-api-design.md)), MCP catalog wiring templates ([`./mcp-catalog-design.md`](./mcp-catalog-design.md)), A2A server + client + Google Agent Registry publishing ([`./a2a-design.md`](./a2a-design.md)), AG-UI server + client + CopilotKit reference deployment ([`./ag-ui-design.md`](./ag-ui-design.md)), federation adapter framework + `invoke_remote_agent` planner tool ([`./federation-design.md`](./federation-design.md)). New code that instantiates mast's positioning; written against bucket 1's contract from day one.

**Provenance is preserved via attribution, not git history.** Bucket-2 ports carry per-file attribution headers pointing at the core-agent commit SHA they were derived from. The CHANGELOG's v0.1.0 note references the core-agent commit range. Anyone doing archaeology on a specific package can `git log` core-agent's history for that file; there is no cross-repo `git log --follow` chain, but there was never going to be a useful one across the pruning squash either — the squash was already an archaeology-defeating boundary in the original design.

**Trade-off vs. prune-in-place:**

| Dimension | Prune-in-place (original) | Rebuild lean core (revised) |
|---|---|---|
| Wall-clock to `go build` clean | ~1 day (mechanical prune) | ~1-2 weeks (bucket 1 + bucket 2 in parallel) |
| Wall-clock to `mast --task=debug ...` runnable end-to-end | ~1-2 weeks after phase 1 (phase 2's boundary cleanup + DefaultInstruction rewrite) | ~2-3 weeks (bucket 1 + bucket 2 + minimum bucket 3) |
| Long-term code health | Loop carries v1 assumptions; DefaultInstruction rewrite chases them post-hoc; boundary cleanup discovers rather than designs | Loop is v2-native by design; no v1 assumptions to chase; boundaries designed against v2's contract |
| Risk of losing subtle behaviors | Low — soaked code preserved verbatim | Medium — checkpointer/compactor/plan-first behaviors must be reimplemented; some subtle interactions may reappear as bugs (mitigated by contract tests + UAT before v0.1.0 tag) |
| Squash commit reviewability | Huge, mechanical, "look at the tree not the diff" | N/A — multiple normal PRs (P1.1 … P1.6, see Phase 1) replace the squash |
| Fits (E) sibling-products framing | Fine | Better — rebuild forces us to encode mast's positioning into the core loop from the first commit rather than layer it above core-agent's shape |
| ADK-boundary sync burden | Same (ADK-touching shared code still on version boundary) | Same |

The rebuild costs 1-2 more weeks of wall-clock and one class of risk (subtle-behavior regression). It buys code that structurally embodies mast's positioning from the first commit, a smaller loop with fewer moving parts, and freedom from a v1→v2 migration diff that would show up in every future `git blame`.

## Phase 0: decisions before code moves

These need answers before phase 1; deferring them creates rework.

| Decision | Suggested default | Notes |
|---|---|---|
| Project name | **`mast`** | Short (4 chars), nautical-fits-`go-steer`-org, "load-bearing structural element" is the right metaphor for an agent runtime substrate. GitHub search 2026-06-11: no collision in the agent/AI/LLM space; the two notable Go projects named `mast` are niche (a math DSL parser at `fatlotus/mast`, a Merkle Search Trees implementation at `jrhy/mast`). Tagline writes itself: *"mast: agent runtime for unattended, library, and multi-provider workloads."* |
| Repo home | **`github.com/go-steer/mast`** | Same-org as core-agent. Signals "same team, two products." Verified 2026-06-11: repo does not yet exist. |
| Go module path | **`github.com/go-steer/mast`** | Affects every import in the kept code. Touched once at fork time via `go mod edit -module github.com/go-steer/mast` + sed. |
| Binary name | **`mast`** | Drops from `cmd/core-agent/main.go` → `cmd/mast/main.go`. CLI invocation becomes `mast --task=debug ...`. |
| License | Carry forward Apache 2.0 | No reason to change unless **(C)**. |
| Versioning | Start fresh at v0.1.0 | Signals "new project, not a continuation." Inherits design maturity, drops API stability promises. |
| ADK dependency | **Keep, and adopt v2 from day one.** No concrete pain; provides known working code. v2's graph engine, durable HITL, and agent modes are load-bearing for mast's positioning (see [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) and [`./specialists-design.md`](./specialists-design.md)). Revisit replacement only if a specific bug or limitation surfaces. | Replacing `google.golang.org/adk/v2` would be 2-3 months of careful work (tool-call correlation, parallel function calls, streaming delta protocols, content-part ordering rules — all the fiddly translation ADK does between Gemini and Anthropic semantics). v2 makes that even less appealing by delivering the graph scheduler, durable HITL, and agent modes we would otherwise own. Version disposition: the lean core (bucket 1) is written fresh against v2; adapter ports (bucket 2) migrate v1→v2 at port time. No v1→v2 migration diff persists in mast's history. See "Recommended approach" and "Sync discipline" for the follow-on implications. Owning ADK's job is a separate decision that needs its own trigger. |
| Backward-compat surface | None (clean break) | Existing consumers consume the original core-agent for as long as they need to. The fork doesn't promise import-path stability with core-agent. |
| CI / release infra | **Independent at start.** Port `dev/ci/presubmits/*` as-is; lean fork owns its own GitHub Actions workflows and release pipeline from day one. | Presubmits are the project's quality bar; carry them over. Shared workflow infrastructure (one source, both repos consume) is the more elegant long-term option but couples release cadences and isn't worth the operational overhead at start. Revisit at the 6-12 month mark alongside the shared-infrastructure-repo question. |
| Docs site | Fresh site, not a port — **Astro + Starlight** *(corrected 2026-07-26: this row originally said "Hugo"; core-agent's actual `docs/site` convention is Astro ^7 + @astrojs/starlight, and the Hugo references across the corpus were stale. The v0.1 skeleton shipped at `docs/site/` on that stack.)* | Site docs are too entangled with the old positioning; cheaper to rewrite. The library + design docs in `docs/` port over. |

## Trigger condition for Phase 1

**Revised 2026-07-26.** The original trigger (issues #158-#161 + the shared-memory stack "PRs #13/14/15") was audited against core-agent's actual repo state and found partially stale and partially unfalsifiable: #161 is closed but #158/#159/#160 remain open; and the shared-memory PR numbers now resolve to unrelated merged work (config flags, fetch_url, attach endpoints) with **no shared-memory work filed in core-agent at all** — a trigger conditioned on unfiled work is an indefinite delay. The trigger was always a proxy for one thing: *don't port moving code*. The revision states that directly and gates only the work that actually depends on it:

1. **P1.1, P1.2, and the bucket-3 minimum start immediately.** They share no code with core-agent; spike 2 (`mast-prototype`, tags `spike1`/`spike2`) is a validated majority of P1.2 + bucket-3 and graduates rather than restarts. Waiting exposes them to zero less churn because they are exposed to zero churn now.
2. **Only P1.3 (bucket-2 adapter ports) gates — on core-agent's three *code* cleanup milestones closing:** *Cleanup: Correctness & durability* (includes #357/#367 Anthropic-adapter bugs, #370 Vertex caching, #363 watchdog — all in port-list packages), *Cleanup: Security hardening* (#384/#385 — attach), and *Cleanup: Substrate & API structure* (#388 `pkg/agent` split, #390 attach public-API cleanup, #386 `pkg/compose` extraction — these restructure the exact packages P1.3 ports; porting first means porting a shape core-agent is about to abandon, then re-porting every fix across the ADK v1/v2 boundary). The *Docs, deps & test hygiene* milestone does not gate (nothing in it is ported), though #393 (drop k8s.io from the library module graph) is a welcome bonus for mast's `go.mod`. P1.3's per-file attribution SHA is pinned after these milestones close.
3. **Issues #158/#159/#160 are demoted from the trigger.** They land in core-agent because core-agent wants them, on core-agent's schedule. Mast does not wait: the bash search-gate (#158) and tool-catalog defaults (#160) are applied at port time regardless of whether core-agent has landed them, and the watchdog→model routing (#159) is *built fresh* in bucket 3 as the v2-native emitting-node shape this doc already calls cleaner than the v1 transport. (#161's probe is closed.)
4. **The shared-memory stack is removed from the Phase-1 trigger entirely and re-homed as the gate on mast's v0.2+ memory work** ([`./memory-design.md`](./memory-design.md)) — which is where mast's own phasing always consumed it; gating Phase 1 on it was the over-wait the 2026-07-25 review flagged. When that stack is actually planned in core-agent, file it there so the dependency is trackable.

This honors the (E) framing more strictly than the original trigger did: only the port *commit* sits on core-agent's schedule; the project does not.

**ADK v2 migration does *not* extend the trigger.** The v1→v2 migration is different: it's a substrate-version choice that mast's positioning depends on structurally (graph engine, durable HITL, agent modes) while core-agent's audience does not need in the same way. Making core-agent's v2 migration a prerequisite would (1) put core-agent's schedule on mast's critical path — the exact failure mode the (E) sibling-products framing was chosen to avoid, (2) force a compromise migration done for mast's benefit under core-agent's embedded-consumer constraints, and (3) delay the fork indefinitely if core-agent's audience never asks for v2 features. Under the rebuild mechanic, mast is v2-native by construction; adapter ports absorb whatever v1→v2 adaptation the imported packages need at port time. Core-agent stays on ADK v1 for as long as its audience wants; the sibling-products framing means the two products can genuinely diverge on substrate version. Follow-on implication: ADK-touching shared-infrastructure code (session store, provider adapters, watchdog signal routing) sits on opposite sides of the v1/v2 API boundary — sync discipline covers this below.

**Note: `mast-web` work doesn't gate on this trigger.** Phases A+B+C of [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) consume the attach protocol that already exists in core-agent, so frontend work can proceed in parallel — built against `core-agent --attach-listen` today, repointed at mast at fork time. See [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md)'s Phasing section for the dependency table.

## Phase 1: rebuild + port (multiple PRs, ~3-4 weeks)

*Revised 2026-07-25 (scope re-cut).* Two changes from the earlier draft: (1) the wall-clock claim is now honest — the per-PR estimates below sum to ~14-19 working days on the critical path (P1.3 parallel), i.e. **~3-4 weeks**, where the earlier "~2-3 weeks" header contradicted its own arithmetic (~23 days pre-cut); (2) **P1.4 is re-cut**: the A2A *server*, AG-UI server + client, and registry publishing move to Phase 2 / v0.2. Rationale: none of the three serves the four pillars in v0.1 — they serve "not an island," which matters once there is a durable, unattended core worth reaching; two of the three need spec rework before coding regardless (a2a-design's pre-0.2-draft endpoint layout; AG-UI's interrupt lifecycle being a draft extension); and [`./durable-execution-design.md`](./durable-execution-design.md)'s phasing puts programmatic pause — which outbound long-running A2A composition depends on — in v0.2 anyway. What v0.1 keeps of the interop surface: MCP wiring templates (P1.5), the `federation.Adapter` interface + `invoke_remote_agent` planner stub (so nothing ossifies wrong), and the synchronous **A2A client** (smallest useful slice for the kagent/registry story, written against A2A v0.3+ from day one).

Phase 1 is a set of coordinated PRs against a fresh `github.com/go-steer/mast` repo, not one squash. Sequencing:

**P1.1 — bootstrap.** New repo initialized. Empty `cmd/mast/main.go`, minimal `go.mod` pinning `google.golang.org/adk/v2`, `LICENSE` (Apache 2.0), initial `README.md` stub, `.gitignore`, `CHANGELOG.md`, CI skeleton adapted from mast-web (lint + build + test workflows). ~1 day.

**P1.2 — lean core (bucket 1).** The core agent loop as a small shim over v2 primitives:
- v2 runner integration; `agent.Agent` interface consumers.
- Chat / Task / SingleTurn mode wiring; helper-tool auto-installation surfaces exposed to bucket 2's config layer.
- HITL propagation: `RequestInputEvent` surfaces plumbed through to the attach transport (interface only; concrete attach lands in bucket 2).
- Event-stream plumbing: session events emitted uniformly regardless of node vs. agent execution.
- Watchdog signal-emission integration point (interface; concrete watchdog lands in bucket 3).
- Plan-first gate reimplemented as a graph-entry pattern (not a stateful mid-loop hook).
- Per-mode DefaultInstruction variants (Chat conversational; Task opinionated-unattended; SingleTurn minimal).
- Contract tests bucket 2 will write against.

Estimated size: ~500-1500 LOC. Written fresh; no v1 code carried forward. ~3-5 days.

**P1.3 — adapters (bucket 2).** Port from core-agent with per-file `// Originally derived from go-steer/core-agent@<SHA>` headers. Minimal changes for v2 compat (unified `agent.Context`, `session.NewEvent` signature, event fields in eventlog). Can proceed in parallel with P1.2 once the core's contract is stable (probably day 3-5 of P1.2). Packages:

| Package | Port notes |
|---|---|
| `pkg/providers/` (Gemini, Vertex, Anthropic, Anthropic-Vertex, echo, scripted) | ADK-touching: `agent.Context` change; adapter methods take the unified context. |
| `pkg/permissions/` (gate, path scope, URL scope) | ADK-independent; straight port. |
| `pkg/eventlog/` (durable sessions, audit) | ADK-touching: new event fields (`IsolationScope`, `Output`, `Routes`, `RequestedInput`, `NodeInfo`) must persist. `session.NewEvent` signature. **Re-scoped 2026-07-25 (spike 2):** ADK v2.1.0 ships `session/database.NewSessionService` (GORM: SQLite pure-Go via glebarez, Postgres same call) + `AutoMigrate`, verified durable across `kill -9` — so the *store* is ADK's. The port narrows to what session/database doesn't do: the audit/query surface (operator-facing event queries, retention/TTL per session, audit-export). Decide at port time whether that layers on session/database's schema or runs beside it. |
| `pkg/attach/` (HTTP/SSE) | ADK-touching: emit v2 event shape; expose `RequestInputEvent` schema to mast-web. |
| `pkg/mcp/` (client + transparent wrap) | ADK-touching lightly; wrap and filtering unchanged in shape. |
| `pkg/config/`, `pkg/instruction/`, `pkg/modeltier/`, `pkg/taskclass/` | ADK-independent; port with `--task=<class>` → agent-mode mapping added in `taskclass`. |
| `pkg/usage/`, `pkg/pricing/` | ADK-independent; straight port. |
| `pkg/digest/` | ADK-independent; straight port. |
| `pkg/tools/` (built-in tool surface) | ADK-touching lightly (tool.Tool contract unchanged); port with bash-search-gate applied (core-agent issue #158). |
| `pkg/tools/agentic/` (Mechanism B wrappers) | ADK-independent aside from digest calls; straight port. |
| `pkg/agent-card/` | ADK-independent; straight port. |
| `pkg/skills/` + `adk/tool/skilltoolset` | ADK-touching (agent context on skill invocation); port for SKILL.md format support per [`./skills-design.md`](./skills-design.md). Reinstated 2026-07-01 after skill-publisher landscape (GKE + Google teams) inverted the audience-fit assumption behind the earlier cut. |

Not ported (see "Packages not ported" below).

Estimated wall-clock: ~5-8 days in parallel with P1.2 completion.

**P1.4 — specialists loader + workload bundles + durability surface + minimum bucket 3.** *(Re-cut 2026-07-25 — A2A server, AG-UI server+client, and registry publishing moved to Phase 2; see the phase header note.)* The `pkg/specialists/` package per [`./specialists-design.md`](./specialists-design.md), loading `.tmpl` files, registering as Task-mode (or SingleTurn-mode per `mode:` frontmatter) `LlmAgent`s, with allowlist enforcement per the normative table (spike-2-verified `FilterToolset` mechanism). The `pkg/workloads/` package per [`./orchestration-design.md`](./orchestration-design.md) — bundle schema, loader, `--workload=<name>` CLI flag, envelope routing, basic classifier-first dispatch. The `pkg/session/` durability surface per [`./durable-execution-design.md`](./durable-execution-design.md) — resume + HITL/external-signal pause against ADK's `session/database` store; CLI `mast sessions list/show/resume/abort`. The `pkg/budget/` meter (event-stream `UsageMetadata` folding, per [`./orchestration-design.md`](./orchestration-design.md) budget substrate). The `pkg/a2a/` **client only** — synchronous A2A client against A2A v0.3+, static `.agents/a2a/*.yaml` config. The `pkg/federation/` package reduced to the `federation.Adapter` interface + the A2A client adapter + planner `invoke_remote_agent` tool (interface frozen with an event/interrupt channel in mind — see [`./federation-design.md`](./federation-design.md) open questions — so v0.2's streaming/HITL propagation isn't a breaking change). Planner scaffolded (tool vocabulary schema in place; `run_shape_*` tools wire to the two v0.1 reference shapes, which land in this PR or earlier — LLM-as-router + fan-out-fan-in per [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md), fixing the earlier P1.4→P1.5 sequencing inversion); learning explicitly deferred to v0.3. Much of this exists in validated form in the `mast-prototype` repo (spike 2) and graduates rather than starts from zero. ~6-8 days.

**P1.5 — smoke examples + observability + presubmits.** Port enough of `examples/gke-parallel-triage` and `examples/cloud-run-deploy` to build. Ship `pkg/observability/` per [`./observability-design.md`](./observability-design.md) — Prometheus `/metrics` endpoint with the base metric families; JSON structured logs; basic OTel trace export. Ship deployment starters (`examples/deploy/{standalone,library-embedded,gke,cloud-run}/`) per [`./deployment-design.md`](./deployment-design.md). Ship MCP wiring templates for v0.1 catalog per [`./mcp-catalog-design.md`](./mcp-catalog-design.md). Port `dev/ci/presubmits/*` scripts. Port `dev/uat/` scaffolding. Goal: `go build ./... && go test ./... && dev/ci/presubmits/*` all green. ~3-4 days.

**P1.6 — v0.1.0 tag.** CHANGELOG note referencing `go-steer/core-agent@<SHA-range>`; README rewritten to full lean-positioning language (per [`./positioning.md`](./positioning.md)); DESIGN.md written fresh for the trimmed surface. ~1 day.

**Phase 1 exit criteria:**

1. `go build ./... && go test ./... && dev/ci/presubmits/*` all green.
2. `mast --task=debug --provider=gemini <toy prompt>` runs end-to-end (laptop path), produces expected output, session persists in eventlog.
3. `mast --workload=<sample> --provider=gemini <envelope>` runs end-to-end (unattended path) against a hand-authored bundle under `.agents/workloads/` — resolves bundle, applies tool catalog + specialist roster + budgets, produces expected output.
4. Attach mode reachable from `mast-web` (once mast-web's Phase C+ is repointed at mast per its own doc).
5. Specialists loader recognizes a sample `.tmpl` file and registers it as a callable tool (Task and SingleTurn modes).
6. HITL round-trip: an interactive prompt from a specialist reaches attach, waits, resumes on operator response — pause survives process restart per [`./durable-execution-design.md`](./durable-execution-design.md).
7. Prometheus `/metrics` endpoint reachable; base metric families populated; JSON logs to stdout with session correlation IDs (per [`./observability-design.md`](./observability-design.md)).
8. Library-embedded consumer scenario: `mast.RunWorkload(ctx, ...)` succeeds from a Go test with programmatic bundle registration (per [`./library-api-design.md`](./library-api-design.md)).
9. Federation round-trip (client side): sample workload's planner invokes `invoke_remote_agent("a2a://sample-external", ...)` against a stub A2A v0.3 server via the A2A client adapter; result surfaces to the planner as tool output. *(Re-cut 2026-07-25: the earlier criteria 9 and 11 — A2A server discovery/task-submission/registry-publish and the AG-UI round-trip — moved to Phase 2 / v0.2 alongside their subsystems.)*
10. Budget round-trip: a workload bundle's `budget.max_cost_usd` trips mid-turn from event-stream usage metering and aborts the run with a structured error (per [`./orchestration-design.md`](./orchestration-design.md) budget substrate).
11. GKE triage anchor (per [`./triage-demo-plan.md`](./triage-demo-plan.md)): the workload bundle accepts a synthetic `InjectPayload` for each failure mode and produces a structured `INCIDENT SUMMARY`; the change-safety-gate round-trips HITL for a `rollout_undo` suggestion, surviving a process restart mid-pause.

**Packages not ported:**

- `pkg/agent/{agent,runner,loop,scheduler,checkpointer,compactor,autonomous,inbox}.go` — replaced by bucket 1 (lean core) + bucket 3 (autonomous+inbox as cyclic graphs, landing in Phase 2).
- ~~`pkg/skills/` + `adk/tool/skilltoolset` — skills subsystem cut~~ *Reversed 2026-07-01: skills reinstated as first-class consumable. See [`./skills-design.md`](./skills-design.md); moved to the bucket-2 port list (below) rather than the cut list.*
- Any package or example targeting developer-laptop interactive-coding UX polish.
- LSP / AST tooling references (none today, just preventative).
- Documentation under `docs/site/content/docs/` that targets the developer-coding-assistant reader (site rewritten fresh in Phase 4).
- Any model-specific prompt-engineering scaffolding for code search beyond what's load-bearing (per `docs/gemini-tier1-followup-plan.md` — measured ineffective).
- `examples/basic`, `examples/with-tools` — keep as smoke tests in `examples/_smoke/` (or similar) but stop recommending them as starting points. README points new readers at platform/SRE-shaped examples instead.

**Explicitly kept and reshaped in Phase 2/3, not Phase 1:**

- `examples/gke-deploy`, `examples/scheduled-monitor`, `examples/autonomous`, `examples/plan-first`, `examples/streaming`, `examples/replay`, `examples/autonomous-handle` — reshape or promote to reference graphs (see [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)) as bucket 3 lands.
- `dev/parallel-probe/` — port as-is when the workflow scaffolding library needs it for testing.

## Phase 2: bucket-3 completion (multiple PRs, ~3-6 weeks)

The original Phase 2 (refactor / boundary cleanup, DefaultInstruction rewrite, tool catalog defaults per task class, bash search-gate, watchdog→model routing) largely moots under the rebuild:

- **DefaultInstruction rewrite** — done in bucket 1 as part of building the core; not a follow-on refactor.
- **Package consolidation / boundary cleanup** — bucket 2 ports are the opportunity to consolidate; boundaries that want rework get reworked at port time, not as a follow-on pass.
- **Tool catalog defaults per task class (issue #160)** — done in `pkg/taskclass/` during bucket 2 port; agent-mode mapping surfaces the change naturally.
- **Bash search-gate (issue #158)** — applied to `pkg/tools/` during bucket 2 port (assumes it lands in core-agent first per trigger).
- **Watchdog → model context routing (issue #159)** — the emitting-node integration point is scaffolded in bucket 1; the concrete watchdog signal-emission lands in Phase 2 as bucket 3 work.

What remains of Phase 2 is bucket-3 work not scoped into Phase 1's minimum. In rough priority:

1. **Reference-graph library** — the seven canonical shapes per [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md), each with `main.go` + `README.md` + `config.yaml` under `examples/workflows/<shape>/`. Ship 2-3 shapes per PR; the whole set incrementally.
2. **Planner completion + `orchestrate` task class exposed** per [`./orchestration-design.md`](./orchestration-design.md) — all `run_shape_*` planner tools wired to the reference-graph library; `plan_review_required` HITL flow end-to-end; bundle-scoped nested classifiers.
3. **Autonomous+inbox as cyclic graphs** — rewrite `pkg/agent/autonomous.go` and `pkg/agent/inbox.go` (both unported from Phase 1) as v2 cyclic graphs. Landing pattern: define the graph in `pkg/autonomous/` or similar, expose `mast --autonomous <config>` entry point.
4. **Watchdog signal-emission** — concrete watchdog running inside an emitting function node, injecting alerts into the session event stream per core-agent issue #159.
5. **Small-tier-parent classifier as LLM-as-router** — replaces the substring matcher (positioning.md open Q #4, now resolved). Ships as a specialist `.tmpl` with `mode: SingleTurn` plus a router node in the appropriate reference graph.
6. **DefaultInstruction refinements** — the first-pass split from bucket 1 will need per-mode iteration once real workloads exercise them. Includes the planner-mode DefaultInstruction template ([`./orchestration-design.md`](./orchestration-design.md) open Q #7).
7. **A2A server + registry publishing** (moved from Phase 1 in the 2026-07-25 re-cut): agent-card generation, task submission endpoints per A2A v0.3+ JSON-RPC surface, `a2a.expose` bundles, `mast a2a publish` (gated on Google Agent Registry API maturity — Public Preview as of 2026-07). Then **A2A streaming + push notifications + dynamic registry discovery** per [`./a2a-design.md`](./a2a-design.md) phasing.
8. **Federation adapters (HTTP/RPC + basic mast-native)** per [`./federation-design.md`](./federation-design.md) v0.2 phasing — planner can dispatch to remote agents via `invoke_remote_agent`.
9. **AG-UI server + client** (moved from Phase 1 in the 2026-07-25 re-cut; the interrupt lifecycle is a draft spec extension — version-pin the community Go SDK and isolate interrupt encoding behind `pkg/agui`), then activity events + push-notification-equivalent reconnect (a mast extension, not an AG-UI-spec pattern) + Slack-via-CopilotKit deployment starter per [`./ag-ui-design.md`](./ag-ui-design.md) phasing.

Phase 2 changes are visible to consumers. Bump to v0.2.0 at completion of items 1-2; v0.3.0 at 3-8.

## Phase 3: shared memory + multi-session + MCP creds + bundle learning (ongoing, ~2-4 months)

What the lean repo focuses energy on next, matching [`./positioning.md`](./positioning.md) priority order:

1. **Shared memory + audit-derived memory implementation.** Per the core-agent shared-memory-stack design; consumed via state-bound nodes in downstream reference graphs (positioning.md priority #7). Direct enabler for item 4 below.
2. **Multi-session deployment story end-to-end.** Supervisor+workers reference graph with `WithIsolationScope(tenantID)` as the concrete shape; walk-through from "single dev" to "shared team deployment with per-user auth and audit isolation" (positioning.md priority #5). Composes with `isolation.scope` on workload bundles.
3. **MCP credential resolution end-to-end.** Design doc → implementation → docs page (positioning.md priority #6). Bundle `tool_catalog.mcp[].server` references resolve per bundle context.
4. **Bundle learning + refinement** per [`./orchestration-design.md`](./orchestration-design.md) — map-reduce over audit corpus proposes new workloads and refines existing ones; review UI in mast-web. Depends on item 1 being real.
6. **Full mast-native federation** per [`./federation-design.md`](./federation-design.md) v0.3 phasing — gRPC transport, cross-instance session-state propagation, cross-instance HITL, cross-instance durability. Enables hierarchical federation topologies and cross-tenant federation with opt-in.
7. **A2A cost attribution + advanced auth** per [`./a2a-design.md`](./a2a-design.md) v0.3 phasing — OAuth 2.0 flows end-to-end; Google IAM Workload Identity as default in GKE deployments; cost attribution extension.

This phase is "what was the point of the fork." Lands at v0.5.0+ — by which time the ADK-dependency question (deferred from Phase 0) should be revisited with Phase 1-2 hindsight.

## Phase 4: docs site + outward-facing rewrite (parallel with phase 3)

*(Corrected 2026-07-26: this section originally said "Hugo site". The stack is **Astro + Starlight** — core-agent's actual `docs/site` convention; the Hugo references were stale. A v0.1 skeleton of this site shipped at `docs/site/` — landing, install, three quickstarts, reference, roadmap — with build-only CI; deploy is deferred until the repo goes public.)*

Fresh site, not a port. Targets:

- **Landing page.** "Agent infrastructure for unattended / library / multi-provider workloads. Not a Claude Code competitor." Top-of-fold message.
- **Quickstart.** Three flavors: (1) library embedding, (2) GKE platform agent, (3) interactive REPL + web UI (`mast-web`). The first two are the moat; the third keeps the user-pinned interactive story via browser rather than terminal.
- **Reference docs.** Port from old site selectively. Anything that assumed the broader scope gets rewritten.
- **Migration page** for existing core-agent users (more critical under **(B)/(D)** than **(A)**).

## What happens to the old `core-agent` repo

Under **(E)** — the resolved motivation — core-agent stays a first-class, actively developed project with its own positioning and agenda. Not maintenance-only, not deprecated, not archived. Specifically:

- **README on `core-agent` repo gets re-positioned** to make its job clear: *"core-agent is the experimentation and integration substrate for embedding agents into Go programs. For unattended/platform agent runtime, see <fork-name>."* The repo stops trying to address every reader.
- **Both repos ship releases on their own cadence.** Neither is gated on the other.
- **The lean fork's README reciprocates:** *"For embedded/experimental agent integration with richer interactivity surface, see core-agent."* So a developer landing on either knows what they have and where to go if their use case doesn't fit.
- **Cross-references in docs.** Each project's design docs that touch on the boundary (e.g. positioning) name the sibling explicitly so readers don't have to deduce the relationship.
- **No EOL for core-agent.** It serves a real audience (embedded/experimentation integrations); that audience doesn't go away because the lean fork exists. The two products coexist because they have genuinely different jobs.

## Sync discipline under (E)

Two indefinitely-maintained repos with divergent agendas need real discipline. The good news: divergent agendas mean *less* overlap requiring sync than (A) would. The bad news: the discipline never ends.

**Categorize every change.** Each commit in either repo falls into one of four buckets:

| Category | Sync rule |
|---|---|
| **Shared infrastructure** — bug fixes in code that lives in both repos (provider adapters, eventlog, permission gate, etc.) | Land in *whichever repo finds it first*, then port within ~1 week. Bidirectional, but the direction is determined by where the bug was reported, not a fixed rule. |
| **Security fix** | Land in both within 48 hours. No exceptions. |
| **Lean-fork-specific feature** (workflow scaffolding, audit-derived memory, opinionated task profiles, watchdog→model routing) | Lean fork only. Not ported to core-agent unless someone there explicitly asks for it. |
| **core-agent-specific feature** (richer interactivity surface, experimental skills/slash commands, embedded-integration helpers) | core-agent only. Not ported to lean fork. |

**Track sync state explicitly.** Each repo carries a `docs/sibling-sync.md` (or `cross-repo-sync.md`) listing:
- Shared-infrastructure SHAs ported in either direction (with date)
- Shared-infrastructure SHAs explicitly *not* ported, with a one-line reason
- Security-fix correlation table

This is more discipline than weekly cherry-pick batches; the upside is that diverging features stay genuinely divergent without anyone forgetting a critical bug fix.

**Avoid the failure mode** of letting "shared infrastructure" creep until it's most of the codebase. Concretely: when adding a new feature to the lean fork, default to *not* touching shared-infrastructure code; extend in the lean-fork-specific layer when possible. Same in reverse for core-agent. This keeps the shared core small enough to sync confidently.

**ADK-version-boundary flag.** Because mast runs on ADK v2 and core-agent stays on v1 (see "Trigger condition for Phase 1"), any shared-infrastructure code that consumes ADK types sits on opposite sides of the v1/v2 API boundary. The concrete subset: session store (event schema change), provider adapters (context type change), watchdog + signal routing (event-emission surface), any custom `InvocationContext` implementations (must add `IsolationScope()` / `ResumedInput()` on the v2 side). Ports across the boundary need adaptation, not a straight cherry-pick. The sibling-sync doc's per-SHA entries should call out ADK-boundary items explicitly so the port doesn't get scheduled as a five-minute cherry-pick. ADK-independent shared code (`pkg/permissions/`, `pkg/pricing/`, `pkg/digest/`, most of `pkg/config/`) is unaffected and ports cleanly.

**Optional, longer-term:** if the shared-infrastructure layer stays meaningfully large after 6-12 months, consider extracting it to a third repo (`agent-substrate` or similar) that both projects depend on. Don't do this on day one — premature extraction couples the two projects' release cadences in a way that defeats the point of (E). Wait until the shared surface has stabilized enough that a separate release cadence wouldn't slow either project down.

## Risks + mitigations

| Risk | Mitigation |
|---|---|
| **Two-repo confusion for users** during transition (any of A/B/D) | Clear banners on both READMEs; pick a date and stick to it; over-communicate. |
| **ADK dependency keeps the kitchen sink in via transitive deps** | Phase 0 decision is to defer; Phase 3 revisit. Measure first (`go mod graph \| wc -l`) after Phase 1 — the rebuild lets us pick a truly minimal `go.mod` from day one. |
| **ADK v1/v2 boundary in shared-infrastructure sync** | Flagged in sync-discipline as an ADK-boundary port class; expect adaptation on port, not straight cherry-pick. Concrete subset (session store, provider adapters, watchdog signal routing, custom `InvocationContext`) is small and stable. If it grows, that's a signal to revisit the shared-infrastructure-repo option earlier than the 6-12 month mark. |
| **Rebuild misses subtle behaviors from soaked v1 code** (checkpointer edge cases, compactor timing, plan-first gate corner conditions) | Bucket-1 contract tests exercise the primitives that checkpointer/compactor/plan-first handled. Where bucket-2 ports touch behaviors that had adjacent v1-loop assumptions, port the relevant behavior tests from core-agent (they run against interfaces, not v1 loop internals). Full UAT walkthrough against `examples/gke-parallel-triage` and `examples/scheduled-monitor` before the v0.1.0 tag. Accept that some subtle regressions will surface in early releases; keep v0.1.x cadence tight to fix them fast. |
| **Bucket-1 core takes longer than 3-5 days** because v2 primitives don't compose the way we expect | De-risk by prototyping bucket 1 in a scratch worktree during the trigger-wait period (core-agent's #158-#161 + shared-memory landing). By the time the trigger fires, we know whether the ~1500 LOC estimate holds. If it balloons past ~3000 LOC, that's signal that we've reintroduced v1's shape on top of v2 — stop and rethink before doubling down. |
| **Examples that worked under core-agent break under the rebuild** | Catch in P1.5 via `go build ./examples/...`. Any example that breaks in a non-trivial way is either reshaped (probably as a reference-graph shape in Phase 2) or dropped with a one-line CHANGELOG note. Smoke examples in `examples/_smoke/` are the minimum required to build; the platform/SRE-shaped set becomes reference-graph-anchored in Phase 2. |
| **Existing core-agent issues + PRs orphaned** | Triage at Phase 1 start. Issues that apply to the lean scope get ported as fresh issues in mast (linking back). PRs that target deleted code are closed with a note. |
| **Loss of contributor momentum** during the transition | Communicate the rebuild plan publicly *before* Phase 1 lands; give contributors a heads-up so in-flight work isn't wasted. Under (E) — single team owning both repos — this is more about internal calendar coordination than public announcement. |
| **Naming collision / SEO confusion** | Pick a name distinctive enough to be findable. Avoid prefixes/suffixes on "agent" — too generic. (Resolved: `mast`.) |

## Open questions to resolve before phase 1

1. ~~**What about AX integration?**~~ *Retired 2026-07-27: the adjacent distributed-runtime effort this referenced no longer plays a role in mast's planning (confirmed by the maintainer); the boundary audit is dropped, not deferred. If a distributed-runtime layer materializes later it enters as a new design question on its own merits.*
2. **Does core-agent's own README/positioning get updated as part of the fork landing**, or as a separate effort? Bias: update at the same time, so users landing on either repo immediately see the sibling and know which fits their use case. Inconsistent positioning across the two repos is the most common failure mode of (E)-style splits.
3. ~~**Task-class name for `SingleTurn` mode.**~~ *Resolved 2026-07-01 in [`./orchestration-design.md`](./orchestration-design.md): SingleTurn is an internal mode, not a user-facing task class. Consumed by the classifier-first workload dispatcher, `mode: SingleTurn` specialists, LLM-as-router classifiers, and the small-tier-parent classifier. Public task classes stay: chat / debug / implement / research / review, plus new `orchestrate` for planner-enabled workloads.*

(For `mast-web`-specific open questions — TypeScript-or-vanilla, framework adoption trigger, hosting model, slash command alignment — see [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md).)

### Resolved

**2026-06-11:**

- **ADK:** stays. No concrete pain; provides known working code. (Updated 2026-07-01: adopt v2 from day one — see below.)
- **Strategic motivation:** (E) — sibling products with divergent agendas. Lean fork = platform-agent product; core-agent = experimentation/integration substrate for embedded consumers.
- **In-flight work disposition:** all in-flight work lands in core-agent first. Both products benefit; lean fork inherits stronger baseline.
- **Phase 1 trigger:** after #158-#161 AND shared-memory stack (#13/14/15) land in core-agent.
- **Contributor communication:** moot — single team owns both repos. No public announcement plan needed.
- **Gray-area packages:** "parts of all" — usage/pricing, agentic wrappers, digest, agent-card all stay. Skills replaced by specialists subsystem (subagent-as-tool pattern with richer per-specialist config; see `./specialists-design.md`).
- **CI/release infra:** independent at start. Each repo owns its workflows from day one. Revisit at 6-12 months alongside shared-infrastructure-repo question.
- **Project name:** `mast`. Repo at `github.com/go-steer/mast`. Binary `mast`. Available, no collision in the agent/AI space.
- **Interactive UI:** web, not terminal. Embedded terminal TUI dropped from mast's scope (the use case lives in core-agent, which keeps `core-agent-tui` as before). New project `mast-web` at `github.com/go-steer/mast-web` ports the rendering surface of an earlier internal browser-WASM UI experiment as a thin client over mast's existing attach-mode protocol. Architecture pattern is "browser-as-thin-client, mast-as-backend-agent" — *not* that experiment's "browser-WASM-as-agent + auth-proxy" pattern, which fit its original embedded use case but is structurally wrong for mast. See [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md).
- **`core-agent-tui` disposition:** not forked. Mast doesn't ship a terminal TUI. Core-agent keeps `core-agent-tui` for its audience. **Compatibility (not shipping ≠ incompatible):** any attach-mode-compatible TUI, including `core-tui`, connects to `mast --attach-listen` unchanged for basic session-drive (Level 1: turn output, tool visibility, session listing, resume/abort). V2-enriched event fields (`IsolationScope`, `Output`, `Routes`, `RequestedInput`, `NodeInfo`) work if the TUI tolerates unknown JSON fields; else a small adapter is needed. V2-native features (HITL response-schema forms, planner turn detail, workflow-node visualization, federation/A2A task detail, snapshot/replay controls) surface only if the TUI has UI for them — mast delivers them via attach; rendering is the TUI's concern. `mast-web` remains the shipped + v2-feature-complete client.
- **Skills → specialists:** ~~core-agent's `pkg/skills/` (Anthropic-SKILL.md-compat loader) replaced in mast by `pkg/specialists/`.~~ *Reversed 2026-07-01: skills reinstated as first-class consumable alongside specialists (not replaced by them). Rationale: GKE + broader Google teams publishing skills as first-class artifacts inverted the audience-fit assumption behind the cut — mast operators are exactly the audience skill publishers are targeting. Specialists and skills coexist as complementary authoring models (specialists = mast-authored subagents; skills = consumed published templates). See [`./skills-design.md`](./skills-design.md) for the reinstatement design and [`./specialists-design.md`](./specialists-design.md) for the coexistence framing.*

**2026-07-01 (ADK v2 disposition):**

- **ADK v2 from day one.** The v2 release ships the graph engine (`google.golang.org/adk/v2/workflow`), durable HITL, and agent modes (`Chat` / `Task` / `SingleTurn`) that mast's positioning and workflow-scaffolding subsystem depend on structurally. Building on v1 first and migrating later would double the work.
- **v1→v2 migration disposition:** ~~phase 1's squash absorbs the migration on the pruned tree.~~ *Superseded 2026-07-01 (fork mechanic revision below): the lean core is written fresh against v2, no migration diff exists in mast's history.* The trigger is *not* extended to wait for core-agent to migrate — mast and core-agent diverge on substrate version, consistent with (E). See "Trigger condition for Phase 1" for the reasoning.
- **Task-class profiles shaped by v2 agent modes:** `--task=chat` → `Chat` mode; `--task=debug|implement|research|review` → `Task` mode; new `SingleTurn` profile pending naming (open question #3 above). Auto-installed helper tools (`finish_task`, `single_turn`, `task`) replace prompt-engineering we would otherwise have to do to signal completion. `task`-mode agents can't be used as static graph nodes — workflow-scaffolding examples use dynamic nodes (`RunNode[T]`) for Task-mode sub-agents.
- **HITL is a first-class primitive** on both plain `LlmAgent`s (v2 gain) and workflows, delivered via `RequestInputEvent` + attach mode + `mast-web`. Not workflow-wrapping. Response schemas (`session.RequestInput.ResponseSchema`) drive `mast-web` form generation. Cross-runtime resume (shared interrupt format with Python ADK) is a future-preserved option; don't design the attach-side protocol in a way that breaks compat, even though the Python side isn't v0.1 scope.
- **Workflow-scaffolding as a first-class subsystem.** Six + one canonical shapes (fan-out-fan-in, sequential pipeline, supervisor+workers, autonomous loop, adversarial verifier, map-reduce, LLM-as-router) expressed as reference graphs on v2 primitives. Mast's contribution is the domain wiring (which MCP servers / tools compose in), not the workflow engine itself. See [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md).
- **ADK-boundary sync discipline:** shared-infrastructure code that consumes ADK types (session store, provider adapters, watchdog signal routing, custom `InvocationContext` implementations) is flagged in the sibling-sync doc as version-boundary — ports across need adaptation, not straight cherry-pick. ADK-independent shared code ports cleanly. See "Sync discipline under (E)" above.

**2026-07-01 (fork mechanic revision):**

- **Phase-1 mechanic is rebuild-lean-core, not prune-in-place.** Superseding the original "hard-fork-then-prune, one squash commit" recommendation. Rationale: ADK v2 delivers most of what v1's core agent loop provided; porting that loop would carry v1 assumptions forward into code we'd want to rewrite anyway. Three buckets — bucket 1 rebuilds the lean core on v2 (~500-1500 LOC), bucket 2 ports adapter packages with per-file `// Originally derived from ...` attribution headers, bucket 3 builds mast-specific v2-native subsystems (specialists, reference graphs, autonomous+inbox-as-cyclic-graphs, watchdog signal-emission). See "Recommended approach" for the full trade-off table and "Phase 1" for the P1.1-P1.6 PR sequencing.
- **Provenance via attribution, not git history.** No cross-repo `git log --follow` chain; per-file attribution headers on bucket-2 ports point at the source SHA in core-agent. CHANGELOG v0.1.0 note carries the commit range. Same archaeological reach as the original prune-squash design (the squash was already a `git log --follow` boundary).
- **De-risk bucket 1 in the trigger-wait period.** During the wait for core-agent's #158-#161 + shared-memory to land, prototype bucket 1 in a scratch worktree to validate the ~1500 LOC estimate. If it balloons past ~3000 LOC, that's signal we've reintroduced v1's shape on top of v2 — stop and rethink before doubling down. *2026-07-25 status: two spikes have run (standalone `mast-prototype` repo — see [`./triage-demo-plan.md`](./triage-demo-plan.md) resolved open Q #1 — tags `spike1`/`spike2`, findings in its `FINDINGS.md` and folded into the corpus). Verified on ADK v2.1.0: workflowagent-wrapped graphs run as the runner root (the runner's Chat-mode restriction applies only to LlmAgent roots); durable HITL survives `kill -9` on ADK's own SQLite session service; resume is reconstruct-and-re-execute (see [`./durable-execution-design.md`](./durable-execution-design.md) side-effect semantics); usage metering + per-tool MCP filtering need no ADK changes. The working loader/builder/dispatch/inject/budget slice sits well under the LOC envelope. Bucket-1 pin: `google.golang.org/adk/v2 v2.1.0`.*
- **Phase 2 largely moots.** Original Phase 2 items (DefaultInstruction rewrite, tool catalog defaults, bash search-gate, watchdog routing) get baked into buckets 1/2 rather than deferred as a follow-on refactor pass. What remains of "Phase 2" is bucket-3 completion (reference-graph library, autonomous+inbox rewrite, concrete watchdog signal-emission, small-tier-parent classifier as LLM-as-router). Phase 3 unchanged in shape.

## Out of scope for this doc

- The actual name choice.
- A detailed line-by-line cut list (phase 1 produces it as the squash commit's diff; pre-listing it duplicates work).
- The docs-site IA / writing (Astro + Starlight per Phase 4).
- Pricing or commercial positioning.
- Whether to do the fork at all — that's a strategic decision the positioning doc doesn't resolve. This doc only covers *how* if the answer is yes.
