# Fan-out-fan-in starter

Forkable starter for **shape #1 (fan-out-fan-in)** from
[`docs/workflow-scaffolding-design.md`](../../../docs/workflow-scaffolding-design.md):
a planner emits N independent work items, workers execute them in parallel
under a concurrency cap, a join barrier collects the results, and a
summarizer folds N results into one output.

The domain is a generic service-health sweep (the mast anchor instance is
`gke-parallel-triage`). Every node is a deterministic function node, so the
whole graph runs offline with no model at all — swap the worker for an
LLM-backed node when the per-item work needs one.

## The graph

```
START ──> plan ─────────> check_workers ─────────> collect_reports ──> summarize
          (FunctionNode,   (ParallelWorker over     (JoinNode,          (FunctionNode,
           emits []probe)   check_service,           fan-in barrier)     N reports → 1
                            maxConcurrency=3)                            fleet summary;
                                                                         escalation lives
                                                                         HERE, after the join)
```

ADK v2 primitives used: `workflow.NewFunctionNode` (planner, worker,
summarizer), `workflow.NewParallelWorker(name, wrapped, maxConcurrency, cfg)`
(one worker activation per `[]probe` element, outputs aggregated in input
order, per-item branch isolation by default), `workflow.NewJoinNode`, and
`workflowagent.New` wrapping the graph as the runner's root agent.

## Run it offline

```sh
go run ./examples/workflows/fan-out-fan-in
```

Or pipe a fleet (comma- or newline-separated service names):

```sh
echo "web, api, worker" | go run ./examples/workflows/fan-out-fan-in
```

Expected output (default fleet; statuses are a deterministic hash of the
service name, so this is stable run over run):

```
== sweep-1: services=[checkout payments search inventory auth]
   [fleet_health_sweep] output: [{checkout} {payments} {search} {inventory} {auth}]
   [fleet_health_sweep] output: [{checkout healthy 141} {payments degraded 158} {search healthy 33} {inventory unhealthy 199} {auth unhealthy 119}]
   [fleet_health_sweep] output: map[check_workers:[{checkout healthy 141} ...]]
   [fleet_health_sweep] output: fleet summary: 5 services checked
  - checkout   healthy   141ms
  - payments   degraded  158ms
  - search     healthy   33ms
  - inventory  unhealthy 199ms
  - auth       unhealthy 119ms
totals: 2 healthy / 1 degraded / 2 unhealthy
ESCALATE: needs operator attention: inventory, auth
```

Smoke test: `go test ./examples/workflows/fan-out-fan-in` (runs the sweep
twice and asserts determinism).

## The two constraints this shape lives under

**1. No HITL inside parallel branches (`ErrParallelHITLUnsupported`).**
Nothing executed by the ParallelWorker — the `check_service` worker here —
may raise a `RequestInputEvent`. Verified in spike 2; constrains shapes
#1/#5/#6 alike. Operator escalation belongs **after the join**: the
summarizer is the right seat (turn it into an `EmittingFunctionNode`,
ResumedInput-first, stash-in-state before interrupting), or route the
escalating item out of the parallel section first. This starter marks the
seat with the `ESCALATE:` line.

**2. Concurrency caps are three different knobs.**

- `NewParallelWorker(..., maxConcurrency, ...)` — caps in-flight workers
  *inside this node*. This is the lever that protects provider quotas when
  the worker calls an LLM/API, and the one this starter uses.
- `workflow.WithMaxConcurrency(n)` — graph-wide cap on *graph-scheduled
  nodes*. Two caveats from spike 2 / v2.1.0 source: it does **not** govern
  dynamic `RunNode` children, and `workflowagent.New` does not expose
  `workflow.Option`s at all, so a workflowagent-rooted graph can't set it —
  the per-ParallelWorker argument is the cap you actually control here.
- Dynamic `RunNode` fan-out (supervisor+workers, shape #3) is bounded only by
  the Go code that issues the `RunNode` calls.

## Fork it

Per the fork-and-forget lifecycle (workflow-scaffolding-design.md, "Shapes
are forkable starters, not demonstrations"): copy this directory out and it's
your code — no upgrade path, no compatibility promise; mast's CI only keeps
the in-repo original building. It imports only ADK (a starter never imports
another starter, and duplication between starters is accepted on purpose).

Customization points:

1. **`probe` / planner** — carry whatever a worker needs to act
   independently (IDs, URLs, log paths); parse your real trigger instead of
   the comma-separated list.
2. **Worker** — replace `syntheticHealth` with the real check. For LLM-backed
   per-item work, wrap a Task-mode agent in `workflow.NewAgentNode` and make
   the wrapped node invoke it (see `pkg/graph` for the RunNode idiom); keep
   `maxConcurrency` small.
3. **Summarizer** — this is *summarize* (N → structured report); if your
   reduce step is a true N→1 fold, you're in shape #6 (map-reduce), same
   primitives.
4. **Escalation** — add the post-join HITL gate described above, and switch
   the runner to `session/database.NewSessionService` so the pause survives
   restarts.

## Governing design docs

- [`docs/workflow-scaffolding-design.md`](../../../docs/workflow-scaffolding-design.md) —
  shape #1 (and #6 for the reduce variant), the `NewParallelWorker` API
  correction, the `ErrParallelHITLUnsupported` note, and "Spike-2
  verification notes".
- [`docs/spike-findings.md`](../../../docs/spike-findings.md) — Q1 API
  corrections (`NewParallelWorkerNode`/`AddFanOutDynamic` don't exist;
  `WithMaxConcurrency` doesn't govern RunNode children) and the Q2 resume
  contract for the post-join gate.
- [`docs/durable-execution-design.md`](../../../docs/durable-execution-design.md) —
  side-effect semantics if you add HITL (node bodies re-execute on resume).
