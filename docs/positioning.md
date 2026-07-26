# mast: positioning + scope

**Status:** draft, 2026-06-10 (updated 2026-07-01 — ADK v2 implications on task-class profiles, workflow scaffolding, and small-tier-parent classifier). Strategy doc, not a design doc — proposes the positioning thesis for `mast`, the lean fork of core-agent (per [`./fork-design.md`](./fork-design.md)). Concrete cut/keep/change-shape decisions follow from the thesis below.

Under the (E) — sibling-products motivation in [`./fork-design.md`](./fork-design.md), this doc is *mast's* positioning; core-agent retains its own (broader, experimentation-shaped) positioning, to be captured separately as that work lands.

## Thesis

**mast** is the agent-infrastructure substrate for **unattended, library-embedded, multi-provider, durable** workloads. It is explicitly *not* a Claude Code / Antigravity competitor. The dev-laptop interactive coding experience is downstream of model + IDE + training investment we cannot match; competing there loses on every axis. The cloud-native / headless / library shape is genuinely underserved and is where the moat lives.

The four pillars:

1. **Unattended.** Runs without a human watching.
2. **Library-embedded.** Composes as a Go library inside larger services, not just as a standalone binary.
3. **Multi-provider.** Same config switches between Gemini and Claude without code changes.
4. **Durable.** Sessions survive process restarts, pod restarts, cluster migrations, paused HITL waits, and budget exhaustion pauses. Work resumes where it stopped. (See [`./durable-execution-design.md`](./durable-execution-design.md).)

*Durable* was added 2026-07-01 after ADK v2 exposed session-durable pause/resume as a first-class primitive. Prior to v2, "unattended" implicitly assumed either idempotent workloads that could restart from scratch, or operator tolerance for lost work on infrastructure churn. Both are unacceptable for the platform-team workloads mast targets. Durable execution is the fourth pillar because *unattended without durable is unwatched but fragile*.

**On "library-embedded" specifically:** it names a *capability* mast commits to (few agent frameworks are cleanly embeddable), not a deployment mode that excludes the standalone binary. Mast ships **both** consumer shapes as equal first-class citizens — a `mast` binary for Cloud Run / GKE / systemd / laptop CLI use, and a Go library (`import "github.com/go-steer/mast/..."`) for host services that want agent capabilities inline. Same subsystems, same features, same durability + observability + orchestration in both; only the config-injection surface differs. [`./deployment-design.md`](./deployment-design.md) enumerates all four production topologies (Cloud Run, GKE, library-embedded, standalone) and [`./library-api-design.md`](./library-api-design.md) covers the library-consumer contract.

The 2026-06-10 debug session (`.agents/sessions/2026-06-10T13-58-07Z.json`, in the core-agent repo) is the proximate motivator: frontier Gemini ran 164 turns / $5.41 / 196K context on a code-investigation prompt that Claude Code with Opus handles in a handful of turns. That gap is real, structural, and the wrong fight to pick. mast sharpens scope around the fights this kind of substrate *can* win.

## What this means concretely

**We win when:**
- An agent runs inside a Cloud Run pod / Kubernetes operator / scheduled job, mostly unattended.
- An agent is a Go library compiled into a larger service.
- The same config switches between Gemini and Claude without code changes.
- Multi-tenant auth, audit logs, cost ceilings, permission gates are first-class requirements.
- The workload is platform/SRE-shaped — incident triage, deployment automation, monitoring, drift detection, runbook automation — not developer-shaped.

**We do not try to win when:**
- A developer is sitting at their laptop wanting brilliant code-edit UX.
- The user is doing open-ended exploratory bug hunts in unfamiliar codebases.
- LSP-style symbol/AST awareness is core to the workflow.
- The user already has Claude Code / Antigravity / Cursor and is happy.

## What stays

### Keep verbatim (user-pinned + critical infrastructure)

- **Interactive REPL** (small, useful for headless smoke testing and scripting). Embedded terminal TUI is *not* part of this scope — the interactive surface is web-based. User-confirmed 2026-06-11.
- **Attach mode (HTTP/SSE)** (`pkg/attach/`). User-pinned. Critical for operating unattended deployments — the *primary mast-native* interactive transport (richer than ecosystem protocols; carries workflow-node visualization, planner turn detail, federation cross-instance spans, snapshot/replay controls). `mast-web` is the shipped and v2-feature-complete client; the protocol itself is client-agnostic and accepts any conformant consumer (attach-mode TUIs including `core-tui`, custom CLIs, third-party UIs). Not shipping a TUI is a productization decision, not a compatibility wall — see [`./fork-design.md`](./fork-design.md) `core-agent-tui` disposition for the compatibility levels. **Attach mode coexists with AG-UI** ([`./ag-ui-design.md`](./ag-ui-design.md)) — attach mode for mast-native rich UX (mast-web); AG-UI for ecosystem-standard consumers (CopilotKit React apps, chat-platform bots).
- **Web UI (`mast-web`)** — thin client over the attach protocol, separate repo, statics embedded into the mast binary via `go:embed`. Replaces the terminal TUI for mast's audience. See [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md).
- **Specialists subsystem** (`pkg/specialists/`) — subagent-as-tool pattern using ADK's `agenttool`. `.tmpl` files with YAML frontmatter define specialists with budgets, model overrides, and tool allowlists. Replaces the skills surface from core-agent's scope. See `./specialists-design.md`.
- **Multi-provider abstraction** (`pkg/providers/`): Gemini, Vertex, Anthropic, Anthropic-Vertex, echo, scripted. The multi-provider moat.
- **Agent core + runner + scheduler + watchdog + checkpointer** (`pkg/agent/`). The actual loop.
- **Session DB + event log + audit** (`pkg/eventlog/`, durable sessions via sqlite). Governance moat.
- **Permission gate + path scope + URL scope** (`pkg/permissions/`). Governance moat.
- **MCP integration + transparent MCP wrap**. Every platform/SRE-shaped workload composes via MCP. The unattended moat at the tool-surface level.
- **Per-MCP credential resolution** (design doc shipped, v2.4). Multi-tenant requirement.
- **Multi-session core-agent** (per-user auth, cross-session isolation, v2.4). Multi-tenant moat at the runtime level.
- **Shared memory + audit-derived memory** (PRs #14, #15 stacked behind #13). Biggest single unattended differentiator. No other agent product I know of treats audit logs as memory.
- **Plan-first gate** (`cd199a1`). Much more valuable for unattended (no human to course-correct) than for interactive.
- **Watchdog + cost ceilings + small-tier-parent guard.** Guardrails that matter most when no human is watching.
- **Task class profiles** (`pkg/taskclass/`). Bundling pattern for opinionated defaults; extend aggressively (see "Change shape").
- **Background agents + spawn_agent + scheduler.** Workflow scaffolding for the gke-parallel-triage pattern.
- **Auto-continue / inbox.** The unattended-loop primitive.
- **Container images + cloud-deployment recipes** (`examples/cloud-run-deploy`, `examples/gke-deploy`). The deploy story.
- **Library API + agent-card publishing.** The embedding story. Agent-card publishing now concrete via A2A — see [`./a2a-design.md`](./a2a-design.md).
- **A2A protocol support (first-class).** Both server (expose workloads to Google Agent Registry, kagent, and similar) and client (invoke external A2A agents). Table stakes for platform-team-substrate positioning — without A2A, mast is invisible to cross-framework registries.
- **Federation across mast instances + protocols.** One-mast-as-coordinator dispatching to remote agents via A2A / mast-native / HTTP/RPC. See [`./federation-design.md`](./federation-design.md).
- **Skills subsystem (`pkg/skills/`, SKILL.md format).** Consume published skill bundles (Anthropic-SKILL.md format) from Google Agent Registry, GKE team, community, corporate internal catalogs. Reversal of the earlier cut — see [`./skills-design.md`](./skills-design.md) for rationale and [Change shape → Skills coexistence](#change-shape) for the framing update. Coexists with specialists as complementary authoring model (specialists = mast-authored subagents; skills = consumed published templates).
- **AG-UI protocol support (first-class).** Both server (expose workloads to CopilotKit React apps, chat-platform bots for Slack/Teams/Discord/Telegram/WhatsApp) and client (invoke external AG-UI-reachable agents). Table stakes for the platform-team-substrate positioning alongside A2A + MCP + skills — without AG-UI, mast is invisible to CopilotKit's chat-platform ecosystem, which is exactly the surface that gets us "@mention mast in an incident Slack channel" without building the Slack integration ourselves. See [`./ag-ui-design.md`](./ag-ui-design.md).

### Cut or de-emphasize

- **Investment in interactive-coding UX polish beyond "functional".** `edit_file`, `write_file` stay; no further investment in matching Claude Code's editing experience. No LSP integration, no AST tooling, no syntax-aware diff UI.
- **Effort to make Gemini-customtools good at open-ended bare-tool debug loops.** Per `docs/gemini-tier1-followup-plan.md:166` — the team itself concluded this needs workflow-shaped subagents, not better primitives. Route open-ended debug to Claude in core-agent; build out workflow scaffolding for Gemini's strengths.
- ~~**Skills subsystem** (`pkg/skills/` + ADK `skilltoolset`). Cut from mast's scope.~~ *Reversed 2026-07-01: skills reinstated as first-class consumable. GKE + broader Google teams publishing skills as first-class artifacts inverted the audience-fit calculus that motivated the original cut. See [`./skills-design.md`](./skills-design.md) for the reinstatement rationale and design; see the keep-list entry above. Specialists and skills now coexist as complementary authoring models — specialists for mast-authored subagents, skills for consumed published templates.*
- **Per-model prompt engineering for code search.** Measured ineffective for Gemini (`docs/gemini-tier1-followup-plan.md`), unnecessary for Claude. Don't invest further.
- **Examples that target the developer-coding-assistant shape.** `examples/basic`, `examples/with-tools` stay as smoke tests; we don't ship more in this shape.

### Change shape

- **DefaultInstruction** (`pkg/agent/agent.go:76`): today is a generic helpful-assistant frame + parallelism mandate + plan nudge. Should be reoriented around unattended-loop discipline: "conservative defaults; explicit state persistence to the eventlog; fail-fast on ambiguity; structured tool preference; plan-before-act required; subagents over open-ended search." Post-ADK-v2: split into per-mode variants — `Chat`-mode gets conversational framing for attach-mode / `mast-web` operators; `Task`-mode gets the opinionated unattended-loop frame; `SingleTurn`-mode gets minimal framing (used by LLM-as-router classifiers). The 4-paragraph generic-assistant prose becomes three role-shaped guidance blocks.
- **Default tool catalog.** `--task=implement` keeps the current broad set (edit/test cycle needs it). Every other task class defaults to a curated structured subset (per filed issue #160). Bash gets gated against search-shaped commands by default (per filed issue #158). Task-class profiles are shaped by ADK v2 agent modes: `--task=chat` → `Chat` mode; `--task=debug|implement|research|review|orchestrate` → `Task` mode. SingleTurn mode is internal (classifier-first workload dispatch, `mode: SingleTurn` specialists, LLM-as-router classifiers) rather than a public task class — see [`./orchestration-design.md`](./orchestration-design.md).
- **Unattended dispatch is bundle-driven, not CLI-flag-driven.** The `--task=X` flag stays for laptop / library-embedder use, but unattended entry points (HTTP webhooks, queue consumers, scheduled jobs) resolve task class + operational profile from **workload bundles** — declarative YAML files under `.agents/workloads/*.yaml` naming specialists, tool catalog, budgets, HITL policy, and whether the planner runs. Four resolution paths (explicit / envelope / bundle-selection / classifier-first) coexist. See [`./orchestration-design.md`](./orchestration-design.md).
- **README + Hugo site entry points.** Lead with unattended / platform / library. The "I'm a developer who wants Claude Code" reader should be told within 30 seconds: *"Use Claude Code; this isn't that."* The "I'm a platform team deploying agents into Cloud Run / Kubernetes" reader should see their use case in the first screen. *(Added 2026-07-25: third routing pointer — the "I just want one simple agent loop in Go, no governance, no durability" reader gets sent to **raw ADK v2** with the same 30-second honesty, plus the specific return triggers: come back when the loop must survive restarts, needs budget/permission governance, needs provider switching, or stops having a human watching it. See "Smaller agents, slim embeds" below.)* Both surfaces need a rewrite, not a tweak.
- **Examples directory.** Rebalance toward platform/SRE/library patterns. Target ratio: 70% unattended (GKE triage, Cloud Run, scheduled monitor, library embedding, MCP runbooks), 30% interactive (web UI, plan-first, attach-mode driving). Today's mix skews the other way.
- **Subagent / spawn_agent / workflow scaffolding.** This becomes first-class, and — post-ADK-v2 — is delivered as **reference graphs on v2's workflow package**, not as a helper layer above the runtime. Seven canonical shapes shipped as `examples/workflows/<shape>/`:
  - Fan-out-fan-in (`gke-parallel-triage` becomes the reference implementation)
  - Sequential pipeline (extract → transform → propose)
  - Supervisor + workers (long-running orchestrator dispatching scoped tasks; multi-tenant story lands here)
  - Autonomous loop (scheduled monitor pattern; v2 cyclic graph replaces `pkg/agent/autonomous.go` custom loop machinery)
  - Adversarial verifier (proposer + skeptic pattern — useful for unattended decision-making)
  - Map-reduce over corpus (digest-summarize many inputs into one output; audit-derived-memory backfill is a real instance)
  - LLM-as-router (classifier dispatches to per-category specialist; also replaces the substring-matcher small-tier-parent classifier — see open question #4 below)

  Mast's surface for this priority collapses from "helper packages + examples" to "domain wiring on v2 primitives (which MCP servers / specialists / tools compose in)." See [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) for the full design, per-shape v2-primitive mapping, and composition rules.
- **Watchdog routing.** Per filed issue #159, alerts should reach the model's next-turn context, not just operator UI. Critical for unattended where there is no operator.

## What this implies for the next 6 months

In rough priority order:

1. **README + Hugo site repositioning sweep.** First impression matters most. Lead with unattended / platform story. Cheap, immediate, sets the frame for everything else.
2. **Land filed issues #158-#161.** Compose to "make Gemini less bad at the failure shape we care about" for cases where Claude isn't available. (Post-v2 note: watchdog→model routing per issue #159 gets a cleaner shape — watchdog runs inside an emitting function node and injects alerts into the session event stream; no separate transport.)
3. **Ship shared-memory design (PRs #13/14/15).** Biggest single moat-builder in the queue.
4. **Build out the workflow-scaffolding reference-graph library.** Seven canonical shapes on ADK v2 primitives, each with example + doc page. See [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md).
5. **Multi-session deployment story end-to-end (v2.4).** Walk-through from "single dev" to "shared team deployment with per-user auth and audit isolation." (Post-v2 note: supervisor+workers with `WithIsolationScope(tenantID)` is the concrete shape — see workflow-scaffolding doc.)
6. **MCP credential resolution end-to-end.** Design doc → implementation → docs page. Multi-tenant story closed loop.
7. **Audit-derived-memory implementation.** Thesis lives in shared-memory design; needs implementation work to become real. (Post-v2 note: consumer nodes read via state-bound nodes (`state:"<key>"` tags); the derivation pipeline is a map-reduce reference-graph instance.) Direct enabler for workload-bundle learning (see [`./orchestration-design.md`](./orchestration-design.md)) — the same audit-derived-memory pipeline that surfaces operational memory also proposes new workload bundles and refines existing ones.

**Explicitly off this list:**
- Better Gemini code-search prompts (measured: doesn't move).
- LSP integration / AST tooling.
- Skill ecosystem for developer-laptop workflows.
- Polishing interactive edit UX beyond functional.
- Any work whose primary motivation is "to compete with Claude Code on axis X."

## Smaller agents, slim embeds (added 2026-07-25)

Spike 2 surfaced a real audience segment: people who need one purpose-built control loop or a simple one-shot flow — the shapes the spikes themselves are — for whom mast's full surface is conceptual overhead. Resolved answer: **serve them with three cheap moves inside mast; do not create a product tier below it.**

1. **Forkable starters.** The reference-graph library's shapes are starters people fork into purpose-built agents, standalone-runnable and self-contained ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) "Shapes are forkable starters"). `mast-prototype` is the first instance. Fork-and-forget lifecycle; no release track.
2. **Slim embeds.** "Pay for what you import" becomes a tested v0.1 library guarantee with a reference consumer and CI enforcement ([`./library-api-design.md`](./library-api-design.md) "Slim-embed guarantee"). The minimal-control-loop user starts *inside* mast; adding durability/budgets/HITL later is an import, not a migration.
3. **Honest routing.** The truly-simple-loop reader is routed to raw ADK v2 (README pointer above) — that niche is ADK's, and wedging a mast-branded thin agent between ADK and mast would shrink every time ADK improves.

Explicitly rejected: prebuilt single-purpose binaries (`mast-triage`, `mast-monitor`, …) — a standing support surface with none of the substrate's leverage, and a third product muddying the two-sibling story under (E). Convergence check: if starters get real adoption, the "which flow do I need?" question grows into the planner/`orchestrate` story (v0.2+) — the path leads back into mast, which is the tell that this is the right shape.

## Open questions

1. **Where does AX fit?** Per `reference_ax_runtime`, AX is the distributed-runtime layer above core-agent. As core-agent sharpens around unattended single-process, the boundary with AX gets clearer — but also raises *"should some of what core-agent does today move up to AX?"* (Background agents? Multi-session coordination? Cross-process inbox?) Needs a dedicated audit.
2. **MCP server catalog: build or consume?** Should core-agent ship its own MCP servers (Prometheus, Cilium, Istio, GCP IAM/Logging) or stay a substrate that consumes others'? Probably the latter, but gke-parallel-triage shows there's value in shipping the *wiring config* even when servers are external.
3. ~~**Is "interactive mode + TUI" a long-term commitment or a transitional one**...~~ *Resolved 2026-06-11: interactive surface is long-term; shape is web UI over attach mode, not embedded terminal TUI. See [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md).*
4. **How does the small-tier-parent classifier age** as `gemini-3.5-flash` lands and `gemini-3.5-pro` GAs? The substring matcher needs revisiting (filed issue #161 begins this). *2026-07-01 update: ADK v2's `SingleTurn` agent mode is a natural replacement — a lightweight LlmAgent classifier on flash, invoked as the LLM-as-router shape ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)), ages gracefully as model IDs change and shifts the question from "can our substring matcher keep up" to "which model should the classifier target." The substring-matcher goes when the classifier lands.*
5. **Canonical positioning name.** "Agent infrastructure" is generic. "Platform agent runtime" is closer. "Agent substrate" is what design docs use internally. Naming matters for the README sweep — pick one and use it consistently.

## What gets simpler if we commit

A non-trivial chunk of code, docs, and example surface exists today because we haven't picked which fight is ours. Committing to this thesis means:

- We stop apologizing for being less polished than Claude Code on dev-laptop UX.
- We stop measuring ourselves against Antigravity's IDE experience.
- We get permission to be sharper about which models work for which task classes (route to what works; stop trying to make everything work everywhere).
- Documentation tells a coherent story instead of trying to address every possible reader.
- Roadmap prioritization becomes mechanical: does this work serve unattended/library/multi-provider/governance? If yes, ship. If no, defer or cut.

The cost of the commitment is small: mostly the README sweep and a discipline about what we say yes to. The value is large: an actual position in the market instead of a diffuse one.

## Out of scope for this doc

- Specific API or schema designs for any of the keep/change-shape items (each has its own design doc track).
- Pricing / commercial positioning.
- Cross-product synergies with AX, Cogo, or other adjacent codebases (referenced where load-bearing; not designed here).
- Marketing copy. The README sweep is real work but the *what* it should say is what this doc resolves, not the *how*.
