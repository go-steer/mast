# mast orchestration: workload bundles, planner, learning

**Status:** draft, 2026-07-01. Companion to [`./positioning.md`](./positioning.md) (the thesis — unattended / library / multi-provider workloads are the audience this doc serves), [`./fork-design.md`](./fork-design.md) (ADK v2 from day one as the substrate), [`./specialists-design.md`](./specialists-design.md) (the specialists that populate agent nodes and workload rosters), and [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) (the seven canonical reference graphs the planner uses as vocabulary). Covers three related subsystems as one story: **workload bundles** (operational profile per named class of work), **the planner** (supervisor-body agent that constructs execution from workload vocabulary), and **bundle learning** (audit-derived refinement of bundles over time).

## Why one doc

The three subsystems split cleanly on surface — bundles are declarative YAML, the planner is a runtime agent, learning is a periodic pipeline — but they couple hard on infrastructure and story. Bundles are what the planner consumes as config; learning is what the audit corpus feeds back to bundles; every path involves specialists as vocabulary, mast-web as review surface, audit-derived memory as substrate. Splitting them into three docs would triple the cross-reference overhead without changing the reading order — nobody reads "planner" without workload context; nobody reads "learning" without both. One doc keeps the story intact.

The doc is skimmable per subsystem: sections 3, 4, 5 are contained enough that a reader focused on one can read only that section. Sections 2, 6, 7 pay off from being together.

## Overview

The unattended orchestration pipeline in one flow:

```
Unattended entry point (HTTP / queue / scheduler / attach)
  receives a work item.
    ↓
Task-class + workload resolution
  (4 paths: explicit / envelope / bundle-selection / classifier-first)
    ↓
Session instantiated with resolved bundle
  (specialists roster, tool catalog, budgets, HITL policy, planner on/off)
    ↓
IF bundle.planner.enabled:
    Planner LlmAgent runs (Task mode)
    Vocabulary: bundle specialists + reference-graph shapes as tools
    Each planner turn = one tool call = one node/subgraph execution
ELSE:
    Plain LlmAgent runs against bundle-configured tool catalog
    ↓
Session events recorded to eventlog (uniform v2 span tree)
    ↓
Audit-derived memory pipeline (periodic map-reduce) reads eventlog
    ↓
Bundle-learning pipeline proposes new bundles / refinements
    ↓
Operator reviews proposals in mast-web
  approves / rejects / edits;
  approved bundles land in .agents/workloads/
```

Interactive sessions (attach + mast-web) enter the same pipeline — the coordinator LlmAgent (Chat mode) infers task class and bundle per user turn, or the operator selects explicitly.

## Workload bundles

A **workload bundle** is an operator-authored declarative profile for a named class of work. One file per bundle, under `.agents/workloads/`.

### File layout

```
.agents/workloads/
  incident-triage.yaml
  drift-detection.yaml
  cost-alert-response.yaml
  release-canary-review.yaml
  ...
```

### Schema (v1)

```yaml
# .agents/workloads/incident-triage.yaml
name: incident-triage
description: |
  GKE pod-failure investigation. Triggered by pager alert
  from Prometheus AlertManager or manual re-triage via
  mast-web. Fans out per affected service, verifies
  root cause, proposes remediation.

task_class: orchestrate           # public task classes: chat|debug|implement|research|review|orchestrate

specialists:                       # references .agents/specialists/*.tmpl
  - ImagePullBackOff
  - CrashLoopBackOff
  - OOMKilled
  - ServiceEndpointMissing
  - NodeNotReady

skills:                            # skill bundles (see skills-design.md); coexists with specialists roster
  - gke-triage                     # local .agents/skills/gke-triage.skill/
  - google://gke-team/incident-triage@v1.2   # registry-discovered

tool_catalog:                      # applied on top of task-class default
  builtin: [read_file, grep]
  mcp:
    - server: gke
      tools: [get_k8s_resource, describe_k8s_resource, get_k8s_logs, list_k8s_events]
    - server: prometheus             # empty tools = all tools from this server

planner:
  enabled: true
  plan_review_required: false      # true = HITL on first planner turn
  reference_shapes:                # subset of vocabulary the planner may use
    - fan-out-fan-in
    - adversarial-verifier
    - llm-as-router

budget:
  max_wallclock_seconds: 600
  max_cost_usd: 5.00
  max_turns: 20                    # planner turn cap; individual specialists have their own

hitl_policy:
  on_ambiguity: escalate           # options: escalate | proceed | abort
  on_mutation: require_approval    # options: require_approval | apply | dry_run
  on_budget_exhaustion: escalate   # options: escalate | abort

isolation:
  scope: per_request               # options: per_request | per_tenant | global
```

### Field reference

| Field | Type | Required | Notes |
|---|---|---|---|
| `name` | string | yes | Unique within `.agents/workloads/`; used by explicit-selection and classifier-first resolution. |
| `description` | string | yes | Human-readable; also consumed by classifier-first prompt construction. Phrase as "invoke this workload when …" |
| `task_class` | string | yes | Public task class the bundle runs under. Determines agent mode + DefaultInstruction variant. |
| `specialists` | []string | no | Roster of specialist names (filename minus `.tmpl`). Available as `invoke_specialist` targets to the planner; also available to plain-agent invocation. Empty = no specialists. |
| `tool_catalog.builtin` | []string | no | Allowlist of built-in tools. If absent, inherits task-class default. |
| `tool_catalog.mcp[]` | []MCPAllow | no | Per-MCP-server tool allowlist (same shape as specialists). |
| `planner.enabled` | bool | no (default false) | If true, planner LlmAgent runs; if false, plain agent runs. |
| `planner.plan_review_required` | bool | no (default false) | If true, planner's first-turn plan output is escalated via HITL before any execution. |
| `planner.reference_shapes` | []string | no | Subset of the 7 reference-graph shapes exposed to the planner as tools. Absent = all shapes; empty = specialists-only vocabulary. |
| `budget.max_wallclock_seconds` | int | no | Session-level wall-clock cap. |
| `budget.max_cost_usd` | float | no | Session-level cost cap. Composes with per-specialist caps. |
| `budget.max_turns` | int | no (default 20) | Planner turn cap (or plain-agent turn cap when planner disabled). |
| `hitl_policy.on_ambiguity` | enum | no (default `escalate`) | What happens when the planner (or plain agent) hits genuine ambiguity. |
| `hitl_policy.on_mutation` | enum | no (default `require_approval`) | What happens before a state-mutating tool call (write, apply, delete). |
| `hitl_policy.on_budget_exhaustion` | enum | no (default `escalate`) | What happens when a budget cap is hit. |
| `isolation.scope` | enum | no (default `per_request`) | Session isolation scope. Maps to `WithIsolationScope(scopeID)` on the root run. |

### Resolution paths

Task-class + bundle resolution is a runtime concept, not a session-level flag. Four paths coexist:

| Path | When it fires | Example |
|---|---|---|
| **Explicit at launch** | CLI flag or library API names the workload | `mast --workload=incident-triage` |
| **Envelope field** | Incoming request carries workload identifier | HTTP `X-Mast-Workload: incident-triage` header; `workload` field on queue message |
| **Bundle-selection at entry-point config** | Deployment binds request URL / queue subject to a bundle | HTTP `/webhook/incidents` → `incident-triage` bundle; SQS `arn:...:incident-queue` → `incident-triage` |
| **Classifier-first** | SingleTurn classifier examines input, picks bundle name | Generic inbox drainer with mixed message types |

Paths compose. An entry point can have a default bundle and allow envelope override; classifier-first can be the fallback when envelope is absent. Precedence: envelope > entry-point binding > classifier > default.

### Classifier-first dispatch

The classifier-first path is the LLM-as-router shape (`./workflow-scaffolding-design.md`) applied at the *session-configuration* level, not just within a graph.

- **Classifier is a `SingleTurn` LlmAgent** on the cheap tier (typically gemini flash / claude haiku).
- **System prompt built dynamically** from the `description` fields of all bundles under `.agents/workloads/*.yaml` plus a `default` fallback. Format: "Classify the incoming work item into one of: {name: description, …}. If none fit, respond with 'default'."
- **Output is a single workload name string** or the `default` sentinel.
- **No classifier retraining when bundles change** — prompt is rebuilt from files at startup (or on `SIGHUP` if hot-reload is enabled later).
- **Unknown bundle name in output** → falls back to declared default; logged as a classifier miss for later review.

The classifier itself is not a specialist file — it's an internal component of the workload-resolution layer, invoked on entry rather than as a callable subroutine. Its behavior is nonetheless a `SingleTurn` LlmAgent, so any per-provider quirks in that mode surface here first.

## The planner

### Design space (recap)

Five shapes were considered:

- **A. Graph-builder** — planner emits structured graph spec; runtime materializes. Highest power, highest risk (malformed graphs, unbounded cost, hard to audit).
- **B. Shape-selector** — planner picks one canonical shape + parameterizes. Constrained; single-shape only.
- **C. Supervisor-body** — planner is body of dynamic node; calls `RunNode` per iteration. Naturally cost-bounded, checkpointable, reviewable.
- **D. Sub-workflow composer** — planner picks sub-workflow nodes and composes. Explicit vocabulary; materialization step.
- **E. Multi-tier** — frontier sketches strategy; SingleTurn agents fill parameters. Over-engineered for v0.1.

### Recommended: C with light D flavor

**Primary shape: supervisor-body planner.** The planner is a Task-mode `LlmAgent` invoked when the resolved bundle has `planner.enabled: true`. Each planner turn = one tool call = one node or sub-workflow execution. The "graph" is emergent from the sequence of decisions, recorded to the eventlog turn-by-turn.

**Light D flavor:** reference-graph shapes are exposed as tools so the planner picks from a known vocabulary rather than emitting free-form graph JSON. Reference library becomes the planner's toolbox.

Why not A: unconstrained graph generation is too dangerous for unattended. Cost-bound and structural-bound risks are real; the value-per-risk is bad for mast's positioning.

Why not B alone: real workloads (GKE incident triage: triage → verify → propose → apply) don't decompose into one canonical shape.

### Tool vocabulary

| Tool | What it does | Notes |
|---|---|---|
| `invoke_specialist(name, inputs)` | Runs one bundle-enumerated specialist as agent node | `WithUseSubBranch(true)` isolates specialist context |
| `invoke_skill(name, inputs)` | Runs one bundle-enumerated skill as a callable tool | Skill body loaded into the tool's context; tool allowlist is the intersection of skill's `allowed_tools` and bundle's `tool_catalog` (see [`./skills-design.md`](./skills-design.md)) |
| `run_shape_fan_out_fan_in(planner_fn, workers, joiner)` | Fan-out-fan-in sub-workflow | Workers must be from bundle's specialist roster |
| `run_shape_pipeline(nodes)` | Sequential pipeline | Nodes may be specialists, tools, or `run_shape_*` results |
| `run_shape_supervisor_workers(supervisor_body, workers)` | Supervisor+workers sub-workflow | Nested planners possible; budget composes |
| `run_shape_autonomous_loop(step, exit_condition)` | Cyclic-graph loop | Bounded by parent planner's remaining budget |
| `run_shape_adversarial_verifier(proposer, skeptic, decision)` | Proposer + skeptic + decision | Decision node may HITL-escalate |
| `run_shape_map_reduce(items, mapper, reducer)` | Map-reduce | Concurrency capped by `WithMaxConcurrency` |
| `run_shape_llm_router(classifier, handlers)` | LLM-as-router | Classifier is a bundle-scoped SingleTurn agent |
| `request_operator_input(message, schema)` | HITL escalation | `RequestInputEvent` with `ResponseSchema` |
| `invoke_remote_agent(reference, inputs)` | Dispatches to a remote agent via a federation adapter | See [`./federation-design.md`](./federation-design.md) — reference format `<protocol>://<name-or-endpoint>[?...]`; adapters: A2A, mast-native, HTTP/RPC |
| `finish_task(result)` | Signals planner completion | ADK v2 auto-installed for Task mode |

Reference-graph tools return the shape's result to the planner as the next reasoning input. The planner is a Task-mode agent so `finish_task` is the canonical exit; hitting `budget.max_turns` without `finish_task` triggers `hitl_policy.on_budget_exhaustion`.

The `planner.reference_shapes` bundle field restricts which shape-tools are exposed. A bundle whose author knows it's a fan-out shape can set `reference_shapes: [fan-out-fan-in]` and prevent the planner from over-engineering.

### Planner turn loop

```
Turn 1: Input: work item + workload context + audit-derived memory (state-bound)
        Decide: what shape / specialist to invoke next?
        Emit: one tool call
        (If plan_review_required and this is turn 1 — first emit plan_it_out,
         escalate via HITL, wait for approval, then continue)
Turn N: Input: prior tool's result + accumulated context
        Decide: next step?
        Emit: tool call
        ...
Turn K: Emit finish_task(result) → planner exits, result returns to caller.
```

### Planner is bounded by

- **`budget.max_turns`** from bundle (default 20 for planner).
- **`budget.max_cost_usd`** from bundle. Includes all sub-invocations (specialists, sub-workflows).
- **`budget.max_wallclock_seconds`** from bundle.
- **`hitl_policy.on_budget_exhaustion`** determines what happens when any cap is hit.

Sub-invocations count against the planner's remaining budget. A planner that invokes a specialist with its own `budget.max_cost_usd` uses the min of remaining-planner-budget and specialist-declared-cap.

### Plan-first gate composition

Plan-first gate remains a root-agent primitive. When the planner is the root agent (which is the common case when a workload has `planner.enabled: true`):

- Plan-first requires the planner's first turn to be a `plan_it_out` textual summary of intended composition — not immediate tool invocations.
- The output is prose that *describes* the structured plan (which shapes, which specialists, expected fan-out, expected budget).
- If `plan_review_required: true`, the `plan_it_out` output is escalated via HITL before turn 2.
- If `plan_review_required: false`, the plan is recorded to eventlog for audit but execution proceeds.

Prose plan + optional HITL is the best of both: human-readable, review-ready, audit-friendly, without requiring the planner to emit machine-parseable graph JSON.

## Bundle learning + refinement

Once audit-derived memory is real (positioning.md priority #7, phased for v0.3+), workload bundles become self-improving via two modes:

### Two modes

- **Propose-new.** Agent notices recurring pattern in default-bundle / unclassified traffic and suggests a named workload. *"You've had 47 sessions in the last week that all invoked the same 3 specialists, hit the same MCP tools, and finished under $0.80. Want to freeze this as a workload named `X`?"*
- **Refine-existing.** Agent notices declared bundle Y's actual runs consistently exceed a budget cap, invoke a specialist not in its list, or trigger HITL for the same class of ambiguity. Proposes a delta to bundle Y. *"Bundle `incident-triage` declared `max_cost_usd: 5.00` but p95 is $7.20; the specialists set doesn't include `NodeNotReady` which appears in 30% of runs. Suggest raising the cap and adding the specialist."*

### Mechanism

The learning pipeline is a map-reduce reference graph (shape #6 from `./workflow-scaffolding-design.md`) pointed at the audit corpus. Steps:

1. **Group** recent sessions by clustering criterion — same declared bundle + task-class, same classifier output, same first-few-tool-calls, same MCP-server usage. Grouping hyperparameters are per-deployment configurable.
2. **Per-session digest** extracts profile: specialists invoked (multi-set with counts), tool set (multi-set), budget consumed (histogram), HITL escalations (count + reason distribution), terminal state (`finish_task` / error / HITL-abandoned / budget-exhausted).
3. **Reducer** applies frequency thresholds (default 30% co-occurrence to enter recommended specialist set; p95 × 1.2 for budget-cap suggestions). Emits candidate bundle YAML with confidence scores (sample size, session-count, source-corpus fingerprint).
4. **Diff generation** for refinements: candidate compared against declared bundle; only changed fields presented for review.

### Source-corpus filtering

- **Default:** sessions terminated in `finish_task` without HITL escalation on their plan (i.e., "clean" sessions).
- **Operator-broadenable:** include HITL-escalated sessions to learn escalation patterns; include failed sessions to learn what tool sets don't work.
- **Operator-narrowable:** exclude sessions above certain cost, or in certain time windows.

The filter is a bundle-level knob on the learning pipeline itself (which is defined as a `.agents/workloads/_learning.yaml` — a workload that runs the learning), not a global default.

### Review flow

- Proposals surface in `mast-web`'s review UI.
- Views: diff (for refinements), YAML (for new bundles), linked sample sessions.
- Operator actions: approve / reject / edit / defer.
- **Approved bundles** written to `.agents/workloads/staged/<name>.yaml`. Operator moves to production location and commits to their config repo (deliberate — mast doesn't own the operator's git history).
- **Rejected proposals** recorded so the same proposal doesn't re-surface. Audit-derived memory learns from the rejection.

### Cadence

Configurable per deployment. Default: weekly pass; operator can invoke ad-hoc. Not real-time — would create review noise and reduce proposal quality.

### Risks + mitigations

| Risk | Mitigation |
|---|---|
| **Overfits to biased sample** — last week's incidents skewed to OOMKilled → proposal too narrow | Minimum-N-sessions threshold; confidence scores exposed in review UI; operator sees sample size |
| **Scope creep on declared bundles** — bundle "learns" from every session and gradually loses meaning | Never auto-mutate; each refinement is an explicit operator-approved delta |
| **Ephemeral tools / anti-patterns** — rare-single-session tools enter the bundle; suboptimal sessions teach anti-patterns | Frequency thresholds; source-corpus filter defaults to clean sessions |
| **Cross-tenant leakage** in multi-tenant deployments | `isolation.scope` respected in learning pipeline — same-tenant only by default; explicit opt-in for cross-tenant aggregation |
| **Bundle rot** — proposal accepted in month 1 doesn't reflect month 6 | Refine-existing loop keeps bundles fresh; operator opts in per-bundle to periodic refinement checks |
| **Learning proposal itself is a state-mutating action** | Follows same `hitl_policy` semantics — proposals surface via HITL, never auto-committed |

## Evaluation + regression harness

Unattended workloads that run continuously for months need ongoing validation. Without evaluation, bundle-learning proposals surface but nothing detects when a *declared* bundle's behavior silently regresses. Ships as a small subsystem alongside learning, phased for v0.3+ alongside audit-derived memory.

### Signals to detect

- **Cost regression.** Bundle X's p95 cost was $2.10 last month; this month it's $4.80 with no config change. Provider price change? Bad specialist behavior? Increased HITL escalation? Investigate.
- **Latency regression.** Bundle X's p95 duration doubled. MCP server slow? Provider throttling? Planner picking a heavier shape?
- **Success-rate regression.** Bundle X's `finish_task`-outcome rate dropped from 92% to 78%. Errors up? HITL-abandonment up? Budget exhaustion up?
- **Specialist-behavior drift.** Specialist Y's `finish_task`-first-turn rate dropped (specialist is thrashing more before finishing). Provider model update changed behavior? Specialist prompt no longer matches current inputs?
- **HITL-pattern shift.** Bundle X used to escalate for ambiguity 5%; now 25%. Real change in incoming work shape? Something about the workload is broken?

### Regression detection mechanism

Regression detection composes on top of bundle-learning infrastructure — same map-reduce pipeline over audit corpus, different reducer semantics.

- **Baseline capture.** Each bundle carries an implicit *baseline* (rolling window of the last N sessions, or an explicit operator-anchored baseline set) — cost distribution, latency distribution, success rate, HITL rate.
- **Ongoing comparison.** New sessions compared against baseline; delta above threshold flags an alert.
- **Alert surface.** Regressions emit alerts via observability ([`./observability-design.md`](./observability-design.md)) — Prometheus alert firing rules; mast-web surfaces in a `Regressions` view; audit-derived memory learns from operator resolution.
- **Not auto-mitigating.** Detection surfaces; operator decides whether to rollback, refine bundle, escalate. Auto-mitigation is out of scope (too many failure modes).

### Golden-trace comparison

For high-value workloads (revenue-affecting incident triage, cost-sensitive report generation), operators can capture *golden traces* — anchor snapshots that represent "known good" behavior:

- `mast sessions capture-golden <session-id> --workload=incident-triage --tag=v1` — captures the session's event stream as a golden trace for the bundle.
- Continuous evaluation runs subsequent sessions against the golden: same input class, same bundle version, compared event stream. Deltas flag as regression signal.
- Comparison is at the shape-level (which specialists invoked in which order, which shapes the planner picked, HITL/no-HITL) not the token-level (LLM output is nondeterministic).

Golden-trace comparison is opt-in per bundle — the overhead is real; operators pick which workloads warrant it.

### A/B testing bundles

Two bundles for the same task class can run against traffic side-by-side; comparison surfaces which is better on chosen dimensions.

- `.agents/workloads/incident-triage-a.yaml` and `incident-triage-b.yaml` — same `task_class`, different specialist rosters or budget or planner shape.
- Deployment config routes traffic: 90% to bundle A (control), 10% to bundle B (candidate); random assignment per session with a stable-per-request hash so retries land on the same bundle.
- Metrics tagged with `bundle_variant` label surface per-variant cost / latency / success rate / HITL rate.
- After sufficient sample size (operator-configured, default 500 sessions per variant), a report surfaces in mast-web: variant A vs. variant B on each dimension with confidence intervals.
- Operator promotes winner (or reverts to control) explicitly. No auto-promotion.

### Cost regression detection specifically

Cost is the most operationally-sensitive signal; gets its own detection path:

- Rolling p50/p95/p99 cost per bundle per tenant emitted as Prometheus metrics ([`./observability-design.md`](./observability-design.md)).
- Alert rules ship as `examples/observability/alerts/mast-cost-regressions.yaml` — configurable thresholds.
- Cost regressions on tenant-scoped bundles alert the tenant's operator; cross-tenant regressions alert deployment-level.
- Composes with `budget.max_cost_usd` on bundles — hitting the cap triggers `hitl_policy.on_budget_exhaustion`; approaching the cap regularly triggers a regression alert.

### Phasing

| Version | Scope |
|---|---|
| **v0.1** | Not shipped; needs audit-derived memory + bundle-learning substrate |
| **v0.2** | Basic regression signals emitted as Prometheus metrics (cost p95, latency p95, success rate) per bundle |
| **v0.3** | Golden-trace capture + comparison; A/B testing framework; regression alert rule starters |
| **v0.4+** | Auto-mitigation options (opt-in per bundle); cross-bundle-family evaluation; long-horizon trend detection |

## Composition

| Subsystem | Interaction with workloads | Interaction with planner | Interaction with learning |
|---|---|---|---|
| Specialists (`./specialists-design.md`) | Enumerated in `specialists:` roster; loaded per session | Vocabulary via `invoke_specialist` tool; `WithUseSubBranch` isolates | Specialist co-invocation patterns are strongest clustering signal |
| Skills ([`./skills-design.md`](./skills-design.md)) | Enumerated in `skills:` roster (local + registry-discovered); loaded per session; tool allowlist intersected with `tool_catalog` | Vocabulary via `invoke_skill` tool | Skill usage patterns feed reducers; bundle-learning proposes skill additions based on published-catalog matches |
| Reference-graph library (`./workflow-scaffolding-design.md`) | Available shapes referenced in `planner.reference_shapes` | Vocabulary via `run_shape_*` tools | Reference-graph selection patterns are learnable |
| `mast-web` | Bundle browser + editor | Turn-by-turn planner visibility; HITL on plan review | Review UI for propose-new + refine-existing |
| Attach mode | Envelope routing at entry | HITL escalation via `RequestInputEvent` | Proposals surface via attach events |
| Task classes | Bundle names `task_class` | Planner respects task-class mode (typically `orchestrate` → `Task`) | Learned bundles carry `task_class` |
| Audit-derived memory | Bundle-level pattern extraction via state-bound nodes | Planner input via state-bound nodes | Learning IS the primary consumer |
| Watchdog | Signal routing per bundle `hitl_policy` | Watchdog signals reach planner via emitting-node pattern | Watchdog trigger patterns become learning signals |
| Isolation | `isolation.scope` field on bundle | Planner subagents use `WithUseSubBranch`; per-tenant workers use `WithIsolationScope` | Same-tenant filter respects isolation |
| Plan-first gate | N/A | Planner's turn-1 `plan_it_out` satisfies gate; HITL if `plan_review_required` | N/A |
| A2A ([`./a2a-design.md`](./a2a-design.md)) | Bundle field `a2a.expose` opts workload into A2A publication (skill in agent card); bundle field `a2a_agents` enumerates external A2A agents the workload may invoke | External A2A agents in planner vocabulary via `invoke_remote_agent("a2a://...", ...)` | A2A remote agents surface as one signal in bundle-learning |
| Federation ([`./federation-design.md`](./federation-design.md)) | Bundle field `federation.remotes` enumerates remote agents (any adapter); `federation.state_propagation` for mast-native cross-instance state; `federation.cross_tenant` for cross-tenant federation | Planner tool `invoke_remote_agent(reference, inputs)`; adapter selected by reference protocol scheme | Cross-instance session correlation feeds learning; federation-shape choice becomes learnable |

## Task-class resolution + the dissolved SingleTurn open question

The four resolution paths (explicit / envelope / bundle-selection / classifier-first) collectively make task class runtime-resolved. The `--task=X` CLI flag remains the laptop / library-embedder path but is no longer the sole entry point.

### Public task classes

`chat` / `debug` / `implement` / `research` / `review` / `orchestrate`

- **`orchestrate`** is new. Bundles that enable the planner typically declare `task_class: orchestrate`. Maps to `Task` mode with per-planner DefaultInstruction (opinionated-orchestration frame).
- **`chat`** → `Chat` mode. Interactive coordinator role.
- **`debug` / `implement` / `research` / `review`** → `Task` mode with per-class DefaultInstruction variants.

### SingleTurn is internal, not user-facing

The SingleTurn ADK v2 agent mode is used by:

- Classifier-first dispatcher (session-resolution component).
- `mode: SingleTurn` specialists (per specialists-design.md schema).
- LLM-as-router classifiers within workflow reference graphs.
- Small-tier-parent classifier (replacing the substring matcher).

None of these are user-facing task classes. **The pending open question "task-class name for SingleTurn mode" (fork-design.md open question #3) resolves as N/A** — SingleTurn is an internal mode, exposed via specialist frontmatter (`mode: SingleTurn`) and via internal runtime components. No public `--task=classify` / `route` / `one-shot` / `dispatch` needed.

Clean reduction in surface: the public task-class set stays at six, and every use case for SingleTurn is served by the specialist schema or by runtime-internal components.

## Phasing

| Version | Ships | Depends on |
|---|---|---|
| **v0.1** (fork trigger fires) | Workload bundle schema + loader (hand-authored bundles). Envelope + bundle-selection resolution paths. `--workload=<name>` CLI flag. Basic classifier-first dispatch. Planner scaffolded — schema for tool vocabulary in place, but `run_shape_*` tools may not all be implemented yet. Learning: not included. | Phase 1 of the fork; specialists loader; minimal reference-graph library. |
| **v0.2** | Planner complete — all `run_shape_*` tools wired; `plan_review_required` works end-to-end. `orchestrate` task class exposed. Bundle-scoped classifiers (nested) supported. | Reference-graph library completion (2 canonical shapes in v0.1; remainder in v0.2). |
| **v0.3** | Bundle learning + refinement. Review UI in mast-web. | Audit-derived memory implementation (positioning.md priority #7) is real. |
| **v0.4+** | Learning cadence tuning; cross-deployment federation (if operators managing many similar deployments ask); GitOps PR integration for learned bundles. | Real-world operator feedback from v0.3. |

## Open questions

1. **Classifier-miss fallback.** What happens when the classifier's output doesn't match any bundle AND there's no declared default? Options: fail-loud (error to source), fail-soft (run against a generic bundle), or ask-operator (HITL escalation). Bias: fail-loud in unattended mode; ask-operator in interactive mode (attach connected).
2. **Learning proposals: per-deployment vs. cross-deployment.** Default per-deployment (privacy, tenancy). Optional cross-deployment aggregation for operators managing many similar deployments. Deferred to v0.4+.
3. **Bundle-load-time vs. session-start-time validation.** When a workload bundle's `specialists` list references a specialist file that doesn't exist, error at bundle-load time or at session-start time? Bias: bundle-load time (fail fast). Same for `tool_catalog.mcp[].server` referencing an unconfigured MCP server.
4. **`spawn_agent` as planner escape hatch.** Should the planner's tool vocabulary include `spawn_agent` (dynamic subagent creation) as an escape hatch when no reference shape fits? Bias: yes, but budget-gated — planner-spawned agents count against remaining planner budget, and are subject to the same `hitl_policy.on_mutation` gate as any state-mutating action.
5. **GitOps integration.** Should learning proposals be opened as PRs against the operator's config repo? Nice for audit; big enough to be its own follow-on effort. Deferred to v0.4+.
6. **Cross-bundle specialist sharing.** When two bundles both need `NodeNotReady`, is that a duplicated reference or a canonical roster? Bias: duplicated reference (bundles stay self-contained; specialists live once in `.agents/specialists/`). Learning can still recognize the shared specialist as a signal.
7. **Planner instruction template.** The planner's system prompt needs a template that references the bundle's specialists + reference shapes + budget + HITL policy. Instruction structure: single instruction file with template variables, or per-bundle override? Bias: single template with variables in `pkg/instruction/` (uses the existing loader v2 mechanism); per-bundle override as an escape hatch.
8. **Budget attribution on nested planners.** When a planner invokes `run_shape_supervisor_workers` with another planner as the supervisor body, whose budget applies? Bias: nested planner's `max_cost_usd` composes as `min(parent-remaining, nested-declared)`; nested planner's `max_turns` is its own.

## Out of scope

- **Cross-deployment federation** of workload bundles — deferred to v0.4+.
- **Automated bundle promotion** — learning proposals require operator approval; no auto-merge to `.agents/workloads/`.
- **Planner as graph-materialization engine** — explicit non-goal; planner is supervisor-body, not graph-JSON-emitter.
- **Bundle versioning / history** — bundles are files in the operator's config repo; git provides versioning. Mast doesn't own a versioning surface.
- **Cross-runtime planner interop** — Python ADK has its own planning story; not designed for interop.
- **Real-time / per-session learning** — weekly cadence minimum; ad-hoc invoke available. Real-time creates review noise + poor signal-to-noise on proposals.
- **Planner as a user-facing agent for interactive coding** — Chat-mode coordinator + specialists remain the interactive shape; planner is unattended-shaped.
- **Bundle inheritance / composition** — bundles don't extend other bundles. If two bundles share config, factor to a shared MCP config file consumed by both; bundles themselves stay flat. Revisit if operator authoring friction becomes real.

## Related

- [`./positioning.md`](./positioning.md) — the thesis this serves; priorities #4 (workflow scaffolding), #5 (multi-session deployment), #7 (audit-derived memory) all compose here.
- [`./fork-design.md`](./fork-design.md) — ADK v2 from day one; the phasing table above sits alongside the P1.1–P1.6 bucket sequencing.
- [`./specialists-design.md`](./specialists-design.md) — specialists are the vocabulary that bundles enumerate and planners invoke.
- [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) — the seven canonical shapes that form the planner's tool vocabulary and that the learning pipeline is itself an instance of (map-reduce shape #6).
- [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) — the operator review UI for bundle proposals and HITL on plan reviews.
- [core-agent's context-management-design.md](https://github.com/go-steer/core-agent/blob/main/docs/context-management-design.md) — Mechanism B / digest pattern underlying the per-session digest step in the learning pipeline.
- ADK v2 workflow package (`google.golang.org/adk/v2/workflow`) — the substrate for planner-invoked sub-workflows.
