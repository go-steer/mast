# mast v0.2 end-to-end UAT plan

**Status:** draft, 2026-08-05. Companion to [`./durable-execution-design.md`](./durable-execution-design.md)
(the spine under test), [`./observability-design.md`](./observability-design.md) (the metric
families the assertions scrape), [`./a2a-design.md`](./a2a-design.md) and
[`./ag-ui-design.md`](./ag-ui-design.md) (the back-half surfaces whose scenarios grow into this
plan as they land). This is a **dev/testing plan**, not a user-facing feature doc — it lives
under `docs/` alongside [`./triage-demo-plan.md`](./triage-demo-plan.md) and
[`./spike-findings.md`](./spike-findings.md) and does not carry a docs-site mirror.

## Why this doc exists

The v0.2 durable-execution spine has shipped in four merged slices — the recorded-effect
outbox (#70), programmatic pause/abort (#42), boot-time auto-resume (#41), and the
observability fixed-registry pass (#50). Each landed with unit and integration coverage
(`pkg/transcript`, `pkg/effects`, `cmd/mast` `newTurnHarness`/`httptest`), and
[`./triage-demo-plan.md`](./triage-demo-plan.md) plus `scripts/demo-spike2.sh` already prove
the v0.1 graph/HITL/budget path end-to-end against a running binary.

What does **not** exist yet is an acceptance suite that drives the *whole v0.2 spine* through a
real daemon process — boot, inject, crash, restart, drain, resume, abort, scrape — as one
deterministic, credential-free, CI-runnable pass. The per-PR tests each verify a seam; this
UAT verifies the seams compose. It is also the acceptance frame the A2A server and AG-UI work
will extend: every back-half feature adds its scenarios here.

This doc is the plan (scenarios, assertions, harness shape). The harness itself
(`scripts/uat-v0.2.sh` + its CI wiring) lands as the follow-on, tracked separately, and grows
alongside the A2A→AG-UI stack.

## Implementation status (2026-08-08)

`scripts/uat-v0.2.sh` has landed with its CI wiring (`dev/ci/presubmits/e2e.sh`, the `e2e`
step in `all.sh`, the `e2e` job in `ci.yml`). It ships the **deterministic, no-blocking-tool
subset** of the catalogue below and passes 38 assertions in under a minute on the offline echo
model alone. Two findings from building it correct assumptions the plan above was drafted on —
read these before extending the harness:

### Correction 1 — the scripted model cannot plant a dangling mutating effect

The plan (S1/S2/S4/S8/S9, "Provider: scripted") assumed a scripted `FunctionCall` to an
undispatched tool could be `kill -9`'d between intent-persist and response-persist, leaving a
dangling effect for the outbox to catch. It cannot. ADK persists the `FunctionCall` intent
before dispatch (`base_flow.go:612`), but for an **unregistered** tool name it synthesizes an
error `FunctionResponse` **in the same turn** (`base_flow.go:1090-1211`) — the call is *paired*,
never dangling, so no ambiguous-effect mode is ever entered. The effects predicate
(`pkg/effects/effects.go:118-134`) classifies by name (default-deny-unknown → mutating), and the
outbox refusal (`ambiguous_prior_effect`) fires in `beforeTool`, which only runs for **registered**
`FunctionTool`s. Getting a genuine dangling intent therefore requires a **real registered
blocking tool** plus an interruption landed between the two persists — which the offline
echo/scripted models cannot provide.

### Correction 2 — mast cannot consume a local/stdio MCP tool yet (blocking-tool prerequisite)

> **Prerequisite met (#87, 2026-08-09).** mast now parses `mcp.json` and wires MCP servers
> generically by transport, including local **stdio** (`command:`) servers, with the old
> `gke`-only guard removed (`pkg/mcp`; see [`./mcp-catalog-design.md`](./mcp-catalog-design.md)
> "Implementation status"). A credential-free offline UAT can therefore wire a controllable
> blocking tool by pointing the fixture at a small stdio MCP server under `--model scripted`.
> Implementing the deferred legs below against such a server is the remaining follow-on; the
> blocking-tool *capability* they were waiting on now exists.

The natural way to supply that registered blocking tool is a local MCP server. When this plan was
first written, v0.2 mast could not consume one: `mcp.json` was never parsed (its fields were
decorative), and MCP wiring was hardcoded to the HTTP `gke` server, gated on `model != echo`. A
credential-free offline UAT thus had **no registered tool it could dispatch and block**. The
fixture (`testdata/uat/workload.yaml`) declares tool *policies* (`read_status` read-only,
`apply_change` mutating) for effect classification only; it wires no MCP.

**Consequence — deferred scenarios.** The legs that need a controllable registered blocking tool
were **deferred until local/stdio MCP support landed** (now shipped in #87 — making `mcp.json` real
for `command:` servers and dropping the `gke`-only guard):

| Scenario | Why it needs the blocking tool |
|---|---|
| S1 (ambiguous-effect ack) | needs a dangling mutating effect (Correction 1) |
| S2 (read-only auto-resume repair) | needs a dangling read-only call |
| S3 (planned-stop `--pause-sessions`) | the drain gate-pause only fires for a session with an **active in-flight turn** (`shutdown.go:266`, `active[sid] > 0`); echo turns finish instantly |
| S4 exit-3 (drain-expired) / S5 (watchdog) | need an in-flight turn to still be draining at the deadline (already flagged as caveats) |
| S8 mid-turn cancel / S9 loop-breaker | need a turn held open across the abort / a failing continuation turn |

### What shipped (deterministic subset)

- **Boot priming** — all v0.2 metric families (plus the A2A + AG-UI families) present at zero for
  `workload=uat` on a fresh boot.
- **Auth** — inject refused 401 without a valid bearer; 202 with it.
- **S7** — operator gate pause: turns blocked 409 while paused; `mast_gate_pauses_total{source=
  "operator"}` counts once and an in-place refresh does **not** double-count (#50 F1); extend-token
  moves the deadline; resume-by-token clears the gate.
- **S7b (token lifecycle correction)** — the plan's "a consumed/expired token is rejected on
  replay" conflated two behaviors. A **consumed** gate token replayed is a deliberate **idempotent
  no-op → 202** ("the resume the token asked for has happened", `resumeByToken`
  `main.go:658-663`), *not* a rejection. The genuine rejection path is an **expired** token →
  **409** (`main.go:665-667`); the harness exercises it with a sub-second TTL and asserts the
  pause remains.
- **S6** — a timed pause (`resume_at`) fires through `ConsumeScheduled` and records
  `mast_timed_pause_fires_total{outcome="resumed"}`, auto-resuming the session. (The plan's
  "after restart" reseed leg is deferred with the crash-restart family; the fires-and-resumes
  invariant ships now, no restart needed.)
- **S8 (marker path)** — `/abort` writes the terminal marker; `assert_state aborted`;
  `mast_aborts_total` counts once; a later inject is refused 409; re-abort keeps the counter at 1.
- **S9 (cardinality)** — no `session_id` appears as a label anywhere in `/metrics`.
- **S4a** — clean SIGTERM drain with no in-flight turn → exit 0; bad flag → exit 2.

### Latent bug the UAT surfaced — `/abort` re-abort returns HTTP 500

Re-aborting an already-aborted session returns **HTTP 500**: `abortHandler`
(`cmd/mast/main.go:704-720`) returns `store.Abort`'s `ErrAlreadyAborted` sentinel raw, and the
inject server maps an unrecognized error to 500. Contrast `/pause`, which maps the same sentinel
to 409 (`main.go:834`), and A2A `tasks/cancel`, which maps it to success (`a2a.go:532`). The
durable marker **is** idempotent (the counter stays at 1), so this is a status-code wart, not a
correctness bug. The harness asserts the durable invariant (counter stays 1, state stays
aborted), not the buggy status, and this is tracked for a follow-up fix (map
`ErrAlreadyAborted` → 409 in `abortHandler`, mirroring `/pause`).

## What v0.2 ships (the surface under test)

| Slice | PR | Surface exercised end-to-end |
|---|---|---|
| Recorded-effect outbox | #70 | Ambiguous-effect mode after a dangling mutating call; `ack-effects` clears it; read-only work proceeds during ambiguity. |
| Programmatic pause/abort | #42 | `/pause` gate + token lifecycle (expire/extend/replay); `/abort` terminal marker + in-flight cancel; timed pause fires. |
| Boot-time auto-resume | #41 | On boot, eligible interrupted sessions get a continuation turn; ineligible ones are classified and left. `--auto-resume[=false]`, `--auto-resume-window`. |
| Observability fixed registry | #50 | Five durable-execution counter families primed to zero; typed increments; `/metrics` on the inject listener; teardown watchdog exit-4. |
| Planned stop / drain | #39/#42 | `mast stop [--pause-sessions]`; SIGTERM drain; 503 + Retry-After during drain; exit codes 0/3/4. |

**Scope note (dispatch):** v0.2 boot-time auto-resume is **coordinator-only** by design
(#41 slice-1; graph/workflowagent turn-driving for interrupted-but-not-paused sessions is
unverified and deferred). Every UAT scenario that exercises auto-resume therefore runs
`--dispatch=coordinator`, not the `--dispatch=graph` that `demo-spike2.sh` uses. Scenarios
that only exercise pause/abort/drain are dispatch-agnostic and default to coordinator for
uniformity.

## Harness shape

`scripts/uat-v0.2.sh` — same idiom as `scripts/demo-spike2.sh`: build the binary once, run it
against the **offline echo model** (and the **scripted provider** where a mutating tool call
is needed), a **real SQLite session DB**, drive the HTTP surface with `curl`, and assert on
session state, `/metrics` output, log lines, and process exit codes.

Non-negotiables that keep it CI-safe:

- **Deterministic + credential-free + offline.** `--model=echo` (no creds, no network) for
  read-only/no-tool flows; `--model=scripted --provider=scripted` with a `MAST_SCRIPT` JSONL
  fixture (and `MAST_SCRIPT_STRICT=1`) for the flows that require an actual mutating
  `FunctionCall` — the echo model emits only `finish_task`/classifier reasons and **cannot**
  produce an arbitrary mutating tool call, so the outbox/ambiguous-effect scenarios must use
  the scripted provider.
- **`--session-db` SQLite (default driver).** In-memory sessions never survive a restart, so
  every crash/restart/auto-resume scenario needs the SQLite file. The Postgres backend
  (`--session-db-driver=postgres`) is a documented manual matrix run, not a CI default (no
  Postgres in the presubmit image).
- **State under `${TMPDIR:-/tmp}/mast-uat-v02`** (house rule #5), fixed port, `set -euo
  pipefail`, `trap cleanup EXIT` that kills any survivor PID. Never `$HOME`.
- **Auth via env.** `MAST_INJECT_TOKEN` (and `MAST_ATTACH_TOKEN` where attach is exercised)
  set to fixed test values.

### Fixture workload

A minimal `testdata/uat/` (or `examples/workloads/uat-fixture/`) bundle with exactly two
tools declared in `tool_catalog.tools`:

- `read_status` — read-only (classified non-mutating), used for the "dangling read-only call
  → repaired + resumed" leg.
- `apply_change` — mutating, used for the "dangling mutating call → ambiguous-effect →
  ack-effects" leg. Its scripted response drives the crash-mid-mutation timing.

Keeping the fixture tiny (two tools, one coordinator agent, no MCP) makes the scripted JSONL
short and the assertions legible. The scripted fixture files live next to the script under
`testdata/uat/scripts/*.jsonl`.

### Assertion helpers (bash)

- `assert_metric <substr>` — scrape `GET /metrics`, grep for an **exact** counter line (e.g.
  `mast_gate_pauses_total{source="operator",workload="uat"} 1`). Proves priming *and* value.
- `assert_state <session-id> <state>` — `mast sessions show <id> --session-db=<db>` and match
  `paused|aborted|interrupted|idle`. Works with no daemon running (direct DB read).
- `assert_exit <expected>` — capture the daemon's exit code across a stop/kill.
- `assert_http <method> <path> <expected-status>` — for the 503+Retry-After-during-drain and
  409-on-gate-paused checks.
- `wait_for <predicate>` — bounded poll loop (mirror `demo-spike2.sh`'s `start()` readiness
  spin), never an unbounded sleep.

### CI wiring

Add `dev/ci/presubmits/e2e.sh` (Apache header) that runs `scripts/uat-v0.2.sh`, wire it into
a new `e2e` job in `.github/workflows/ci.yml` **and** into `dev/ci/presubmits/all.sh`'s step
list (house rule #6 — the scripts are the single source of truth; local == CI). The e2e job
builds the binary from source like the other jobs; it needs no services beyond what SQLite +
the offline models provide. Runtime target: under ~60s so it stays a presubmit, not a
nightly.

## Scenarios

Each scenario names its **intent**, **setup**, **action**, **assertions**, the **feature it
proves**, and the **provider** it needs. Assertions are the contract; the harness fails the
build if any miss.

### S1 — Crash mid-mutation → ambiguous-effect → operator ack → completion
- **Intent:** the once-and-only-once backstop. A mutating call whose completion did not fsync
  before a crash must NOT be auto-resumed; it must land in ambiguous-effect mode and require
  an operator ack.
- **Setup:** scripted provider; fixture emits `apply_change`; `--dispatch=coordinator`,
  `--session-db`, `--auto-resume=true`.
- **Action:** inject → let the model emit the mutating `apply_change` call → `kill -9` before
  the response is recorded → restart fresh on the same DB.
- **Assert:** `mast_autoresume_total{outcome="skipped_ambiguous"}` +1 (auto-resume declined);
  next inject/turn is refused `ambiguous_prior_effect` (read-only `read_status` still
  proceeds); `mast sessions ack-effects <id>` clears it; a subsequent turn completes;
  `assert_state idle`.
- **Proves:** #70 outbox + #41 eligibility gate (H1) composing — the load-bearing safety
  interaction of the whole spine.
- **Provider:** scripted.

### S2 — Crash mid read-only call → auto-resume repairs + completes
- **Intent:** the happy auto-resume path. A read-only tool cut mid-flight is repairable.
- **Setup:** scripted provider; fixture emits `read_status`; auto-resume on; coordinator.
- **Action:** inject → model emits `read_status` → `kill -9` before response → restart.
- **Assert:** boot pass answers the dangling call with the synthetic
  `interrupted before completion` response and drives a continuation turn;
  `mast_autoresume_total{outcome="resumed"}` +1; marker cleared; `assert_state idle`.
- **Proves:** #41 repair path (H2/H3) + the trailing-event classifier.
- **Provider:** scripted.

### S3 — Planned stop `--pause-sessions` → restart skips gate-paused
- **Intent:** a gate pause outranks the interruption marker; auto-resume must not touch it.
- **Setup:** coordinator; auto-resume on.
- **Action:** inject → `mast stop --pause-sessions` → restart.
- **Assert:** `mast_gate_pauses_total{source="planned_stop"}` +1 at stop;
  `mast_autoresume_total{outcome="skipped_...}` (gate-paused, not resumed) on restart;
  session stays `paused` until an explicit resume.
- **Proves:** #42 planned-stop gate-pause + #41 precedence rule.
- **Provider:** echo (no tool call needed).

### S4 — SIGTERM drain: exit codes + 503 during drain
- **Intent:** the drain contract and its exit-code encoding.
- **Setup:** coordinator; a scripted turn long enough to still be in-flight when SIGTERM
  arrives (or a fixture tool that blocks).
- **Action:** inject → SIGTERM mid-turn → observe `/inject` + `/resume` responses during the
  drain → observe exit.
- **Assert:** during drain `/inject` and `/resume` return **503 + Retry-After**; clean drain
  → **exit 0**; drain window expired with interrupted survivors → **exit 3**. (Exit 3 needs a
  controllable drain deadline or a blocking tool — see *Determinism caveats*.)
- **Proves:** #39/#42 drain path + exit-code table.
- **Provider:** scripted (for the in-flight turn).

### S5 — Teardown watchdog → exit 4
- **Intent:** a wedged post-drain unwind surfaces a diagnostic, not a hang.
- **Setup:** requires an injectable post-drain stall (a build-tagged test hook or an env-gated
  sleep in a Close path) — production code has no natural 15s post-drain deadlock. Flagged as
  a **harness-hook scenario**: if no hook exists, this stays a Go-test assertion
  (`armTeardownWatchdog` unit coverage from #50) rather than a shell e2e leg.
- **Assert (if hooked):** stderr carries a goroutine stack dump; **exit 4**.
- **Proves:** #50 teardown watchdog.
- **Provider:** n/a.

### S6 — Timed pause fires after restart
- **Intent:** a `ResumeAt`-scheduled pause survives a restart — the boot ops-scan reseeds the
  scheduler.
- **Setup:** coordinator; auto-resume on.
- **Action:** pause a session with a near-future `ResumeAt` (rate-limit-backoff reason) →
  restart before it fires → wait past `ResumeAt`.
- **Assert:** `mast_timed_pause_fires_total{outcome="resumed"}` +1 after restart; session
  advances; token consumed once (no double-fire).
- **Proves:** #42 timed pause + boot reseed.
- **Provider:** echo.

### S7 — Gate pause blocks every turn kind (409) + token lifecycle
- **Intent:** an operator gate pause holds against inject/resume/timed drives; token
  expire/extend/replay behave.
- **Setup:** coordinator.
- **Action:** `/pause` (operator) → attempt `/inject` and `/resume` → `/extend-token` →
  expire → replay a consumed token.
- **Assert:** turns blocked with **409** while gate-paused; `mast_gate_pauses_total{source=
  "operator"}` counts **once** for the initial pause and **not** for an in-place refresh
  (the #50 F1 created-vs-refresh contract); extend moves the deadline; a **consumed** token
  replayed is an idempotent no-op (202) while an **expired** token is rejected (409) — see the
  token-lifecycle correction under *Implementation status*.
- **Proves:** #42 gate pause + token lifecycle + #50 counter semantics.
- **Provider:** echo.

### S8 — Terminal abort mid-turn
- **Intent:** `/abort` cancels the in-flight turn, writes a durable terminal marker, purges
  tokens/timers, and is idempotent.
- **Setup:** coordinator; a scripted in-flight turn.
- **Action:** inject → `/abort` mid-turn → retry a turn on the aborted session → re-abort.
- **Assert:** in-flight turn cancelled; `assert_state aborted`;
  `mast_aborts_total{workload="uat"}` +1; subsequent turns refused **409**; re-abort is a
  no-op (marker landed once).
- **Proves:** #42 abort path.
- **Provider:** scripted.

### S9 — Restart-loop breaker + metric priming/cardinality
- **Intent:** a poison session cannot loop the daemon forever, and the metric surface is
  well-formed.
- **Setup:** coordinator; a fixture session that fails its continuation turn repeatedly.
- **Action:** restart N times.
- **Assert:** after the per-session cap (3 attempts / 10m window) the session is
  `skipped_loopbreak`, not retried; at every boot **all five families are present and start
  at zero** (priming); **no `session_id` appears as a label** on any series (grep the raw
  `/metrics` for the known session IDs → must be absent).
- **Proves:** #41 loop breaker + #50 priming/cardinality guarantees.
- **Provider:** scripted.

## Exit-code contract (asserted in S4/S5)

| Code | Meaning | Scenario |
|---|---|---|
| `0` | Clean drain — all in-flight turns finished. | S4 |
| `1` | Error. | (negative-path spot check) |
| `2` | Usage. | (flag-parse spot check) |
| `3` | Drain window expired with interrupted survivors. | S4 |
| `4` | Teardown watchdog fired (post-drain unwind deadlocked past 15s). | S5 |

## Metric assertions (cross-cutting)

Every boot, before any work, the harness scrapes `/metrics` and asserts all five families are
present at zero for the workload label: `mast_autoresume_total`,
`mast_marker_write_failures_total`, `mast_aborts_total`, `mast_gate_pauses_total`,
`mast_timed_pause_fires_total`. Per-scenario deltas are listed inline above. The
cardinality check (no `session_id` label; no unbounded label values) runs once per boot in S9
but the raw scrape is captured in every scenario for post-hoc inspection.

## Determinism caveats (call them out, don't paper over)

- **Exit 3 (drain-expired) and S5 (watchdog)** need a *controllable deadline* — a blocking
  fixture tool or a test-only short drain/watchdog bound. If a clean env-gated hook can't be
  added without touching production paths, these two degrade to Go-test assertions (both
  already have unit coverage from #42/#50) and the shell e2e asserts only the deterministic
  exit 0 path. The plan does not pretend a race is deterministic.
- **`kill -9` timing (S1/S2)** — the scripted fixture must make the crash window
  deterministic: the model emits the tool call, the harness waits for the call to appear in
  the log (bounded poll), *then* `kill -9`, guaranteeing the response was never recorded.
  This is exactly `demo-spike2.sh`'s durable-HITL pattern, reused.
- **No wall-clock sleeps for correctness** — every "wait" is a bounded poll on an observable
  (log line, session state, metric value), never a fixed `sleep` that assumes timing.

## Out of scope (this UAT)

- **Live-provider realism** — Gemini/Anthropic runs are a separate manual smoke; the UAT is
  offline by contract.
- **Multi-replica / HA** — single daemon, single DB, per the v0.2 substrate scope.
- **Postgres backend** — SQLite is the CI default; the Postgres path (`--session-db-driver=
  postgres`) is a documented manual matrix run.
- **Graph-dispatch auto-resume** — coordinator-only in v0.2 (#41 slice-1); a
  `skipped_unsupported` assertion under `--dispatch=graph` is the only graph-path coverage
  here.

## A2A / AG-UI growth (living checklist)

As the back-half stack lands, append scenarios here (harness grows in lockstep):

- **A2A server:** fetch `/.well-known/agent-card.json` (and a per-workload card) →
  schema-shape assertion; `POST /a2a` `message/send` round-trip → task created with
  `taskId == sessionId`; `tasks/get` reflects lifecycle; `tasks/cancel` → aborted; a
  missing/insufficient-scope token → **403**; `message/stream` SSE emits ordered events.
  Auth uses a static bearer `TokenValidator` for determinism (no JWKS network).
- **AG-UI:** `RunAgentInput` accepted on the per-workload endpoint → ordered event stream
  (lifecycle/text/tool-calls/state); a HITL interrupt (`RunFinished{interrupt}`) projects
  mast's durable pause and a follow-up `RunAgentInput.resume` resumes it — reusing the S6/S7
  durability assertions through the AG-UI plane.

Both reuse the same fixture workload (opted into `a2a:`/`agui:` exposure) and the same
assertion helpers, so the back-half coverage is incremental, not a second harness.

## Sequencing

1. **This plan doc** (now) — reviewed, merged as the acceptance frame.
2. **Harness skeleton + S1–S9** — `scripts/uat-v0.2.sh`, the fixture bundle + scripted JSONL,
   the bash assertion helpers, `dev/ci/presubmits/e2e.sh`, the `e2e` CI job. Deterministic
   scenarios wired; the two race-shaped ones (exit-3, watchdog) resolved per the caveat.
3. **Grow with the stack** — A2A server scenarios land with the A2A server PRs; AG-UI
   scenarios with the AG-UI PRs. The plan's living checklist is the running TODO.

## Related

- [`./durable-execution-design.md`](./durable-execution-design.md) — the spine under test
- [`./observability-design.md`](./observability-design.md) — the five metric families asserted
- [`./triage-demo-plan.md`](./triage-demo-plan.md) — the v0.1 end-to-end demo this extends
- [`./spike-findings.md`](./spike-findings.md) — the resume-contract / allowlist semantics the
  scenarios rely on
- [`./a2a-design.md`](./a2a-design.md), [`./ag-ui-design.md`](./ag-ui-design.md) — the
  back-half surfaces whose scenarios grow into this plan
