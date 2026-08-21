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

# uat-v0.5.sh — end-to-end acceptance pass for mast v0.5
# (docs/v0.5-plan.md). One subject:
#
#   an unattended monitoring cycle gathers its own facts, the gathering
#   costs nothing, and what those facts MEAN stays with the tool that
#   said it.
#
# Legs (all offline: no credentials, no network):
#
#   U-collect (W4.2) — the seam the rest of M4b sits on.
#       A  a scheduled cycle over a `bounded` workload with a
#          `monitor.collect` block: the collection call fires exactly
#          once, before the model is woken, and the whole cycle still
#          costs ONE model call — the same number scripts/uat-v0.4.sh's
#          U-bounded-cost/steps measured for a bounded incident with no
#          collection at all. The collection leg's own cost is the
#          difference between those two numbers, and the difference is
#          zero. The collected tool is declared MUTATING under
#          `on_mutation: require_approval`, so the second assertion is
#          that nothing parked: a model holding this tool would have
#          stopped the cycle for an operator at whatever hour the
#          cadence came due, which is what makes the leg mast's rather
#          than the model's in the first place.
#       B  the same tool reachable from a specialist as well: the daemon
#          REFUSES the roster at startup rather than serving it. Ungated
#          is only safe while it is unreachable — a tool mast runs on
#          its own behalf at the top of a cycle and that a specialist
#          can also call mid-turn makes "was this write approved?"
#          depend on which door it came through.
#
#   U-transitions (W4.4) — where the classification comes from.
#       A  the classifier reports `escalated` for a subject whose
#          severity mast can see did NOT change, and mast reports it as
#          escalated anyway. Plus a class this build has never heard of,
#          counted correctly by its own name. The leg fails the moment
#          anyone adds a local heuristic — a "sanity check" against the
#          severities would turn this cycle into nothing changed, and
#          the operator would never hear about the one finding that was
#          getting worse.
#       B  a classifier answer with no summary line — what a truncated
#          read produces — FAILS the cycle rather than reading as an
#          empty transition set. Empty is the wire for "all quiet", so a
#          lenient read here is a monitor reporting calm because its
#          collection broke.
#
# Row 9 of the parity board (docs/v0.3-plan.md §1) is what leg A flips,
# and it is deliberately runnable on its own: the row must not be
# implied by row 10's green, which is a different claim measured by a
# different script.
#
# The meters are the two an operator already scrapes — the daemon's
# `session_model_calls` log field (budget.Meter.Snapshot) and
# `mast_model_calls_total` on /metrics. Cost is read there and nowhere
# else: wallclock and token totals move with the model, the prompt and
# the machine, so a cost claim inferred from either is a claim about the
# afternoon it was measured. The blocker's own call ledger is the
# independent witness that the collection call happened at all.
#
# Usage: scripts/uat-v0.5.sh
# State goes under ${TMPDIR:-/tmp}/mast-uat-v05 (house rule #5); port 7791.

set -euo pipefail

PORT="${MAST_UAT_V05_PORT:-7791}"
BASE="http://127.0.0.1:${PORT}"
WORK="${TMPDIR:-/tmp}/mast-uat-v05"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${WORK}/mast"
FIXTURE="${REPO}/testdata/uat"
TOKEN="uat-v05-token"
export MAST_INJECT_TOKEN="${TOKEN}"
PID=""
DB=""

PASS=0
FAIL=0

# ---- output ---------------------------------------------------------
say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
ok()   { PASS=$((PASS + 1)); printf '   \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { FAIL=$((FAIL + 1)); printf '   \033[31mFAIL\033[0m %s\n' "$*"; }

cleanup() {
  [ -n "${PID}" ] && kill "${PID}" 2>/dev/null || true
  return 0
}
trap cleanup EXIT

# ---- the blocker ----------------------------------------------------
# The same stdio MCP server the v0.2, v0.3 and v0.4 harnesses drive. It
# is the only offline tool that records the arguments it was actually
# called with, which is what turns "the daemon logged that it collected"
# into "the tool was called".
BLOCKER="${WORK}/blocker"
BLOCKDIR="${WORK}/blockdir"
export MAST_UAT_BLOCKER="${BLOCKER}"
export UAT_BLOCKER_DIR="${BLOCKDIR}"

reset_blocker() {
  rm -rf "${BLOCKDIR}"
  mkdir -p "${BLOCKDIR}"
  : > "${BLOCKDIR}/apply_change.release"
  : > "${BLOCKDIR}/read_status.release"
  : > "${BLOCKDIR}/findings_diff.release"
}

calls_count() {
  local f="${BLOCKDIR}/$1.calls"
  if [ -f "${f}" ]; then wc -l < "${f}" | tr -d ' '; else echo 0; fi
}

# ---- lifecycle ------------------------------------------------------
# --model=toolactor rather than echo, for the reason the v0.4 harness
# records: wireMCPToolsets skips MCP entirely under the echo model, so
# an echo daemon would have no tool schemas to resolve and the
# collection call would fail closed on every leg — red for the wrong
# reason, and green for the wrong reason on the refusal leg.
DISPATCH=coordinator
start_daemon() {
  local log="$1"; shift
  "${BIN}" --workload="${WL}" --dispatch="${DISPATCH}" \
    --listen=":${PORT}" --model=toolactor --session-db="${DB}" \
    --log-level=info "$@" >"${log}" 2>&1 &
  PID=$!
  local i
  for i in $(seq 1 100); do
    if ! kill -0 "${PID}" 2>/dev/null; then break; fi
    if curl -sf -m 1 "${BASE}/" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  echo "daemon failed to start; log:" >&2; cat "${log}" >&2; exit 1
}

# start_refused <logfile> <workload> <dispatch> — run the daemon
# expecting it to REFUSE the roster and exit. Returns 0 when it did. A
# daemon that comes up instead is killed and the caller reports a
# failure: the value of a build-time refusal is that it lands before the
# workload is serving, so "it errored later" is not the same result.
start_refused() {
  local log="$1" wl="$2" dispatch="$3" i rc=0
  "${BIN}" --workload="${wl}" --dispatch="${dispatch}" \
    --listen=":${PORT}" --model=toolactor --session-db="${DB}" \
    --log-level=info >"${log}" 2>&1 &
  local pid=$!
  for ((i = 0; i < 200; i++)); do
    if ! kill -0 "${pid}" 2>/dev/null; then
      wait "${pid}" || rc=$?
      [ "${rc}" -ne 0 ] && return 0
      return 1
    fi
    sleep 0.1
  done
  kill -9 "${pid}" 2>/dev/null || true
  wait "${pid}" 2>/dev/null || true
  return 1
}

stop_term() {
  kill -TERM "${PID}" 2>/dev/null || true
  wait "${PID}" 2>/dev/null || true
  PID=""
}

# ---- readers --------------------------------------------------------
# show_field <session-id> <label> — one labelled line from `mast sessions
# show`. awk reads to EOF rather than exiting early: an early exit
# SIGPIPEs the CLI, which under pipefail kills the run with 141.
show_field() {
  "${BIN}" sessions show "$1" --session-db="${DB}" 2>/dev/null \
    | awk -v k="$2:" '$1 == k && !seen { sub(/^[[:space:]]*[^:]*:[[:space:]]*/, ""); print; seen = 1 }'
}

state_is() { [ "$(show_field "$1" State)" = "$2" ]; }

# model_calls <logfile> — the meter's own count for the last completed
# turn, off the daemon's `turn complete` record.
model_calls() {
  grep -o '"session_model_calls":[0-9]*' "$1" | tail -n 1 | cut -d: -f2
}

# metric_value <metric-with-labels> — one sample from the daemon's
# /metrics, exactly as an operator's scrape would read it. The second,
# independently computed view of the same number.
metric_value() {
  curl -sf -m 5 "${BASE}/metrics" 2>/dev/null | awk -v k="$1" '$1 == k { print $2 }'
}

# log_field <logfile> <message> <field> — one field off the FIRST JSON
# log line carrying <message>.
log_field() {
  { grep -F -- "$2" "$1" || true; } \
    | sed -n "1s/.*\"$3\":\"\{0,1\}\([^\",}]*\).*/\1/p"
}

sched_fires_atleast() {
  local n
  n="$(grep -c -- 'scheduled trigger fired' "$1" || true)"
  [ "${n}" -ge "$2" ]
}

wait_for() {
  local budget="$1"; shift
  local deadline=$((budget * 10)) i
  for ((i = 0; i < deadline; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  return 1
}

# ---- assertions -----------------------------------------------------
assert_eq() {
  if [ "$2" = "$3" ]; then ok "$1 (${2})"; else bad "$1 (got '${2:-<none>}', want '$3')"; fi
}

# Here-strings, not `printf ... | grep -Fq`: this script runs under
# `pipefail`, `grep -q` exits on its first match, and the writer's
# resulting SIGPIPE (141) is then promoted over grep's 0 — so a match
# can read as a miss (PR #196).
assert_has() {
  if grep -Fq -- "$3" <<<"$2"; then ok "$1"; else bad "$1 — missing: $3"; fi
}

assert_state() {
  if wait_for 30 state_is "$2" "$3"; then
    ok "$1 (state=$3)"
  else
    bad "$1 (state=$(show_field "$2" State), want $3)"
  fi
}

assert_no_log() {
  if grep -q -- "$3" "$2"; then bad "$1 — log contains: $3"; else ok "$1"; fi
}

# ====================================================================
# fixtures
# ====================================================================
rm -rf "${WORK}" && mkdir -p "${WORK}"

say "Build"
(cd "${REPO}" && go build -o "${BIN}" ./cmd/mast)
(cd "${REPO}" && go build -o "${BLOCKER}" ./testdata/uat/blocker)
note "built ${BIN} + ${BLOCKER} (toolactor model — no credentials, no network)"

# ---- the collection fixture: bounded-triage, on a cadence -----------
# The shipped bounded-triage bundle plus a monitor block, because the
# claim needs both halves in one workload: `bounded` is what makes the
# model's own cost a constant to compare against, and the monitor block
# is what has to add nothing to it. Its specialist is SingleTurn and
# holds no tools at all, which is why this roster passes the collection
# fence — it is the clearest form of the whole idea.
#
# read_status is declared MUTATING here, which it is not, and that is
# the point rather than a fixture bug. The tool a real monitor collects
# from — a run-to-run finding diff — advances persisted state as a side
# effect of answering, so it is mutating and is right to be; and mast's
# predicate defaults every un-annotated MCP tool to mutating anyway.
# Declaring it mutating under on_mutation: require_approval is what
# makes leg A's second claim testable: the cycle must NOT park.
BCOLLECT="${WORK}/bounded-collect"
cp -r "${REPO}/examples/workloads/bounded-triage" "${BCOLLECT}"
cp "${FIXTURE}/mcp.json" "${BCOLLECT}/mcp.json"
cat > "${BCOLLECT}/workload.yaml" <<'YAML'
# Harness fixture for scripts/uat-v0.5.sh's U-collect/A leg.
# examples/workloads/bounded-triage with a monitor block and a cadence.
name: uat-bounded-collect
description: Fixture workload for the mast v0.5 zero-token collection leg.
mode: single_session

dispatch: bounded

tool_catalog:
  mcp:
    - server: uat-blocker
  tools:
    - name: read_status
      mutating: true

specialists:
  - incident-report

budget:
  max_wallclock_seconds: 120

hitl:
  # Spelled out because it is what the leg varies against: under this
  # policy a model holding read_status would park every fire.
  on_mutation: require_approval

monitor:
  collect:
    - tool: read_status
      as: status

edge_trigger:
  scheduled:
    # Long enough that one fire lands and the assertions finish well
    # before the next tick, so the counters below are a single cycle's
    # and not a race with the second.
    interval: 10s
    jitter: 0s
    prompt: 'A monitoring cycle woke you: {"reason":"CrashLoopBackOff"}. Report on what was gathered.'
YAML

# ---- the refusal fixture: the same tool behind both doors -----------
# testdata/uat's roster, whose uat-worker already names read_status in
# its own allowlist, plus a monitor block that collects it. One tool,
# two doors — the thing the fence exists to make impossible. The cadence
# is here because the loader requires one: a collect block with nothing
# to run it is refused earlier, by a different check, and this leg is
# about the composition-time one.
BCREFUSE="${WORK}/collect-refuse"
cp -r "${FIXTURE}" "${BCREFUSE}"
cat > "${BCREFUSE}/workload.yaml" <<'YAML'
# Harness fixture for scripts/uat-v0.5.sh's U-collect/B leg.
name: uat-collect-refuse
description: Fixture workload for the mast v0.5 collection-fence refusal.
mode: single_session

tool_catalog:
  mcp:
    - server: uat-blocker
  tools:
    - name: read_status
      mutating: false
    - name: apply_change
      mutating: true

specialists:
  - uat-worker

budget:
  max_wallclock_seconds: 300

hitl:
  on_mutation: apply

monitor:
  collect:
    - tool: read_status

edge_trigger:
  http:
    path: /inject
    auth: bearer
  scheduled:
    interval: 3s
    jitter: 0s
    prompt: 'Sweep the fixture cluster: {"reason":"ReadStatus"} and remediate what you find.'
YAML

# ---- the classification fixture: a cycle that knows what changed ----
# Same bounded shape as the collection fixture, plus the leg W4.4 adds:
# one of the collected results is named as the run-to-run
# classification, so mast parses it instead of passing the text through.
#
# findings_diff is the blocker's lookout-shaped tool — TEXT records, one
# per line, terminated by a mandatory summary. It is registered
# low-level in the fixture precisely so it answers the way a real
# classifier does; a stub that answered in structured JSON would
# exercise a path production never takes.
BTRANS="${WORK}/transitions"
cp -r "${REPO}/examples/workloads/bounded-triage" "${BTRANS}"
cp "${FIXTURE}/mcp.json" "${BTRANS}/mcp.json"
cat > "${BTRANS}/workload.yaml" <<'YAML'
# Harness fixture for scripts/uat-v0.5.sh's U-transitions legs.
name: uat-transitions
description: Fixture workload for the mast v0.5 transition-classification legs.
mode: single_session

dispatch: bounded

tool_catalog:
  mcp:
    - server: uat-blocker
  tools:
    - name: read_status
      mutating: true
    - name: findings_diff
      # Mutating, like the real thing: a run-to-run diff advances the
      # state it compares against as a side effect of answering. This is
      # the whole reason the call is mast's and not the model's.
      mutating: true

specialists:
  - incident-report

budget:
  max_wallclock_seconds: 120

hitl:
  on_mutation: require_approval

monitor:
  collect:
    - tool: read_status
      as: status
    - tool: findings_diff
      args: {transitions: "new,escalated,resolved"}
      as: transitions
  # The half W4.4 adds. Without it the records would ride to the model
  # as a wall of text and mast would know nothing about whether anything
  # changed — which is the question W4.5 notifies on.
  transitions_from: transitions

edge_trigger:
  scheduled:
    interval: 10s
    jitter: 0s
    prompt: 'A monitoring cycle woke you: {"reason":"CrashLoopBackOff"}. Report on what changed.'
YAML

# ====================================================================
# U-collect (W4.2) — the facts are mast's, and they cost nothing
# ====================================================================

# ---- leg A: zero-token collection -----------------------------------
# uat-v0.4.sh's U-bounded-cost/steps says a cycle's REASONING is one
# model call. This says the cycle's FACTS cost none at all, because mast
# gathers them itself before the model is woken.
say "U-collect/A: a monitoring cycle gathers its facts for zero model calls"
WL="${BCOLLECT}"
DISPATCH=
DB="${WORK}/collect.db"
LOG="${WORK}/collect.log"
reset_blocker
start_daemon "${LOG}"

# Armed before anything fires, and it says which calls: an operator's
# first question about a monitor is what it is going to run.
BCARM="$(grep -- 'monitoring cycle armed' "${LOG}" || true)"
assert_has "the daemon armed the collection leg at startup" "${BCARM}" 'read_status'

if wait_for 60 sched_fires_atleast "${LOG}" 1; then
  ok "the cadence fired with nothing calling it"
else
  bad "the cadence never fired"
fi
BCSID="$(log_field "${LOG}" 'scheduled trigger fired' session)"
assert_state "the cycle finished on its own" "${BCSID}" idle

# It ran, once, before the model. The blocker's ledger is the
# independent witness: the daemon's own log line says it collected, and
# the tool's ledger says it was called.
assert_eq "the collection call fired exactly once" "$(calls_count read_status)" 1
BCCOL="$(grep -- 'collected before waking the model' "${LOG}" || true)"
assert_has "the daemon says what it gathered" "${BCCOL}" 'status'
assert_has "and which cycle it gathered it for" "${BCCOL}" "${BCSID}"

# THE ASSERTION THE WORKSTREAM EXISTS FOR. One fire, one model call —
# the same number U-bounded-cost/steps measured for a bounded incident
# with no collection at all. The collection leg's cost is the
# difference, and the difference is zero.
assert_eq "the whole cycle cost one model call" "$(model_calls "${LOG}")" 1
assert_eq "and the exported counter agrees" \
  "$(metric_value 'mast_model_calls_total{workload="uat-bounded-collect"}')" 1

# The second claim, and the reason the collection leg is mast's at all.
# read_status is declared mutating and the workload's policy is
# require_approval, so a model holding this tool would have parked the
# cycle for an operator — at whatever hour the cadence came due. Nothing
# parked, because no model asked for anything.
assert_no_log "nothing parked for an operator" "${LOG}" 'HITL PAUSE'
# The gate is REGISTERED — the bundle asked for it and mast obliged —
# and it adjudicated nothing, because nothing came through the door it
# watches. Both halves matter: a leg that passed because the gate was
# absent would be proving the wrong thing.
assert_has "the write gate was in force" "$(grep -- 'write gate registered' "${LOG}" || true)" 'require_approval'
assert_no_log "and it was never asked to adjudicate" "${LOG}" '"msg":"write gate"'
stop_term

# ---- leg B: the exception is fenced, and the fence is startup -------
say "U-collect/B: a roster that can also reach the collect tool will not start"
DB="${WORK}/cfence.db"
LOG="${WORK}/cfence.log"
if start_refused "${LOG}" "${BCREFUSE}" coordinator; then
  ok "the daemon refused the roster instead of serving it"
else
  bad "the daemon came up with read_status reachable through both doors"
fi
BCREF="$(grep -- 'failed to construct root agent' "${LOG}" || true)"
[ -n "${BCREF}" ] || note "no refusal logged; last line was: $(tail -n 1 "${LOG}")"
assert_has "the refusal names the specialist" "${BCREF}" 'uat-worker'
assert_has "and the tool both doors reach" "${BCREF}" 'read_status'
assert_has "and says the collection leg is why" "${BCREF}" 'on its own behalf'
# Credential-free, like uat-v0.4.sh's /steps refusal and for the same
# reason: the check runs in compose.CheckRoster, ahead of any MCP
# wiring, so an operator without credentials reads the roster problem
# rather than a 403 standing in for it.
assert_no_log "it refused before wiring MCP" "${LOG}" 'MCP toolset wired'

# ====================================================================
# U-transitions (W4.4) — the classification is the classifier's
# ====================================================================

# ---- leg A: mast reports what it was told, not what it can see ------
# The stub reports `escalated` for a subject whose severity did NOT
# change — same severity as last time, open since yesterday. Every field
# mast can see argues the other way, and mast reports it as escalated
# anyway, because the classification belongs to the tool that made it: a
# real classifier escalates on burn rate, repeat count or a policy window
# mast has no view of.
#
# This is the leg that fails if anyone ever adds a local heuristic. A
# "sanity check" against the severities would quietly turn this cycle
# into nothing changed, and the operator would never hear about the one
# finding that was getting worse.
say "U-transitions/A: an escalation mast cannot corroborate is still an escalation"
WL="${BTRANS}"
DISPATCH=
DB="${WORK}/trans.db"
LOG="${WORK}/trans.log"
reset_blocker
cat > "${BLOCKDIR}/findings_diff.out" <<'DIFF'
transition=escalated subject_key=pod/prod/api-7d9/Unhealthy severity=warning prev_severity=warning first_seen=2026-08-20T09:00:00Z message="probe failing since yesterday"
transition=new subject_key=pod/prod/web-2f1/CrashLoopBackOff severity=critical message="back-off 5m0s restarting failed container"
transition=quiesced subject_key=node/gke-pool-3 reason=Drained
scanned=412 findings=3 elapsed=1.9s
DIFF
start_daemon "${LOG}"

# The armed line names the source, because which result gets parsed
# changes what a failed cycle means.
TRARM="$(grep -- 'monitoring cycle armed' "${LOG}" || true)"
assert_has "the daemon armed the classification source" "${TRARM}" 'transitions_from'
assert_has "and named which collected result it is" "${TRARM}" 'transitions'

if wait_for 60 sched_fires_atleast "${LOG}" 1; then
  ok "the cadence fired"
else
  bad "the cadence never fired"
fi
TRSID="$(log_field "${LOG}" 'scheduled trigger fired' session)"
assert_state "the cycle finished on its own" "${TRSID}" idle
assert_eq "the classifier was called once" "$(calls_count findings_diff)" 1

TRCLS="$(grep -- 'classified what changed' "${LOG}" || true)"
[ -n "${TRCLS}" ] || note "no classification line; last line was: $(tail -n 1 "${LOG}")"
# THE ASSERTION THIS LEG EXISTS FOR.
assert_has "the escalation survived a mast that could not corroborate it" "${TRCLS}" '"escalated=1"'
assert_has "the new finding is reported as new" "${TRCLS}" '"new=1"'
# A class this build has never heard of is a class the classifier
# shipped, not an error: it is counted, by its own name, with no mast
# release in between.
assert_has "a class mast has no vocabulary for is counted anyway" "${TRCLS}" '"quiesced=1"'
assert_has "the tally covers every record" "${TRCLS}" '"transitions":3'
# `scanned` is the difference between "quiet" and "not looking". A cycle
# with nothing changed and nothing scanned is a broken monitor, and
# without this number the two read the same.
assert_has "and says how much was looked at to find them" "${TRCLS}" '"scanned":412'
assert_has "the cycle it belongs to is named" "${TRCLS}" "${TRSID}"

# Still one model call. Classification is collection's second half, not
# a second turn: nothing here asked the model what changed.
assert_eq "classifying cost no model calls of its own" "$(model_calls "${LOG}")" 1
assert_no_log "and nothing parked for an operator" "${LOG}" 'HITL PAUSE'
stop_term

# ---- leg B: a truncated classifier is not a quiet cluster -----------
# The stub answers with records and no summary line — what a killed
# process, a truncated read or a half-written pipe produces. Read
# leniently that is an empty transition set, and an empty transition set
# is the wire for "all quiet": the cycle would report calm and W4.5 would
# decline to notify anyone. So the fire FAILS, before the model is woken.
say "U-transitions/B: a truncated classification fails the cycle instead of reading as calm"
WL="${BTRANS}"
DISPATCH=
DB="${WORK}/transbad.db"
LOG="${WORK}/transbad.log"
reset_blocker
cat > "${BLOCKDIR}/findings_diff.out" <<'DIFF'
transition=new subject_key=pod/prod/api-7d9/CrashLoopBackOff severity=critical
DIFF
start_daemon "${LOG}"

if wait_for 60 grep -q 'scheduled fire failed' "${LOG}"; then
  ok "the cycle failed rather than waking the model with a hole in it"
else
  bad "the cycle did not fail on a classification with no summary line"
fi
TRBAD="$(grep -- 'scheduled fire failed' "${LOG}" || true)"
assert_has "the failure names the bundle key" "${TRBAD}" 'transitions_from'
assert_has "and says what was wrong with the bytes" "${TRBAD}" 'summary line'
# No model call was spent on a question mast could not gather the facts
# for. The collection leg runs before runTurnPre, so a failure here is
# free — which is what makes failing closed affordable at 3am.
# The counter is registered at startup, so it reads zero rather than
# going missing — which is the stronger evidence: the meter exists, an
# operator is scraping it, and it has counted nothing.
assert_eq "no model call was spent on the failed cycle" \
  "$(metric_value 'mast_model_calls_total{workload="uat-transitions"}')" 0
assert_no_log "and no turn was recorded as complete" "${LOG}" 'turn complete'
# The classifier itself still ran: the failure is in reading the answer,
# not in getting one, and an operator debugging this needs to know the
# call happened.
assert_eq "the classifier was called" "$(calls_count findings_diff)" 1
stop_term

# ====================================================================
say "Summary"
printf '   %d passed, %d failed\n' "${PASS}" "${FAIL}"
if [ "${FAIL}" -gt 0 ]; then exit 1; fi
