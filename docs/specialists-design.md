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

# Capability (optional; defaults to read_only)
# read_only specialists may not reach a mutating tool — mast refuses to
# build a roster where one can. See "Capability" below.
capability: read_only                # options: read_only, change_executor

# Model override (optional; inherits parent if absent) — one of:
model: gemini-2.5-flash              # full model ID; provider inferred from parent's config
tier: small                          # OR a portable tier: small | mid | frontier
                                     # (resolved per provider; see "Model and tier")

# Report contract (optional; free-form output if absent)
# Path to a JSON-Schema document, relative to this .tmpl file. A shared
# file, not an inline block: a report shape is a contract with its
# consumers, and inlining makes it private to one specialist.
output_schema: ../schemas/finding.json

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
| `capability` | string | `read_only` | Declares whether the specialist is allowed to change anything. `read_only` (the default, so the safe state is what you get by saying nothing) means mast refuses to build the roster if the specialist can reach a mutating tool — see [Capability](#capability-read_only-vs-change_executor-2026-08-14). `change_executor` exempts it and is logged at startup as part of the workload's write surface. The value is a closed enum: an unrecognized one (including the near-miss `change-executor`) fails the load rather than silently defaulting. |
| `model` | string | inherit parent | Full model ID (`gemini-2.5-flash`, `claude-haiku-4-5`, etc.). Dispatched by model id, exactly like `--model`; the parent's provider alias only disambiguates the Anthropic backend, so a cross-provider override is legal (open Q#4, resolved 2026-08-12). Resolution is memoized per id, and an override that cannot be resolved fails the build rather than silently inheriting the parent's model. Under an offline-fake parent (`echo` / `scripted` / `toolactor`) every override collapses back to the parent so tiered bundles still run credential-free. Common pattern: frontier parent dispatching to cheap-tier specialists for high-volume tasks. Mutually exclusive with `tier`. |
| `tier` | string | inherit parent | Provider-portable model override: `small`, `mid` or `frontier`, resolved to a concrete model id for the running provider through `pkg/taskclass.ModelForTier` — the same table `--task` uses (v0.4 W1.1a; see [Model and tier](#model-and-tier-2026-08-15)). Says how much model the step is worth rather than which vendor's model it must run on, so a shipped bundle can declare its own cost shape and still load on any provider. Closed enum: an unrecognized value fails the load, as does declaring `model` and `tier` on the same specialist. Everything `model` guarantees holds here too — memoized per resolved id, unresolvable fails the build, offline-fake parents collapse it. |
| `output_schema` | string | none | Path to a JSON-Schema document (`.json`, `.yaml`, `.yml`) **relative to the `.tmpl` file's own directory**, so a roster stays relocatable. Absent = the specialist returns free-form output. The document is read, type-normalized and checked at *load* time — a malformed contract fails the roster on startup, not on the first turn that dispatches to the specialist. Enforcement is ADK's: in `Task` mode the schema becomes the `finish_task` declaration and a non-conforming call comes back as a validation error the model can correct; in `SingleTurn` mode the reply is validated on the way out and a violation refuses the delegation. Either way the caller sees a refusal that names the offending key, and non-conforming output never becomes the specialist's result. Constraints checked at load: the top level must be an object (see below), every node needs a `type`, arrays need `items`, objects need `properties`, and every `required` name must be a declared property. Mast does not interpret the schema — there is no `Finding` Go type; the shape is a workload asset (`examples/workloads/gke-triage/schemas/finding.json`). |
| `tools.builtin` | []string | — | **A declaration, not a grant ([status](#the-builtin-axis-is-a-declaration-not-a-grant-2026-08-21)).** Names built-in tools the specialist is declared to use. mast offers specialists no built-in tools, so nothing here is granted or narrowed and absent and empty mean the same thing. What reads it is the capability split, the fan-out branch check, and the write-surface startup log — each holding the specialist to the claim rather than acting on it. |
| `tools.mcp[].server` | string | required if `mcp` set | MCP server name as configured under `.agents/mcp/` (path per [`./config-layout-design.md`](./config-layout-design.md), which is authoritative for layout; an earlier `.agents/mcp.json` reference here was stale). |
| `tools.mcp[].tools` | []string | all from this server | Allowlist of tools from this MCP server. Absent = whole server; non-empty = narrowed to those names (enforced via stock `tool.FilterToolset`, verified 2026-07-25). |
| `tools.skills` | []string | inherit all | **Not implemented as of v0.4 — a non-empty list fails the load ([status](#the-skills-axis-does-not-exist-yet-2026-08-20)).** Allowlist of skills (SKILL.md bundles per [`./skills-design.md`](./skills-design.md)) the specialist may invoke. References resolve the same way as workload-bundle `skills:` entries (local name or registry URL). Absent = inherit the bundle's `skills:` roster; present-but-empty = deny skill access (normative table below). Skill invocation from a specialist follows the standard three-way policy layering: skill's `allowed_tools` ∩ specialist's `tools` ∩ workload bundle's `tool_catalog` — narrowest wins. Skill budget *hints* are advisory (per [`./skills-design.md`](./skills-design.md) — hints are not enforcement); the enforced ceiling is the specialist's `budget.max_cost_usd`, itself bounded by the bundle's. |

### Why `output_schema` must be an object at the top level (2026-08-13)

Mast refuses a scalar or array top-level schema, and the reason is that ADK's two modes disagree about them. Task mode wraps a non-object schema under a `result` key in the `finish_task` declaration and unwraps it again on the way out; SingleTurn's validator unmarshals the reply into a `map[string]any` *before* it consults the schema, so a scalar contract can never validate there at all. The same document would therefore mean two different things depending on a field elsewhere in the frontmatter. A report contract is an object anyway, so the restriction costs nothing and removes a trap.

Two smaller load-time refusals are worth naming for the same reason — each is a document that parses cleanly and constrains nothing:

- **Unknown keys are fatal.** `propertys:` for `properties:` would otherwise produce a schema with no properties, which is not a narrower contract but the absence of one.
- **An object with no properties is fatal.** ADK's validator rejects any key the schema does not name, so an empty-property object accepts nothing at all — never what the author meant.

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

**One column of that table is the mcp axis and one is aspirational.** As of v0.4 only `mcp` resolves this way in code: `builtin` names no grant mast can narrow (below) and `skills` is refused outright ([below](#the-skills-axis-does-not-exist-yet-2026-08-20)). The table stays as written because it is the format's contract — it is what an embedder's own runtime, and core-agent's, resolve against — but read the builtin and skills columns as *what the shape would mean*, and the two status sections that follow as what this build does.

### The builtin axis is a declaration, not a grant (2026-08-21)

Same finding as the skills section below, one notch weaker: the axis *is* read, it just does not do what the table says the axis does.

Nothing populates `specialists.BuildOptions.Tools`. `internal/compose` builds every specialist with `{Model, Resolve, ResolveTier}` and adds `Toolsets` for Task-mode specs, so a specialist's tools are its filtered MCP toolsets plus whatever ADK installs itself (`finish_task`, delegation tools). **It holds no built-in tools at all**, and there is nothing for `builtin:` to whitelist. mast's own built-ins are the planner's control-plane vocabulary, wired onto the *root* under planner dispatch and never offered to a specialist.

| Frontmatter | What the table above promises | What mast does |
|---|---|---|
| `builtin:` absent | inherit every built-in tool | inherits nothing, because none are offered |
| `builtin: []` | deny all built-ins | denies nothing that was ever granted — same outcome as absent |
| `builtin: [get_pod]` | whitelist `get_pod` | the specialist cannot call `get_pod`, or anything else built-in |

So the field is kept and re-documented rather than refused, because unlike `skills` it has real consumers — three of them, all reading it as a **claim the spec makes about itself** and holding it to that claim:

- `internal/compose.CheckCapabilitySplit` refuses a `read_only` specialist that lists a mutating name here.
- `pkg/graph.checkBranchTools` refuses a fan-out branch that does, and separately refuses one that lists `request_operator_input` (a branch cannot park a run).
- The capability startup log reports it as part of the workload's declared write surface.

All three run in the *refusing* direction, which is sound under either reading: holding a specialist to a claim costs nothing when the claim turns out to grant nothing.

**One consumer ran the other way and was corrected.** The write gate's producer contract (`changeSurface`, v0.4 W7.0) folded `builtin` names into the set of tools a proposed change may name — so a `change_executor` declaring `builtin: [patch_k8s_resource]` had proposals for that tool *accepted* at report time, and then nothing to run them with. That is precisely the silent-approval failure the contract exists to prevent: an operator approves a patch, the executor turns out not to hold the tool, and the incident ends with an approval and no change. The executable surface is now the MCP allowlist and nothing else; an executor that declares its whole surface in `builtin:` gets an empty one, refuses every proposal, and says so at startup.

Reporting the axis honestly is also why `GET /sessions/{id}/subagents` publishes it as **`builtin_declared`** rather than `builtin` (#218): under the table's name an operator would read it as what the specialist can call.

The way to make the table true would be to populate `BuildOptions.Tools` with a built-in set and let `builtin` narrow it the way `mcp` narrows toolsets. That is a feature, not a fix, and it needs a built-in set to exist first (#219).

### The skills axis does not exist yet (2026-08-20)

One of the three axes above is enforced code. This is the weakest of the other two, and the table read as though it were the strongest.

`mcp` is enforced by `filterToolsets` via stock `tool.FilterToolset`; `builtin` is read as a declaration by the capability split and the fan-out branch check ([above](#the-builtin-axis-is-a-declaration-not-a-grant-2026-08-21)). `skills` is read by **no production code at all** — because there is nothing for it to narrow. mast ships no skills subsystem: no `pkg/skills`, no SKILL.md loader, no `invoke_skill`, no `mast skills` CLI. [`./skills-design.md`](./skills-design.md) schedules the loader for **v0.1**; four releases have shipped without it and nothing in this corpus said so (#211).

So as of v0.4, on the one file whose job is to state what a sub-agent may touch:

| Frontmatter | What the table above promises | What the loader does |
|---|---|---|
| `skills:` absent | inherit the bundle roster | inherits nothing, because there is no roster — same outcome |
| `skills: []` | deny all skills | loads; deny-all is what this build does anyway — **honest** |
| `skills: [a, b]` | whitelist a and b | **refused at load**, naming the file |

The refusal is the same discipline the loader already applies to a misspelled `capability` or `tier`: a declaration that cannot take fails on startup rather than reading like it worked. Accepting it would let an allowlist that grants nothing look identical to one that grants three things.

Two adjacent surfaces are declared-but-inert for the same reason and are documented in place rather than removed: `attach.ToolSourceSkill` (a shared wire enum an embedder or core-agent's daemon can still produce) and the `"skill"` namespace entry in `pkg/permissions`' plan-exempt table (whose exemption should be re-decided when the loader lands — it is written for three read-only tools and exempts a whole namespace).

**None of this is a broken user promise:** the docs site never advertised skills, and no shipped spec, example, or fixture uses the axis. It is a corpus-vs-code gap, now recorded in both places.

**Two enforcement corrections, both found on 2026-08-14 while building the capability split (mast W2.4).** The table above was normative and the code did not implement it:

- **`mcp: []` granted the whole catalog.** The loader tested list *length*, so present-but-empty and absent took the same branch — the row that reads "deny all on that axis" was the row that inherited everything. Presence is now read as nil-vs-empty (`ToolAllowlist.InheritsAllMCP`), pinned by a test against yaml.v3's decoding, because this whole table rests on that one distinction surviving a round trip.
- **A non-empty `mcp:` whitelist matched nothing.** Entries are matched to wired toolsets by name, and ADK's `mcptoolset` reports the same constant name for every server it builds — so a specialist that enumerated its tools got *none of them*, and the whitelist row was, in practice, a deny-all. Mast now names each toolset with its catalog key. If you write an allowlist, exercise it against a real server: this failed silently for two releases because no shipped bundle enumerated.

### Capability: `read_only` vs `change_executor` (2026-08-14)

A specialist's allowlist says what it *may* call. `capability:` says what it may *do*, and mast enforces the two against each other before the agent exists.

`read_only` is the default. Building a roster refuses — with a message naming the specialist, the offending tools, and the fix — if a read-only specialist:

- names a mutating tool in `tools.mcp[].tools` or `tools.builtin`,
- grants itself a whole MCP server (`- server: gke` with no `tools:`), since the server's future tools are unreviewed, or
- declares no `tools.mcp` at all while the workload ships a `tool_catalog` — inheriting the catalog inherits everything mutating in it.

The escape hatches are to enumerate the read-only tools it needs, to write `mcp: []` if it needs none, or to declare `capability: change_executor` if it is genuinely meant to change things. `SingleTurn` specialists are exempt: the mode carries no tools at all.

"Mutating" is not a list maintained here — it is the same default-deny predicate the effect outbox and the write gate use (built-in annotation, MCP `readOnlyHint`, and the workload's `tool_catalog.tools[].mutating` override). A tool nobody has classified is treated as mutating, so an unreviewed catalog fails the roster rather than the incident. Note the practical consequence recorded in [`./orchestration-design.md`](./orchestration-design.md): because ADK drops MCP annotations, `tool_catalog.tools[].mutating` is the *only* place a read tool can be declared safe.

The check runs at roster construction, so it holds for every entry point (daemon, one-shot, library, eval rig) and both dispatch shapes. It is separate from — and weaker than — the fan-out branch check in `pkg/graph`, which refuses a mutating branch *even if it declares `change_executor`*, because every branch runs before the one synthesis gate.

Why declare capability at all rather than infer it from the allowlist? Because the declaration is what makes the write surface auditable: every `change_executor` in the roster is logged at startup, so "which specialists in this workload can change the cluster" is answerable from a log line rather than by re-deriving the intersection of three files.

### Model and tier (2026-08-15)

A specialist may override the parent's model in one of two spellings, and the difference is portability.

`model: claude-haiku-4-5` names an exact id. It is the right answer when a bundle has a reason to pin one — a model whose behaviour a prompt was tuned against, a deployment with one provider and no plans for another. It is the wrong answer for anything shipped, because the id is a vendor: a Gemini deployment cannot adopt a bundle whose analysts say `claude-haiku-4-5` without editing every `.tmpl`.

`tier: small | mid | frontier` says the same thing about *spend* without saying anything about *vendor*. At build time `internal/compose` derives the provider family — the explicit `--provider` alias when there is one, otherwise the root model id's prefix, the same dispatch `BuildModel` makes — and resolves the tier through `pkg/taskclass.ModelForTier`. One roster declaration, `tier: small`, becomes `gemini-3.5-flash-lite` under a Gemini root and `claude-haiku-4-5` under an Anthropic one. Because it is the same table `--task` resolves against, `--task=debug` and `tier: frontier` cannot disagree about what the frontier model is.

The properties `model:` earned in v0.3 W1.1 all carry over, because tier resolution runs *through* the same memoized resolver rather than beside it:

- **Memoized per resolved id** — twelve small-tier diagnosers share one client, not twelve.
- **Unresolvable fails the build.** A tier with no mapping for the running provider — or a root model whose family cannot be derived and no `--provider` to say — is a startup error naming the specialist. It never falls back to the parent's model, because a roster that reads "twelve cheap diagnosers" while everything runs on the frontier model is a cost story that is fiction.
- **Offline fakes collapse it.** Under `echo` / `scripted` / `toolactor` every tier resolves back to the root, *before* the family is derived — `echo` has no provider family, and a tiered bundle must still run credential-free in the S/U/E test tiers.
- **The bill follows the tier.** `internal/compose.MeterScopes` prices a tiered specialist at the model its tier resolved to. Both the build path and the pricing path go through one `SpecModelName`, because they were separate once and only the build path knew about tiers.

Declaring both on one specialist is a load error rather than a precedence rule. They are two answers to one question, and no reading of the file tells you which one lost.

What a tier resolved to is logged once per specialist at startup (`specialist tier resolved`, with the tier and the concrete id), since the answer depends on how mast was started and is therefore not readable from the bundle alone.

The shipped `examples/workloads/ns-audit` is the worked example: four `tier: small` analysts and a `tier: mid` synthesis — cheap reads, one capable merge — running unmodified on either provider and under `--model=echo`.

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
| Cost ceilings | Per-specialist `budget.max_cost_usd` composes with the workload's per-session ceiling — *shipped 2026-08-13 (v0.3 W1.2)*, see open question 2. Composition is tightest-cap-wins by construction: every model call is checked against the authoring specialist's ceiling and the session's, and whichever is crossed first stops the run. The `budget_exceeded`-result-to-parent shape this row originally described — specialist stops, parent decides whether to retry, escalate, or move on — is *not* what ships: the meter runs on the event stream, outside the specialist's own run, where the only lever is the run context. It is the better shape and it needs the pre-call seam open question 2 still names. |
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
2. **Does `agenttool` natively enforce budgets** (max_turns, max_wallclock_seconds, max_cost_usd)? *2026-07-01 update:* partly answered by v2. Per-node `Timeout` maps to `max_wallclock_seconds`; per-node `RetryConfig` bounds transient failures. `max_turns` still needs enforcement inside the specialist's own runtime (sub-agent turn count); `max_cost_usd` still needs the mast-side cost interceptor because ADK doesn't track USD. *2026-07-25 update (spike 2): the cost-interceptor substrate is verified — every model call's `UsageMetadata` rides the runner event stream (`Event` embeds `LLMResponse`), with `Branch`/`NodeInfo` for per-specialist attribution; the prototype's `pkg/budget` meter enforces `max_cost_usd` by folding usage per event and cancelling the run context on breach (demonstrated tripping a $0.01 cap mid-turn). Wallclock caps map to context deadlines. Remaining open: per-turn cap plumbing and pre-call (rather than post-call) gating via a `model.LLM` wrapper or the v2.1.0 TaskRunner seam.* **Resolved 2026-08-13 (v0.3 W1.2): `max_turns` and `max_cost_usd` are enforced, by the session meter rather than by `agenttool`.** The roster's declarations become per-agent scopes on the meter (`internal/compose.MeterScopes` → `budget.Config.Scopes`); each event is checked against its scope and against the session, and a specialist that declares a `model:` override is priced at that model's rate. Three corrections to the 2026-07-25 note:

   - **Attribution is by `Event.Author`, not `Branch`.** `Author` carries the agent's name on every dispatch shape mast builds — a coordinator's sub-agent tool, a workflow-graph node, the planner's `invoke_specialist` — which is what makes one attribution rule enough for all of them. `Branch` is empty in the coordinator/sub-agent-tool shape, so a branch-keyed meter would have silently metered nothing there. *(Corrected 2026-08-21, [#226](https://github.com/go-steer/mast/issues/226): one rule, but not one seam. The planner runs each `invoke_specialist` dispatch on a private runner, so its events never reach the outer stream at all — a specialist dispatched that way was metered by nothing through v0.4, and its declared scope was unenforceable. `planner.SubRunObserver` hands those events to the host, which folds them into the same meter; the Author on them is still the specialist's name, so the scope binds unchanged.)*
   - **`max_turns` needs no "inside the specialist's own runtime" counting.** A turn is a model call, and every model call the specialist authors is on the stream with its name on it.
   - **Enforcement is still after the call, and it still stops the session.** Both limitations are recorded in the `pkg/budget` package doc under "Known limitations"; both need the pre-call seam (wrap `model.LLM`, or ADK's `BeforeModel` plugin callback), which is where the `budget_exceeded`-to-parent shape in the composition table above also lives. *(One shape got the second half early, 2026-08-21: a planner dispatch is observed from inside the tool call that started the sub-run, so a crossed ceiling there stops the specialist and returns the planner a `status: halted` result — the `budget_exceeded`-to-parent shape, arrived at from the seam's position rather than from the interceptor. Coordinator and graph dispatch still stop the session.)*
3. ~~**MCP per-tool allowlist mechanism.**~~ *Resolved 2026-07-25 (spike 2): yes, and it's stock ADK — `tool.FilterToolset(ts, tool.AllowedToolsPredicate(names))` narrows any toolset per specialist at Build time; no mast-side machinery needed. Implemented + unit-tested in `mast-prototype` `pkg/specialists`.*
4. ~~**Model override means constructing a new provider client per specialist**~~ *Resolved 2026-08-12 (v0.3 W1.1, when the override was actually honored — `pkg/specialists.Build` had been parsing `model:` and dropping it).* **Cross-provider overrides are allowed.** The same-provider restriction this question floated turns out to cost more than it saves: `internal/compose.BuildModel` already dispatches on the model id, so a `gemini-*` specialist under a `claude-*` parent needs no new machinery — while *refusing* it would mean writing a provider-family classifier as a second source of truth beside that dispatch, and would fence off the multi-provider-by-pillar property mast otherwise leads on. Three constraints came out of the implementation:

   - **Resolution is memoized per model id**, so the "new client per specialist" cost is really new-client-per-*tier*: eight analysts on one tier share one client.
   - **A declared override that cannot be resolved fails the build**, never falls back to the parent's model. Silent fallback is what this task was fixing; re-introducing it one level up would let a bundle read as "Haiku analysts, Sonnet synthesis" while everything ran on the parent and the cost story was a fiction. The corollary is that credentials for every provider in the roster must resolve at construction — which is when you want to find out, not mid-incident.
   - **When the parent is an offline fake** (`echo` / `scripted` / `toolactor`), every override collapses back to it. A tiered bundle must still run credential-free, or tiering one would break the offline S/U/E test tiers ([`./v0.3-plan.md`](./v0.3-plan.md) §2).

   ~~*Left open by the resolution:*~~ *Closed 2026-08-15 (v0.4 W1.1a).* `model:` names a concrete model id, which hard-binds a bundle to one provider — so `tier: small | mid | frontier`, resolving through `pkg/taskclass.ModelForTier`, ships beside it as the portable spelling. Both are supported; declaring both on one specialist is a load error. See [Model and tier](#model-and-tier-2026-08-15) and the W1.1a stanza in [`./v0.4-plan.md`](./v0.4-plan.md).
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
