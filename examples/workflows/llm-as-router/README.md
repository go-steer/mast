# LLM-as-router starter

Forkable starter for **shape #7 (LLM-as-router)** from
[`docs/workflow-scaffolding-design.md`](../../../docs/workflow-scaffolding-design.md):
a cheap SingleTurn classifier reads the input, a route node turns its answer
into a typed route, and the graph dispatches to the matching Task-mode
specialist — with a `Default` fallback for everything the classifier can't
place.

The domain here is generic on purpose (support-ticket routing). The
GKE-flavoured instance of the same shape is `pkg/graph` + the
`examples/workloads/gke-triage` workload; this starter does **not** import
`pkg/graph` — it copy-adapts the pattern, because starters are self-contained
by design (see "Fork it" below).

## The graph

```
START ──> classify ──────> route_by_category
          (SingleTurn       (EmittingFunctionNode,
           AgentNode)        sets Event.Routes)
                               │
             ┌─────────────────┼───────────────────┬──────────────────┐
     "billing"            "outage"           "account"        workflow.Default
             │                 │                   │                  │
             v                 v                   v                  v
      handle_billing     handle_outage      handle_account     handle_general
      (DynamicNode ──RunNode──> Task-mode specialist AgentNode, one each)
```

ADK v2 primitives used: `workflow.NewAgentNode` (classifier + specialists),
`workflow.NewEmittingFunctionNode` (route node emitting `Event.Routes`),
`workflow.StringRoute` / `workflow.Default` edges,
`workflow.NewDynamicNode` + `workflow.RunNode` (Task specialists cannot be
static graph nodes), and `workflowagent.New` wrapping the graph as the
runner's **root agent** — no coordinator above it (spike-2 root-agent rule).

## Run it offline

No credentials, no network — the classifier and specialists are
deterministic in-process fakes:

```sh
go run ./examples/workflows/llm-as-router
```

Or pipe your own tickets, one per line:

```sh
echo "I was double-charged on my last invoice" | go run ./examples/workflows/llm-as-router
```

Expected output (built-in samples — a route hit, a second route hit, and the
Default fallback):

```
== ticket-1: I was double-charged on my last invoice, please refund the duplicate.
   [TicketClassifier] billing
   [billing] function_call:finish_task
   [billing] function_response:finish_task
   [billing] output: map[result:[billing] duplicate charge confirmed; refund issued and invoice corrected]
   [support_router] output: map[result:[billing] duplicate charge confirmed; refund issued and invoice corrected]
== ticket-2: Your API has been returning 500s since 09:00 UTC and our dashboard is unreachable.
   [TicketClassifier] outage
   ...
== ticket-3: How do I export my project data to CSV?
   [TicketClassifier] unknown
   [general] output: map[result:[general] no specialist matched; ticket queued for a human agent with full context attached]
```

(Note the terminal output of a Task specialist is the whole `finish_task`
argument map — `map[result:...]` — not just the string.)

Smoke test: `go test ./examples/workflows/llm-as-router` runs all four routes
offline.

## Fork it

Per the fork-and-forget lifecycle
(workflow-scaffolding-design.md, "Shapes are forkable starters, not
demonstrations"): copy this directory out, and it's your code — no upgrade
path, no compatibility promise; mast's CI only keeps this in-repo original
building. The starter imports mast `pkg/agent` (mode constructors + the
fake-model idiom) and ADK — never another starter and never `pkg/graph`.

Customization points, in the order you'll hit them:

1. **Categories** — the `categories` table at the top is the whole routing
   domain: name, fake-classifier keywords, specialist instruction, canned
   resolution. Replace wholesale.
2. **Models** — `classifierModel` and `specialistModel` are the seams where
   real models go (e.g. `gemini.NewModel(ctx, "gemini-2.5-flash", ...)`);
   delete the keyword table and the canned resolutions, nothing else in the
   graph changes. Keep the classifier SingleTurn: one call, cheap tier.
3. **Tools** — give specialists real tools via
   `TaskAgentConfig.Tools/Toolsets` (per-specialist MCP allowlisting:
   `tool.FilterToolset`).
4. **HITL approval gate** — if a specialist's action needs operator approval,
   gate it inside the `handle_*` DynamicNode body, **ResumedInput-first**:
   check `ctx.ResumedInput(id)` before any `RunNode` call and stash the
   specialist result in session state before interrupting. Dynamic-node
   bodies re-execute on resume and `RunNode` does **not** cache child results
   across the pause turn (spike-2 finding). `pkg/graph` carries the full
   worked gate; also switch the runner to `session/database.NewSessionService`
   so the pause survives a restart.

## Governing design docs

- [`docs/workflow-scaffolding-design.md`](../../../docs/workflow-scaffolding-design.md) —
  shape #7, "Design principle: reference graphs, not helper packages",
  "Shapes are forkable starters, not demonstrations", and the
  "Spike-2 verification notes" (root-agent rules, ResumedInput-first).
- [`docs/spike-findings.md`](../../../docs/spike-findings.md) — Q1
  (graph-as-root, verified live) and Q2 (the resume contract).
- [`docs/specialists-design.md`](../../../docs/specialists-design.md) — how a
  specialists registry populates these agent nodes in the full runtime.
