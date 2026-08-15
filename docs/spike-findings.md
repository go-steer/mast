# Spike 2 findings — ADK v2.1.0 against the mast design corpus

**Status:** complete, 2026-07-25. Branch `spike2` (spike 1 baseline tagged `spike1`).
Every finding below was verified by running code in this repo against
`google.golang.org/adk/v2 v2.1.0`; nothing is inferred from docs alone.
Structured to feed the post-spike docs PR in `go-steer/mast` (updates to
`adk-v2-usage.md`, `workflow-scaffolding-design.md`, `durable-execution-design.md`,
`specialists-design.md`, `orchestration-design.md`, `triage-demo-plan.md`).

## Q1 — can a workflow graph be the execution shape? YES (spike-1 conclusion reversed)

- **`workflowagent.New(Config{Edges, SubAgents})` produces a root agent the
  runner drives directly.** The runner's Chat-mode restriction applies only
  when the root IS an `LlmAgent` (`runner.go` `isLlmAgent` branch); any other
  agent — including a wrapped graph — takes the generic root path. Spike 1's
  `pkg/router` comment ("a bare Workflow cannot be a root agent") was wrong;
  the SubAgents-coordinator detour was unnecessary. Both shapes now coexist
  behind `--dispatch=coordinator|graph`.
- The full LLM-as-router shape from workflow-scaffolding-design (#7) runs
  end-to-end: SingleTurn classifier `AgentNode` → `EmittingFunctionNode`
  route node setting `Event.Routes` → `StringRoute` per reason +
  `workflow.Default` → per-reason `DynamicNode` invoking the Task specialist
  via `RunNode`. Verified live for both a route hit and the Default fallback.
- **Task-mode agents in graphs:** wrap in `AgentNode`, invoke via
  `RunNode` from a `DynamicNode` body (upstream `examples/workflow/dynamic/llm`
  idiom). `finish_task` is auto-installed and visible in
  `LLMRequest.Tools` — a fake model can (and ours does) key on it.
- **API corrections for `adk-v2-usage.md`:** `NewParallelWorkerNode` /
  `NewParallelWorkers` / `AddFanOutDynamic` do not exist. Actual:
  `NewParallelWorker(name, wrapped, maxConcurrency, cfg)` or
  `NodeConfig{ParallelWorker: true}`. Also relevant sentinels:
  `ErrParallelHITLUnsupported` (HITL inside parallel branches is
  unsupported), and `WithMaxConcurrency` does not govern dynamic
  `RunNode` children.

## Q2 — durable HITL across process death? YES, with a sharp resume contract

Verified end-to-end: inject → classifier → specialist → HITL pause persisted
to SQLite → `kill -9` → fresh process, same DB → `POST /resume` → terminal
output `{triage: <stashed result>, approval: <schema-validated verdict>}`.
The specialist was not re-invoked on resume.

- **ADK Go v2.1.0 ships persistent sessions**:
  `session/database.NewSessionService(gorm.Dialector)` + `AutoMigrate`
  (call it — the service does not self-migrate). Works with
  `glebarez/sqlite` (pure Go, no cgo); Postgres is the same GORM surface.
  *Implication for deployment-design: the SQLite/Postgres session-store
  story largely rides on ADK, and the Cloud-Run-needs-Postgres-in-v0.1
  contradiction is cheaper to fix than the docs assume. Implication for
  fork-design P1.3: the `pkg/eventlog` port needs re-scoping against
  what session/database already covers (audit query surface vs. store).*
- **Resume model (the durable-execution-design open question, now
  concrete):** it is *reconstruct-and-re-execute*, not deterministic
  replay. `ReconstructRunState` rebuilds paused state from session
  history every turn — no in-memory state, so restart-survival is free
  once the store is durable. Dynamic node bodies RE-RUN on resume
  (`RerunOnResume`); the resume payload arrives via
  `ctx.ResumedInput(interruptID)`. The runner reuses the paused run's
  invocation ID for the resume turn.
- **Footgun (cost us a debugging cycle, belongs in the docs):**
  `RunNode` does NOT return cached child results across the pause
  turn — dynamic children are not in the static graph, so
  `ReconstructRunState` cannot rehydrate their outputs. An unguarded
  `RunNode` before the `ResumedInput` check re-invokes the child LLM on
  resume, which then fails on the resume turn's orphan
  `FunctionResponse` ("no function call event found..."). The sanctioned
  shape is **ResumedInput-first, stash-in-state**: check
  `ctx.ResumedInput` at the top of the body; persist anything the
  resume pass needs via a `StateDelta` event before interrupting.
- **Side-effect implication for durable-execution-design:** because node
  bodies re-execute, any side effect before the interrupt re-runs unless
  guarded. Tool idempotency / recorded-effect semantics must be a design
  decision, not an accident. (Confirms review finding; now with a
  concrete mechanism.)
- Resume wire shape: a user turn whose `FunctionResponse.ID` equals the
  pending `InterruptID`; payload under `Response["response"]`
  (`decodeResumeResponse`). `RequestInput.ResponseSchema` is
  `*jsonschema.Schema` (github.com/google/jsonschema-go) — this is the
  answer to adk-v2-usage open question #5.
- Deterministic InterruptIDs (`approve-<specialist>`) are safe with
  per-incident sessions and make operator resumes scriptable; ADK
  recommends UUID-fresh IDs when a session may raise the same logical
  interrupt more than once.

**Later additions (verified against v2.2.0 while building W7.0, 2026-08-15).**
Same standard as the rest of this file: each was found by a failing run,
not by reading.

- **A user-authored event emitted mid-turn disables ADK's confirmation
  resume.** `llminternal.RequestConfirmationRequestProcessor` walks the
  session backwards to the most recent event authored `user` and gives
  up entirely if that event carries no `FunctionResponse` — so anything
  the graph emits with `Content.Role == user` after the operator's
  confirmation shadows it, and the parked call is never re-dispatched.
  The failure is silent and reads exactly like success: the run finishes
  `idle`, no error is logged, and nothing was applied. ADK authors an
  event `user` purely from its content role (`agent.getAuthorForEvent`),
  so the fix is to emit graph-authored narration as `RoleModel`; the
  receiving specialist still sees the text, as
  `"[<agent>] said: …"` (`ConvertForeignEvent`). This is what made the
  diagnoser→executor handoff's approved call vanish under graph
  dispatch (`pkg/graph/graph.go` `routeChange`,
  `scripts/uat-v0.4.sh` U-handoff/A).
- **A mid-turn `StateDelta` is not visible to a later node in the same
  turn.** State reads resolve against the turn's starting snapshot, so a
  node cannot hand a value to a downstream node by writing session
  state — the carrier has to be the node's output or a session event.
- **A Task-mode `LlmAgent` reached via `workflow.RunNode` ignores the
  node input** and assembles its prompt from session history
  (`llmagent.RunLLMAgentAsNode` seeds a wrapped session only for
  `ModeSingleTurn`). Anything a Task specialist must read has to be on
  the transcript before its node runs.
- **A confirmation resume re-enters the graph at START**, so routing
  predicates re-derive from a classifier reply that is now the
  operator's answer rather than an incident. The route has to be durable
  (`pkg/graph`'s `mast_route`) or the resume lands on a different
  specialist than the one that parked — which surfaces as
  `no function call event found for function responses ids`.

## Q3 — budget/cost substrate? Usage data YES, pricing+enforcement are mast's

- **`session.Event` embeds `model.LLMResponse`, so every model call's
  `UsageMetadata` (prompt/candidates/total tokens) streams past anything
  consuming runner events.** Token accounting requires no ADK changes.
  Events also carry `Branch` and `NodeInfo`, giving per-branch
  attribution hooks for the orchestration-design budget-composition
  story (`min(parent-remaining, specialist-cap)`).
- What ADK does not provide: pricing (port `pkg/pricing`) and
  enforcement. `pkg/budget` demonstrates the minimal mast-side shape —
  per-session meter over the event stream; crossing `max_cost_usd`
  cancels the run context mid-turn (verified: $0.01 cap trips on the
  third inject with "budget exceeded: $0.0136 > cap $0.0100").
  Event-stream metering is enforcement-after-the-call; pre-call gating
  wants a `model.LLM` wrapper (composes cleanly) and/or the v2.1.0
  TaskRunner seam for tool fan-out.
- `budget.max_wallclock_seconds` maps to a per-dispatch context
  deadline — trivial.
- **Per-tool MCP filtering is stock:** `tool.FilterToolset` +
  `tool.AllowedToolsPredicate` narrow a toolset per specialist.
  Resolves specialists-design open question #3 with zero mast
  machinery. Spike commits an interpretation of the (currently
  contradictory) allowlist algebra: absent/empty `tools.mcp` inherits
  all; non-empty is a whitelist; listed server with `tools[]` narrows;
  without `tools[]` passes whole. The docs pass must pin this (or a
  corrected version) in one normative table.

## Carry-over items for the docs PR (beyond the three questions)

1. `adk-v2-usage.md`: apply API corrections (Q1 bullet 4), add
   `session/database` + `AutoMigrate`, `ResumeOrRequestInput`,
   `ResumedInput`-first re-entry pattern, `ResponseSchema` =
   `*jsonschema.Schema`, resume wire shape, v2.1.0 additions (model
   registry, agent-registry package, auth/credential package,
   TaskRunner seam).
2. `workflow-scaffolding-design.md`: root-agent rules (workflowagent
   root fine; LlmAgent root must be Chat), dynamic-child caching
   limits, `ErrParallelHITLUnsupported` constraint on shapes #1/#5/#6,
   name the v0.1 shape subset.
3. `durable-execution-design.md`: resume model is
   reconstruct-and-re-execute; side-effect idempotency section;
   session/database as the store substrate (v0.1 SQLite AND Postgres
   both plausible).
4. `specialists-design.md` + `skills-design.md` + `config-layout`:
   normative allowlist table (spike interpretation as starting point);
   OQ #3 resolved.
5. `orchestration-design.md`: budget substrate = event-stream
   UsageMetadata + mast pricing/enforcement; per-branch attribution via
   Branch/NodeInfo; pre-call gating via model wrapper.
6. `triage-demo-plan.md`: graph shape confirmed end-to-end; per-incident
   sessions (not shared mode); change-safety-gate as in-graph
   ResumedInput-first gate; resolve its open question #2 (gate fires on
   a `needs_approval`-style decision in the dynamic node — spike gates
   unconditionally via `hitl.require_approval`).
7. `fork-design.md`: re-scope `pkg/eventlog` port vs. ADK
   session/database; P1.2's "root agent" assumptions.

## What spike 2 deliberately did not touch

Multi-instance coordination, isolation scopes / multi-tenancy, A2A/AG-UI,
observability export, real Gemini runs (echo model only — the fake now
synthesizes UsageMetadata and plays finish_task), MCP against a live GKE
endpoint, cyclic graphs / autonomous loop. None were on the three
questions; cyclic graphs are the nearest follow-up candidate.
