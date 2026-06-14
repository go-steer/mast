# mast: fork design

**Status:** draft, 2026-06-11 (updated 2026-06-13 — moved into `go-steer/mast/docs/` from core-agent's worktree). Companion to [`./positioning.md`](./positioning.md) (the thesis), [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) (the operator-facing UI), and [`./specialists-design.md`](./specialists-design.md) (the subagent-as-tool subsystem). This doc covers the *mechanics* of extracting `mast` from current core-agent. Reading the positioning doc first is assumed — this doc takes the keep/cut/change-shape decisions as given and answers *how* to make them happen via a fork.

## The strategic question that determines everything

**Why fork instead of refactoring core-agent in place?**

The mechanical work is similar — same set of code gets deleted either way. The right shape of the answer is wildly different depending on which of these motivates the move:

| Motivation | Fork shape |
|---|---|
| **(A)** Backward compat for existing users. core-agent stays for current consumers; lean fork is a new product for a new audience. Both maintained indefinitely. | Two repos, two release tracks, ongoing sync discipline. Highest maintenance burden. |
| **(B)** Branding / market reset. core-agent name is too generic or too associated with kitchen-sink; the fork is the same team's sharper-branded successor. Eventual deprecation of core-agent. | One repo becomes primary, the other enters maintenance-only mode. Migration story for existing consumers. |
| **(C)** License / governance / ownership reset. Something about the current project (ADK dependency, contributor agreement, org membership, etc.) is a constraint the fork escapes. | Hard cutoff. No sync back. Existing core-agent likely continues with original constraints; fork lives independently. |
| **(D)** Refactor-in-place would touch too much at once and risk breaking everyone simultaneously. Fork is the "staging area" for the cleanup; once stable, it replaces core-agent. | Temporary two-repo state. Fork lands, soak period, then `core-agent` repo gets archived/redirected. |
| **(E)** Sibling products with divergent agendas. The lean fork targets unattended/platform agents. core-agent stays alive as the experimentation / integration substrate (cogo-shaped consumers — embedded use, richer interactivity, kitchen-sink acceptable). Different jobs, not different cohorts. Both maintained indefinitely. | Two repos, each with its own positioning, README, audience, and roadmap. Shared maintenance discipline on the *intersection* (bug fixes, security); deliberately divergent on features. |

**Resolved (2026-06-11): the motivation is (E) — sibling products with divergent agendas.** The lean fork is the platform-agent product (per `./positioning.md`); core-agent stays alive as the experimentation/integration substrate for cogo and similar embedded consumers. Both have a future. This is meaningfully different from (A) — both maintained — because the two have *different jobs*, not just different user cohorts for the same job. That distinction reduces the sync burden (less overlap to keep in lockstep) but raises the bar for clear positioning so users know which to reach for.

## Recommended approach: hard-fork-then-prune, single squash commit

Three viable mechanics:

1. **Hard fork.** `git clone` the repo, rename, start deleting on a fresh branch. History preserved verbatim. Pro: full provenance. Con: every git log entry references files that no longer exist; the diff to land the prune is huge.
2. **Code extraction.** Create empty repo, copy in the keep list, write a fresh initial commit. Pro: clean history matching the new scope. Con: loses provenance entirely; tests/CI/coverage start from zero.
3. **Hard-fork-then-prune in one squash commit (recommended).** Hard-fork to preserve history, then land all cuts in a single squash commit titled `chore: prune to lean scope (forked from go-steer/core-agent@<SHA>)`. Pro: provenance preserved, but history *after* the fork point reads cleanly against the new scope. Con: the squash commit is enormous.

The squash is worth it. Anyone doing archaeology can `git log --follow` back through the squash into the original history. Anyone working forward sees a clean tree.

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
| ADK dependency | **Keep.** No concrete pain; provides known working code. Revisit only if a specific bug or limitation surfaces. | Replacing `google.golang.org/adk` would be 2-3 months of careful work (tool-call correlation, parallel function calls, streaming delta protocols, content-part ordering rules — all the fiddly translation ADK does between Gemini and Anthropic semantics). The lean thesis is about *positioning + opinionated defaults*, not about owning every line of the substrate. Owning ADK's job is a separate decision that needs its own trigger. |
| Backward-compat surface | None (clean break) | Existing consumers consume the original core-agent for as long as they need to. The fork doesn't promise import-path stability with core-agent. |
| CI / release infra | **Independent at start.** Port `dev/ci/presubmits/*` as-is; lean fork owns its own GitHub Actions workflows and release pipeline from day one. | Presubmits are the project's quality bar; carry them over. Shared workflow infrastructure (one source, both repos consume) is the more elegant long-term option but couples release cadences and isn't worth the operational overhead at start. Revisit at the 6-12 month mark alongside the shared-infrastructure-repo question. |
| Hugo site | Fresh Hugo site, not a port | Site docs are too entangled with the old positioning; cheaper to rewrite. The library + design docs in `docs/` port over. |

## Trigger condition for Phase 1

The lean fork starts *after* the following in-flight work lands in core-agent:

1. **Issues #158-#161** (bash search-gate, watchdog→model routing, `--task=debug` profile extensions, gemini-3.5-flash probe). Small, scoped, apply equally to both products. Land in core-agent first because they belong there *and* because the lean fork inherits stronger baseline behavior.
2. **Shared-memory stack (PRs #13/14/15).** Audit-derived memory is core to the lean fork's value proposition, but it's also a real feature core-agent's experimentation/integration consumers want. Land in core-agent first; the lean fork inherits a real-soak-tested implementation at fork time rather than building it twice or porting half-finished.

Starting phase 1 before this work lands means doing it twice (once in each repo); starting after means the fork ships with a stronger v0.1. The few-week delay is worth it.

**Note: `mast-web` work doesn't gate on this trigger.** Phases A+B+C of [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) consume the attach protocol that already exists in core-agent, so frontend work can proceed in parallel — built against `core-agent --attach-listen` today, repointed at mast at fork time. See [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md)'s Phasing section for the dependency table.

## Phase 1: fork-then-prune (1 PR, ~1 day)

One PR titled `chore: prune to lean scope (forked from go-steer/core-agent@<SHA>)`. Contents:

1. **Rename module path everywhere.** `go mod edit -module github.com/<org>/<name>`; sed all imports. One mechanical commit.
2. **Delete cut-list items per `./positioning.md`** (see "Concrete delete list" below). One squash commit.
3. **Trim transitively-orphaned code.** Anything depending only on deleted code gets removed. Iterate until `go build ./...` is clean.
4. **Update CHANGELOG.md** to mark v0.1.0 with the fork note.
5. **README rewrite** to lean positioning (per `./positioning.md`).
6. **Update DESIGN.md** to reflect the trimmed surface.

**Concrete delete list** (initial pass — refine during phase 1):

- Any package or example targeting developer-laptop interactive-coding UX polish.
- LSP / AST tooling references (none today, just preventative).
- Documentation under `docs/site/content/docs/` that targets the developer-coding-assistant reader (port the rest fresh; see Phase 4).
- Any model-specific prompt-engineering scaffolding for code search beyond what's load-bearing (per `docs/gemini-tier1-followup-plan.md` — measured ineffective).
- `examples/basic`, `examples/with-tools` — keep as smoke tests in `examples/_smoke/` (or similar) but stop recommending them as starting points. README points new readers at platform/SRE-shaped examples instead.
- **`pkg/skills/` + `adk/tool/skilltoolset`** — skills subsystem cut. The "callable task template" use case is replaced by the new specialists subsystem (`pkg/specialists/`) using ADK's `agenttool` pattern. The Anthropic-SKILL.md-compat shape stays in core-agent for its audience. See `./specialists-design.md` for the replacement schema and loader design.

**Notably *not* in the delete list** (resolved 2026-06-11, "parts of all"):

- `pkg/usage/`, `pkg/pricing/` — cost tracking matters in both products.
- `pkg/tools/agentic/` — Mechanism B wrappers stay; they're the right shape for unattended where context budget matters most.
- `pkg/digest/` — required by agentic wrappers; stays.
- `pkg/agent-card/` (agent-card publishing) — useful for unattended deployment discovery; stays.

**Concrete keep list** — explicit so phase 1 has a definite stopping point:

- `pkg/agent/` (loop, runner, scheduler, watchdog, checkpointer, compactor, autonomous, inbox)
- `pkg/providers/` (multi-provider abstraction)
- `pkg/tools/` (built-in tool surface)
- `pkg/tools/agentic/` (Mechanism B wrappers)
- `pkg/permissions/` (gate, path scope, URL scope)
- `pkg/eventlog/` (durable sessions / audit)
- `pkg/config/`, `pkg/modeltier/`, `pkg/taskclass/`, `pkg/usage/`
- `pkg/attach/` (HTTP/SSE attach mode)
- `pkg/mcp/` (MCP client + transparent wrap)
- `pkg/instruction/` (instruction loader v2)
- `pkg/digest/`, `pkg/pricing/`
- **`pkg/specialists/`** (new — replaces `pkg/skills/`) — subagent-as-tool loader for `.tmpl` files under `.agents/specialists/`. See `./specialists-design.md`.
- `cmd/core-agent/` → rename to `cmd/<new-name>/`
- Companion repo `core-agent-tui` consumed at current pinned version (no fork of it required v0.1; revisit if the lean repo's needs diverge)
- `examples/gke-deploy`, `examples/gke-parallel-triage`, `examples/cloud-run-deploy`, `examples/scheduled-monitor`, `examples/autonomous`, `examples/plan-first`, `examples/streaming`, `examples/replay`, `examples/autonomous-handle`
- `docs/` design docs (the positioning + lean-fork docs migrate first; rest come over selectively in phase 4)
- `dev/ci/presubmits/`, `dev/uat/`, `dev/parallel-probe/` (testing infra)

The goal of phase 1 is **`go build ./... && go test ./... && dev/ci/presubmits/* ` all green on the new tree.** No new features, no DefaultInstruction rewrite yet. Just establish the trimmed baseline.

## Phase 2: refactor / boundary cleanup (2-4 PRs, ~1-2 weeks)

With the surface trimmed, opportunities to simplify boundaries become visible:

1. **Package consolidation.** Some `pkg/` boundaries were drawn assuming a larger team or larger surface. Re-evaluate; merge anything that's a thin wrapper.
2. **DefaultInstruction rewrite** per `./positioning.md` change-shape section. Reorient around unattended-loop discipline.
3. **Tool catalog defaults per task class** (issue #160 from `worktree-debug` session). `--task=implement` keeps broad set; other classes default to curated structured subset.
4. **Bash search-gate** (issue #158). Composes with tool catalog defaults.
5. **Watchdog → model context routing** (issue #159). Composes with both above.

Phase 2 changes are visible to consumers — bump to v0.2.0 at the end.

## Phase 3: new investment (ongoing)

What the lean repo focuses energy on, in rough priority (matches `./positioning.md`):

1. Shared memory + audit-derived memory implementation
2. Workflow-scaffolding example library (4-6 canonical shapes)
3. Multi-session deployment story end-to-end
4. MCP credential resolution end-to-end
5. AX-boundary audit + integration

This phase is "what was the point of the fork." Lands at v0.5.0+ — by which time the ADK-dependency question (deferred from phase 0) should be revisited with phase 1-2 hindsight.

## Phase 4: Hugo site + outward-facing rewrite (parallel with phase 3)

Fresh Hugo site, not a port. Targets:

- **Landing page.** "Agent infrastructure for unattended / library / multi-provider workloads. Not a Claude Code competitor." Top-of-fold message.
- **Quickstart.** Three flavors: (1) library embedding, (2) GKE platform agent, (3) interactive REPL + web UI (`mast-web`). The first two are the moat; the third keeps the user-pinned interactive story via browser rather than terminal.
- **Reference docs.** Port from old site selectively. Anything that assumed the broader scope gets rewritten.
- **Migration page** for existing core-agent users (more critical under **(B)/(D)** than **(A)**).

## What happens to the old `core-agent` repo

Under **(E)** — the resolved motivation — core-agent stays a first-class, actively developed project with its own positioning and agenda. Not maintenance-only, not deprecated, not archived. Specifically:

- **README on `core-agent` repo gets re-positioned** to make its job clear: *"core-agent is the experimentation and integration substrate for embedding agents into Go programs (cogo-shaped consumers). For unattended/platform agent runtime, see <fork-name>."* The repo stops trying to address every reader.
- **Both repos ship releases on their own cadence.** Neither is gated on the other.
- **The lean fork's README reciprocates:** *"For embedded/experimental agent integration with richer interactivity surface, see core-agent."* So a developer landing on either knows what they have and where to go if their use case doesn't fit.
- **Cross-references in docs.** Each project's design docs that touch on the boundary (e.g. positioning) name the sibling explicitly so readers don't have to deduce the relationship.
- **No EOL for core-agent.** It serves a real audience (cogo and similar embedded experiments); that audience doesn't go away because the lean fork exists. The two products coexist because they have genuinely different jobs.

## Sync discipline under (E)

Two indefinitely-maintained repos with divergent agendas need real discipline. The good news: divergent agendas mean *less* overlap requiring sync than (A) would. The bad news: the discipline never ends.

**Categorize every change.** Each commit in either repo falls into one of four buckets:

| Category | Sync rule |
|---|---|
| **Shared infrastructure** — bug fixes in code that lives in both repos (provider adapters, eventlog, permission gate, etc.) | Land in *whichever repo finds it first*, then port within ~1 week. Bidirectional, but the direction is determined by where the bug was reported, not a fixed rule. |
| **Security fix** | Land in both within 48 hours. No exceptions. |
| **Lean-fork-specific feature** (workflow scaffolding, audit-derived memory, opinionated task profiles, watchdog→model routing) | Lean fork only. Not ported to core-agent unless someone there explicitly asks for it. |
| **core-agent-specific feature** (richer interactivity surface, experimental skills/slash commands, cogo-shaped integration helpers) | core-agent only. Not ported to lean fork. |

**Track sync state explicitly.** Each repo carries a `docs/sibling-sync.md` (or `cross-repo-sync.md`) listing:
- Shared-infrastructure SHAs ported in either direction (with date)
- Shared-infrastructure SHAs explicitly *not* ported, with a one-line reason
- Security-fix correlation table

This is more discipline than weekly cherry-pick batches; the upside is that diverging features stay genuinely divergent without anyone forgetting a critical bug fix.

**Avoid the failure mode** of letting "shared infrastructure" creep until it's most of the codebase. Concretely: when adding a new feature to the lean fork, default to *not* touching shared-infrastructure code; extend in the lean-fork-specific layer when possible. Same in reverse for core-agent. This keeps the shared core small enough to sync confidently.

**Optional, longer-term:** if the shared-infrastructure layer stays meaningfully large after 6-12 months, consider extracting it to a third repo (`agent-substrate` or similar) that both projects depend on. Don't do this on day one — premature extraction couples the two projects' release cadences in a way that defeats the point of (E). Wait until the shared surface has stabilized enough that a separate release cadence wouldn't slow either project down.

## Risks + mitigations

| Risk | Mitigation |
|---|---|
| **Two-repo confusion for users** during transition (any of A/B/D) | Clear banners on both READMEs; pick a date and stick to it; over-communicate. |
| **ADK dependency keeps the kitchen sink in via transitive deps** | Phase 0 decision is to defer; phase 3 revisit. Measure first (`go mod graph | wc -l` before vs. after the prune) — may matter less than expected. |
| **Phase 1 squash commit is huge and unreviewable** | Acceptable cost. The squash commit's *job* is to establish a baseline, not to be human-reviewable. The diff inside it is mechanical. Anyone reviewing should look at the resulting *tree*, not the diff. |
| **Examples that worked under core-agent break under the lean fork** | Catch in phase 1 via `go build ./examples/...`. Any example that breaks is either ported or moved to the cut list with a one-line CHANGELOG note. |
| **Existing core-agent issues + PRs orphaned** | Triage at fork time. Issues that apply to the lean scope get ported as fresh issues in the fork (linking back). PRs that target deleted code are closed with a note. |
| **Loss of contributor momentum** during the transition | Communicate the fork plan publicly *before* phase 1 lands; give contributors a heads-up so in-flight work isn't wasted. |
| **Naming collision / SEO confusion** | Pick a name distinctive enough to be findable. Avoid prefixes/suffixes on "agent" — too generic. |

## Open questions to resolve before phase 1

1. **What about AX integration?** Per `./positioning.md` open question — boundary needs an audit. Probably out of scope for v0.1 but worth scoping the audit.
2. **Does core-agent's own README/positioning get updated as part of the fork landing**, or as a separate effort? Bias: update at the same time, so users landing on either repo immediately see the sibling and know which fits their use case. Inconsistent positioning across the two repos is the most common failure mode of (E)-style splits.

(For `mast-web`-specific open questions — TypeScript-or-vanilla, framework adoption trigger, hosting model, slash command alignment — see [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md).)

### Resolved (2026-06-11)

- **ADK:** stays. No concrete pain; provides known working code.
- **Strategic motivation:** (E) — sibling products with divergent agendas. Lean fork = platform-agent product; core-agent = experimentation/integration substrate for cogo and similar.
- **In-flight work disposition:** all in-flight work lands in core-agent first. Both products benefit; lean fork inherits stronger baseline.
- **Phase 1 trigger:** after #158-#161 AND shared-memory stack (#13/14/15) land in core-agent.
- **Contributor communication:** moot — single team owns both repos. No public announcement plan needed.
- **Gray-area packages:** "parts of all" — usage/pricing, agentic wrappers, digest, agent-card all stay. Skills replaced by specialists subsystem (subagent-as-tool pattern with richer per-specialist config; see `./specialists-design.md`).
- **CI/release infra:** independent at start. Each repo owns its workflows from day one. Revisit at 6-12 months alongside shared-infrastructure-repo question.
- **Project name:** `mast`. Repo at `github.com/go-steer/mast`. Binary `mast`. Available, no collision in the agent/AI space.
- **Interactive UI:** web, not terminal. Embedded terminal TUI dropped from mast's scope (the use case lives in core-agent, which keeps `core-agent-tui` as before). New project `mast-web` at `github.com/go-steer/mast-web` ports cogo-wasm2's rendering surface as a thin client over mast's existing attach-mode protocol. Architecture pattern is "browser-as-thin-client, mast-as-backend-agent" — *not* cogo-wasm2's "browser-WASM-as-agent + auth-proxy" pattern, which fits cogo's job but is structurally wrong for mast. See [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md).
- **`core-agent-tui` disposition:** not forked. Mast doesn't ship a terminal TUI. Core-agent keeps `core-agent-tui` for its audience.
- **Skills → specialists:** core-agent's `pkg/skills/` (Anthropic-SKILL.md-compat loader) replaced in mast by `pkg/specialists/` (subagent-as-tool pattern with YAML frontmatter for budget/model/tool-allowlist). See `./specialists-design.md`.

## Out of scope for this doc

- The actual name choice.
- A detailed line-by-line cut list (phase 1 produces it as the squash commit's diff; pre-listing it duplicates work).
- The Hugo site IA / writing.
- Pricing or commercial positioning.
- Whether to do the fork at all — that's a strategic decision the positioning doc doesn't resolve. This doc only covers *how* if the answer is yes.
