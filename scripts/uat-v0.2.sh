#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# uat-v0.2.sh — end-to-end acceptance pass for the mast v0.2 durable-
# execution spine. Drives a real daemon process (boot, inject, pause,
# abort, drain, restart, scrape) and asserts on session state, /metrics
# output, HTTP status, and process exit codes. Two offline models, no
# credentials, no network: --model=echo for the read-only / control-plane
# legs, and --model=toolactor + a local stdio MCP blocker (testdata/uat/
# blocker) for the blocking-tool crash/drain/abort legs. Deterministic (a
# fixed test bearer, bounded polls, no unbounded sleeps) and CI-runnable.
#
# See docs/uat-v0.2-plan.md for the scenario catalogue, the assertion
# contract, and Corrections 1-3, which explain why the blocking-tool legs
# use a request-driven fake model + a real stdio tool (they need a
# registered tool that BLOCKS mid-call, which the echo/scripted models
# cannot supply) and which scenarios remain unit-only (S4 503-during-drain,
# S9 loop-breaker, the "resumed" read-only repair).
#
# Usage: scripts/uat-v0.2.sh
# State goes under ${TMPDIR:-/tmp}/mast-uat-v02 (house rule #5); port 7788.

set -euo pipefail

PORT="${MAST_UAT_PORT:-7788}"
BASE="http://127.0.0.1:${PORT}"
WORK="${TMPDIR:-/tmp}/mast-uat-v02"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${WORK}/mast"
FIXTURE="${REPO}/testdata/uat"
TOKEN="uat-inject-token"
export MAST_INJECT_TOKEN="${TOKEN}"
PID=""
BGPID=""

# Blocking-tool infrastructure (the crash/drain/abort legs). BLOCKER is a
# controllable stdio MCP server (testdata/uat/blocker) that BLOCKS a tool
# call until the harness releases it — the registered, blocking tool the
# deferred legs were waiting on (docs/uat-v0.2-plan.md "Blocking-tool
# prerequisite"). The fixture's mcp.json resolves ${MAST_UAT_BLOCKER} +
# ${UAT_BLOCKER_DIR} from the daemon env; they are exported so a
# --model=toolactor daemon wires and launches the server on first tool use.
# The echo legs are unaffected: wireMCPToolsets is a no-op under echo.
BLOCKER="${WORK}/uat-blocker"
BLOCKDIR="${WORK}/block"
export MAST_UAT_BLOCKER="${BLOCKER}"
export UAT_BLOCKER_DIR="${BLOCKDIR}"

PASS=0
FAIL=0

# ---- output ---------------------------------------------------------
say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
ok()   { PASS=$((PASS + 1)); printf '   \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { FAIL=$((FAIL + 1)); printf '   \033[31mFAIL\033[0m %s\n' "$*"; }

# ---- lifecycle ------------------------------------------------------
cleanup() {
  [ -n "${PID}" ] && kill "${PID}" 2>/dev/null || true
  [ -n "${BGPID}" ] && kill "${BGPID}" 2>/dev/null || true
  return 0
}
trap cleanup EXIT

# start <logfile> [extra flags...] — launch the daemon on the fixture
# workload against the shared SQLite DB and spin until it answers.
start() {
  local log="$1"; shift
  "${BIN}" --workload="${FIXTURE}" --dispatch=coordinator \
    --listen=":${PORT}" --model=echo --session-db="${DB}" \
    --log-level=info "$@" >"${log}" 2>&1 &
  PID=$!
  local i
  for i in $(seq 1 100); do
    # Liveness first: if OUR process already died (e.g. a port-bind failure
    # because a stale daemon holds ${PORT}), bail now rather than mistaking
    # the stale daemon's /  response for our fresh boot.
    if ! kill -0 "${PID}" 2>/dev/null; then break; fi
    if curl -sf -m 1 "${BASE}/" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  echo "daemon failed to start; log:" >&2; cat "${log}" >&2; exit 1
}

# stop_term — graceful SIGTERM, reap, and return the daemon exit code in
# the global STOP_CODE (never trips set -e).
stop_term() {
  STOP_CODE=0
  kill -TERM "${PID}" 2>/dev/null || true
  wait "${PID}" 2>/dev/null || STOP_CODE=$?
  PID=""
}

# ---- HTTP drivers ---------------------------------------------------
# inject_code <uid> — POST a minimal edge event; echo the HTTP status.
inject_code() {
  curl -s -m 30 -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/inject" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"kind\":\"uat-event\",\"reason\":\"UATReason\",\"namespace\":\"default\",\"name\":\"pod-$1\",\"uid\":\"$1\",\"message\":\"uat\",\"cluster\":\"uat\"}"
}

# inject_code_unauth <uid> <bearer> — inject with an explicit (wrong) token.
inject_code_unauth() {
  curl -s -m 30 -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/inject" \
    -H "Authorization: Bearer $2" -H 'Content-Type: application/json' \
    -d "{\"kind\":\"uat-event\",\"reason\":\"UATReason\",\"namespace\":\"default\",\"name\":\"pod-$1\",\"uid\":\"$1\",\"message\":\"uat\",\"cluster\":\"uat\"}"
}

# post_json <path> <json> — POST an authed JSON body; echo the response body.
post_json() {
  curl -s -m 10 -X POST "${BASE}$1" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' -d "$2"
}

# post_code <path> <json> — POST an authed JSON body; echo the HTTP status.
post_code() {
  curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST "${BASE}$1" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' -d "$2"
}

# ---- assertions -----------------------------------------------------
# assert_http <label> <got> <want>
assert_http() {
  if [ "$2" = "$3" ]; then ok "$1 (HTTP $2)"; else bad "$1 (HTTP $2, want $3)"; fi
}

# assert_metric <label> <exact metric line> — the line must appear verbatim
# in a fresh /metrics scrape (proves both priming and value).
assert_metric() {
  if curl -s -m 5 "${BASE}/metrics" | grep -Fxq "$2"; then
    ok "$1"
  else
    bad "$1 — missing metric line: $2"
  fi
}

# assert_state <label> <session-id> <want> — read the SQLite DB directly
# (no daemon required) and match the rendered State.
assert_state() {
  local got
  got="$("${BIN}" sessions show "$2" --session-db="${DB}" 2>/dev/null | awk '/^State:/ {print $2}')"
  if [ "${got}" = "$3" ]; then ok "$1 (state=${got})"; else bad "$1 (state=${got:-<none>}, want $3)"; fi
}

# assert_no_session_label <label> <session-id...> — no session id may
# appear anywhere in the raw /metrics output (cardinality guarantee).
# Fails CLOSED: an empty or workload-less scrape proves nothing about
# absence, so it is a FAIL, not a vacuous pass. Always returns 0 so a real
# leak records a FAIL without aborting the run under `set -e` (this is the
# one helper whose final command isn't an ok/bad printf).
assert_no_session_label() {
  local label="$1"; shift
  local scrape; scrape="$(curl -s -m 5 "${BASE}/metrics")"
  if ! printf '%s' "${scrape}" | grep -Fq 'workload="uat"'; then
    bad "${label} — /metrics scrape empty or missing workload label"
    return 0
  fi
  local sid leaked=""
  for sid in "$@"; do
    if printf '%s' "${scrape}" | grep -Fq "${sid}"; then
      leaked="${leaked} ${sid}"
    fi
  done
  if [ -n "${leaked}" ]; then
    bad "${label} — session id(s) leaked into /metrics:${leaked}"
  else
    ok "${label}"
  fi
  return 0
}

# wait_for <seconds> <cmd...> — bounded poll until cmd succeeds; returns
# non-zero on timeout. Never an unbounded sleep.
wait_for() {
  local budget="$1"; shift
  local deadline=$((budget * 10)) i
  for ((i = 0; i < deadline; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  return 1
}

metric_has() { curl -s -m 5 "${BASE}/metrics" | grep -Fxq "$1"; }

# boot_scan_done <daemon-log> — true once the boot auto-resume pass has logged
# a TERMINAL line. The pass runs in a goroutine the serving path does NOT await
# (main.go serves `/` before bootDone), so a stay-at-zero auto-resume counter
# must be scraped only AFTER this releases, else a fast scrape reads the still-
# primed 0 and false-greens a regression. At --log-level=debug the pass emits a
# terminal line in BOTH outcomes: the correctly-excluded path logs "no
# interrupted sessions" (candidates==0, no increments); a regression that admits
# the session instead logs "auto-resume decision" AFTER incrementing its outcome
# counter (autoresume.go run(): AutoResume() then the decision log). Either match
# is a guaranteed happens-after the (non-)increment.
boot_scan_done() { grep -qE 'no interrupted sessions|auto-resume decision' "$1"; }

# state_is <session-id> <want> — true when the DB-rendered State equals want.
# Reads the State: field with awk (padding-independent), so a layout change in
# `sessions show` can't silently defeat the readiness gates that poll on it.
state_is() {
  local got
  got="$("${BIN}" sessions show "$1" --session-db="${DB}" 2>/dev/null | awk '/^State:/ {print $2}')"
  [ "${got}" = "$2" ]
}

# await_idle <session-id> — a HARD assertion that the injected turn ran to
# completion (session reached idle) before we drive pause/abort against it.
# Not best-effort: if the echo turn never lands, that is a real failure — and
# without it every downstream control-plane assertion would pass vacuously
# against a session that never started (adversarial false-green finding).
await_idle() {
  if wait_for 15 state_is "$1" idle; then
    ok "$1 reached idle after inject"
  else
    bad "$1 did not reach idle within budget"
  fi
}

# now_plus <seconds> — an RFC3339 UTC timestamp <seconds> in the future.
# GNU coreutils form first (CI/Linux), BSD/macOS date as the fallback, so
# `dev/ci/presubmits/all.sh` is green on a developer laptop too (house rule
# #6: local == CI). Prints nothing and returns non-zero if neither form works.
now_plus() {
  date -u -d "+$1 seconds" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
    || date -u -v+"$1"S +%Y-%m-%dT%H:%M:%SZ
}

# ---- blocking-tool drivers (toolactor legs) -------------------------
# start_toolactor <logfile> [extra flags...] — launch the daemon under
# --model=toolactor, which drives the blocking MCP tools deterministically
# (pkg/agent/toolactor.go). Same readiness spin as start(). Uses the current
# ${DB}, so a per-leg DB reassignment isolates each crash/restart leg's
# auto-resume metric deltas, and the current ${WL} (defaults to the fixture)
# — the S4-exit3 leg points it at a budget-free variant so the drain window,
# not the turn budget, is what expires.
start_toolactor() {
  local log="$1"; shift
  mkdir -p "${BLOCKDIR}"
  "${BIN}" --workload="${WL:-${FIXTURE}}" --dispatch=coordinator \
    --listen=":${PORT}" --model=toolactor --session-db="${DB}" \
    --log-level="${LOGLEVEL:-info}" "$@" >"${log}" 2>&1 &
  PID=$!
  local i
  for i in $(seq 1 100); do
    if ! kill -0 "${PID}" 2>/dev/null; then break; fi
    if curl -sf -m 1 "${BASE}/" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  echo "toolactor daemon failed to start; log:" >&2; cat "${log}" >&2; exit 1
}

# inject_bg <uid> <reason> — POST an edge event whose turn will BLOCK inside
# the MCP tool, backgrounded with a long client timeout. inject is
# SYNCHRONOUS (the turn runs on the request context; 202 is written only
# after it completes — pkg/inject/server.go:362,383), so a blocking turn
# must be backgrounded or the client-timeout would cancel the turn. The HTTP
# status lands in ${WORK}/inject-<uid>.code once the turn resolves. Sets the
# background curl PID in the global BGPID so the caller can reap it with
# `wait "${BGPID}"` — this MUST NOT be called in a command substitution, or
# the curl would be a child of the subshell (unreapable, and orphaned when
# the subshell exits) instead of the main shell.
inject_bg() {
  local uid="$1" reason="$2"
  curl -s -m 120 -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/inject" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"kind\":\"uat-event\",\"reason\":\"${reason}\",\"namespace\":\"default\",\"name\":\"pod-${uid}\",\"uid\":\"${uid}\",\"message\":\"uat\",\"cluster\":\"uat\"}" \
    > "${WORK}/inject-${uid}.code" &
  BGPID=$!
}

# inject_done <uid> — true once a backgrounded inject has recorded its HTTP
# code (the turn resolved). Used as the cancellation observable: an aborted
# in-flight turn must unwind PROMPTLY rather than wait out its budget.
inject_done() { [ -s "${WORK}/inject-$1.code" ]; }

# started_exists <tool> — true once the blocker has ENTERED the tool handler
# (it writes <tool>.started on entry). The handler runs only after ADK has
# persisted the FunctionCall intent (base_flow.go:612), so this is the
# deterministic "intent persisted, response not yet recorded" crash window.
started_exists() { [ -f "${BLOCKDIR}/$1.started" ]; }

# wait_started <tool> — HARD assertion + bounded poll for the dispatch of a
# blocking tool. Not best-effort: if the tool never dispatches, the crash /
# abort / stop the leg is about to perform would land on nothing and the
# downstream assertions would pass vacuously.
wait_started() {
  if wait_for 15 started_exists "$1"; then
    ok "$1 dispatched (intent persisted, response not yet recorded)"
  else
    bad "$1 did not dispatch within budget"
  fi
}

# release <tool> — let a blocked tool call return (it polls for this file).
release() { : > "${BLOCKDIR}/$1.release"; }
# prerelease_all — pre-create every release marker so any tool call a
# continuation/auto-resume turn drives returns immediately instead of
# blocking the boot pass. Used before a restart in the crash legs.
prerelease_all() { release apply_change; release read_status; }
# reset_blocker — clear all markers between legs so a stale .started/.release
# cannot satisfy a later poll.
reset_blocker() { rm -f "${BLOCKDIR}"/*.started "${BLOCKDIR}"/*.release 2>/dev/null || true; }

# kill9 — SIGKILL the daemon and reap it (the crash in the crash-restart
# legs). Leaves any durable markers on disk; the blocked tool's response was
# never recorded.
kill9() {
  [ -n "${PID}" ] && kill -9 "${PID}" 2>/dev/null || true
  [ -n "${PID}" ] && wait "${PID}" 2>/dev/null || true
  PID=""
}

# term_nowait — SIGTERM the daemon WITHOUT reaping it. The crash legs use
# this to make the daemon's beginDrain durably PRE-MARK the in-flight
# session interrupted (shutdown.go), but the drain then blocks for the whole
# budget on the wedged turn — so the leg confirms the marker landed and then
# kill9s to skip that wait. PID stays set for the follow-up kill9.
term_nowait() { [ -n "${PID}" ] && kill -TERM "${PID}" 2>/dev/null || true; }

# stop_pause_code — planned stop (`mast stop --pause-sessions`) against the
# running daemon; echoes the CLI exit code (0 on a 2xx ACK). The command
# reads MAST_INJECT_TOKEN from the env (already exported) to authenticate,
# ACKs immediately, and the daemon drains async — gate-pausing every session
# the drain marks so boot-time auto-resume hands them back, never continues.
stop_pause_code() {
  local c=0
  "${BIN}" stop --addr="${BASE}" --pause-sessions --reason="uat planned stop" >/dev/null 2>&1 || c=$?
  echo "${c}"
}

# ---- boot invariants (every fresh boot) -----------------------------
# The five durable-execution families, plus the A2A and AG-UI families,
# must all be primed to zero for workload=uat before any work.
assert_primed() {
  say "Boot invariant: metric families primed to zero"
  local m
  for m in \
    'mast_autoresume_total{outcome="resumed",workload="uat"} 0' \
    'mast_autoresume_total{outcome="skipped_ambiguous",workload="uat"} 0' \
    'mast_marker_write_failures_total{operation="mark",workload="uat"} 0' \
    'mast_aborts_total{workload="uat"} 0' \
    'mast_gate_pauses_total{source="operator",workload="uat"} 0' \
    'mast_gate_pauses_total{source="planned_stop",workload="uat"} 0' \
    'mast_timed_pause_fires_total{outcome="resumed",workload="uat"} 0' \
    'mast_a2a_server_tasks_total{outcome="completed",workload="uat"} 0' \
    'mast_agui_runs_total{outcome="success",workload="uat"} 0' \
    'mast_agui_run_duration_seconds_count{workload="uat"} 0' ; do
    assert_metric "primed: ${m%% *}" "${m}"
  done
}

# ====================================================================
# main
# ====================================================================
rm -rf "${WORK}" && mkdir -p "${WORK}"
DB="${WORK}/sessions.db"

say "Build"
(cd "${REPO}" && go build -o "${BIN}" ./cmd/mast)
(cd "${REPO}" && go build -o "${BLOCKER}" ./testdata/uat/blocker)
note "built ${BIN} + ${BLOCKER} (echo/toolactor models — no credentials, no network)"

start "${WORK}/boot.log"

# ---- boot invariants ------------------------------------------------
assert_primed

# ---- Auth spot check ------------------------------------------------
say "Auth: inject without a valid bearer is refused (401)"
assert_http "wrong bearer -> 401" "$(inject_code_unauth auth-x bogus-token)" 401
assert_http "valid bearer -> 202" "$(inject_code auth-ok)" 202

# ---- S7: gate pause blocks turns (409) + token lifecycle ------------
say "S7: operator gate pause blocks turns (409) + token lifecycle"
assert_http "inject s7 -> 202" "$(inject_code s7)" 202
await_idle incident-s7
# `|| true` guards the pipeline: a regressed /pause with no token in the body
# makes grep exit non-zero, which would abort the whole run under set -o
# pipefail; instead S7TOK goes empty and the extend/resume asserts FAIL loudly.
S7TOK="$(post_json /pause '{"session_id":"incident-s7","reason":"operator","message":"uat gate"}' | grep -o '"token":"[^"]*"' | cut -d'"' -f4 || true)"
note "gate token: ${S7TOK}"
assert_state "s7 paused" incident-s7 paused
assert_metric "operator gate pause counted once" 'mast_gate_pauses_total{source="operator",workload="uat"} 1'
assert_http "inject on gate-paused -> 409" "$(inject_code s7)" 409
# In-place refresh must NOT advance the counter (#50 F1 created-vs-refresh).
# Assert the refresh itself succeeded (200) in argument form — a bare curl
# here would abort the run under set -e on any transport error.
assert_http "in-place refresh -> 200" "$(post_code /pause '{"session_id":"incident-s7","reason":"operator","message":"refresh"}')" 200
assert_metric "refresh does not double-count" 'mast_gate_pauses_total{source="operator",workload="uat"} 1'
# Extend moves the deadline.
assert_http "extend-token -> 200" "$(post_code /extend-token "{\"token\":\"${S7TOK}\",\"ttl\":\"48h\"}")" 200
# Resume with the token clears the gate. Replaying the *consumed* token is a
# deliberate idempotent no-op (202) — "the resume the token asked for has
# happened" (resumeByToken, main.go:658-663) — NOT an error.
assert_http "resume by token -> 202" "$(post_code /resume "{\"token\":\"${S7TOK}\"}")" 202
assert_state "s7 resumed to idle" incident-s7 idle
assert_http "replay consumed token is a no-op -> 202" "$(post_code /resume "{\"token\":\"${S7TOK}\"}")" 202

# The genuine token-rejection path is an *expired* token (main.go:665-667 maps
# Expired -> ErrConflict -> 409). Mint one with a sub-second TTL on a fresh
# session, let it lapse, then resume: the pause remains and the replay is
# refused. Deterministic (bounded wait, no minimum-TTL floor: PauseSpec only
# rejects negative or over-default TTLs).
say "S7b: an expired resume token is refused (409); the pause remains"
assert_http "inject s7b -> 202" "$(inject_code s7b)" 202
await_idle incident-s7b
S7BTOK="$(post_json /pause '{"session_id":"incident-s7b","reason":"operator","message":"short ttl","ttl":"500ms"}' | grep -o '"token":"[^"]*"' | cut -d'"' -f4 || true)"
assert_state "s7b paused" incident-s7b paused
sleep 1
assert_http "resume with expired token -> 409" "$(post_code /resume "{\"token\":\"${S7BTOK}\"}")" 409
assert_state "s7b remains paused after expired replay" incident-s7b paused

# ---- S6: timed pause fires and auto-resumes -------------------------
# A gate pause carrying a near-future resume_at arms the timed-pause
# scheduler; at the deadline the daemon fires through ConsumeScheduled and
# records mast_timed_pause_fires_total{outcome="resumed"} (newTimedFireCallback,
# main.go:284-317). No blocking tool needed — the timer is the daemon's own
# commitment. Deterministic: a fixed near-future deadline + a bounded poll.
say "S6: a timed pause fires and auto-resumes the session"
assert_http "inject s6 -> 202" "$(inject_code s6)" 202
await_idle incident-s6
# +5s (not +2s): `date` truncates to whole seconds, so a +N request fires in
# [N-1, N). A wider window guarantees the "paused pending timer" sample below
# — which spawns the mast binary to read SQLite and can take ~1s on a loaded
# runner — observes the pause before the precise-timer scheduler fires it.
RESUME_AT="$(now_plus 5)"
assert_http "arm timed pause -> 200" \
  "$(post_code /pause "{\"session_id\":\"incident-s6\",\"reason\":\"operator\",\"message\":\"timed\",\"resume_at\":\"${RESUME_AT}\"}")" 200
assert_state "s6 paused pending timer" incident-s6 paused
if wait_for 12 metric_has 'mast_timed_pause_fires_total{outcome="resumed",workload="uat"} 1'; then
  ok "timed pause fired once (outcome=resumed)"
else
  bad "timed pause did not fire within budget"
fi
assert_state "s6 auto-resumed to idle" incident-s6 idle

# ---- S8: terminal abort marker + idempotency + 409 ------------------
say "S8: terminal abort marker + idempotency (marker path)"
assert_http "inject s8 -> 202" "$(inject_code s8)" 202
await_idle incident-s8
assert_http "abort s8 -> 202" "$(post_code /abort '{"session_id":"incident-s8","reason":"uat abort"}')" 202
assert_state "s8 aborted" incident-s8 aborted
assert_metric "abort counted once" 'mast_aborts_total{workload="uat"} 1'
assert_http "inject on aborted -> 409" "$(inject_code s8)" 409
# Re-abort of an already-terminal session is a state conflict (#88): the
# /abort door returns 409 (mirroring /pause), and the durable marker stays
# idempotent so the counter holds at 1.
assert_http "re-abort -> 409" "$(post_code /abort '{"session_id":"incident-s8","reason":"again"}')" 409
assert_metric "re-abort marker idempotent" 'mast_aborts_total{workload="uat"} 1'

# ---- S9 (partial): metric cardinality — no session_id label ---------
say "S9: metric cardinality — no session id appears as a label"
assert_no_session_label "no session ids in /metrics" \
  incident-s6 incident-s7 incident-s7b incident-s8 incident-auth-ok

# ---- S4a: clean SIGTERM drain -> exit 0 -----------------------------
say "S4a: clean SIGTERM drain (no in-flight turn) -> exit 0"
stop_term
assert_http "clean drain exit code" "${STOP_CODE}" 0

# ====================================================================
# Blocking-tool legs (--model=toolactor + the stdio blocker)
# ====================================================================
# These legs need a REGISTERED tool that BLOCKS mid-call so a crash / abort
# / planned-stop can land while a turn is genuinely in flight — the
# scenarios docs/uat-v0.2-plan.md deferred until local/stdio MCP (#87). Each
# runs its own daemon on a PER-LEG DB so a restart's auto-resume boot scan
# sees only that leg's session and its metric deltas start from a primed
# zero. The echo section above shares one DB; from here every leg reassigns
# ${DB}.
#
# Reachable auto-resume outcomes, and why (the decision tree in
# cmd/mast/autoresume.go): the fixture drives tools THROUGH a
# coordinator->worker delegation. A crash mid-tool-call therefore leaves TWO
# dangling calls — the coordinator's delegation to uat-worker (the worker
# never returned) AND the worker's own tool call — and that pins the
# reachable outcomes:
#   - worker tool is MUTATING (apply_change) -> skipped_ambiguous (S1): the
#     once-and-only-once gate (scan.Mutating, step 3) fires FIRST, ahead of
#     everything else.
#   - worker tool is read-only (read_status) -> skipped_unsupported (S2):
#     scan.Mutating is empty, so the dangling DELEGATION (scan.Deferred,
#     step 3) decides — a control call slice-1 does not drive. The
#     sub-agent-authored trailing-event check (step 4) is a second gate to
#     the same outcome (neutralize-verified: S2 only goes green->red when
#     BOTH are disabled).
# The "resumed" Case-A read-only REPAIR needs a COORDINATOR-authored
# dangling call with no dangling delegation, which this topology never
# yields; it stays covered by cmd/mast/autoresume_test.go
# (TestAutoResumeRepairsDanglingReadOnly).
#
# DEGRADED to unit coverage (infeasible as deterministic shell legs, and
# documented in docs/uat-v0.2-plan.md):
#   - S4 503-during-drain: the drain closes the inject listener FIRST
#     (main.go beginDrain ordering), so a fresh connection is refused, not
#     503'd; the drain-gate 503 is reachable only over an already-open
#     keep-alive connection — not what a fresh curl makes.
#   - S9 restart-loop breaker: needs >=3 sequential process-crashing
#     continuations inside one attempt window with no deterministic forcing
#     hook; covered by TestAutoResumeLoopBreaker.

# ---- S8-mid-turn: /abort cancels a turn blocked inside a tool ---------
say "S8-mid-turn: /abort cancels an in-flight turn blocked inside a tool"
DB="${WORK}/s8m.db"
reset_blocker
start_toolactor "${WORK}/s8m.log"
inject_bg s8m ApplyChange
wait_started apply_change
assert_http "abort a blocked turn -> 202" \
  "$(post_code /abort '{"session_id":"incident-s8m","reason":"uat mid-turn abort"}')" 202
# Cancellation observable (distinct from S8's idle-session marker path): the
# abort writes its marker then cancelSession cancels the turn ctx, so the
# daemon abandons the blocked MCP call and the backgrounded inject unwinds
# PROMPTLY — it does NOT wait out the turn's budget. A bounded poll on the
# recorded HTTP code proves the turn was actually cancelled, not merely
# marked (neutralize cancelSession and this poll times out).
if wait_for 15 inject_done s8m; then
  ok "aborted turn unwound promptly (cancellation took effect)"
else
  bad "aborted turn did not unwind within budget (cancellation ineffective)"
fi
wait "${BGPID}" 2>/dev/null || true
assert_state "s8m aborted mid-turn" incident-s8m aborted
if wait_for 10 metric_has 'mast_aborts_total{workload="uat"} 1'; then
  ok "mid-turn abort counted once"
else
  bad "mid-turn abort not counted once"
fi
assert_http "inject on aborted s8m -> 409" "$(inject_code s8m)" 409
stop_term

# ---- S1: crash mid-mutation -> skipped_ambiguous -> operator ack ------
say "S1: crash mid-mutation -> auto-resume declines (skipped_ambiguous) -> ack"
DB="${WORK}/s1.db"
reset_blocker
start_toolactor "${WORK}/s1-boot.log"
inject_bg s1 ApplyChange
wait_started apply_change
# SIGTERM first: beginDrain durably marks the in-flight session interrupted
# BEFORE waiting on the (forever-blocked) turn. Do NOT reap — the daemon
# would otherwise wait out its whole budget on the wedged turn.
term_nowait
if wait_for 15 state_is incident-s1 interrupted; then
  ok "s1 marked interrupted by drain pre-mark"
else
  bad "s1 not marked interrupted within budget"
fi
# SIGKILL to skip the long drain wait: the marker is durable and the
# mutating apply_change call is dangling (intent persisted, response never
# recorded) — exactly the ambiguous-effect crash window.
kill9
wait "${BGPID}" 2>/dev/null || true
assert_state "s1 interrupted after crash" incident-s1 interrupted
# Restart on the same DB: the boot auto-resume scan must DECLINE the
# dangling mutation (the once-and-only-once backstop), never silently replay.
start_toolactor "${WORK}/s1-restart.log"
if wait_for 15 metric_has 'mast_autoresume_total{outcome="skipped_ambiguous",workload="uat"} 1'; then
  ok "auto-resume declined the dangling mutation (skipped_ambiguous)"
else
  bad "auto-resume did not record skipped_ambiguous"
fi
assert_state "s1 left interrupted for the operator" incident-s1 interrupted
# The operator's ambiguous-effect acknowledgement is accepted (202).
assert_http "ack-effects s1 -> 202" \
  "$(post_code /ack-effects '{"session_id":"incident-s1","reason":"uat operator ack"}')" 202
stop_term

# ---- S2: crash mid read-only -> skipped_unsupported (slice-1 scope) ---
say "S2: crash mid read-only (worker-authored) -> auto-resume out-of-scope skip"
DB="${WORK}/s2.db"
reset_blocker
start_toolactor "${WORK}/s2-boot.log"
inject_bg s2 ReadStatus
wait_started read_status
term_nowait
if wait_for 15 state_is incident-s2 interrupted; then
  ok "s2 marked interrupted by drain pre-mark"
else
  bad "s2 not marked interrupted within budget"
fi
kill9
wait "${BGPID}" 2>/dev/null || true
# Restart: the crash left the coordinator's delegation to uat-worker
# dangling (a control call slice-1 does not drive), so auto-resume declines
# the session as unsupported rather than repairing the read-only tool call
# under it — the runtime enforcement of the documented scope boundary.
# (Coordinator-authored read-only repair -> "resumed" is unit-covered; see
# the scope note above.)
start_toolactor "${WORK}/s2-restart.log"
if wait_for 15 metric_has 'mast_autoresume_total{outcome="skipped_unsupported",workload="uat"} 1'; then
  ok "auto-resume declined the dangling delegation (skipped_unsupported)"
else
  bad "auto-resume did not record skipped_unsupported"
fi
assert_state "s2 left interrupted (out of slice-1 scope)" incident-s2 interrupted
stop_term

# ---- S3: planned stop --pause-sessions gate-pauses; restart skips -----
say "S3: planned stop --pause-sessions gate-pauses an in-flight turn; restart skips it"
DB="${WORK}/s3.db"
reset_blocker
start_toolactor "${WORK}/s3-boot.log"
inject_bg s3 ApplyChange
wait_started apply_change
# Planned stop ACKs immediately and drains async, gate-pausing every session
# it marks (--pause-sessions makes the mark AND the pause travel together).
assert_http "mast stop --pause-sessions -> 0" "$(stop_pause_code)" 0
# Poll the DB directly (the draining daemon has closed its inject listener,
# so /metrics is gone): the durable gate pause is the observable, and paused
# outranks interrupted in the state ladder.
if wait_for 15 state_is incident-s3 paused; then
  ok "s3 gate-paused by planned stop"
else
  bad "s3 not gate-paused by planned stop within budget"
fi
# SIGKILL to skip the budget-long drain wait on the still-blocked turn; the
# durable pause + interruption markers are already on disk.
kill9
wait "${BGPID}" 2>/dev/null || true
assert_state "s3 paused after planned stop" incident-s3 paused
# Restart with auto-resume ON: candidates are interrupted-only and paused
# outranks interrupted, so the session is NOT a candidate -> it stays paused
# (handed back to the operator, never silently continued). Run at --log-level=
# debug so the zero-candidate boot pass emits its terminal "no interrupted
# sessions" line — the barrier below needs it (boot_scan_done).
LOGLEVEL=debug
start_toolactor "${WORK}/s3-restart.log"
LOGLEVEL=""
assert_state "s3 stays paused after restart (auto-resume skips it)" incident-s3 paused
# HAPPENS-AFTER BARRIER: the boot auto-resume scan runs in a goroutine the
# serving path does not await, so gate the stay-at-zero counter asserts on the
# scan having finished. Without this the scrape can win the race and read the
# still-primed 0 — false-greening the very paused-exclusion regression this leg
# exists to catch (a fast scrape would miss the skipped_stale increment).
if wait_for 15 boot_scan_done "${WORK}/s3-restart.log"; then
  ok "s3 restart boot auto-resume scan completed (counters now stable)"
else
  bad "s3 restart boot auto-resume scan did not complete within budget"
fi
# State-alone is confounded: a correctly-excluded paused session and one that
# was admitted-then-declined both render "paused" (paused outranks interrupted),
# so assert the per-leg auto-resume counters to tell them apart. A session that
# is correctly excluded is never a candidate, so NO outcome is recorded.
#
# The decisive counter is skipped_stale, NOT skipped_ambiguous. A gate-paused
# session projects as StatePaused with a ZERO InterruptedAt: project() returns
# at the paused branch before it reads the interrupt-time marker (see
# pkg/transcript project() precedence). So if the paused-exclusion in
# ScanInterrupted regressed and this session were fed to resumeOne, it would be
# declined at the freshness gate — interrupted_at is the zero time, older than
# the window — as skipped_stale; it never reaches the dangling-mutation
# (skipped_ambiguous) check. skipped_stale==0 is therefore what actually
# distinguishes "excluded" from "admitted-and-declined"; ambiguous/resumed==0
# are broader defense-in-depth. (That the boot scan runs at all is proven by
# S1/S2, which require a specific non-zero outcome on their own DBs.)
assert_metric "s3 not admitted to auto-resume (no stale decline)" 'mast_autoresume_total{outcome="skipped_stale",workload="uat"} 0'
assert_metric "s3 not declined ambiguous by auto-resume" 'mast_autoresume_total{outcome="skipped_ambiguous",workload="uat"} 0'
assert_metric "s3 not resumed by auto-resume" 'mast_autoresume_total{outcome="resumed",workload="uat"} 0'
stop_term

# ---- S4-exit3: drain expires on a wedged turn -> exit 3 --------------
# The exit-3 durability contract (#42): a SIGTERM whose drain window elapses
# with a turn still in flight freezes the survivors, cancels them, and exits
# 3 so restart=on-failure supervision revives the daemon for the boot repair
# pass. It needs drainBound < the turn's own ceiling. The main fixture sets
# budget.max_wallclock_seconds, which makes drainBound == that ceiling AND
# bounds the turn by it, so the turn self-cancels first -> exit 0. A
# BUDGET-FREE workload variant breaks the tie: no budget means an UNBOUNDED
# turn (cmd/mast/main.go:1361 installs the wallclock ceiling only when a
# budget is set) against the 30s default drain (defaultDrainBound,
# shutdown.go), so the drain window is what expires. Built at runtime under
# ${WORK} (house rule #5) by copying the fixture and dropping its budget.
say "S4-exit3: SIGTERM drain expires on a wedged turn -> exit 3"
NOBUDGET_WL="${WORK}/uat-nobudget"
mkdir -p "${NOBUDGET_WL}/specialists"
cp "${FIXTURE}/mcp.json" "${NOBUDGET_WL}/mcp.json"
cp "${FIXTURE}/specialists/uat-worker.tmpl" "${NOBUDGET_WL}/specialists/uat-worker.tmpl"
# Same fixture, minus the `budget:` block (grep out those two lines).
grep -v -e '^budget:' -e 'max_wallclock_seconds:' "${FIXTURE}/workload.yaml" \
  > "${NOBUDGET_WL}/workload.yaml"
DB="${WORK}/s4e3.db"
reset_blocker
WL="${NOBUDGET_WL}"
start_toolactor "${WORK}/s4e3.log"
WL=""
inject_bg s4e3 ApplyChange
wait_started apply_change
# SIGTERM + reap: with no budget the turn never self-cancels, so it is still
# wedged in apply_change when the ~30s drain window elapses -> freeze() +
# cancelTurns() -> errDrainExpired -> exit 3. stop_term's wait blocks the
# ~30s until the daemon exits; STOP_CODE carries the code. cancelTurns cancels
# the tool ctx, so the blocker unwinds and the marker is left durable.
stop_term
assert_http "drain-expired exit code" "${STOP_CODE}" 3
wait "${BGPID}" 2>/dev/null || true
# The cut-short session carries its interruption marker (the drain pre-marked
# it) — the honest record that boot-time auto-resume would act on next start.
assert_state "s4e3 interrupted (drain survivor)" incident-s4e3 interrupted

# ---- Usage spot check: exit 2 ---------------------------------------
say "Exit-code contract: bad flag -> exit 2 (usage)"
USAGE_CODE=0
"${BIN}" --nonexistent-flag >/dev/null 2>&1 || USAGE_CODE=$?
assert_http "usage exit code" "${USAGE_CODE}" 2

# ---- summary --------------------------------------------------------
say "Summary"
note "PASS=${PASS}  FAIL=${FAIL}"
note "logs + state under ${WORK}"
if [ "${FAIL}" -ne 0 ]; then
  echo "UAT FAILED" >&2
  exit 1
fi
echo "UAT PASSED"
