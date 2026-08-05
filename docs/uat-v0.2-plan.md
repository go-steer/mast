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
  (the #50 F1 created-vs-refresh contract); extend moves the deadline; a consumed/expired
  token is rejected on replay.
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
