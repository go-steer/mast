# mast durable execution: design

**Status:** draft, 2026-07-01 (updated 2026-07-25 — spike-2 verification: the resume model is pinned down (reconstruct-and-re-execute, verified across `kill -9`), ADK's `session/database` service reshapes the storage table, side-effect semantics under re-execution added as a mandatory design section, and the v0.1/v0.2 programmatic-pause phasing contradiction fixed. Updated 2026-07-26 — the v0.1 operator CLI shipped (`mast sessions list|show|resume|abort`; resume by InterruptID, abort as durable marker) and two ADK substrate constraints recorded under "Persistent session services"). Companion to [`./positioning.md`](./positioning.md) (in which "durable" is proposed as a fourth pillar alongside unattended / library / multi-provider), [`./fork-design.md`](./fork-design.md) (ADK v2 provides the substrate; bucket 1's lean core exposes the semantics), [`./orchestration-design.md`](./orchestration-design.md) (planner pause/resume + workload budget composition), [`./specialists-design.md`](./specialists-design.md) (specialist HITL pause), and [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) (cyclic graphs / autonomous loops that survive restarts). This doc treats **durable execution as its own subsystem** rather than as an implementation detail scattered across other designs.

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

  *Two substrate constraints found building the v0.1 sessions surface (2026-07-26, `pkg/session`): (1) ADK's `Service.List` returns sessions **without their events**, and pause state is an event-log property — so listing paused sessions is an N+1 (one `Get` per session to scan for pending interrupts). Fine at operator-CLI scale; the P1.3 eventlog/query surface should index pause state so `list --state=paused` doesn't degrade to a full scan at fleet scale. (2) `Service.AppendEvent` type-asserts the service's **own concrete session type** — a session loaded through one service implementation cannot be appended through another, so you can't mix services (e.g. two `NewSessionService` instances, or database + in-memory) over one logical store handle; every writer must go through the same service instance, which is also why the v0.1 abort path routes through the owning daemon.*
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
- **Fsync-durable per event.** An event isn't considered persisted until fsync (or equivalent cloud durability guarantee). Losing the last N events on crash means losing them, not corrupting state; resumes from the last durable event.
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
8. **Recorded-effect outbox shape (v0.2).** Given the at-least-once contract ("Side-effect semantics" above), what does the tool-execution wrapper's effect record look like — keyed by `(session, invocation, function-call ID)`, stored as events or a side table, and how does it interact with retention? Added 2026-07-25.
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
