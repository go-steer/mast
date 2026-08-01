# mast durable execution: design

**Status:** draft, 2026-07-01 (updated 2026-07-25 — spike-2 verification: the resume model is pinned down (reconstruct-and-re-execute, verified across `kill -9`), ADK's `session/database` service reshapes the storage table, side-effect semantics under re-execution added as a mandatory design section, and the v0.1/v0.2 programmatic-pause phasing contradiction fixed. Updated 2026-07-26 — the v0.1 operator CLI shipped (`mast sessions list|show|resume|abort`; resume by InterruptID, abort as durable marker) and two ADK substrate constraints recorded under "Persistent session services". Updated 2026-07-30 — the "Shutdown contract" section added and shipped: bounded drain + pre-marked durable interruption markers + the `interrupted` transcript state, closing issues #38/#39; boot-time auto-resume and planned-stop classification recorded as deferred to v0.2 in issues #41/#42. Updated 2026-07-31 — adversarial review of the shipped contract found the markers violated a previously-unrecorded substrate constraint (session handle = write lease) and killed the turns they marked on database stores; markers moved to a companion ops row, substrate constraint (3) recorded, resume turns budget-wrapped, and new attach turns refused during the drain — issues #45–#48. Second 2026-07-31 pass, after adversarial re-review of v0.1.1: SQLite write hardening extended to the default session path — the storage requirement the list had been silently failing (#53) — regression tests de-neutralized (#54), mark/clear ordering serialized (#55), the reserved suffix enforced everywhere (#56), the attach interrupt-audit moved into the adapter's handle-free window (#57), and the drain's stop-accepting-work step made true (#58). Updated 2026-08-01 — open question #8 (recorded-effect outbox shape) resolved: the session log **is** the outbox (`FunctionCall` = intent, `FunctionResponse` = completion, retention inherited), checked from one process-wide runner-plugin tool-interception layer shared with the permission gate's runtime wiring, with a fail-closed ambiguous-effect mode scoped to the mutation predicate — see the new subsection under "Side-effect semantics"; this is the gate issue #41's boot-time auto-resume was waiting on.) Companion to [`./positioning.md`](./positioning.md) (in which "durable" is proposed as a fourth pillar alongside unattended / library / multi-provider), [`./fork-design.md`](./fork-design.md) (ADK v2 provides the substrate; bucket 1's lean core exposes the semantics), [`./orchestration-design.md`](./orchestration-design.md) (planner pause/resume + workload budget composition), [`./specialists-design.md`](./specialists-design.md) (specialist HITL pause), and [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) (cyclic graphs / autonomous loops that survive restarts). This doc treats **durable execution as its own subsystem** rather than as an implementation detail scattered across other designs.

## Why this is a fourth pillar

Positioning names three pillars: unattended, library-embedded, multi-provider. Practical experience with unattended production agents surfaces a fourth that has been implicit but not named: **durable**.

An unattended agent that loses state on crash, on redeploy, on autoscale-in, or on a paused-HITL-timing-out is not actually unattended — it's *unwatched but fragile*. The moment infrastructure churns, work restarts from scratch. For operator workloads (incident triage, config drift response, cost investigations) that routinely span minutes-to-hours-to-days, restart-from-scratch is a category error. The workflow world resolved this a decade ago (Temporal, Cadence, Restate, Airflow); the LLM-agent world is only starting to (Trigger.dev, Inngest, LangGraph's checkpoint-based execution). Mast can adopt this as a design pillar cheaply because ADK v2 provides the substrate — session-durable pause/resume, reconstruction from event history, shared interrupt format with Python ADK — without asking us to build a distributed-scheduler layer ourselves.

The competitive framing is worth being explicit about: **"durable execution for agents"** is the shape people compare to Temporal/Cadence for the workflow world and to Trigger.dev/Inngest for the LLM world. Mast's unattended positioning is more defensible when we say "durable by default" than when we say "we survive restarts if you're lucky."

### The four pillars, revised

1. **Unattended.** Runs without a human watching.
2. **Library-embedded.** Composes as a Go library inside larger services, not just as a standalone binary.
3. **Multi-provider.** Same config switches between Gemini and Claude without code changes.
4. **Durable.** Sessions survive process restarts, pod restarts, cluster migrations, paused HITL waits, budget exhaustion pauses, and scheduled maintenance windows. Work resumes where it stopped.

## Substrate: what ADK v2 gives us

Durable execution in mast is not something we *implement*; it is something we *expose and constrain*. V2's execution model provides:

- **Session-durable state.** Every node execution's state is written to the session. Custom `InvocationContext` implementations must supply `IsolationScope()` and `ResumedInput(id string)` — the primitives that make replay work.
- **Reconstructable pause.** ADK "can even reconstruct a paused workflow by scanning session history — so a workflow can resume after a process restart." The session event stream *is* the state.

  *Verified 2026-07-25 (spike 2), and the mechanism matters:* **the resume model is reconstruct-and-re-execute, not deterministic replay.** `Workflow.ReconstructRunState` rebuilds paused state from session history on *every* turn (there is no in-memory workflow state between turns — restart-survival is free once the store is durable, proven across `kill -9`). On resume, interrupted node bodies **re-execute** (`RerunOnResume`), with the human reply available via `ctx.ResumedInput(interruptID)`; the runner reuses the paused run's invocation ID. Two hard consequences: (1) `RunNode` does **not** return cached dynamic-child results across the pause turn — bodies must be ResumedInput-first and stash pre-pause results into session state (`StateDelta`), else the child re-runs (and, worse, the re-run LlmAgent fails on the resume turn's orphan `FunctionResponse`); (2) anything a node body did before the interrupt happens again on re-entry unless guarded — see "Side-effect semantics under re-execution" below. Temporal-style replay-through-arbitrary-code is NOT what the substrate provides; designs assuming it (mid-node crash recovery, replay-with-alternate-config) must be re-derived from the reconstruct-and-re-execute model.
- **Persistent session services in-box (2026-07-25).** `session/database.NewSessionService(gorm.Dialector)` + `database.AutoMigrate` gives SQLite (pure-Go `glebarez/sqlite`) and Postgres (same call, different dialector) session stores without mast-side storage code. Verified: HITL pause persisted to SQLite survives `kill -9`; a fresh process resumes it. This re-scopes the `pkg/eventlog/` port ([`./fork-design.md`](./fork-design.md) P1.3) toward the audit/query/retention surface rather than the store itself.

  *Two substrate constraints found building the v0.1 sessions surface (2026-07-26, `pkg/session` — renamed `pkg/transcript` at v0.1.0): (1) ADK's `Service.List` returns sessions **without their events**, and pause state is an event-log property — so listing paused sessions is an N+1 (one `Get` per session to scan for pending interrupts). Fine at operator-CLI scale; the P1.3 eventlog/query surface should index pause state so `list --state=paused` doesn't degrade to a full scan at fleet scale. (2) `Service.AppendEvent` type-asserts the service's **own concrete session type** — a session loaded through one service implementation cannot be appended through another, so you can't mix services (e.g. two `NewSessionService` instances, or database + in-memory) over one logical store handle; every writer must go through the same service instance, which is also why the v0.1 abort path routes through the owning daemon. (3) **A session handle is a write lease** (found 2026-07-31, issues #45/#46): the database service enforces optimistic concurrency on `last_update_time`, so ANY out-of-band append to a session row invalidates every other holder's handle — the runner's next `AppendEvent` on a live turn fails with `stale session error`. core-agent hit the identical failure with subagent runners writing to the parent's row and resolved it with derived session IDs (their `docs/eventlog-decisions.md`); mast follows the same rule: operator/daemon markers (abort, shutdown interruption) write to a companion ops row (`<sid>:mast-ops`, a reserved suffix hidden from listings and refused on every session-ID surface, the inject payload's UID included — #61), never to the primary row. A second consequence (2026-07-31, #62): two concurrent runner turns on ONE session are equally unsupported — the second turn's first append dies on the same check — so the daemon serializes turns per session; a same-session inject/resume queues behind the in-flight turn, bounded by the wallclock budget. All ops-row writes additionally serialize through a store-level mutex (#64) so an operator abort cannot collide with the shutdown pre-mark pass on the ops row itself.*
- **Shared interrupt format with Python ADK.** Cross-runtime resume is possible in principle — a mast session's paused state can be resumed by a Python ADK runtime and vice versa, provided the session storage is compatible.
- **HITL as one flavor of pause.** `RequestInputEvent` is one interrupt shape; the underlying pause primitive is more general.
- **Idempotent resume + typed errors.** `ErrInvalidResumeResponse`, `ErrNothingToResume` — the resume path fails cleanly, not silently.

Mast's job:

- Make the pause/resume primitive available beyond HITL (programmatic pause, timed pause, external-signal pause).
- Ensure session storage supports the operational modes (multi-replica coordination, cross-cluster migration, snapshot export).
- Define the operator-facing surface (CLI, mast-web, library API) for inspecting and resuming paused sessions.
- Preserve the Python-ADK compat option without gating v0.1 on it.

## Pause types

Beyond HITL, mast exposes several pause modes. Each writes a durable pause record to the session; each has a corresponding resume trigger.

### Programmatic pause

An agent (or a specialist, or a planner) calls `agent.Pause(ctx, agent.PauseSpec{...})` mid-execution. The session is checkpointed; the caller returns immediately with a pause handle. Resume is by external signal.

Use cases:
- **Watchdog-triggered pause for review.** Watchdog detects anomaly; pauses the session; operator inspects and resumes or aborts.
- **Cost-cool-down pause.** Session detects it's about to exceed a budget cap; pauses; operator raises cap or approves proceeding at a higher spend.
- **Ambiguity pause without operator prompt.** Session encounters ambiguity that doesn't warrant a synchronous HITL prompt (no operator present) but should not proceed silently; pauses and enqueues to a review queue.

Schema:

```go
type PauseSpec struct {
    Reason      string            // structured; e.g. "budget_exhaustion", "watchdog_anomaly", "cost_cool_down"
    Message     string            // human-readable, surfaces in mast-web
    Metadata    map[string]any    // free-form; surfaces in mast-web + audit
    ResumeToken string            // opaque token the resume caller presents; auto-generated if empty
}
```

### Timed pause

Session pauses for a specified duration; resume triggers automatically at wall-clock time T. Composes with any of the pause types.

Use cases:
- **Maintenance-window pause.** "Deploy in 2 hours; pause until then."
- **Rate-limit backoff.** "Provider quota exhausted; pause 15m."
- **Debounce.** "Wait 30s to see if more events arrive before acting."

Implementation: pause record carries a `ResumeAt time.Time`; a lightweight scheduler (single per-mast-instance goroutine watching a min-heap of `ResumeAt` values) triggers the resume. In multi-replica deployments, the scheduler is coordinated via shared session storage (see [`./deployment-design.md`](./deployment-design.md)).

### External-signal pause

Session pauses waiting for an external signal (webhook, queue message, mast-web operator action). The `ResumeToken` is exposed via the pause event; whoever holds the token can resume.

Use cases:
- **Waiting on an out-of-band approval.** "Change requires SRE-on-call sign-off; pause until they respond."
- **Waiting on a slow external process.** "Terraform apply takes 20 minutes; pause until webhook."
- **Cross-session coordination.** Session A pauses waiting for session B to complete.

### HITL pause (existing)

Special case of external-signal pause: `RequestInputEvent` with `ResponseSchema` surfaces to attach mode / mast-web; operator responds; response validates against schema; session resumes with response as input. Covered in [`./orchestration-design.md`](./orchestration-design.md), [`./specialists-design.md`](./specialists-design.md), and [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md).

## Resume triggers

Every pause has a corresponding resume path:

| Pause type | Resume trigger | Idempotency |
|---|---|---|
| Programmatic | External call: `agent.Resume(ctx, resumeToken, input)` | Idempotent via `ResumeToken` — repeat calls with same token + same input are no-ops after the first |
| Timed | Scheduler fires at `ResumeAt`; internal call to resume | Wall-clock uniqueness of `ResumeAt` per session; scheduler idempotency handled internally |
| External-signal | External call: `agent.Resume(ctx, resumeToken, input)` | Same as programmatic |
| HITL | Operator response through attach / mast-web | ADK v2's `ErrInvalidResumeResponse` / `ErrNothingToResume` handle validation |

Resume tokens are opaque strings (default: base64-encoded random 128 bits). They are the only thing needed to resume — the resumer does not need mast instance state, does not need to know which pod handled the pause, does not need to authenticate beyond token possession (though token possession + permissions gate + audit trail together give a full authorization story).

## Session storage requirements

For durability to be real, session storage has hard requirements:

- **Portable across replicas.** Any mast instance in a deployment can pick up any session for resume. Rules out in-process-only stores for production; the v0.1 SQLite store per bucket 2 is fine for single-instance; multi-instance requires either shared storage (network filesystem, cloud-hosted SQLite equivalent) or a proper distributed store.
- **Event-log-primary.** Session state is derived from the event log, not stored separately. V2 already encodes this; mast's port of `pkg/eventlog/` preserves it.
- **Fsync-durable per event.** An event isn't considered persisted until fsync (or equivalent cloud durability guarantee). Losing the last N events on crash means losing them, not corrupting state; resumes from the last durable event. *(SQLite status, verified 2026-07-31: glebarez defaults to `synchronous=FULL`, so per-commit fsync holds. The requirement this list was actually failing was a different one — see the next bullet.)*
- **Write-safe under concurrent sessions.** SQLite is a single-writer store; lock-upgrade conflicts inside open transactions return `SQLITE_BUSY` immediately, bypassing `busy_timeout`. Every SQLite session-service construction therefore applies write serialization + WAL + busy_timeout — the attach path always did (pkg/eventlog); the plain `--session-db` path was missed and silently lost transcript events AND drain-time interruption markers under concurrent incidents (issue #53, found by adversarial re-review; both paths now share `eventlog.OpenSessionService`'s building blocks so they cannot drift). Postgres needs none of this (MVCC).
- **Content-addressable session IDs.** Session IDs must be unique across all mast instances in a deployment. UUID v7 (time-sortable) recommended.
- **Retention policy per session, not per store.** Some workloads want 7 days; audit-sensitive workloads want years. Session-level TTL, not global.

Storage backends (v0.1 → future):

| Backend | Suitable for | v0.x scope |
|---|---|---|
| SQLite (per-instance file) | Single-instance dev / laptop / library-embedded single-process | v0.1 default — via ADK `session/database` + `glebarez/sqlite` (pure Go), *not* a core-agent port (revised 2026-07-25; verified in spike 2) |
| SQLite (shared filesystem) | Small multi-instance where a network FS is acceptable | ~~v0.1 supported; documented caveat~~ *Dropped 2026-07-25: SQLite's own docs warn most network-FS lock implementations corrupt databases; a "documented caveat" is not acceptable cover for a durability-pillar product. Multi-instance goes straight to Postgres.* |
| Postgres | Standard multi-instance; Cloud Run (ephemeral filesystem) | ~~v0.2 (needs adapter in `pkg/eventlog/`)~~ *Revised 2026-07-25: same ADK `NewSessionService` call with a Postgres dialector — near-free, and pulled forward as the fix for the Cloud-Run-v0.1 durability contradiction in [`./deployment-design.md`](./deployment-design.md).* |
| Cloud Spanner / CockroachDB | Multi-region, high-throughput, tenant-isolated | v0.3+ (deployment-design.md dependency; GORM dialector permitting) |
| Cloud Firestore | GCP-native serverless | v0.3+ |
| Custom (via `SessionStore` interface) | Anything else | v0.1 (interface + docs; concrete adapters as needed) |

## Side-effect semantics under re-execution (added 2026-07-25 — mandatory design surface)

Because resume re-executes node bodies (see Substrate) and crash recovery resumes from the last fsynced event, **mutating tool calls are at-least-once**: a `rollout_undo` or `apply_manifest` whose completion event wasn't durably written before a crash *will run again* on recovery, and any side effect a node body performs before its interrupt point *will run again* on resume unless guarded. For an SRE-facing product this is the single most important durability semantic, and it is a contract we owe tool authors in writing:

1. **Declared default: at-least-once, with guards.** Mast does not build an exactly-once illusion. The runtime provides the guard primitives; tool and node authors use them.
2. **Node-body guard:** ResumedInput-first + session-state stash (verified pattern; the change-safety-gate in the triage anchor demo implements it).
3. **Tool-call guard (v0.1 guidance, v0.2 mechanism):** mutating tools should accept an idempotency key derived from `(session ID, invocation ID, function-call ID)` — all three already exist on events. A recorded-effect outbox (check the event log for a completed effect before re-executing) is the v0.2 candidate mechanism, sited in the tool-execution wrapper so individual tools don't reimplement it.
4. **The permission gate composes:** re-executed mutations pass through the gate again by construction — fail-closed under re-execution, not fail-open.

This section resolves the previously-implicit question; the residual open question (#8 below) is only the v0.2 outbox mechanism's shape.

The `SessionStore` interface is one of the extension points enumerated in [`./library-api-design.md`](./library-api-design.md). Third-party stores can be plugged in.

### Recorded-effect outbox (v0.2 design, added 2026-08-01 — resolves open question #8)

The v0.2 mechanism behind tool-call guard #3 above. Three decisions, each shaped by a v0.1.x lesson:

**1. Siting: one process-wide interception layer (ADK runner plugin), not per-toolset wrappers.** Every tool execution — MCP toolsets, builtin function tools, the planner vocabulary, `invoke_remote_agent` — converges on ADK's `Flow.callTool`, which brackets the call with the runner plugin's `BeforeToolCallback` / `AfterToolCallback` (`plugin.Config` wired through `runner.Config.PluginConfig`). A `BeforeToolCallback` that returns a non-nil result *skips the tool and substitutes the result* — exactly the short-circuit the outbox needs, at the only seam a new construction path cannot miss. That siting is the #53 lesson applied forward: guards must live where every path gets them, and mast constructs runners in exactly two places (the daemon and the library root), both of which take the same plugin. Per-agent tool callbacks and wrapping toolsets were considered and rejected — both multiply construction sites. The same interception layer is where the permission gate's runtime wiring (deferred from v0.1) lands; ordering within the chain is outbox-check first, gate second — a replayed recorded result performs no new effect and needs no fresh approval, and a refused-as-ambiguous call never reaches the gate. `agent.Context` exposes the full key at this point (`SessionID()`, `InvocationID()`, `FunctionCallID()`); the turn-level scan below sits naturally in the plugin's `BeforeRunCallback`.

**2. Record shape: the session event log IS the outbox — no new table.** A durable `FunctionCall` event is the *intent* record; its paired `FunctionResponse` event is the *completion* record; both already carry the `(session ID, invocation ID, function-call ID)` key. Consequences fall out by construction: retention is inherited from session retention (no second policy surface — the retention sub-question of open question #8 dissolves); Python-ADK deserialize-tolerance is free (no mast-specific event types); and the record cannot drift from the transcript because it *is* the transcript. The wrapper only ever **reads** history at check time — reads take no write lease (substrate constraint 3 governs appends), using the same event-scan pattern `pkg/transcript` already uses for pending-interrupt derivation. One fallback is recorded ahead of implementation: if verification shows `FunctionCall` events are not durably appended *before* the tool executes, the wrapper writes an explicit intent marker to the companion ops row (`<sid>:mast-ops`, the established write-lease-safe out-of-band channel) — the log-as-outbox contract is unchanged; only the intent carrier moves.

**3. Semantics: fail-closed per-turn ambiguity mode plus per-call replay, mutating tools only.** Scope is exactly [`./orchestration-design.md`](./orchestration-design.md)'s mutation predicate (built-in `Mutating` annotation; MCP `readOnlyHint` with default-deny-unknown; audited per-tool override) — read-only tools never pay the check.

- **Turn-level — the guard issue #41 actually needs.** At turn start on a session with prior history, if the log holds a **dangling mutating intent** (a durable `FunctionCall` from a prior attempt with no completion), the turn runs in *ambiguous-effect mode*: the wrapper refuses every mutating call with a structured `ambiguous_prior_effect` error — the model/planner sees the refusal and surfaces it; non-mutating work proceeds. The dangling call may or may not have executed; the window between an external effect committing and its completion event fsyncing cannot be closed, only detected. Clearing the mode is an operator action through the existing surfaces (resume with explicit acknowledgement, or abort) — mast does not guess.
- **Call-level.** A call whose exact key already has a durable completion returns the recorded `FunctionResponse` result instead of re-executing — idempotent replay. This branch is belt-and-suspenders: reconstruct-and-re-execute normally re-invokes the model over history rather than replaying recorded calls, so it exists to guard any ADK resume path that re-fires a recorded call verbatim.
- **Honest coverage limit (1): fresh function-call IDs.** A re-invoked model that emits a *fresh* function-call ID for a semantically identical mutation is a new call by construction — the exact-key check cannot and does not dedupe it. That class is covered by the turn-level mode (the dangling intent is precisely what makes such a turn dangerous), by node-body guard #2, and by the permission gate re-running under `hitl_policy.on_mutation`. Content-hash dedup (tool name + canonicalized arguments) was considered and **rejected**: legitimately repeated mutations inside one invocation are real (scale-up called twice is two scale-ups), and a silent false-positive dedup in an SRE-facing product is worse than the duplicate it prevents.
- **Honest coverage limit (2): parallel branches (verified 2026-08-01).** `workflow.NewParallelWorker` consumes its workers' event streams internally and forwards only each worker's final aggregated output — a sub-agent's `FunctionCall`/`FunctionResponse` events inside a parallel branch are **never appended to the session log at all** (`parallel_worker.go`: workers report through a result channel; only the merged `Output` event is yielded). The log-as-outbox therefore has no intent and no completion to key on there: parallel-branch mutations stay plain at-least-once, exactly the v0.1 contract. The interception layer itself still fires inside branches (the plugin manager rides the invocation context), so ambiguous-effect *refusal* works everywhere — only the *records* are missing. Authoring guidance, aligned with the existing `ErrParallelHITLUnsupported` constraint: mutating tools belong in sequential paths (the triage anchor already models this — read-only fan-out, mutations behind the sequential change-safety gate).

At-least-once remains the declared contract (guard #1 above). What the outbox changes: the duplicate window narrows to crash-during-execution, and the residual ambiguity becomes *visible and blocking* instead of silent — which is the property boot-time auto-resume was gated on. Issue #41's eligibility check becomes: interruption marker present AND no dangling mutating intent AND inside the freshness window AND the restart-loop breaker is quiet.

**Implementation gate — the three assumed substrate behaviors, verified 2026-08-01 (source-level, ADK v2.1.0; the implementation's regression tests still exercise them end-to-end):**

- **(a) `FunctionCall` appended-before-run: HOLDS on the standard path, by construction.** The flow yields the model-response event (carrying the `FunctionCall` parts) *before* `handleFunctionCalls` executes any tool (`base_flow.go` `runOneStep`), and the runner's consumption loop calls `sessionService.AppendEvent` for every non-partial event *inside the loop body, before the yield returns* (`runner.go` `Run`) — Go's range-over-func semantics block the producer until that body completes, so the intent record is committed (fsynced, under the hardened SQLite settings) before `tool.Run` can start. The event-buffering path that reorders tool events exists only in the Live bidirectional-streaming loop (transcription buffering), which mast does not use. The exception is coverage limit (2) above: inside `NewParallelWorker` branches nothing is appended at all. The ops-row fallback intent carrier is therefore **not needed**; it stays recorded here only in case a future ADK version moves the append.
- **(b) Dangling unpaired `FunctionCall` in history: ADK does not repair it.** The contents processor rearranges and merges function events and hard-errors on the *opposite* orphan (a `FunctionResponse` with no matching call — `"no function call event found"`); a call with no response passes through to the provider verbatim. Behavior is then provider-dependent (Anthropic rejects an assistant `tool_use` with no `tool_result`; Gemini has historically been lenient) — and notably, **no ADK path re-fires a recorded `FunctionCall` through `callTool`**: re-execution risk comes exclusively from fresh model emissions, which confirms the turn-level mode as the primary guard and the call-level replay as belt-and-suspenders. Consequence for issue #41: auto-resume should *repair before resuming* — append a synthetic `FunctionResponse` (`{"error": "interrupted before completion"}`-shaped, model-visible and honest) for each dangling call so the continuation turn is provider-valid; safe at boot because no turn is in flight, so the primary-row append takes no one's write lease.
- **(c) Plugin coverage of MCP tools: HOLDS.** `Flow.callTool`'s plugin bracket (`RunBeforeToolCallback` / `RunAfterToolCallback` / `RunOnToolErrorCallback`) is tool-type-agnostic — it wraps every `FunctionTool`, and the MCP toolset's tools implement that interface; the plugin manager travels on the invocation context, so the bracket fires inside parallel workers too.

Metric families (effects replayed / ambiguous refusals — and the #50 marker-failure counter) are named in the v0.2 fixed-registry pass ([`./observability-design.md`](./observability-design.md)), not here.

*(Shipped 2026-08-01, `pkg/effects`, hardened through a pre-merge adversarial gate that refuted the first implementation twice — the refinements below are load-bearing, not cosmetic. (1) **A third class, `Spawning`, joined the predicate:** `invoke_specialist` and the `run_shape_*` vocabulary start sub-runs whose inner tool calls this process cannot individually guard from the spawn site (the planner dispatch runner is a separate in-memory-session runner — the known sub-runner bypass debt, TODO'd at the call site), so ambiguous-effect mode refuses them when they arrive through the tool seam; containment nuance recorded honestly: ADK's coordinator RE-dispatch of an already-recorded task delegation bypasses that seam, and there containment holds at the inner-call level instead (the sub-run inherits the invocation ID, so its own mutating calls are refused individually). `invoke_remote_agent` classifies plain `Mutating` — remote effects are invisible, period. (2) **All history reads happen once per turn, at turn start:** the per-call tool context structurally has no session access (ADK v2.1.0's tool-context `Session()` is unconditionally nil), so the first implementation's per-call replay lookup was dead code — the gate caught it; the shipped shape snapshots dangling intents AND recorded completions in `BeforeRun` off the invocation context, and the per-call checks consult the snapshot. A recorded completion with a nil payload replays an explicit marker result (returning nil would mean "execute"). (3) **Task delegations are excluded from the dangling scan:** the coordinator emits delegations as FunctionCalls named after the sub-agent and deliberately leaves them unresolved across user turns (a specialist asking a clarifying question) — without the exclusion (`effects.SubAgentNames(root)`, wired at all three construction sites) the guard wedged mast's default composition on its happy path. Engine control calls (`adk_request_input`/`credential`/`confirmation`, `finish_task`, `transfer_to_agent`, `task_completed`, `exit_loop`), long-running calls, and empty-ID calls are likewise excluded. (4) **The predicate's MCP clause degrades to default-deny in practice:** ADK v2.1.0's `mcptoolset` drops tool annotations entirely at conversion, so `readOnlyHint` never reaches mast and every MCP tool classifies mutating until the workload's `tool_catalog.tools` override un-gates it (`mutating: false`, audit-logged at startup). Upstream affordance — surfacing annotations through the toolset — is a candidate ask alongside adk-go#1229. (5) **The ack surface:** `mast sessions ack-effects <id>` (daemon `/ack-effects` by default — serialized against in-flight turns via the session turn lock, so a watermark can never cover intents still being persisted; `--session-db` direct mode for DBs no daemon serves, e.g. one-shot task sessions), plus `resume --ack-effects` for the paused-session case (watermark written under the same turn lock, before the resume turn's scan) and `mast.AckEffects` (library twin). The watermark rides the companion ops row (`mast_effects_ack_time/reason`), covers only intents persisted at or before it — post-ack intents still refuse, tested — and is deliberately not a transcript state. If an ack lands and the resume turn then fails, the watermark stays: it acknowledges the prior intents, not the new turn. Verification (a) is pinned end-to-end in the suite (a tool asserts its own FunctionCall is readable through a fresh store handle before it runs), as are end-to-end replay, nil-payload replay, the delegation wire shape, and the paused-HITL exclusion. The outbox plugin is attached at the three runner construction sites mast owns — serve, one-shot, and the library root; the planner dispatch sub-runner remains the recorded gap.)*

## Shutdown contract (added 2026-07-30, shipped with issues #38/#39/#40)

What SIGTERM/SIGINT means for a serving daemon. Durability makes shutdown *survivable* by construction — every event is persisted as it streams, so nothing already written is ever at stake — but survivable is not the same as *accounted for*: the pre-contract daemon dropped in-flight turns invisibly. The contract:

1. **Drain, bounded by the turn's own ceiling.** On signal the daemon stops accepting new work: the inject listener closes immediately (Shutdown launches before the pre-mark pass), requests already past accept are refused by a drain gate on the inject and resume handlers, and new attach-injected turns are refused with a clear error while the attach surface itself stays connected (revised 2026-07-31 twice: issues #48, then #58 for the listener ordering and handler gates the first revision claimed but didn't ship). The daemon then waits for in-flight turns, inject-handler and attach-driven alike. When the workload sets `budget.max_wallclock_seconds`, every turn (inject, attach, and resume — the last budget-wrapped since issue #47) is bounded by it and the drain bound IS that ceiling: a finishing turn is never cut shorter than its own budget allows. Without a budget, turns are unbounded and the drain is a hard 30s cut. Consequence for deploys: the worst-case drain is known statically; size `terminationGracePeriodSeconds` / `TimeoutStopSec` above it (the K8s base and the systemd unit in the tree do). After the window elapses, survivors' contexts are cancelled and given a short bounded grace to unwind before teardown.
2. **Pre-mark, then wait, then clear.** *Before* the drain waits, every session with a turn in flight gets a durable interruption marker appended — so a SIGKILL landing mid-drain finds the markers already on disk. Markers (interruption AND abort) write to the session's **companion ops row** (`<sid>:mast-ops`), never to the primary row: ADK session handles are write leases, and an out-of-band append to the primary would kill the very turn being marked with a `stale session error` (substrate constraint 3 above; issues #45/#46 — caught by adversarial review after the first implementation wrote to the primary row). A turn that completes inside the window clears its marker; the log then honestly records a completed turn. Turns that outrun the window keep the marker. Like abort, the marker is state, not preemption: the engine ignores it, and a later turn on the session proceeds normally under reconstruct-and-re-execute. A side effect of the ops row: marker writes work even before the runner's auto-create has committed the primary session, and the abort event is no longer part of the model-visible transcript (previously incidental — the daemon refuses resumes on aborted sessions).
3. **The `interrupted` transcript state.** `pkg/transcript` derives it from the marker with precedence `aborted > paused > interrupted > idle` — a session that reached a HITL pause is resumable and reports `paused` regardless of any marker. This stays inside the package's honesty rule (states only from what the log proves): the process that *was* running the turn recorded the interruption durably before stopping. `mast sessions list --state=interrupted` filters on it.
4. **The attach surface stays up through the drain** so operators live-tailing a finishing turn see its final events; it closes after the drain resolves.
5. **Exit code is 0 for any signal-initiated stop.** K8s ignores exit codes at termination; the standalone unit compensates with `Restart=always`. A planned-stop vs unexpected-stop classification is deliberately deferred (below).

**Deliberately deferred, and why (the sequencing is the contract):**

- **Boot-time auto-resume of interrupted sessions (v0.2, issue #41).** The markers make interrupted sessions *findable*; re-running them re-executes node bodies, and mutating tools are at-least-once until the recorded-effect outbox (open question #8) lands. Auto-resume before the outbox means a rolling upgrade can silently re-fire a mutating tool call. Dropping a turn visibly beats re-executing a mutation; outbox first, then auto-resume (with a freshness window and a restart-loop breaker as its safety rails).
- **Planned-stop classification (v0.2, issue #42).** An operator-initiated stop is closer to "pause the daemon's sessions" than to an exit-code convention; it belongs to the v0.2 programmatic pause/abort surface.

## Operator-facing surface

Durability without inspection is opaque. Operators need to see paused sessions, their reasons, their resume tokens, and to trigger resumes.

### CLI

```
mast sessions list                                  # all sessions (running, paused, completed)
mast sessions list --state=paused                   # only paused
mast sessions show <session-id>                     # detailed view including pause metadata
mast sessions resume <session-id> --token=<token>   # trigger resume
mast sessions resume <session-id> --token=<token> --input='{"approved":true}'   # with input
mast sessions abort <session-id> --reason="operator cancelled"
mast sessions snapshot <session-id> > snapshot.tar  # export for debugging
mast sessions replay snapshot.tar --config=alt.yaml # replay with alternate config
```

*(Shipped 2026-07-26, `cmd/mast/sessions.go` — the v0.1 surface is `mast sessions list|show|resume|abort`, with two deliberate deviations from the sketch above. (1) **Resume is keyed by interrupt, not token:** `resume <session-id> --interrupt=<interrupt-id> --response='{"approved":true}'`. Resume tokens are the v0.2 programmatic-pause surface; the v0.1 pause is a HITL `RequestInput`, whose verified resume contract is keyed by `InterruptID` — `show` prints the pending interrupt IDs, response schemas, and a ready-to-paste resume command. (2) `snapshot`/`replay` are not in the v0.1 CLI, per the phasing table. Mechanically: `list`/`show` read the SQLite session DB directly (with or without a running daemon), deriving state from the event log — paused = a `RequestedInput` with no later matching `FunctionResponse`, and honestly no "running"/"completed" labels, since in-flight-turn state is runner state, not event-log state; `resume`/`abort` POST to a running daemon, because resume must execute in the runner that owns the workflow and routing abort through the daemon keeps a single SQLite writer.)*

*(Abort semantics, shipped 2026-07-26 — a contract worth stating: **abort is a durable marker plus daemon-side resume refusal, not engine preemption.** `abort` appends an event whose `StateDelta` records the abort reason/time in session state; it does not cancel an in-flight turn, and ADK's workflow reconstruction does not read the marker — as far as the engine is concerned a pending `RequestedInput` is still resumable. It is mast's surface that treats the marker as terminal: `list`/`show` report `aborted` with pending interrupts cleared, and the daemon's `/resume` refuses marked sessions. A second abort returns already-aborted rather than stacking markers. An engine-level terminal state belongs to the v0.2 programmatic-pause/abort work.)*

### mast-web

Sessions view in mast-web (per [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md)):

- List view: running / paused / completed / errored, filterable by workload / tenant / time range.
- Detail view: per-session event stream, pause reason + metadata + resume token, resume-with-input form (schema-driven for HITL pauses).
- Bulk actions: resume all paused with reason=`cost_cool_down` after raising cap, abort all older than N days, etc.

### Library API

Programmatic access to the same surface for library-embedded consumers:

```go
sessions, err := mast.ListSessions(ctx, mast.SessionFilter{State: mast.StatePaused})
err = mast.Resume(ctx, sessionID, resumeToken, input)
```

Covered in [`./library-api-design.md`](./library-api-design.md).

## Composition with other subsystems

| Subsystem | How durability composes |
|---|---|
| **HITL (specialists + planner)** | HITL pause is a special case of external-signal pause. All HITL surface (attach, mast-web, response schemas) applies uniformly. |
| **Planner** ([`./orchestration-design.md`](./orchestration-design.md)) | Planner turn loop is inherently checkpointed — each turn writes to session; resume picks up mid-loop. `plan_review_required: true` pauses after turn 1; budget-exhaustion pauses on `hitl_policy.on_budget_exhaustion: escalate`. |
| **Autonomous loops** ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)) | Cyclic-graph loops (autonomous + inbox) survive restart natively via session-durable state. Iteration count + accumulated state are session values. |
| **Workload budgets** ([`./orchestration-design.md`](./orchestration-design.md)) | Budget exhaustion is a pause trigger governed by `hitl_policy.on_budget_exhaustion`. Operator raises cap and calls `mast.Resume(...)`. |
| **Watchdog** | Watchdog anomalies (per core-agent issue #159, delivered via emitting-function-node pattern) can trigger programmatic pause via `agent.Pause` in the emitting node. |
| **Multi-tenant** ([`./deployment-design.md`](./deployment-design.md)) | `WithIsolationScope(tenantID)` composes with session storage — sessions are stored under tenant scope; resume operations authorize against the same tenant. |
| **A2A** ([`./a2a-design.md`](./a2a-design.md)) | Long-running A2A tasks hosted by mast survive process restart natively — A2A task ID + mast session ID are the same identifier for consistency. Outbound A2A calls to long-running remote skills trigger programmatic pause with `Reason: a2a_task_pending`; resume on remote-task terminal state via subscription or push-notification callback. HITL from a remote A2A task propagates as an `input-required` sub-state through mast's own attach layer. |
| **Federation** ([`./federation-design.md`](./federation-design.md)) | Cross-instance pause/resume works across mast-native federation — parent's `invoke_remote_agent` call pauses; child mast's session runs to completion or its own pause; child result triggers parent resume. If either instance crashes, both replay from last durable events and re-establish. Snapshot/replay works across the federation boundary via the `parent_session_id` link in event metadata. Cross-instance HITL surfaces on the parent's attach connection (unified operator experience). |
| **Audit-derived memory** ([`./memory-design.md`](./memory-design.md)) | Pause/resume events are part of the event stream; audit-derived memory learns operator patterns (which pauses get resumed vs. aborted; how long budget-cool-down pauses typically wait). |
| **Observability** ([`./observability-design.md`](./observability-design.md)) | Pause counts by reason, pause durations, resume latencies, snapshot sizes — all first-class metrics. |
| **Reference graphs** ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)) | Every shape gets durability for free; no per-shape design work. Adversarial-verifier can pause on ambiguous decisions; map-reduce can pause per-partition if inputs arrive slowly. |

## Snapshot + replay

Durable execution enables a debugging capability worth calling out separately: **snapshot-and-replay**.

- `mast sessions snapshot <id>` exports the full session event stream + metadata + pause state to a portable archive.
- `mast sessions replay <snapshot>` restarts the session in a target environment (different provider config, different specialist versions, different bundle config, dry-run mode).
- Replay does not mutate real state — MCP calls are optionally recorded during the original run and replayed from cache during replay; tool calls that would mutate are skipped or intercepted.

Use cases:
- **Debug a production regression** locally: pull the snapshot, replay against a local mast build, step through the events in a debugger.
- **Compare specialist versions**: replay the same session against specialist A's `.tmpl` vs. specialist B's; diff the resulting event streams.
- **Test a bundle refinement** proposed by the learning pipeline: replay historical sessions against the proposed bundle vs. the declared bundle; verify no regressions.
- **Training / documentation**: use anonymized snapshots as reference material for new operators.

Snapshot format is a portable archive (probably JSONL for events + a manifest); versioned; forward-compatible resume from an old snapshot is a v0.2 concern.

## Cross-runtime resume (Python ADK compat)

ADK v2's shared interrupt format with Python ADK opens a polyglot capability we do not build for v0.1 but must not break.

**Design constraints (must-preserve for future work):**
- Session event schema stays compatible with Python ADK's — no mast-specific event types that Python ADK cannot deserialize.
- Interrupt payload (`RequestInputEvent`, `ResponseSchema`) uses the shared v2 shape verbatim.
- Session ID + resume token format stays interoperable — Python ADK can present the same `sessionID + resumeToken` and be accepted.
- Session storage backends (Postgres, Spanner, etc.) that mast uses in production should ideally be readable by Python ADK — avoid mast-specific column encodings.

**What we get if we preserve compat:**
- A mast agent can pause; a Python ADK operator tool can inspect and resume; work continues without either runtime needing to translate.
- A Python ADK sub-agent invoked from mast (via HTTP or shared session storage) can hand off durably.
- Migration between runtimes for a workload (start on mast, migrate to Python ADK for a research prototype, migrate back) is possible without state loss.

**What we defer:**
- Actually building a cross-runtime dispatch mechanism — v0.4+ if operators ask.
- Testing infrastructure for cross-runtime resume — need real Python ADK integration to validate.

**What we do now:**
- Any custom event field mast adds gets tested for Python ADK deserialize-tolerance (they should be able to see the event, even if they ignore mast-specific fields).
- The `SessionStore` interface docs (see [`./library-api-design.md`](./library-api-design.md)) call out the Python-ADK-readable schema as a strong preference.

## Multi-instance coordination

Multi-replica deployments (positioning priority #5, covered in detail in [`./deployment-design.md`](./deployment-design.md)) need coordination beyond shared session storage. Concrete concerns:

- **Session ownership handoff.** When a mast instance receives a resume request for a session it wasn't running, it must claim the session (advisory lock in the session store), replay from the last event, and continue.
- **Timed-pause scheduler.** With N replicas, only one should fire a given timed resume. Options: (a) leader-election for the scheduler; (b) claim-based (each replica polls the pause table and claims eligible pauses); (c) external scheduler (Cloud Scheduler, K8s CronJob) that fires resume RPCs into the fleet.
- **Autonomous-loop assignment.** A cyclic-graph autonomous loop needs to be running on exactly one replica at a time; failover happens on that replica's death.
- **Attach-mode session affinity.** An operator connected via attach to session X should reach the instance running X (or be transparently routed).

These are deployment-design concerns; the pause/resume primitive itself is topology-agnostic.

## Phasing

| Version | Scope |
|---|---|
| **v0.1** | Resume primitive (`agent.Resume`) + HITL pause + external-signal pause (both resumed via `agent.Resume`; verified durable across restart in spike 2). SQLite session store via ADK `session/database` (single-instance); Postgres via the same service where the topology demands it (Cloud Run). CLI: `mast sessions list/show/resume/abort`. Snapshot/replay: export format only, replay in v0.2. *(Revised 2026-07-25: this row previously also claimed `agent.Pause` while v0.2 claimed "programmatic pause" — the same feature on both sides of the boundary. Resolution: programmatic **self**-pause (`agent.Pause` from inside an agent) is v0.2; v0.1 pauses originate from HITL interrupts and external-signal waits only.)* |
| **v0.2** | Programmatic self-pause (`agent.Pause` + `PauseSpec`). Timed pause + single-instance scheduler. Snapshot replay. Recorded-effect outbox for mutating-tool idempotency (see "Side-effect semantics"). *(Note: [`./a2a-design.md`](./a2a-design.md)'s outbound-call composition depends on programmatic pause — outbound A2A in v0.1 would have to block synchronously; another reason the A2A server/client slice belongs in v0.2, per the pending scope re-cut.)* |
| **v0.3** | Multi-instance coordination (session ownership handoff; distributed timed-pause scheduler). Bundle-learning-derived pause-pattern analytics. |
| **v0.4+** | Cross-runtime resume validation (with Python ADK integration tests). Cloud-native session stores (Spanner, Firestore) via community-contributed adapters. |

## Open questions

1. **Pause reason taxonomy.** Should `PauseSpec.Reason` be a free-form string or an enum? Bias: enum with an `other` escape hatch — enables per-reason analytics without an operator-authored taxonomy proliferating.
2. **Resume-token lifetime.** Should resume tokens expire? An external-signal pause waiting on a Terraform apply that takes 45m is fine; a pause waiting for an operator who quit the company 6 months ago is not. Bias: default TTL 30 days per token; per-pause override; operator can extend before expiration.
3. **Partial resume.** Can a HITL response include "resume but skip the next K nodes"? Useful for operator override but complicates the resume-from-event-log model. Bias: no; operator aborts and restarts with different input if they want to skip work.
4. **Snapshot format standardization.** JSONL of events + YAML manifest is one option; a binary protobuf format is another (smaller, faster to parse). Bias: JSONL for v0.1 (readable in text tools; matches audit-log ergonomics); binary format optional in v0.2 for performance.
5. **Idempotency granularity.** Currently resume is idempotent by `(sessionID, resumeToken)`. Is that sufficient, or do we need idempotency keys on Pause too so the same emission doesn't create two pause records? Bias: pause is single-writer per session so intra-session dupes are impossible; cross-session pause coordination doesn't exist yet.
6. **In-process attach for library-embedded** — a Go-library consumer wants to attach programmatically to a paused session inside their own process. Covered in [`./library-api-design.md`](./library-api-design.md); mentioned here as a cross-doc dependency.
7. **Snapshot redaction.** Snapshots may contain secrets pulled from MCP calls, provider responses, etc. Should snapshot export have a redaction pass? Bias: yes; configurable per-workload allowlist / blocklist; default-safe (redact anything matching common secret patterns).
8. ~~**Recorded-effect outbox shape (v0.2).**~~ *Resolved 2026-08-01 — see "Recorded-effect outbox" under "Side-effect semantics": the session log is the outbox (durable `FunctionCall` = intent, `FunctionResponse` = completion, retention inherited — no side table), checked from a process-wide runner-plugin interception layer shared with the permission gate's runtime wiring, with a fail-closed ambiguous-effect mode scoped to the mutation predicate. The three substrate behaviors the design assumed were source-verified the same day (append-before-run holds on the standard path; ADK doesn't repair dangling calls, so #41 repairs before resuming; plugin bracket covers MCP tools) — plus one new coverage limit: parallel-branch tool events never reach the log. Residual: metric-family names belong to the observability fixed-registry pass.*
9. ~~**Resume-token authz binding.**~~ *Direction set 2026-07-25 (ratify in review): resume tokens are bound to the session's tenant scope (a token minted under scope X cannot resume a session in scope Y — checked before any execution), and resume always re-runs the permission gate on whatever the re-executed node does next (free via the gate-on-re-execution property in "Side-effect semantics"). Default TTL drops from 30 days to 7; per-pause override upward requires an explicit operator action, which is audit-logged. Residual: whether tokens should additionally carry an operator-identity claim for audit attribution — open, non-blocking.*

## Out of scope

- **Distributed scheduler as a mast subsystem.** We ride on ADK v2's node runtime; we do not compete with Temporal/Cadence for workflow-general durability. Mast's durability is agent-shaped, not workflow-general.
- **Multi-region active-active session state.** Interesting but v1.0+; requires session-store guarantees mast doesn't provide.
- **Automatic checkpoint compaction.** Long-running autonomous loops can accumulate millions of events; compaction is a real concern but a v0.4+ optimization.
- **Undo / rewind past a resume.** Once a session resumes past a pause, it moves forward; there's no "back up to turn N and try again." Snapshot+replay covers the debugging case.
- **Encryption of session storage.** Storage backends handle encryption at rest; mast doesn't add its own encryption layer. Covered in [`./deployment-design.md`](./deployment-design.md).
- **Session state migration between mast versions.** Forward-compat for the event schema is expected within a minor version; major version migrations need per-version tooling. Not v0.1 concern.

## Related

- [`./positioning.md`](./positioning.md) — durable execution as fourth pillar
- [`./fork-design.md`](./fork-design.md) — bucket 1 exposes the pause/resume primitive
- [`./orchestration-design.md`](./orchestration-design.md) — planner pause, budget-exhaustion pause, HITL composition
- [`./specialists-design.md`](./specialists-design.md) — specialist HITL pause
- [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) — cyclic graphs and reference shapes inherit durability
- [`./deployment-design.md`](./deployment-design.md) — multi-instance session coordination
- [`./memory-design.md`](./memory-design.md) — audit-derived pause-pattern analytics
- [`./observability-design.md`](./observability-design.md) — pause/resume metrics + traces
- [`./library-api-design.md`](./library-api-design.md) — programmatic pause/resume + SessionStore extension point
- [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) — operator UI for pause inspection + resume
- ADK v2 workflow package (`google.golang.org/adk/v2/workflow`) — the pause/resume substrate
- Temporal / Cadence / Restate / Trigger.dev / Inngest — competitive references for durable execution as a category
