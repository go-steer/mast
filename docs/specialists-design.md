# mast specialists: design

**Status:** draft, 2026-06-11 (updated 2026-07-01 — ADK v2 resolves several open questions and reshapes the schema slightly; skills reinstated as first-class alongside specialists, so this doc is no longer framed as "the replacement for skills". Updated 2026-07-25 — spike-2 pass: allowlist semantics rewritten as one normative table with per-field presence, `ToolAllowlist` sketch fixed accordingly (+ `Skills` field), per-tool MCP filtering resolved as stock ADK, budget-substrate findings folded into open Q #2, pre-reversal migration section replaced). Companion to [`./fork-design.md`](./fork-design.md) (bucket 2 ports both `pkg/skills/` and `pkg/specialists/` — see [`./skills-design.md`](./skills-design.md) for the reinstatement rationale), [`./positioning.md`](./positioning.md) (the thesis), [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) (the reference-graph library that instantiates specialists as agent nodes), and [`./skills-design.md`](./skills-design.md) (the complementary authoring model — specialists for mast-authored subagents; skills for consumed published templates). This doc covers the specialist subsystem in detail — schema, loader, registration mechanics, and the open questions to resolve before phase 1 of the fork.

## Why specialists exist (and how they coexist with skills)

Specialists are one of two authoring models mast supports for callable-task-shaped work. The other is **skills** ([`./skills-design.md`](./skills-design.md)) — Anthropic-SKILL.md-format bundles consumed from publishers (Google Agent Registry, GKE team, community, corporate catalogs). Both surface to the planner as callable tools; the choice is *authoring model*, not consumer scope:

- **Specialists** = mast-authored subagents. Full control over budget, HITL policy, agent mode, tool allowlist, model override. Written for a specific deployment; live in the operator's config repo.
- **Skills** = declarative task templates in a format-portable spec. Consumed from publishers; format works across Claude Code, other agent frameworks, and mast.

Reach for specialists when you're authoring for a specific deployment and want mast-native control; reach for skills when you're consuming a published template. Most workloads use both.

**Historical note.** An earlier design (2026-06-11) cut skills from mast's scope and framed specialists as *the* replacement, arguing skills were developer-laptop-shaped and not fit for mast's platform-team audience. That was reversed 2026-07-01 when GKE + Google teams began publishing skills as first-class artifacts for platform work — mast's audience is exactly the audience skill publishers are targeting. See [`./skills-design.md`](./skills-design.md) for reinstatement rationale.

Regardless of which authoring model you use, the *structural* reason for the specialist pattern (subagent-as-tool with own context, budget bounds, mode-appropriate helper tools like `finish_task`) remains: it's the right shape for delegated task execution under ADK v2. Specialists deliver that shape with mast-native controls; skills deliver a compatible shape via the SKILL.md format.
- The "agents as tools" pattern is already proven in [mastersingh24/gke-agent](https://github.com/mastersingh24/gke-agent), where `.tmpl` files under `templates/sub-agents/` are auto-loaded at startup, each wrapped via `llmagent.New` + `agenttool.New`, and added to the root orchestrator's tool list.

Mast extends that pattern with a richer per-specialist config (budget, tool allowlist, model override) so operators don't have to choose between minimal-and-not-enough or having-to-write-Go for non-default specialists.

## File layout

```
.agents/specialists/
  ImagePullBackOff.tmpl
  CrashLoopBackOff.tmpl
  OOMKilled.tmpl
  ServiceEndpointMissing.tmpl
  ...
```

One file per specialist. Filename (without `.tmpl`) becomes the default name. Each file is YAML frontmatter followed by the specialist's system prompt.

## Schema (v1)

```yaml
---
# Identification (description required; name defaults to filename)
description: |
  Diagnoses ImagePullBackOff pod failures. Invoke when a pod reports
  image pull errors in its events. Returns a 3-bullet digest:
  failing image, specific error, concrete remediation.

# Budget — all optional, with sensible defaults
budget:
  max_turns: 5                       # default: 5
  max_wallclock_seconds: 60          # default: 60
  max_cost_usd: 0.50                 # default: 0 (no specialist cap; inherits session ceiling)

# Agent mode (optional; defaults to Task)
# ADK v2 modes; Chat is deliberately not exposed for specialists
# (specialists are sub-agents, not coordinators).
mode: Task                           # options: Task, SingleTurn

# Model override (optional; inherits parent if absent)
model: gemini-2.5-flash              # full model ID; provider inferred from parent's config

# Tool allowlist (optional; specialist inherits ALL parent tools if absent)
tools:
  builtin:                           # core-agent built-in tools
    - read_file
    - grep
  mcp:
    - server: gke
      tools:                         # explicit allowlist within this server
        - get_k8s_resource
        - describe_k8s_resource
        - get_k8s_logs
        - list_k8s_events
    - server: prometheus             # `tools:` empty/absent = all tools from this server
  skills:                            # skills the specialist may invoke (see skills-design.md)
    - gke-triage                     # local .agents/skills/gke-triage.skill/
    - google://gke-team/incident-triage@v1.2   # registry-discovered
---

You are a specialist for diagnosing ImagePullBackOff pod failures in Kubernetes.

OBJECTIVE: Identify the root cause of an ImagePullBackOff and return a 3-bullet
digest:
1. The image that failed to pull
2. The specific error (auth, network, missing tag, wrong registry, etc.)
3. A concrete remediation step

INPUTS: The parent will tell you which pod/namespace to investigate. Use only the
tools available to you. Do not attempt mitigations yourself — return analysis only.
```

### Field reference

| Field | Type | Default | Notes |
|---|---|---|---|
| `description` | string | **required** | Used by parent for tool selection. The parent's prompt sees this as the tool's description; phrase it as guidance for "when to invoke this specialist." |
| `name` | string | filename without `.tmpl` | Override if filename can't carry it (e.g. non-identifier characters). |
| `budget.max_turns` | int | 5 | Specialist's internal LLM turn count cap. After this many turns, the specialist must return whatever it has. Matches `spawn_agent` default. |
| `budget.max_wallclock_seconds` | int | 60 | Hard wall-clock cap; cuts off the specialist regardless of turn count. |
| `budget.max_cost_usd` | float | 0 | Per-invocation cost ceiling. 0 = inherit session ceiling. Useful for cheap-but-frequently-invoked specialists where bounded per-call cost matters more than session-level. |
| `mode` | string | `Task` | ADK v2 agent mode. Options: `Task` (default; auto-installs `finish_task` — specialist returns via `finish_task` argument), `SingleTurn` (one call, no `finish_task` needed — useful for lightweight classifier specialists consumed by the LLM-as-router shape in [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) and by workload-bundle classifier-first dispatch in [`./orchestration-design.md`](./orchestration-design.md)). `Chat` is deliberately not exposed — specialists are sub-agents, not coordinators. |
| `model` | string | inherit parent | Full model ID (`gemini-2.5-flash`, `claude-haiku-4-5`, etc.). Dispatched by model id, exactly like `--model`; the parent's provider alias only disambiguates the Anthropic backend, so a cross-provider override is legal (open Q#4, resolved 2026-08-12). Resolution is memoized per id, and an override that cannot be resolved fails the build rather than silently inheriting the parent's model. Under an offline-fake parent (`echo` / `scripted` / `toolactor`) every override collapses back to the parent so tiered bundles still run credential-free. Common pattern: frontier parent dispatching to cheap-tier specialists for high-volume tasks. |
| `tools.builtin` | []string | inherit all | Allowlist (not denylist) of core-agent built-in tools. Absent = inherit all; present-but-empty = deny all builtins (see the normative table below — empty and absent are NOT equivalent, revised 2026-07-25). |
| `tools.mcp[].server` | string | required if `mcp` set | MCP server name as configured under `.agents/mcp/` (path per [`./config-layout-design.md`](./config-layout-design.md), which is authoritative for layout; an earlier `.agents/mcp.json` reference here was stale). |
| `tools.mcp[].tools` | []string | all from this server | Allowlist of tools from this MCP server. Absent = whole server; non-empty = narrowed to those names (enforced via stock `tool.FilterToolset`, verified 2026-07-25). |
| `tools.skills` | []string | inherit all | Allowlist of skills (SKILL.md bundles per [`./skills-design.md`](./skills-design.md)) the specialist may invoke. References resolve the same way as workload-bundle `skills:` entries (local name or registry URL). Absent = inherit the bundle's `skills:` roster; present-but-empty = deny skill access (normative table below). Skill invocation from a specialist follows the standard three-way policy layering: skill's `allowed_tools` ∩ specialist's `tools` ∩ workload bundle's `tool_catalog` — narrowest wins. Skill budget *hints* are advisory (per [`./skills-design.md`](./skills-design.md) — hints are not enforcement); the enforced ceiling is the specialist's `budget.max_cost_usd`, itself bounded by the bundle's. |

### Allowlist semantics — normative table (rewritten 2026-07-25)

*Earlier revisions of this doc contradicted themselves (field table vs. plain-text list vs. Go sketch) on empty-vs-absent and `tools: {}`. This table is now the single normative statement; the MCP axis is the semantics spike 2 implemented and verified (`mast-prototype` `pkg/specialists.filterToolsets`, using stock `tool.FilterToolset` + `AllowedToolsPredicate`). Presence is tracked **per field**, not per block.*

| Frontmatter state | builtin axis | mcp axis | skills axis |
|---|---|---|---|
| No `tools:` block | inherit all | inherit all | inherit all (bundle roster) |
| `tools: {}` (block present, all fields absent) | inherit all | inherit all | inherit all — *per-field absence rules; this **reverses** the earlier "zero tools" reading, which contradicted per-field inheritance. Pure-reasoning specialists state denial explicitly (next row).* |
| Field present, empty list (`builtin: []` / `mcp: []` / `skills: []`) | deny all on that axis | deny all on that axis | deny all on that axis |
| Field present, non-empty | whitelist: only the listed entries | whitelist: unlisted servers dropped; a listed server with no `tools:` passes whole; with `tools:` it is narrowed to those names | whitelist: only the listed skills |

Cross-axis independence: each axis resolves on its own (e.g. `mcp` listed + `builtin` absent → all builtins, only the listed MCP surface). Composition with bundles and skills is intersection, narrowest wins: skill `allowed_tools` ∩ specialist `tools` ∩ bundle `tool_catalog`.

This is allowlist-by-design: enumerating the few tools a specialist *should* see is much easier than excluding the many it shouldn't.

## Implementation shape

```go
// pkg/specialists/load.go

type Spec struct {
    Name        string
    Description string
    Budget      Budget
    Model       string         // empty = inherit parent
    Tools       ToolAllowlist  // zero value = inherit all from parent
    Instruction string         // body of the .tmpl file
}

type Budget struct {
    MaxTurns            int     // default 5
    MaxWallclockSeconds int     // default 60
    MaxCostUSD          float64 // default 0 = inherit session ceiling
}

type ToolAllowlist struct {
    // Presence is tracked per field (custom UnmarshalYAML or *[]string):
    // nil = field absent = inherit all on that axis; non-nil empty = deny
    // all on that axis; non-nil non-empty = whitelist. Matches the
    // normative table above. (Rewritten 2026-07-25 — the earlier
    // block-level `set bool` could not represent per-field inheritance,
    // and the struct predated the skills reinstatement.)
    Builtin *[]string
    MCP     *[]MCPAllowlist
    Skills  *[]string
}

type MCPAllowlist struct {
    Server string
    Tools  []string // empty = all from this server
}

// Load reads all .tmpl files under dir, parses frontmatter + body.
func Load(dir string) ([]Spec, error)

// Register wires each Spec as a tool on the parent, using ADK's
// agenttool wrapper (composing with the runtime to enforce budgets).
func Register(specs []Spec, parent *agent.Agent) ([]tool.Tool, error)
```

Estimated size: ~200-400 LOC including YAML parsing, allowlist resolution, model override, and agent wiring — spike 2's working loader + builder + MCP-allowlist filtering came in under this envelope. (An earlier size comparison against `pkg/skills/` predated the skills reinstatement and is moot: both packages exist now, doing different jobs.)

## Auto-discovery

At startup, mast scans `.agents/specialists/*.tmpl` once and registers each as a tool. The `--task` profile may default specialists on/off:

| Task class | Specialists default |
|---|---|
| `debug` | on |
| `implement` | on |
| `research` | on |
| `review` | on |
| `chat` | off (lightweight conversational; specialists add cognitive overhead the model doesn't need) |

CLI override: `--specialists=on|off` and `--specialists-dir=<path>` for explicit control.

Hot-reload at runtime (file watcher) is **not** in v1 scope. If a specialist file changes, restart the agent. Worth adding later when a use case forces it.

## Composition with existing patterns

Specialists compose naturally with the patterns already shipped or designed:

| Pattern | Composition with specialists |
|---|---|
| `spawn_agent` (background subagents) | Specialists are *static* (defined ahead of time, predictable). `spawn_agent` is *dynamic* (parent decides at runtime to spawn an ephemeral subagent for a one-off task). Both have a role. The gke-parallel-triage example currently uses `spawn_agent` per-service; with specialists, per-resource-type specialists (`ImagePullBackOff`, `CrashLoopBackOff`) become static, while the per-service investigators stay dynamic. |
| Agentic wrappers (`agentic_grep`, `agentic_research`) | These are *generic* digest-pattern tools. Specialists are *domain-specific* digest-pattern tools. The mechanisms are the same; the difference is who defines the system prompt. Both stay; they serve different scopes. |
| Plan-first gate | Parent specifies its plan; specialists do not. Specialists are scoped narrowly enough that the parent's plan covers them. (Considered: forcing each specialist to plan internally too. Rejected — adds cost without correcting a measured failure mode.) |
| Watchdog | Watches the *parent's* tool-call stream. Specialist-internal loops are bounded by the specialist's own budget caps. Watchdog signals don't propagate into specialist context — the cost ceiling does that job. |
| Cost ceilings | Per-specialist `budget.max_cost_usd` composes with parent's per-turn and per-session ceilings. Specialist exceeding its budget returns to parent with a `budget_exceeded` result; parent decides whether to retry, escalate, or move on. |
| Workflow scaffolding (post-v2) | Two composition patterns coexist. **(1) Specialist as agent node** — the registry exposes each specialist as a graph node the workflow scaffolding shapes drop in directly (primary composition, used by every reference graph in [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)). **(2) Specialist as `agenttool`** — parent decides mid-turn to invoke the specialist during its own reasoning (dynamic; not graph-position-bound). The choice is *when* the specialist is invoked, not which mechanism to use for what. `WithUseSubBranch(true)` on `RunNode` calls to specialist-as-agent-node isolates the specialist's context from the parent's, matching the isolation `agenttool` provides in the other pattern. |
| Human-in-the-loop (post-v2) | Specialists can pause and request operator input via `workflow.NewRequestInputEvent` with a `session.RequestInput{InterruptID, Message, ResponseSchema}`. Response schema drives `mast-web`'s form generation. Resume is durable across process restarts, and works for specialists invoked as agent nodes; specialists invoked via `agenttool` from parent reasoning can also emit `RequestInputEvent` (v2 gain: HITL works on plain `LlmAgent`s, not just workflow-wrapped ones). Common use case: change-safety specialist escalates ambiguous approvals to an operator. |
| Workload bundles (see [`./orchestration-design.md`](./orchestration-design.md)) | Bundles enumerate a `specialists:` roster — the set of specialists available in a given workload. Loader instantiates only the enumerated specialists per session (rather than the full `.agents/specialists/` directory), giving operators per-workload scope control. Planner invokes specialists via `invoke_specialist(name, inputs)`; plain-agent workloads invoke specialists directly via `agenttool`. `mode: SingleTurn` specialists are useful as bundle-scoped classifiers (e.g., a per-workload router that dispatches within the bundle). |
| A2A / federation (see [`./a2a-design.md`](./a2a-design.md), [`./federation-design.md`](./federation-design.md)) | External remote agents (A2A, mast-native, HTTP/RPC) appear to the planner as another class of invocable tools via `invoke_remote_agent(reference, inputs)`. Distinct from specialists (which are in-process, budget-composed, direct tool access); complementary. Same planner treats both uniformly; choice depends on locality + trust. A local specialist can also be *published* as an A2A skill via the workload bundle's `a2a.expose` field — the specialist stays local, but becomes callable by external A2A clients through the wrapping workload. |

## Choosing between a skill and a specialist (rewritten 2026-07-25)

*The previous revision of this section was pre-reversal residue: it told SKILL.md users to convert skills into instruction files or specialist `.tmpl`s. Since the 2026-07-01 skills reinstatement, skills are first-class — a SKILL.md bundle needs no migration at all: drop it in `.agents/skills/` and reference it from the bundle roster.*

The remaining question is authoring-model choice for content *you* write:

1. **Consuming a published SKILL.md** (GKE team, registry, community): use it as-is via `pkg/skills/`. No conversion.
2. **Authoring mast-native operational content** that needs per-invocation budgets, tool allowlists, a model override, or `SingleTurn` mode: write a specialist `.tmpl`.
3. **Knowledge injection** (content that should ride in the parent's own context rather than be a callable): `pkg/instruction/`'s multi-file loader (`AGENTS.md` / `@include`).

Converting an authored skill into a specialist (to gain budgets/allowlists) is [`./skills-design.md`](./skills-design.md)'s `mast skills convert --to-specialist` (v0.3+); this doc no longer proposes a separate migration script.

## Open questions

1. ~~**What does `agenttool.New` return to the parent?**~~ **Resolved 2026-07-01 (ADK v2):** Task-mode agents auto-install a `finish_task` helper tool. Specialists return their focused final via `finish_task`'s argument, not the raw last-assistant-text. The digest pattern is preserved by construction — no wrapper needed. `SingleTurn`-mode specialists (schema `mode: SingleTurn`) return the single-turn output directly, also focused.
2. **Does `agenttool` natively enforce budgets** (max_turns, max_wallclock_seconds, max_cost_usd)? *2026-07-01 update:* partly answered by v2. Per-node `Timeout` maps to `max_wallclock_seconds`; per-node `RetryConfig` bounds transient failures. `max_turns` still needs enforcement inside the specialist's own runtime (sub-agent turn count); `max_cost_usd` still needs the mast-side cost interceptor because ADK doesn't track USD. *2026-07-25 update (spike 2): the cost-interceptor substrate is verified — every model call's `UsageMetadata` rides the runner event stream (`Event` embeds `LLMResponse`), with `Branch`/`NodeInfo` for per-specialist attribution; the prototype's `pkg/budget` meter enforces `max_cost_usd` by folding usage per event and cancelling the run context on breach (demonstrated tripping a $0.01 cap mid-turn). Wallclock caps map to context deadlines. Remaining open: per-turn cap plumbing and pre-call (rather than post-call) gating via a `model.LLM` wrapper or the v2.1.0 TaskRunner seam.*
3. ~~**MCP per-tool allowlist mechanism.**~~ *Resolved 2026-07-25 (spike 2): yes, and it's stock ADK — `tool.FilterToolset(ts, tool.AllowedToolsPredicate(names))` narrows any toolset per specialist at Build time; no mast-side machinery needed. Implemented + unit-tested in `mast-prototype` `pkg/specialists`.*
4. ~~**Model override means constructing a new provider client per specialist**~~ *Resolved 2026-08-12 (v0.3 W1.1, when the override was actually honored — `pkg/specialists.Build` had been parsing `model:` and dropping it).* **Cross-provider overrides are allowed.** The same-provider restriction this question floated turns out to cost more than it saves: `internal/compose.BuildModel` already dispatches on the model id, so a `gemini-*` specialist under a `claude-*` parent needs no new machinery — while *refusing* it would mean writing a provider-family classifier as a second source of truth beside that dispatch, and would fence off the multi-provider-by-pillar property mast otherwise leads on. Three constraints came out of the implementation:

   - **Resolution is memoized per model id**, so the "new client per specialist" cost is really new-client-per-*tier*: eight analysts on one tier share one client.
   - **A declared override that cannot be resolved fails the build**, never falls back to the parent's model. Silent fallback is what this task was fixing; re-introducing it one level up would let a bundle read as "Haiku analysts, Sonnet synthesis" while everything ran on the parent and the cost story was a fiction. The corollary is that credentials for every provider in the roster must resolve at construction — which is when you want to find out, not mid-incident.
   - **When the parent is an offline fake** (`echo` / `scripted` / `toolactor`), every override collapses back to it. A tiered bundle must still run credential-free, or tiering one would break the offline S/U/E test tiers ([`./v0.3-plan.md`](./v0.3-plan.md) §2).

   *Left open by the resolution:* `model:` names a concrete model id, which hard-binds a bundle to one provider — a `gemini-*` deployment cannot adopt a bundle whose analysts say `claude-haiku-4-5` without editing every `.tmpl`. A provider-portable `tier: small | mid | frontier` field resolving through `pkg/taskclass.ModelForTier` would fix that, and the table already exists. Not built; see the W1.1 note in [`./v0.3-plan.md`](./v0.3-plan.md).
5. **Specialist visibility in `mast-web`.** *2026-07-01 update:* v2's unified telemetry span tree means specialist invocations share one trace shape with regular tool calls and workflow-node executions. The UI question is now purely "which attribute do we filter on to render specialists distinctly" (`agent.type == "specialist"`, or by name-prefix), not "how do we correlate two separate span shapes." Simpler than pre-v2.
6. **YAML parser choice.** `gopkg.in/yaml.v3` is what the existing skills loader uses; reuse for consistency unless there's a reason to switch.
7. **Should specialists also declare `tools.a2a` / `tools.remote` allowlists?** By symmetry with `tools.skills` (added 2026-07-01 so specialists can invoke skills), specialists could theoretically also be given a curated allowlist of A2A agents and federated remote agents. Bias: not v0.1. Specialists are meant to be *bounded* subagents; granting arbitrary remote-agent access opens unbounded federation chains + cost blowup risks. Planner (`invoke_remote_agent`) remains the primary path for remote-agent dispatch. Revisit if operators consistently ask, and gate behind explicit per-specialist opt-in with tight budget caps.

## Out of scope

- **Hot-reload of specialist files at runtime.** v2 if needed.
- **Per-specialist permission gate override** (specialist can write to /tmp but parent can't, etc.). Out of scope — specialists inherit parent's permission gate.
- **Specialist authoring UI in mast-web.** Operators write `.tmpl` files in their editor of choice. Maybe later.

### Previously out of scope, now supported via v2 primitives

- **Specialist composition** (specialist A calling specialist B). Two paths now available: (a) as **sub-workflow nodes** when specialists are composed inside a workflow graph — trivially expressible; see [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)'s composition section. (b) as **`agenttool`-chained calls** from inside a specialist's own reasoning, provided the specialist's tool allowlist includes the child specialist as a tool. Both paths need attention to the composed budget: parent specialist's `max_cost_usd` should account for downstream specialists' consumption.
- **Streaming partial results from a specialist to the parent.** Available for specialists invoked as **agent nodes** in a workflow via v2's emitting-function-node pattern (`workflow.NewEmittingFunctionNode` composed around the agent node, or the agent node emitting `session.Event`s directly). Specialists invoked via `agenttool` from parent reasoning remain non-streaming — the `agenttool` contract returns the final result at end-of-invocation.

## Related

- [`./positioning.md`](./positioning.md) — the lean-fork thesis this serves
- [`./fork-design.md`](./fork-design.md) — references this doc for the specialists piece; resolves ADK v2 from day one
- [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) — the reference-graph library that instantiates specialists as agent nodes
- [`./orchestration-design.md`](./orchestration-design.md) — workload bundles enumerate specialist rosters per named workload; planner invokes specialists via tool vocabulary
- [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) — discusses specialist visualization in the UI
- ADK v2 `agenttool` package (signature unchanged from v1) — the underlying mechanism for `agenttool`-based invocation
- ADK v2 workflow package (`google.golang.org/adk/v2/workflow`) — the graph engine that hosts specialist-as-agent-node composition
- [mastersingh24/gke-agent](https://github.com/mastersingh24/gke-agent) — the proof-of-concept this design extends
- [core-agent's context-management-design.md](https://github.com/go-steer/core-agent/blob/main/docs/context-management-design.md) — Mechanism B, which specialists are a domain-specific instance of
