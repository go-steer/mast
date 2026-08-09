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
# abort, drain, restart, scrape) against the offline echo model, a real
# SQLite session DB, and asserts on session state, /metrics output, HTTP
# status, and process exit codes. Deterministic, credential-free (a
# fixed test bearer, no network), and CI-runnable in well under a minute.
#
# See docs/uat-v0.2-plan.md for the scenario catalogue, the assertion
# contract, and the "Blocking-tool prerequisite" note explaining which
# in-flight scenarios (S1/S2/S4-exit3/S8-mid-turn/S9-loopbreak) are
# DEFERRED until local/stdio MCP support lands — those need a controllable
# blocking registered tool, which the offline echo/scripted models cannot
# provide. This harness ships the scenarios that need no such tool.
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

PASS=0
FAIL=0

# ---- output ---------------------------------------------------------
say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
ok()   { PASS=$((PASS + 1)); printf '   \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { FAIL=$((FAIL + 1)); printf '   \033[31mFAIL\033[0m %s\n' "$*"; }

# ---- lifecycle ------------------------------------------------------
cleanup() { [ -n "${PID}" ] && kill "${PID}" 2>/dev/null || true; }
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
note "built ${BIN} (echo model — no credentials, no network)"

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
# Re-abort: the durable marker is idempotent (counter stays 1). NOTE: the
# /abort handler currently returns 500 on the ErrAlreadyAborted sentinel
# instead of an idempotent status (unlike A2A tasks/cancel) — a latent bug
# the UAT surfaced; see docs/uat-v0.2-plan.md. We assert the durable
# invariant (marker landed once), not the buggy HTTP status.
post_code /abort '{"session_id":"incident-s8","reason":"again"}' >/dev/null 2>&1 || true
assert_metric "re-abort marker idempotent" 'mast_aborts_total{workload="uat"} 1'

# ---- S9 (partial): metric cardinality — no session_id label ---------
say "S9: metric cardinality — no session id appears as a label"
assert_no_session_label "no session ids in /metrics" \
  incident-s6 incident-s7 incident-s7b incident-s8 incident-auth-ok

# ---- S4a: clean SIGTERM drain -> exit 0 -----------------------------
say "S4a: clean SIGTERM drain (no in-flight turn) -> exit 0"
stop_term
assert_http "clean drain exit code" "${STOP_CODE}" 0

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
