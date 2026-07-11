# mast specialists: design

**Status:** draft, 2026-06-11 (updated 2026-07-01 — ADK v2 resolves several open questions and reshapes the schema slightly). Companion to [`./fork-design.md`](./fork-design.md) (which lists the specialists subsystem as the replacement for the skills surface in mast's scope), [`./positioning.md`](./positioning.md) (the thesis), and [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) (the reference-graph library that instantiates specialists as agent nodes). This doc covers the specialist subsystem in detail — schema, loader, registration mechanics, and the open questions to resolve before phase 1 of the fork.

## Why specialists replace skills

In mast's scope, the skills subsystem (`pkg/skills/`) is being cut. The motivation:

- Core-agent's skills load Anthropic-SKILL-formatted bundles (`SKILL.md` with their published frontmatter) so users can drop existing Claude Code skills into a project. That's a real value-add **for core-agent's audience** (developers experimenting locally, cogo-shaped consumers). It's not mast's audience — mast operators write GKE runbooks, not Anthropic-compat skill bundles.
- The "callable task template" pattern that skills serve is structurally better-served by ADK's existing `agenttool` package: each specialist is a full sub-agent (own system prompt, optionally own tools and model), wrapped as a callable tool the parent invokes when relevant. The specialist's reasoning stays in its own context (Mechanism B / digest pattern), and the parent only sees the final result.
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
| `model` | string | inherit parent | Full model ID (`gemini-2.5-flash`, `claude-haiku-4-5`, etc.). Provider is inferred from the parent's resolved config. Common pattern: frontier parent dispatching to cheap-tier specialists for high-volume tasks. |
| `tools.builtin` | []string | inherit all | Allowlist (not denylist) of core-agent built-in tools. Empty/absent = specialist sees all of parent's builtins. |
| `tools.mcp[].server` | string | required if `mcp` set | MCP server name as configured in `.agents/mcp.json`. |
| `tools.mcp[].tools` | []string | all | Allowlist of tools from this MCP server. Empty/absent = all tools from this server available to specialist. |

### Allowlist semantics, in plain text

- **No `tools` block** → specialist inherits everything the parent has.
- **`tools.builtin: [read_file]`** → specialist sees only `read_file` from builtins; nothing else.
- **`tools.mcp` listed, `tools.builtin` absent** → specialist inherits all builtins, only the listed MCP tools.
- **`tools: {}`** (empty block) → specialist sees zero tools. Useful for pure-reasoning specialists that just digest the parent's input.

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
    set     bool             // true if the YAML had a `tools:` block at all
    Builtin []string         // empty (and set=true) = no builtins; absent (set=false) = inherit all
    MCP     []MCPAllowlist
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

Estimated size: ~200-400 LOC including YAML parsing, allowlist resolution, model override, and `agenttool.New` wiring. Significantly smaller than today's `pkg/skills/`.

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

## Migration story (for core-agent users coming to mast)

Specialists are *not* drop-in replacements for `SKILL.md` bundles. Migration is intentional, not automatic:

1. **Anthropic-skill-style "knowledge injection" use case** (skill body becomes part of the parent's context for a turn): migrate to `pkg/instruction/`'s multi-file loader. The skill body becomes an `AGENTS.md` file or `@include`-d snippet.
2. **Anthropic-skill-style "callable subroutine" use case** (skill body describes how to do X, parent invokes when relevant): migrate to a specialist `.tmpl` file. The frontmatter description becomes the YAML description; the skill body becomes the specialist's system prompt; budget + tools added as the operator sees fit.

A migration helper script (`scripts/migrate-skills-to-specialists.sh`) could automate the second case for the common shape. Not v1; nice-to-have if migration friction becomes a real complaint.

## Open questions

1. ~~**What does `agenttool.New` return to the parent?**~~ **Resolved 2026-07-01 (ADK v2):** Task-mode agents auto-install a `finish_task` helper tool. Specialists return their focused final via `finish_task`'s argument, not the raw last-assistant-text. The digest pattern is preserved by construction — no wrapper needed. `SingleTurn`-mode specialists (schema `mode: SingleTurn`) return the single-turn output directly, also focused.
2. **Does `agenttool` natively enforce budgets** (max_turns, max_wallclock_seconds, max_cost_usd)? *2026-07-01 update:* partly answered by v2. Per-node `Timeout` maps to `max_wallclock_seconds`; per-node `RetryConfig` bounds transient failures. `max_turns` still needs enforcement inside the specialist's own runtime (sub-agent turn count); `max_cost_usd` still needs the mast-side cost interceptor because ADK doesn't track USD. `pkg/agent/background.go`'s `spawn_agent` composition still applies for the remainder.
3. **MCP per-tool allowlist mechanism.** Does the existing MCP integration support filtering an MCP server's tool set per-agent? If the parent has full server access but the specialist should only see some tools, the underlying mechanism needs to support this. Likely yes via tool filtering, but worth confirming. (Unchanged by v2 — orthogonal to the workflow-engine surface.)
4. **Model override means constructing a new provider client per specialist** if the specialist's model is on a different provider than the parent (Gemini specialist under a Claude parent). Likely acceptable cost, but worth noting and possibly limiting to "specialist model must be same provider as parent" for v1 simplicity. (Unchanged by v2.)
5. **Specialist visibility in `mast-web`.** *2026-07-01 update:* v2's unified telemetry span tree means specialist invocations share one trace shape with regular tool calls and workflow-node executions. The UI question is now purely "which attribute do we filter on to render specialists distinctly" (`agent.type == "specialist"`, or by name-prefix), not "how do we correlate two separate span shapes." Simpler than pre-v2.
6. **YAML parser choice.** `gopkg.in/yaml.v3` is what the existing skills loader uses; reuse for consistency unless there's a reason to switch.

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
