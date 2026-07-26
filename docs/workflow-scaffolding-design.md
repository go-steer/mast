# mast workflow scaffolding: design

**Status:** draft, 2026-07-01 (updated 2026-07-25 — spike-2 verification against ADK v2.1.0: API-name corrections in shapes #1/#6, root-agent rules, resume/re-execution contract, parallel-HITL constraint, v0.1 shape subset. See "Spike-2 verification notes" below and [`./adk-v2-usage.md`](./adk-v2-usage.md) for the construct-level detail). Companion to [`./positioning.md`](./positioning.md) (the thesis — priority #4 in the "next 6 months" list is the workflow-scaffolding example library), [`./fork-design.md`](./fork-design.md) (which resolves ADK v2 from day one as the substrate this builds on), and [`./specialists-design.md`](./specialists-design.md) (the subagent-as-tool subsystem that composes with workflow nodes). This doc covers the workflow-scaffolding subsystem in detail — the canonical shapes mast ships, how each maps onto ADK v2 primitives, and the composition rules between shapes, specialists, attach mode, and session state.

## Why this is a real subsystem

Positioning.md priority #4 originally scoped a "workflow-scaffolding example library — 4-6 canonical shapes, each with example + doc page." At the time, the working assumption was that mast would ship helper packages *above* the ADK v1 runtime to make each shape reachable in short code.

ADK v2's graph engine (`google.golang.org/adk/v2/workflow`) delivers all of those shapes as native primitives: nodes + edges + routing express fan-out-fan-in, sequential pipelines, cyclic autonomous loops, dynamic-list orchestration, and HITL pauses without any wrapper. This changes what mast has to build.

**Mast's contribution collapses from "workflow engine + helpers + examples" to "reference graphs that wire v2 primitives to mast's domain (GKE, Prometheus, MCP servers, specialists, task-class profiles)."** The engine is ADK's; the domain wiring is ours. That reduction is the point of this doc — it prevents accidentally reintroducing a helper layer that would compete with v2.

## Design principle: reference graphs, not helper packages

We do **not** ship a workflow abstraction above ADK v2. Every canonical shape is a `main.go` under `examples/workflows/<shape>/` that operators can read, copy, and adapt. Rationale:

- **Every abstraction we add is one operators have to learn on top of v2.** V2 is the substrate the whole Go agent ecosystem will converge on; a mast-specific wrapper is a local dialect.
- **Operators mostly want to adapt, not invoke.** The shapes exist to be modified per deployment (different tool sets, different classifiers, different HITL policies). Helper packages optimize for "call this function"; reference graphs optimize for "read and adapt this file."
- **Divergence in the canonical set is signal.** If an operator finds their real workflow can't be expressed by adapting one of the seven shapes, that's information — either a shape we should add, or a case for a bespoke graph. Helper packages hide that signal.

Exception: cross-cutting concerns that don't fit inside a single graph — cost ceilings, permission gates, watchdog integration — stay as first-class runtime concerns configured on the parent agent (the shapes inherit them via `agent.Context`).

### Shapes are forkable starters, not demonstrations (added 2026-07-25)

The framing above is sharpened by a decision from the smaller-agents discussion (see [`./positioning.md`](./positioning.md) "Smaller agents, slim embeds"): each shape directory is a **starter someone forks to get a purpose-built agent**, not a demo that exists to showcase mast. `mast-prototype` (spikes 1-2) is the proof and the first instance — a complete triage control loop in ~2k LOC that a team could fork, gut, and run. Three disciplines follow:

1. **Standalone-runnable.** Every starter runs end-to-end offline with one command (the `demo-spike2.sh` bar): a deterministic fake model, no credentials, no config beyond the directory itself. A starter that needs the reader to assemble context from three docs has failed its job.
2. **Self-contained.** A starter imports mast packages and ADK — never another starter, never shared "starter helpers." Duplication between starters is accepted on purpose; a shared helper layer would silently rebuild the workflow abstraction this section forbids.
3. **Fork-and-forget is the supported lifecycle.** Forked starters don't get upgrade paths, deprecation cycles, or compatibility promises — they're the reader's code the moment they copy it. Mast's CI keeps the in-repo originals building (P1.5's examples gate); that is the entire maintenance surface. Explicitly rejected: shipping starters as prebuilt single-purpose binaries ("mast-triage", "mast-monitor") — a support surface with none of the substrate's leverage.

If starters get real adoption, the selection question ("which shape do I need?") grows into the planner / `orchestrate` story already phased at v0.2+ ([`./orchestration-design.md`](./orchestration-design.md)) — the smaller-agents path converges back into mast rather than fragmenting it.

## File layout

```
examples/workflows/
  fan-out-fan-in/
    main.go
    README.md
    config.yaml            # example agent config
  sequential-pipeline/
    ...
  supervisor-workers/
  autonomous-loop/
  adversarial-verifier/
  map-reduce/
  llm-as-router/
```

Each directory is a runnable `go run ./examples/workflows/<shape>/`, standalone. `README.md` per shape names the mast-audience use case, the v2 primitives used, and the customization points (which tools, which classifier, which HITL policy).

## The seven canonical shapes

### 1. Fan-out-fan-in

**Description.** A planner emits N independent tasks. Workers execute them in parallel. A join node aggregates the results. A summarizer produces the final output.

**Mast use case.** `gke-parallel-triage` — for each failing service in a cluster, spawn a triage subagent; join per-service diagnoses into a single incident summary.

**v2 primitives.**
- `workflow.NewFunctionNode` — planner emits `[]Task`.
- `workflow.NewEdgeBuilder().AddFanOut(planner, workers...)` if the worker count is static; parallel workers (`workflow.NewParallelWorker(name, wrapped, maxConcurrency, cfg)` or `NodeConfig{ParallelWorker: true}`) if the list is dynamic and homogeneous. *(Corrected 2026-07-25: `NewParallelWorkerNode` / `AddFanOutDynamic` from earlier drafts don't exist in v2.1.0.)*
- Workers are either `agenttool`-wrapped Task-mode `LlmAgent`s (specialist-shaped) or plain function nodes for deterministic work.
- Join node (`AddFanIn`) aggregates outputs into a map.
- Summarizer: `NewFunctionNode` or a Task-mode agent node.

**Sketch:**
```go
plan := workflow.NewFunctionNode("plan-triage", planTriage, cfg)
triage := specialists.AgentNode("ServiceTriage", registry)  // Task-mode
join := workflow.NewJoinNode("collect", cfg)
summary := workflow.NewFunctionNode("summarize", summarize, cfg)

b := workflow.NewEdgeBuilder()
b.AddFanOut(plan, triageWorkers)   // triageWorkers = NewParallelWorker over the triage node
b.AddFanIn(join, triageWorkers)
b.AddRoutes(join, map[string]workflow.Node{"": summary})
```

**Notes.** With a static task list, `AddFanOut` is exact fit. With a dynamic list computed at runtime, either wrap the worker node in `NewParallelWorker` or use a dynamic node that calls `RunNode` per element — see supervisor+workers below. **Constraint (2026-07-25): HITL cannot be raised from inside parallel branches (`ErrParallelHITLUnsupported`)** — operator escalation belongs after the join, or the escalating item must be routed out of the parallel section first.

### 2. Sequential pipeline

**Description.** Node A → B → C, each transforming the previous output. The dullest shape and often the right one.

**Mast use case.** Prometheus alert → extract root cause → propose remediation. First stage is a function node parsing the alert payload; second is a Task-mode LlmAgent digesting metrics into a hypothesis; third is a tool node opening a PR draft.

**v2 primitives.**
- Plain edges connecting nodes in order.
- Any node type (function, agent, tool) is legal at any position.
- State-bound nodes (`NewFunctionNodeFromState`) let later stages pull earlier values from session state without threading them through explicit outputs.

**Sketch:**
```go
parse := workflow.NewFunctionNode("parse-alert", parseAlert, cfg)
diagnose := specialists.AgentNode("RootCauseAnalyst", registry)
propose := workflow.NewToolNode("propose-pr", ghTools.OpenPR, cfg)

b := workflow.NewEdgeBuilder()
b.AddRoutes(parse, map[string]workflow.Node{"": diagnose})
b.AddRoutes(diagnose, map[string]workflow.Node{"": propose})
```

**Notes.** Include this shape as a reference explicitly *because* it's dull — operators tend to reach for supervisor+workers when a pipeline is what they need. The reference implementation is the excuse to say so in the README.

### 3. Supervisor + workers

**Description.** A long-running orchestrator dispatching scoped tasks to workers over time. Workers are instances of the same role but different scopes; the supervisor holds context across rounds.

**Mast use case.** Multi-tenant deployment orchestrator — one supervisor loop per operator session; per-cluster worker subagents spawned on demand, each getting a scoped tool allowlist and isolated history. This is the concrete shape behind positioning.md priority #5 (multi-session deployment story).

**v2 primitives.**
- Dynamic node (`workflow.NewDynamicNode`) — supervisor's orchestration body is plain Go, calling `RunNode[Result](nc, worker, task, workflow.WithIsolationScope(scope))` per work item.
- Task-mode `LlmAgent` workers wrapped as agent nodes.
- `WithIsolationScope(tenantID)` per worker call segregates history across tenants.
- `WithUseSubBranch(true)` isolates the worker's internal reasoning from the supervisor's context.

**Sketch:**
```go
supervisor := workflow.NewDynamicNode("supervisor",
    func(nc agent.Context, in Assignment, emit func(*session.Event) error) (Report, error) {
        var report Report
        for _, cluster := range in.Clusters {
            r, err := workflow.RunNode[ClusterReport](nc, clusterWorker, cluster,
                workflow.WithIsolationScope(cluster.TenantID),
                workflow.WithUseSubBranch(true),
            )
            if err != nil { emit(warnEvent(cluster, err)); continue }
            report.Clusters = append(report.Clusters, r)
        }
        return report, nil
    }, cfg)
```

**Notes.** This is the tenancy story. `WithIsolationScope` is what makes it multi-tenant-safe: without it, prompt history from cluster A leaks into cluster B's worker context. Reference README calls this out explicitly — it's the failure mode operators reach for helper packages to solve.

### 4. Autonomous loop

**Description.** A scheduled or event-triggered loop that runs unattended. Loop condition = routing decision; terminator = edge to finalizer.

**Mast use case.** Scheduled monitor (drift detection, cost check); inbox loop (poll inbox → process → back to poll); watchdog-triggered auto-continue.

**v2 primitives.**
- Cyclic graph (v2 makes cycles first-class — a completed node can be re-triggered).
- Router node emits `continue` vs `exit` route.
- Emitting node for heartbeat events (surfaced to `mast-web` via attach mode).
- State-bound nodes for accumulated loop state (iteration count, last-checkpoint timestamp).
- HITL escalation via `RequestInputEvent` when the loop hits ambiguity — durable across process restarts.

**Sketch:**
```go
step := specialists.AgentNode("MonitorStep", registry)   // Task-mode, single iteration
check := workflow.NewFunctionNode("check-continue", checkContinue, cfg)
finalize := workflow.NewFunctionNode("finalize", finalize, cfg)

b := workflow.NewEdgeBuilder()
b.AddRoutes(step, map[string]workflow.Node{"": check})
b.AddRoutes(check, map[string]workflow.Node{
    "continue": step,       // cycle back
    "exit":     finalize,
})
```

**Notes.** The v2 cyclic graph replaces the custom loop machinery in today's `pkg/agent/autonomous.go` and `pkg/agent/inbox.go`. Pause/resume is durable via the graph engine — a scheduled monitor that pauses for operator input at 02:00 can resume at 09:00 when the operator logs into `mast-web`. This is the sleeper feature of v2 for unattended workloads: HITL escalation from an unattended loop, without workflow wrapping.

### 5. Adversarial verifier

**Description.** Proposer produces a candidate. Skeptic tries to refute it. Decision node consumes both.

**Mast use case.** Change-proposal review — proposer LLM drafts a config change (Kubernetes manifest, Terraform diff, IAM policy); skeptic LLM tries to identify blast radius, missing rollback, credential exposure; decision node either accepts, rejects, or escalates to a human via `RequestInputEvent`.

**v2 primitives.**
- Fan-out from a shared input to proposer + skeptic agent nodes (Task-mode).
- Join collects both outputs.
- Decision function node applies policy (accept if skeptic finds no issues; reject if issues are critical; HITL if ambiguous).
- `RequestInputEvent` with `ResponseSchema` for HITL escalation — the schema drives `mast-web`'s approval-form generation.

**Sketch:**
```go
proposer := specialists.AgentNode("ChangeProposer", registry)
skeptic  := specialists.AgentNode("ChangeSkeptic",  registry)
join     := workflow.NewJoinNode("collect-verdicts", cfg)
decide   := workflow.NewEmittingFunctionNode("decide",
    func(ctx agent.Context, in JoinedVerdicts, emit func(*session.Event) error) (Decision, error) {
        switch policy.Classify(in) {
        case policy.Accept: return Decision{Apply: true}, nil
        case policy.Reject: return Decision{Apply: false, Reason: in.Skeptic.Reason}, nil
        case policy.Ambiguous:
            resp, err := emitHITL(ctx, emit, in)
            if err != nil { return Decision{}, err }
            return Decision{Apply: resp.Approved, Reason: resp.Note}, nil
        }
    }, cfg)
```

**Notes.** Especially valuable for unattended because no human is watching by default; the skeptic is the substitute reviewer. Composes with HITL escalation as the third leg for cases neither the proposer nor the skeptic can resolve confidently. Response schema on HITL escalation should be typed narrowly (`{approved: bool, note?: string}`) — free-form text puts the burden on the decision node to reparse. Two constraints verified 2026-07-25: (1) the `decide` node sits *after* the join, which is required — HITL from inside the parallel proposer/skeptic branches would hit `ErrParallelHITLUnsupported`; (2) `emitHITL` in the sketch must follow the re-entry contract — node bodies re-execute on resume, so the body checks `ctx.ResumedInput(id)` first and anything the resume pass needs (here, the joined verdicts are re-fed as node input, so nothing extra) is stashed in session state before interrupting. See "Spike-2 verification notes" below.

### 6. Map-reduce over corpus

**Description.** Fan out over N inputs (each summarized independently); aggregate to one output.

**Mast use case.** Log-corpus summarizer — per-log-file digest → aggregate incident timeline. Also: audit-log-derived-memory backfill (per-session digest → cross-session memory extract).

**v2 primitives.**
- Parallel workers over the input list.
- Join node collects per-input outputs.
- Reducer function node produces the single aggregate output.

**Sketch:**
```go
digest := workflow.NewFunctionNode("digest-file", digestFile, cfg)
workers, _ := workflow.NewParallelWorker("per-file", digest, maxConcurrency, cfg)
join   := workflow.NewJoinNode("collect-digests", cfg)
reduce := workflow.NewFunctionNode("reduce-timeline", reduceToTimeline, cfg)

b := workflow.NewEdgeBuilder()
b.AddRoutes(workers, map[string]workflow.Node{"": join})
b.AddRoutes(join,    map[string]workflow.Node{"": reduce})
```

**Notes.** Similar to fan-out-fan-in but the reducer step is *reduce* (N → 1) rather than *summarize* (N → structured N-length report). Include as its own shape because the reducer pattern is distinct — and because the audit-derived-memory implementation is a real map-reduce, not a fan-out-fan-in.

### 7. LLM-as-router

**Description.** An `LlmAgent` classifies input; a router node emits the matching route; the graph dispatches to the right handler.

**Mast use case.** Incident-triage dispatcher — classify a pod-failure event by type (`ImagePullBackOff` / `CrashLoopBackOff` / `OOMKilled` / unknown), dispatch to the matching specialist. Directly composes with the specialists subsystem — each route target is a specialist agent node.

**v2 primitives.**
- `LlmAgent` in `SingleTurn` mode as agent node — classifier is cheap and stateless.
- Function node emitting a `StringRoute` matching the classifier's category output.
- `AddRoutes(router, {"ImagePullBackOff": ..., "CrashLoopBackOff": ..., ..., "default": fallback})` with a `Default` route for the unknown-category case.
- Task-mode specialist agent nodes as handlers.

**Sketch:**
```go
classifier := llmagent.New(llmagent.Config{
    Name:  "IncidentClassifier",
    Mode:  llmagent.SingleTurn,
    Model: "gemini-2.5-flash",
    Instruction: "Classify the pod-failure event into one of: ImagePullBackOff, CrashLoopBackOff, OOMKilled, unknown.",
})
routeFn := workflow.NewFunctionNode("route", func(ctx agent.Context, in string) (workflow.StringRoute, error) {
    return workflow.StringRoute(in), nil
}, cfg)

b := workflow.NewEdgeBuilder()
b.AddRoutes(classifier, map[string]workflow.Node{"": routeFn})
b.AddRoutes(routeFn, map[string]workflow.Node{
    "ImagePullBackOff":  specialists.AgentNode("ImagePullBackOff",  registry),
    "CrashLoopBackOff":  specialists.AgentNode("CrashLoopBackOff",  registry),
    "OOMKilled":         specialists.AgentNode("OOMKilled",         registry),
    workflow.Default:    fallback,
})
```

**Notes.** `SingleTurn` mode is the right shape for the classifier — one call, one output, cheap tier. This pattern also replaces the substring-matcher small-tier-parent classifier (positioning.md open Q #4) with a real LLM call that ages gracefully as model IDs change. The fallback route is where "unknown category" work goes — either a generic-diagnostic specialist or an HITL escalation node. **This same shape applied at the session-configuration level is how [`./orchestration-design.md`](./orchestration-design.md)'s classifier-first workload dispatch works** — a SingleTurn classifier picks the workload bundle for the incoming request before any session is instantiated. Same primitive, one level up.

## Composition + reuse

Sub-workflow nodes (`workflow.NewWorkflowNode` embedding an entire sub-graph as a single node) mean the seven shapes compose. Common combinations:

- **LLM-as-router in front of fan-out-fan-in.** Router picks the incident type; the chosen route is itself a fan-out-fan-in graph (per-affected-service triage). Two shapes; one graph.
- **Adversarial verifier as a stage in a sequential pipeline.** Pipeline: parse alert → propose remediation (adversarial verifier decides) → apply. The verifier is a sub-workflow node.
- **Autonomous loop wrapping a supervisor+workers step.** Each loop iteration runs a full supervisor+workers graph; the loop's continue-vs-exit routing depends on the aggregated worker report.

The reference-graph directory should include a `composition/` subdirectory with 2-3 combined examples once the singles are stable. Keep the singles the canonical entry point — combined examples exist to demonstrate the pattern, not to be the default starting shape.

## Interaction with other mast subsystems

### With specialists

Specialists and workflow shapes compose in two ways:

1. **Specialist as agent node.** The specialists registry (see [`./specialists-design.md`](./specialists-design.md)) exposes each specialist as an agent node the graph can drop in directly. This is the primary composition — every workflow shape above uses this pattern.
2. **Specialist as `agenttool`.** For dynamic invocation from *inside* another agent's reasoning (parent decides mid-turn to call a specialist), the existing `agenttool`-wrapped specialist stays the mechanism. Not workflow-driven.

Both patterns coexist; the choice is about *when* the specialist gets invoked — deterministically at graph position X (agent node) vs. dynamically at parent's discretion (`agenttool`).

### With attach mode + mast-web

- **Emitting nodes** are the natural surface for progress events to `mast-web` — heartbeats from autonomous loops, per-worker status from fan-out-fan-in, decision-node explanations from adversarial verifier. `mast-web` renders these in the session event stream without needing separate telemetry plumbing.
- **`RequestInputEvent`** surfaces to `mast-web` as an operator prompt. The event's `ResponseSchema` drives form generation — approval buttons for `{approved: bool}` schemas, free-text fields for `{note: string}`, etc. See [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) for the UI shape.
- **Unified telemetry span tree** (v2 gain) means graph execution, plain-agent execution, and specialist invocation all share one trace shape. `mast-web`'s session-view can render them uniformly.

### With task-class profiles

Task-class profiles (post-v2: shaped by agent modes; see [`./fork-design.md`](./fork-design.md) resolved-decisions) determine which workflow-scaffolding examples are load-bearing per task class:

| Task class | Workflow shapes typically instantiated |
|---|---|
| `debug` | Adversarial verifier (proposer + skeptic on hypotheses), LLM-as-router (dispatch by error type) |
| `implement` | Sequential pipeline (design → implement → test) |
| `research` | Map-reduce (per-source digest → synthesis), fan-out-fan-in (parallel investigations) |
| `review` | Adversarial verifier (author + reviewer), map-reduce (per-file review → aggregate) |
| `chat` | None — interactive coordinator doesn't preordain graph shape |
| *SingleTurn class (pending name)* | LLM-as-router classifier stage; standalone classifier calls |

These are *defaults for the reference examples*, not runtime enforcement. Any task class can use any shape.

### With watchdog + cost ceilings

- **Per-node `Timeout`** and `RetryConfig` (v2's `DefaultRetryConfig` = 5 attempts, 1s→60s, 2× backoff, full jitter) enforce per-node cost bounds — the specialist-budget primitives from `specialists-design.md` map onto these directly.
- **Graph-wide `WithMaxConcurrency(n)`** caps concurrent agent invocations across a workflow — critical for fan-out-fan-in and map-reduce where a naive fan-out could saturate provider quotas.
- **Watchdog signals** reach graph nodes via the session event stream (per core-agent issue #159, resolved by v2's emitting-node pattern). Nodes can check the event stream for watchdog alerts and route accordingly (e.g., autonomous-loop's continue-vs-exit routing can consult watchdog state).

### With session state + audit-derived memory

- **State-bound nodes (`NewFunctionNodeFromState` + `state:"<key>"` tags)** pull session-state values as typed node inputs. This is the primary integration point for audit-derived memory: the memory-derivation pipeline writes to known state keys; downstream nodes consume via tags. No prompt-parsing plumbing.
- **Session-durable pause/resume** means workflow state (including in-flight parallel branches) survives process restarts. Combined with audit-derived memory persisting across sessions, this is the closed-loop unattended primitive.

### With isolation

- **`WithIsolationScope(id)`** on `RunNode` calls (or on parallel workers) segregates history per scope. Multi-tenant supervisor+workers is the primary use case; audit isolation between concurrent operators is the secondary.
- **`WithUseSubBranch(true)`** isolates a child's context from its parent — the specialist-isolation primitive.
- **Branch isolation** (v2 default for parallel branches) prevents prompt history from branch A leaking into branch B. This is why fan-out-fan-in and map-reduce don't need extra care: v2 does the isolation.

## Migration story (from core-agent patterns)

Core-agent's existing patterns port to reference-graph shapes at different levels of effort:

| Core-agent pattern | Mast shape | Effort |
|---|---|---|
| `pkg/agent/autonomous.go` custom loop | Autonomous loop (cyclic graph) | Rewrite the loop as a graph; pause/resume comes for free. |
| `pkg/agent/inbox.go` inbox poll | Autonomous loop with router (poll → process → poll) | Straightforward port. |
| `spawn_agent` dynamic sub-agents | Supervisor+workers with dynamic node | Direct port; `spawn_agent` becomes an escape hatch when the pattern doesn't fit the graph shapes. |
| `examples/gke-parallel-triage` custom fan-out | Fan-out-fan-in reference graph | This *becomes* the reference implementation; less code than today. |
| `examples/scheduled-monitor` custom scheduler | Autonomous loop with time-triggered entry | Trigger stays external (cron / operator); loop body is the graph. |

Migration is intentional per-workload, not automatic. There's no `migrate-to-workflows.sh` — each existing pattern is small enough that porting to the reference shape is faster than writing a migration.

## Spike-2 verification notes (2026-07-25)

The LLM-as-router shape (#7) was built and run end-to-end against ADK v2.1.0 in the `mast-prototype` repo (`pkg/graph`; see its `FINDINGS.md`), against the GKE-triage anchor workload. Facts the shape library must build on:

- **Root-agent rules.** A `workflowagent.New`-wrapped graph runs as the runner's **root agent** directly. The runner's Chat-mode restriction applies only when the root *is* an `LlmAgent` (non-LlmAgent roots take a generic path). An earlier spike-1 conclusion that "a bare Workflow cannot be a root agent" was wrong; graphs do not need a coordinator above them. The SubAgents-dispatch pattern (Chat coordinator + auto-installed `task`/`single_turn` tools per sub-agent) remains a valid *alternative* shape — the prototype keeps both behind a flag for comparison — but it routes by tool-description reading on a frontier coordinator rather than by typed `Event.Routes`, with the cost and legibility differences that implies.
- **Task-mode specialists in graphs.** Wrap in `workflow.NewAgentNode`, invoke via `RunNode` from a `DynamicNode` body. `finish_task` is auto-installed and its argument becomes the node output.
- **Resume/re-execution contract.** Dynamic-node bodies re-execute on resume (`RerunOnResume`); `RunNode` does **not** return cached child results across a pause turn (dynamic children aren't in the static graph `ReconstructRunState` rehydrates). Body shape must be ResumedInput-first with a session-state stash for anything the resume pass needs. Consequence for every shape here that mixes children with HITL: side effects before the interrupt re-run unless guarded — see [`./durable-execution-design.md`](./durable-execution-design.md) side-effect semantics.
- **Parallel-branch HITL is unsupported** (`ErrParallelHITLUnsupported`) — constrains shapes #1, #5, #6 as noted inline.
- **v0.1 shape subset (named, resolving the phasing gap with [`./orchestration-design.md`](./orchestration-design.md)'s "2 canonical shapes in v0.1"):** **LLM-as-router** (#7 — classifier-first dispatch and the triage anchor need it; already proven) and **fan-out-fan-in** (#1 — the `gke-parallel-triage` smoke example needs it). The remaining five ship in v0.2 per fork-design Phase 2. P1.4's planner scaffold wires `run_shape_*` tools to these two; the P1.4→P1.5 sequencing in fork-design should reflect that these two shapes land *before or with* the planner scaffold, not after.

## Open questions

1. ~~**Where does the shared classifier live?**~~ *Resolved (2026-07-01 in [`./orchestration-design.md`](./orchestration-design.md), verified in spike 2): `.agents/specialists/*.tmpl` accepts `mode: SingleTurn` frontmatter; the prototype's specialists loader implements it and the triage classifier runs as a shared SingleTurn specialist.*

2. **How does the reference-graph library get discovered by operators?** Options: README lists them; each shape's directory has a self-describing `spec.yaml`; `mast workflows list` command. Bias: README + directory README-per-shape. A CLI command is optimizing for a use case that isn't visible yet.

3. **Testing conventions for reference graphs.** Each `examples/workflows/<shape>/` should be `go build`-clean and have a smoke test at minimum. Do we want graph-behavior tests (assert on emitted events, verify HITL response shapes)? Probably yes for the ones in `composition/`. `dev/ci/presubmits/` needs a workflow-examples-build stanza.

4. **Interop with core-agent under (E).** Core-agent stays on ADK v1; the reference graphs are v2-only. Do we cross-reference the shape *concepts* in core-agent's docs even though the code doesn't port? Bias: yes, brief pointers from core-agent's docs to mast's reference graphs, framed as "if you're building unattended workflows, this is what the sibling repo ships." No code port.

5. **Interaction with plan-first gate.** Plan-first gates the *root agent's* first turn on producing a plan. When the root agent's execution is a workflow graph, what does plan-first gate on — the whole graph's dispatch (planner node output)? Or does plan-first only apply when the root is a plain `LlmAgent`? Bias: plan-first is a root-agent primitive that doesn't need to reach into graphs; graph-shaped work is already structured. But this should be settled explicitly in the change-shape section of positioning.md.

## Out of scope

- **A "workflow builder" abstraction layer.** Explicit non-goal per the design principle above.
- **Visual workflow editing in `mast-web`.** Operators write `.go` files in their editor of choice. Maybe later; not v0.1.
- **Persisted workflow templates.** `.agents/workflows/*.yaml` as declarative graph definitions is tempting but adds a parser + validator surface for little gain over "write Go and copy from the reference." Revisit if operators consistently ask.
- **Cross-language workflow interop beyond HITL resume.** V2's shared interrupt format with Python ADK enables cross-runtime resume; a mast graph invoking a Python ADK sub-graph over IPC is not v0.1 scope.
- **Auto-migration from core-agent's `pkg/agent/autonomous.go` / `inbox.go` patterns.** Intentional per-workload port; see migration table.

## Related

- [`./positioning.md`](./positioning.md) — priority #4 that this doc realizes; workflow-scaffolding surface is now v2-native.
- [`./fork-design.md`](./fork-design.md) — resolves ADK v2 from day one; phase 1 squash absorbs v1→v2 migration.
- [`./specialists-design.md`](./specialists-design.md) — the specialists registry that populates agent nodes across shapes.
- [`./orchestration-design.md`](./orchestration-design.md) — workload bundles that select reference shapes for the planner's vocabulary; classifier-first dispatch as LLM-as-router applied at session-config level; bundle learning as an instance of map-reduce shape #6.
- [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) — how HITL prompts and emitted events surface in the operator UI.
- ADK v2 workflow package: `google.golang.org/adk/v2/workflow` (announced 2026, see [Google Developers Blog: Announcing ADK-Go 2.0](https://developers.googleblog.com/announcing-adk-go-20/)).
- [core-agent's context-management-design.md](https://github.com/go-steer/core-agent/blob/main/docs/context-management-design.md) — Mechanism B / digest pattern, which several shapes (map-reduce reducer, adversarial-verifier decision node) instantiate.
- [mastersingh24/gke-agent](https://github.com/mastersingh24/gke-agent) — proof-of-concept for the specialists-as-agent-nodes composition.
