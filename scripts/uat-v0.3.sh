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

# uat-v0.3.sh — end-to-end acceptance pass for the mast v0.3 parity work
# (docs/v0.3-plan.md §2, tier U). Where uat-v0.2.sh drives a purpose-built
# fixture through the durable-execution spine, this harness drives the
# SHIPPED anchor workload — examples/workloads/gke-triage, the bundle a
# reader of the README actually runs — and asserts on what an operator
# sees at the end of it.
#
# Legs (all offline: --model=echo, no credentials, no network):
#
#   U-report (W1.4) — a Task specialist that declares `output_schema:`
#     emits a report that satisfies that shared JSON-Schema asset, end to
#     end, and the run reaches an operator with the report intact. Three
#     legs, because a single green leg would be a claim about the fake
#     rather than about mast:
#       A  shipped roster, schema declared -> every declared property is
#          present and the enum member is valid
#       B  same roster with `output_schema:` removed -> ADK's default
#          single-`result` wrapper instead, so leg A's assertions are
#          shown to discriminate
#       C  MAST_FAKE_SCHEMA_VIOLATION=1 -> the violating call is REFUSED
#          and the run ends with no report, so the schema is shown to be
#          enforced rather than merely declared
#
#   U-gate-percall / U-gate-crash / U-gate-scopes (W2.1-W2.3) — a
#     mutating tool call stops before it fires, waits for an operator
#     across a process death, and runs exactly once when approved.
#     Driven over the v0.2 fixture (testdata/uat) with --model=toolactor
#     and its stdio MCP blocker, because that is the only offline roster
#     with a REAL registered mutating tool:
#       percall/A  under the DEFAULT policy the call parks, the tool has
#                  not run, and the parked question names it; approving
#                  runs it exactly once
#       percall/B  the same fixture with on_mutation=apply -> the call
#                  fires with no gate, so leg A's park is shown to be a
#                  property of the gate and not of the harness
#       percall/C  a read-only call under the same policy is not gated
#       percall/D  reject -> the tool never runs at all
#       crash      park, SIGKILL the daemon, restart: the question is
#                  still there, and approving it against the FRESH
#                  process runs the call exactly once (rows 4, 5, L1)
#       scopes     an approval that asks for more than this one call is
#                  refused, and the tool does not run (W2.3)
#
#   U-gate-structural (W2.4) — which specialists can reach a mutating
#     tool at all. Over the shipped roster and one derived from it:
#       A  the shipped roster starts and its startup log names the one
#          specialist that declares write capability, and its tools
#       B  the same roster with patch_k8s_resource handed back to a
#          diagnoser fails to start, naming the specialist and the tool
#       C  the same derived roster with `capability: change_executor`
#          on that diagnoser starts, so leg B is shown to refuse the
#          undeclared write rather than the tool
#
#   U-fanout (W3) — the roster runs concurrently over one incident and
#     the merged report gates ONCE, after synthesis. Driven over
#     examples/workloads/ns-audit, the read-only bundle fan-out needs:
#       A  four analysts run, each on its own branch, and the merged
#          report parks on the single approve-_synthesis gate; approving
#          finishes the run without re-running any of them
#       B  MAST_FAKE_SCHEMA_VIOLATION=1 -> every branch goes silent, so
#          leg A's "4 of 4" is shown to be a count and not a constant,
#          and a report nobody contributed to does not gate at all
#       C  the SHIPPED gke-triage roster under --dispatch=fanout is
#          refused at construction, because its change executor holds
#          patch_k8s_resource and every branch runs before the gate
#
# The observation point is `mast sessions show`: with `--dispatch=graph`
# and the roster's `hitl.require_approval`, each specialist result is
# parked on a durable RequestInput interrupt whose message quotes the
# result verbatim (pkg/graph/graph.go). That needs no new surface, no
# listener and no SSE parsing — the report is read back out of SQLite by
# the same CLI an operator would use.
#
# Usage: scripts/uat-v0.3.sh
# State goes under ${TMPDIR:-/tmp}/mast-uat-v03 (house rule #5); port 7789.

set -euo pipefail

PORT="${MAST_UAT_V03_PORT:-7789}"
BASE="http://127.0.0.1:${PORT}"
WORK="${TMPDIR:-/tmp}/mast-uat-v03"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${WORK}/mast"
# The SHIPPED anchor workload, not a fixture. Driving the real bundle is
# the whole point: the defect this harness was written to catch (a
# specialist's `output_schema:` refusing every report the offline fake
# could produce, W1.3 + W1.2 together) was invisible precisely because
# nothing exercised the shipped roster end to end.
WORKLOAD="${REPO}/examples/workloads/gke-triage"
TOKEN="uat-v03-token"
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
cleanup() {
  [ -n "${PID}" ] && kill "${PID}" 2>/dev/null || true
  return 0
}
trap cleanup EXIT

# start_daemon <logfile> — launch the daemon over ${WL} under ${DISPATCH}
# and spin until it answers. Graph dispatch is the roster's classifier ->
# per-failure-mode specialist -> approval gate path; fanout dispatch runs
# the whole roster concurrently and gates once, after synthesis.
DISPATCH=graph
start_daemon() {
  local log="$1"; shift
  "${BIN}" --workload="${WL}" --dispatch="${DISPATCH}" \
    --listen=":${PORT}" --model=echo --session-db="${DB}" \
    --log-level=info "$@" >"${log}" 2>&1 &
  PID=$!
  local i
  for i in $(seq 1 100); do
    # Liveness first: if OUR process already died (e.g. a port-bind
    # failure because a stale daemon holds ${PORT}), bail now rather than
    # mistaking the stale daemon's response for our fresh boot.
    if ! kill -0 "${PID}" 2>/dev/null; then break; fi
    if curl -sf -m 1 "${BASE}/" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  echo "daemon failed to start; log:" >&2; cat "${log}" >&2; exit 1
}

stop_term() {
  kill -TERM "${PID}" 2>/dev/null || true
  wait "${PID}" 2>/dev/null || true
  PID=""
}

# ---- drivers --------------------------------------------------------
# inject_code <uid> <reason> — POST an edge event; echo the HTTP status.
# inject is synchronous (the turn runs on the request context), so a 202
# means the turn has already reached its parked or finished end.
inject_code() {
  curl -s -m 60 -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/inject" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"kind\":\"Event\",\"reason\":\"$2\",\"namespace\":\"prod\",\"name\":\"api-$1\",\"uid\":\"$1\",\"message\":\"uat\",\"cluster\":\"uat\"}"
}

# inject_audit <uid> — POST the namespace-audit envelope the ns-audit
# bundle listens for. Fan-out has no classifier: every analyst gets the
# same incident, so the envelope carries no failure mode.
inject_audit() {
  curl -s -m 60 -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/inject" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"kind\":\"Namespace\",\"reason\":\"AuditRequested\",\"namespace\":\"prod\",\"name\":\"prod\",\"uid\":\"$1\",\"message\":\"uat\",\"cluster\":\"uat\"}"
}

# resume_code <session-id> <interrupt-id> — answer a parked gate through
# the daemon's HTTP surface, the way an operator (or mast-web) does.
resume_code() {
  curl -s -m 60 -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/resume" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"session_id\":\"$1\",\"interrupt_id\":\"$2\",\"response\":{\"approved\":true,\"note\":\"uat\"}}"
}

# resume_verdict <session-id> <interrupt-id> <response-json> — answer a
# parked WRITE GATE through the same HTTP surface an operator uses. A
# confirmation park takes an operator verdict, not the {"approved":...}
# answer a RequestInput park takes; mast reads the pause kind out of the
# durable log and sends whichever wire shape ADK matches on, so the
# client sends the same request either way.
resume_verdict() {
  curl -s -m 60 -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/resume" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"session_id\":\"$1\",\"interrupt_id\":\"$2\",\"response\":$3}"
}

# show_field <session-id> <label> — the value of one labelled line from
# `mast sessions show` (State, Interrupt, Message, ...). Read from the DB
# with no daemon required. Matched on the label rather than a line
# number, so a layout change cannot silently return the wrong field.
#
# awk reads to EOF rather than `exit`-ing on the first match: exiting
# early SIGPIPEs the CLI, and under `set -o pipefail` that kills the
# whole run with 141 the moment a matched label is not the last line of
# the output. First match wins instead.
show_field() {
  "${BIN}" sessions show "$1" --session-db="${DB}" 2>/dev/null \
    | awk -v k="$2:" '$1 == k && !seen { sub(/^[[:space:]]*[^:]*:[[:space:]]*/, ""); print; seen = 1 }'
}

state_is() { [ "$(show_field "$1" State)" = "$2" ]; }

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

# ---- write-gate drivers (toolactor + the stdio blocker) -------------
# The gate legs need a REAL registered mutating tool: a gate that parks a
# tool nothing would have executed proves nothing about execution. The
# v0.2 fixture already has one (testdata/uat/blocker's apply_change,
# classified mutating by the fixture's tool_catalog), driven by
# --model=toolactor from the inject reason. Reusing it keeps the gate
# legs offline and credential-free.
BLOCKER="${WORK}/blocker"
BLOCKDIR="${WORK}/blockdir"
export MAST_UAT_BLOCKER="${BLOCKER}"
export UAT_BLOCKER_DIR="${BLOCKDIR}"

# start_gate_daemon <logfile> — the daemon over ${WL} under the toolactor
# fake. Coordinator dispatch, matching the fixture's shape in uat-v0.2.sh.
start_gate_daemon() {
  local log="$1"; shift
  mkdir -p "${BLOCKDIR}"
  "${BIN}" --workload="${WL}" --dispatch=coordinator \
    --listen=":${PORT}" --model=toolactor --session-db="${DB}" \
    --log-level=info "$@" >"${log}" 2>&1 &
  PID=$!
  local i
  for i in $(seq 1 100); do
    if ! kill -0 "${PID}" 2>/dev/null; then break; fi
    if curl -sf -m 1 "${BASE}/" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  echo "toolactor daemon failed to start; log:" >&2; cat "${log}" >&2; exit 1
}

# inject_uat <uid> <reason> — POST the fixture's edge event. The reason
# selects the tool the worker drives: "Apply..." -> apply_change
# (mutating), "Read..." -> read_status (read-only).
inject_uat() {
  curl -s -m 60 -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/inject" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"kind\":\"uat-event\",\"reason\":\"$2\",\"namespace\":\"default\",\"name\":\"pod-$1\",\"uid\":\"$1\",\"message\":\"uat\",\"cluster\":\"uat\"}"
}

# reset_blocker — clear every marker between legs, so a stale one cannot
# satisfy a later assertion. Includes the .calls ledger: a leg that
# counted the previous leg's executions would be worse than no count.
reset_blocker() {
  rm -rf "${BLOCKDIR}"
  mkdir -p "${BLOCKDIR}"
  # Pre-release both tools. These legs are about what happens BEFORE a
  # call fires, so a call that does fire should return immediately
  # rather than block (the blocking behaviour is uat-v0.2.sh's subject).
  : > "${BLOCKDIR}/apply_change.release"
  : > "${BLOCKDIR}/read_status.release"
}

# calls_count <tool> — how many times the blocker actually ENTERED the
# tool handler (it appends a line per entry). This is the execution
# count the gate's whole claim rests on.
calls_count() {
  local f="${BLOCKDIR}/$1.calls"
  if [ -f "${f}" ]; then wc -l < "${f}" | tr -d ' '; else echo 0; fi
}

# calls_args <tool> — the ledger's lines, which name the ARGUMENTS each
# entry was called with. The edit legs rest on this: "the tool ran once"
# is satisfied equally by the model's call and the operator's, and only
# the recorded arguments tell them apart.
calls_args() {
  local f="${BLOCKDIR}/$1.calls"
  if [ -f "${f}" ]; then cat "${f}"; fi
}

# kill9 — SIGKILL the daemon and reap it. Nothing is drained and nothing
# is flushed; only what is already durable survives.
kill9() {
  [ -n "${PID}" ] && kill -9 "${PID}" 2>/dev/null || true
  [ -n "${PID}" ] && wait "${PID}" 2>/dev/null || true
  PID=""
}

# ---- assertions -----------------------------------------------------
assert_eq() {
  if [ "$2" = "$3" ]; then ok "$1 (${2})"; else bad "$1 (got '${2:-<none>}', want '$3')"; fi
}

assert_http() {
  if [ "$2" = "$3" ]; then ok "$1 (HTTP $2)"; else bad "$1 (HTTP $2, want $3)"; fi
}

# assert_has <label> <haystack> <needle> — literal substring must appear.
#
# Here-string rather than `printf ... | grep -Fq`, for the reason show_field
# already documents above: `grep -q` exits on its first match, the writer
# then takes SIGPIPE, and `pipefail` promotes that 141 over grep's own 0.
# Piped, a match can read as a miss. assert_hasnt is the dangerous
# direction — there a matched needle is the violation, so the SIGPIPE turns
# a detected violation into `ok`, silently. Not a pipeline, no hazard.
assert_has() {
  if grep -Fq -- "$3" <<<"$2"; then ok "$1"; else bad "$1 — missing: $3"; fi
}

# assert_hasnt <label> <haystack> <needle> — literal substring must not.
# Fails CLOSED on an empty haystack: absence proves nothing when there is
# nothing to search.
assert_hasnt() {
  if [ -z "$2" ]; then bad "$1 — nothing to search (empty)"; return 0; fi
  if grep -Fq -- "$3" <<<"$2"; then bad "$1 — present: $3"; else ok "$1"; fi
}

# assert_state <label> <session-id> <want>
assert_state() {
  if wait_for 20 state_is "$2" "$3"; then
    ok "$1 (state=$3)"
  else
    bad "$1 (state=$(show_field "$2" State), want $3)"
  fi
}

# assert_log_count <label> <logfile> <pattern> <want> — how many times a
# line matched in the daemon log. The report legs turn on this: a
# specialist that gets its report refused RETRIES, so "called finish_task
# once" is what separates an accepted contract from a loop that happens
# to end somewhere.
assert_log_count() {
  local got
  got="$(grep -c -- "$3" "$2" || true)"
  if [ "${got}" = "$4" ]; then ok "$1 (${got})"; else bad "$1 (${got}, want $4)"; fi
}

# assert_no_log <label> <logfile> <pattern>
assert_no_log() {
  if grep -q -- "$3" "$2"; then
    bad "$1 — log contains: $3"
  else
    ok "$1"
  fi
}

# ====================================================================
# main
# ====================================================================
rm -rf "${WORK}" && mkdir -p "${WORK}"

say "Build"
(cd "${REPO}" && go build -o "${BIN}" ./cmd/mast)
note "built ${BIN} (echo model — no credentials, no network)"
note "workload under test: ${WORKLOAD}"

# The nine properties of the shared report contract. Read out of the
# schema asset rather than hard-coded here, so a roster that grows a
# field grows this assertion with it instead of quietly under-checking.
SCHEMA="${WORKLOAD}/schemas/finding.json"
# Fail closed rather than falling back to a hard-coded list: an
# assertion set that silently shrinks is worse than one that will not
# run. python3 is already a docs-CI dependency (.github/workflows/
# docs.yml) and ships on every supported runner.
if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 is required to read ${SCHEMA}" >&2; exit 1
fi
PROPS="$(python3 -c 'import json,sys; print(" ".join(json.load(open(sys.argv[1]))["properties"]))' "${SCHEMA}")"
note "shared report contract: ${SCHEMA} (${PROPS})"

# ---- U-report leg A: the shipped roster, schema declared ------------
say "U-report/A: a declared output_schema is answered end to end"
DB="${WORK}/a.db"
WL="${WORKLOAD}"
LOG="${WORK}/a.log"
start_daemon "${LOG}"

assert_http "inject OOMKilled -> 202" "$(inject_code a1 OOMKilled)" 202
assert_state "session parked at the approval gate" incident-a1 paused

# The interrupt id names the specialist the classifier routed to. Without
# it, a report from the roster's `_fallback` agent would satisfy every
# other assertion below and hide a routing regression.
assert_eq "routed to the OOMKilled specialist (not _fallback)" \
  "$(show_field incident-a1 Interrupt)" approve-OOMKilled

MSG="$(show_field incident-a1 Message)"
note "report as the operator sees it: ${MSG}"

for p in ${PROPS}; do
  assert_has "report carries declared property '${p}'" "${MSG}" "${p}:"
done
# severity is the schema's only enum; a filler that ignored the enum
# would emit free text here and the contract would not in fact be met.
assert_has "severity is a valid enum member" "${MSG}" "severity:critical"
assert_hasnt "no unschema'd 'result' wrapper in a schema'd report" "${MSG}" "result:[echo triage]"

# One call, accepted. The defect this leg exists to catch produced eight:
# a refused report, retried until the specialist's max_turns killed the run.
assert_log_count "finish_task called exactly once" "${LOG}" 'function_call:finish_task' 1
assert_no_log "no budget exhaustion" "${LOG}" 'BUDGET EXCEEDED'
assert_no_log "no dispatch failure" "${LOG}" 'inject dispatch failed'
stop_term

# ---- U-report leg B: the same roster with no output_schema ----------
# The discriminating control. Derived at runtime under ${WORK} (house
# rule #5) by copying the shipped bundle and dropping one frontmatter
# line, so the two legs differ in exactly the thing under test.
say "U-report/B: with output_schema removed, the default wrapper is what ships"
DB="${WORK}/b.db"
WL="${WORK}/schemaless"
LOG="${WORK}/b.log"
rm -rf "${WL}"
cp -r "${WORKLOAD}" "${WL}"
grep -v '^output_schema:' "${WORKLOAD}/specialists/OOMKilled.tmpl" \
  > "${WL}/specialists/OOMKilled.tmpl"
start_daemon "${LOG}"

assert_http "inject OOMKilled -> 202" "$(inject_code b1 OOMKilled)" 202
assert_state "session parked at the approval gate" incident-b1 paused
BMSG="$(show_field incident-b1 Message)"
note "report as the operator sees it: ${BMSG}"
assert_has "unschema'd specialist falls back to the 'result' wrapper" \
  "${BMSG}" "result:[echo triage] diagnosed from envelope: OOMKilled"
assert_hasnt "leg A's enum assertion does not hold here" "${BMSG}" "severity:critical"
assert_hasnt "leg A's property assertion does not hold here" "${BMSG}" "recommended_actions:"
stop_term

# ---- U-report leg C: a violating report is refused ------------------
# MAST_FAKE_SCHEMA_VIOLATION makes the offline fake omit the first
# required property (pkg/agent/schemafill.go). If the schema were merely
# declared and not enforced, this leg would park with a report just like
# leg A.
say "U-report/C: a report that violates the declared schema is refused"
DB="${WORK}/c.db"
WL="${WORKLOAD}"
LOG="${WORK}/c.log"
export MAST_FAKE_SCHEMA_VIOLATION=1
start_daemon "${LOG}"

assert_http "inject OOMKilled -> 202" "$(inject_code c1 OOMKilled)" 202
assert_state "session parked at the approval gate" incident-c1 paused
CMSG="$(show_field incident-c1 Message)"
note "report as the operator sees it: ${CMSG}"
assert_has "the violating report did not become the result" "${CMSG}" "Result: <nil>"
assert_hasnt "no schema'd report reached the gate" "${CMSG}" "severity:critical"
# Exactly one attempt, refused, then the fake gives up: the leg ends in
# "no report", not in "no report plus whichever budget expired first".
assert_log_count "one finish_task attempt" "${LOG}" 'function_call:finish_task' 1
assert_log_count "one refusal" "${LOG}" 'function_response:finish_task' 1
assert_no_log "no budget exhaustion" "${LOG}" 'BUDGET EXCEEDED'
stop_term
unset MAST_FAKE_SCHEMA_VIOLATION

# ====================================================================
# U-fanout (W3) — the roster runs concurrently and gates once
# ====================================================================
# The observation point is the same as U-report's: the durable interrupt
# `mast sessions show` reads back out of SQLite. What differs is the
# shape being asserted — one gate for the whole roster instead of one per
# specialist, and a message that has to account for every analyst.
FANOUT_WL="${REPO}/examples/workloads/ns-audit"
ANALYSTS="networking-audit policy-audit storage-audit workloads-audit"

# ---- U-fanout leg A: the roster reports and the merged report gates --
say "U-fanout/A: four analysts run concurrently, one gate on the merged report"
DB="${WORK}/f.db"
WL="${FANOUT_WL}"
LOG="${WORK}/f.log"
DISPATCH=fanout
start_daemon "${LOG}"

assert_http "inject AuditRequested -> 202" "$(inject_audit f1)" 202
assert_state "session parked at the single post-synthesis gate" incident-f1 paused
assert_eq "the gate is the synthesis gate, not a per-analyst one" \
  "$(show_field incident-f1 Interrupt)" approve-_synthesis

FMSG="$(show_field incident-f1 Message)"
note "merged report as the operator sees it: ${FMSG}"
assert_has "every analyst is accounted for in the gate message" "${FMSG}" "4 of 4 analysts"

# Each analyst's events reach the runner under its own branch tag —
# two apiece for a clean report (the finish_task call and its response).
# This is the property the fan-out was rebuilt for (pkg/graph/fanout.go):
# a branch whose events are suppressed cannot see its own tool results,
# cannot be metered by author, and leaves nothing for crash recovery.
for a in ${ANALYSTS}; do
  assert_log_count "analyst ${a} ran on its own branch" "${LOG}" \
    "\"branch\":\"ns-audit_fan.branch_${a}\"" 2
done
assert_no_log "no dispatch failure" "${LOG}" 'inject dispatch failed'
assert_no_log "no budget exhaustion" "${LOG}" 'BUDGET EXCEEDED'

# Approving finishes the run without re-running the roster: on a resume
# the scheduler re-enters the asker, not its predecessors.
assert_http "approve the merged report -> 202" "$(resume_code incident-f1 approve-_synthesis)" 202
assert_state "approved run finishes" incident-f1 idle
# The approved run's terminal result, which only exists after the gate is
# answered: every analyst's finding is in it, and so is the verdict.
assert_log_count "the approved result counts every analyst's finding" "${LOG}" 'reported:4' 1
assert_log_count "the operator's verdict is on the result" "${LOG}" 'approval:map\[approved:true' 1
for a in ${ANALYSTS}; do
  assert_log_count "analyst ${a} did not re-run on the approval turn" "${LOG}" \
    "\"branch\":\"ns-audit_fan.branch_${a}\"" 2
done
stop_term

# ---- U-fanout leg B: silence is reported, not invented ---------------
# The discriminating control for leg A. MAST_FAKE_SCHEMA_VIOLATION makes
# every analyst's report violate its schema, so every branch goes silent.
# If a silent branch were merged as a finding anyway, this leg would park
# with "4 of 4" exactly like leg A.
say "U-fanout/B: analysts that report nothing are silent, and there is nothing to approve"
DB="${WORK}/g.db"
WL="${FANOUT_WL}"
LOG="${WORK}/g.log"
export MAST_FAKE_SCHEMA_VIOLATION=1
start_daemon "${LOG}"

assert_http "inject AuditRequested -> 202" "$(inject_audit g1)" 202
assert_state "a report no analyst contributed to does not gate" incident-g1 idle
# An idle session prints no result, so the count is read from the run's
# own terminal event in the daemon log rather than from `sessions show`.
assert_log_count "the merged report records that nobody reported" "${LOG}" 'reported:0' 1
assert_no_log "leg A's gate does not appear here" "${LOG}" 'approve-_synthesis' 
assert_no_log "a refused report ends the branch instead of looping it" "${LOG}" 'BUDGET EXCEEDED'
stop_term
unset MAST_FAKE_SCHEMA_VIOLATION

# ---- U-fanout leg C: a mutating roster is refused at construction ----
# The shipped gke-triage roster is a remediation roster: since W2.4 the
# write tools live on one declared specialist (change-executor) instead
# of on seven diagnosers, but a declaration is not a branch exemption.
# Fan-out runs every branch BEFORE the one approval gate, so that roster
# is still not a fan-out roster — and mast has to say so at startup
# rather than discover it mid-incident.
say "U-fanout/C: a roster that can mutate is refused before the daemon serves"
CLOG="${WORK}/h.log"
set +e
"${BIN}" --workload="${WORKLOAD}" --dispatch=fanout \
  --listen=":${PORT}" --model=echo --session-db="${WORK}/h.db" >"${CLOG}" 2>&1
CRC=$?
set -e
assert_eq "startup fails" "${CRC}" 1
CERR="$(cat "${CLOG}")"
assert_has "the refusal names the mutating tool" "${CERR}" "patch_k8s_resource"
assert_has "the refusal names the analyst that holds it" "${CERR}" "fan-out analyst"
assert_hasnt "the daemon never began serving" "${CERR}" "inject server listening"

# ====================================================================
# U-gate-structural (W2.4) — a diagnoser cannot hold a write tool
# ====================================================================
# The write gate above asks an operator per call. This asks a different
# question: which specialists can even reach a mutating tool? Before
# W2.4 the answer for the shipped roster was "seven diagnosers", held
# back by the sentence "Do NOT mutate anything on your own initiative"
# in their prompts. Three legs, over the SHIPPED bundle and one derived
# from it, because the refusal has to be shown to depend on the
# declaration rather than on the tool:
#
#   A  the shipped roster starts, and the startup log names its write
#      surface — one specialist, and the tools it holds
#   B  the same roster with patch_k8s_resource added back to a diagnoser
#      fails to start, naming the specialist and the tool
#   C  the same derived roster, plus `capability: change_executor` on
#      that diagnoser, starts — so leg B is a refusal of the
#      undeclared write, not of the tool
#
# Back to graph dispatch: the fan-out legs above set DISPATCH globally,
# and this section is about the roster mast refuses to BUILD, on the
# dispatch shape the bundle actually ships with.
DISPATCH=graph
STRUCT_BAD="${WORK}/struct-undeclared"
STRUCT_OK="${WORK}/struct-declared"
rm -rf "${STRUCT_BAD}" "${STRUCT_OK}"
cp -r "${WORKLOAD}" "${STRUCT_BAD}"
# Give a diagnoser back the write tool W2.4 took away from it.
sed 's|^        - list_k8s_events$|&\n        - patch_k8s_resource|' \
  "${WORKLOAD}/specialists/OOMKilled.tmpl" > "${STRUCT_BAD}/specialists/OOMKilled.tmpl"
if ! grep -q 'patch_k8s_resource' "${STRUCT_BAD}/specialists/OOMKilled.tmpl"; then
  echo "derived struct-undeclared has no patch_k8s_resource; the shipped tmpl's shape changed" >&2
  exit 1
fi
cp -r "${STRUCT_BAD}" "${STRUCT_OK}"
# ...and now let it say so. One line of YAML is the whole difference
# between leg B and leg C.
sed 's|^output_schema: ../schemas/finding.json$|&\ncapability: change_executor|' \
  "${STRUCT_BAD}/specialists/OOMKilled.tmpl" > "${STRUCT_OK}/specialists/OOMKilled.tmpl"
if ! grep -q 'capability: change_executor' "${STRUCT_OK}/specialists/OOMKilled.tmpl"; then
  echo "derived struct-declared has no capability line; the shipped tmpl's shape changed" >&2
  exit 1
fi

# ---- U-gate-structural leg A: the shipped roster declares its writes -
say "U-gate-structural/A: the shipped roster starts and names its write surface"
DB="${WORK}/struct-a.db"
WL="${WORKLOAD}"
LOG="${WORK}/struct-a.log"
start_daemon "${LOG}"
stop_term
SALOG="$(cat "${LOG}")"
assert_has "the startup log names the one specialist that can write" \
  "${SALOG}" "specialist declares write capability"
assert_has "...by name" "${SALOG}" "change-executor"
assert_has "...with the tools it holds" "${SALOG}" "patch_k8s_resource"
assert_log_count "exactly one specialist declares it" "${LOG}" \
  'specialist declares write capability' 1

# ---- U-gate-structural leg B: an undeclared write fails the roster ---
say "U-gate-structural/B: a diagnoser holding a write tool fails to start"
SBLOG="${WORK}/struct-b.log"
set +e
"${BIN}" --workload="${STRUCT_BAD}" --dispatch=graph \
  --listen=":${PORT}" --model=echo --session-db="${WORK}/struct-b.db" >"${SBLOG}" 2>&1
SBRC=$?
set -e
assert_eq "startup fails" "${SBRC}" 1
SBERR="$(cat "${SBLOG}")"
assert_has "the refusal names the specialist" "${SBERR}" "OOMKilled"
assert_has "the refusal names the tool" "${SBERR}" "patch_k8s_resource"
assert_has "the refusal says what to do about it" "${SBERR}" "change_executor"
assert_hasnt "the daemon never began serving" "${SBERR}" "inject server listening"

# ---- U-gate-structural leg C: declaring it makes the same roster run -
say "U-gate-structural/C: the same roster starts once the write is declared"
DB="${WORK}/struct-c.db"
WL="${STRUCT_OK}"
LOG="${WORK}/struct-c.log"
start_daemon "${LOG}"
assert_http "inject OOMKilled -> 202" "$(inject_code sc1 OOMKilled)" 202
assert_state "the declared roster runs the incident" incident-sc1 paused
stop_term
assert_log_count "both write declarations are on the startup log" "${LOG}" \
  'specialist declares write capability' 2

# ====================================================================
# U-gate (W2.1-W2.3) — a mutating call stops before it fires
# ====================================================================
# Two workloads, derived at runtime under ${WORK} (house rule #5) from
# the v0.2 fixture so the gated and ungated legs differ in exactly one
# line of YAML:
#
#   gate-default  the fixture with its `on_mutation: apply` line removed,
#                 so the workload says nothing about mutation and gets
#                 mast's default. The default is the thing under test:
#                 an unattended workload that never mentions HITL must
#                 not be allowed to write.
#   gate-apply    the fixture as shipped (on_mutation: apply).
say "Build the blocking MCP fixture (the gate needs a real mutating tool)"
(cd "${REPO}" && go build -o "${BLOCKER}" ./testdata/uat/blocker)
FIXTURE="${REPO}/testdata/uat"
GATE_DEFAULT="${WORK}/gate-default"
GATE_APPLY="${WORK}/gate-apply"
rm -rf "${GATE_DEFAULT}" "${GATE_APPLY}"
cp -r "${FIXTURE}" "${GATE_DEFAULT}"
cp -r "${FIXTURE}" "${GATE_APPLY}"
grep -v '^  on_mutation: apply$' "${FIXTURE}/workload.yaml" > "${GATE_DEFAULT}/workload.yaml"
# The derivation has to bite, or leg A would be testing `apply` and pass
# for the wrong reason.
if grep -q 'on_mutation' "${GATE_DEFAULT}/workload.yaml"; then
  echo "derived gate-default still declares on_mutation; the fixture's spelling changed" >&2
  exit 1
fi
note "gated workload:   ${GATE_DEFAULT} (no hitl.on_mutation -> mast's default)"
note "ungated workload: ${GATE_APPLY} (on_mutation: apply, as shipped)"

# ---- U-gate-percall leg A: the default parks a mutating call --------
say "U-gate-percall/A: an unconfigured workload parks its mutating call"
DB="${WORK}/gate-a.db"
WL="${GATE_DEFAULT}"
LOG="${WORK}/gate-a.log"
reset_blocker
start_gate_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat ga1 ApplyChange)" 202
assert_state "session parked on the write gate" incident-ga1 paused
assert_eq "the mutating tool did NOT run" "$(calls_count apply_change)" 0

GAMSG="$(show_field incident-ga1 Message)"
note "the operator's question: ${GAMSG}"
assert_has "the parked question names the tool" "${GAMSG}" "apply_change"
assert_has "the parked question is an approval, not an ADK internal" "${GAMSG}" "Approve mutating call"
GAINT="$(show_field incident-ga1 Interrupt)"
if [ -n "${GAINT}" ]; then ok "the park has an interrupt id (${GAINT})"; else bad "no interrupt id to answer"; fi

assert_http "approve it -> 202" "$(resume_verdict incident-ga1 "${GAINT}" '{"verdict":"approve","note":"uat"}')" 202
assert_state "approved run finishes" incident-ga1 idle
assert_eq "the approved call ran exactly once" "$(calls_count apply_change)" 1
assert_log_count "the approval is on the audit trail" "${LOG}" \
  'mutating tool call approved by operator' 1
stop_term

# ---- U-gate-percall leg B: on_mutation=apply does not gate ----------
# The discriminating control. Same fixture, same fake, same tool — one
# line of YAML different. Without this leg, "the call parked" could be
# any of a dozen things the harness does.
say "U-gate-percall/B: with on_mutation=apply the same call fires unasked"
DB="${WORK}/gate-b.db"
WL="${GATE_APPLY}"
LOG="${WORK}/gate-b.log"
reset_blocker
start_gate_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat gb1 ApplyChange)" 202
assert_state "the run finishes without stopping" incident-gb1 idle
assert_eq "the mutating tool ran, unapproved" "$(calls_count apply_change)" 1
assert_no_log "nothing was parked for approval" "${LOG}" 'awaiting_approval'
stop_term

# ---- U-gate-percall leg C: read-only work is not gated --------------
# A gate that stopped everything would pass leg A too. read_status is
# classified read-only by the same tool_catalog the outbox reads, under
# the same require_approval policy as leg A.
say "U-gate-percall/C: a read-only call under the same policy is not gated"
DB="${WORK}/gate-c.db"
WL="${GATE_DEFAULT}"
LOG="${WORK}/gate-c.log"
reset_blocker
start_gate_daemon "${LOG}"

assert_http "inject ReadStatus -> 202" "$(inject_uat gc1 ReadStatus)" 202
assert_state "the read-only run finishes" incident-gc1 idle
assert_eq "the read-only tool ran without approval" "$(calls_count read_status)" 1
assert_eq "and the mutating tool was never touched" "$(calls_count apply_change)" 0
stop_term

# ---- U-gate-percall leg D: reject means it never happens ------------
say "U-gate-percall/D: a rejected call is not made"
DB="${WORK}/gate-d.db"
WL="${GATE_DEFAULT}"
LOG="${WORK}/gate-d.log"
reset_blocker
start_gate_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat gd1 ApplyChange)" 202
assert_state "session parked on the write gate" incident-gd1 paused
GDINT="$(show_field incident-gd1 Interrupt)"
assert_http "reject it -> 202" \
  "$(resume_verdict incident-gd1 "${GDINT}" '{"verdict":"reject","note":"not during the freeze"}')" 202
assert_state "the rejected run finishes" incident-gd1 idle
assert_eq "the rejected call never ran" "$(calls_count apply_change)" 0
assert_log_count "the refusal is on the audit trail" "${LOG}" 'denied_by_operator' 1
stop_term

# ---- U-gate-crash: the question outlives the process ----------------
# Row 5. mast's own permissions.Prompter cannot do this by construction —
# it is a synchronous in-process ask — which is why the pause is ADK's
# durable confirmation flow and the gate only decides policy.
say "U-gate-crash: a parked call survives kill -9 and is answered by the next process"
DB="${WORK}/gate-e.db"
WL="${GATE_DEFAULT}"
LOG="${WORK}/gate-e-boot.log"
reset_blocker
start_gate_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat ge1 ApplyChange)" 202
assert_state "session parked on the write gate" incident-ge1 paused
GEINT="$(show_field incident-ge1 Interrupt)"
kill9
assert_eq "the tool had not run when the process died" "$(calls_count apply_change)" 0

LOG="${WORK}/gate-e-restart.log"
start_gate_daemon "${LOG}"
assert_state "the fresh process still shows the park" incident-ge1 paused
assert_eq "and still shows the same question" "$(show_field incident-ge1 Interrupt)" "${GEINT}"
EMSG="$(show_field incident-ge1 Message)"
assert_has "which still names the tool" "${EMSG}" "apply_change"

assert_http "approve against the fresh process -> 202" \
  "$(resume_verdict incident-ge1 "${GEINT}" '{"verdict":"approve","note":"after the crash"}')" 202
assert_state "the resumed run finishes" incident-ge1 idle
# L1 / E-exactly-once, at the gate: the crash must not turn one approved
# change into two applied ones.
assert_eq "the call ran exactly once across the crash" "$(calls_count apply_change)" 1
stop_term

# ---- U-gate-scopes: an approval covers this call and no more --------
# W2.3. A verdict that asks to authorize a pattern, a session, or "always"
# is REFUSED rather than narrowed to `once`: silently narrowing would tell
# an operator they had a standing grant when they did not.
say "U-gate-scopes: an approval that reaches past this one call is refused"
DB="${WORK}/gate-f.db"
WL="${GATE_DEFAULT}"
LOG="${WORK}/gate-f.log"
reset_blocker
start_gate_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat gf1 ApplyChange)" 202
assert_state "session parked on the write gate" incident-gf1 paused
GFINT="$(show_field incident-gf1 Interrupt)"
assert_http "approve for the whole session -> 202 (accepted as a message)" \
  "$(resume_verdict incident-gf1 "${GFINT}" '{"verdict":"approve","scope":"session"}')" 202
assert_state "the run finishes" incident-gf1 idle
assert_eq "the over-broad approval did not run the call" "$(calls_count apply_change)" 0
assert_log_count "the scope refusal is on the audit trail" "${LOG}" 'approval_scope_refused' 1

# And the same operator, re-issuing for this one call, is honoured — so
# the leg above is a refusal of the SCOPE and not of the approver.
assert_http "inject ApplyChange again -> 202" "$(inject_uat gf2 ApplyChange)" 202
assert_state "session parked on the write gate" incident-gf2 paused
GF2INT="$(show_field incident-gf2 Interrupt)"
assert_http "approve this one call -> 202" \
  "$(resume_verdict incident-gf2 "${GF2INT}" '{"verdict":"approve","scope":"once"}')" 202
assert_state "the approved run finishes" incident-gf2 idle
assert_eq "the re-issued approval ran the call exactly once" "$(calls_count apply_change)" 1
stop_term

# ---- U-gate-edit leg G: the operator's arguments are the ones that run
# W2.5. The scoreboard row is "an operator can edit the call before it
# runs", and the only end-to-end evidence for it is what the tool was
# handed: the fixture's apply_change records its `replicas` argument, the
# fake proposes 10, and the operator sends 2.
say "U-gate-edit/G: an edited verdict runs the operator's arguments, not the model's"
DB="${WORK}/gate-g.db"
WL="${GATE_DEFAULT}"
LOG="${WORK}/gate-g.log"
reset_blocker
start_gate_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat gg1 ApplyChange)" 202
assert_state "session parked on the write gate" incident-gg1 paused
GGMSG="$(show_field incident-gg1 Message)"
assert_has "the parked question shows the arguments the model proposed" "${GGMSG}" "replicas=10"
GGINT="$(show_field incident-gg1 Interrupt)"
assert_http "edit it down to two replicas -> 202" \
  "$(resume_verdict incident-gg1 "${GGINT}" \
    '{"verdict":"edit","args":{"replicas":2},"note":"ten would exhaust the node pool"}')" 202
assert_state "the edited run finishes" incident-gg1 idle
assert_eq "the call ran exactly once" "$(calls_count apply_change)" 1
GGCALLS="$(calls_args apply_change)"
assert_has "and it ran with the OPERATOR's arguments" "${GGCALLS}" "replicas=2"
assert_hasnt "not with the model's" "${GGCALLS}" "replicas=10"
assert_log_count "the edit is on the audit trail" "${LOG}" 'edit_applied' 1

# The audit gap W2.5 exists to close. ADK re-fires the parked call
# verbatim, so the durable FunctionCall still says replicas=10; without
# the AppliedEdit record `sessions show` would tell an operator, with
# confidence, the wrong thing about what mast did.
GGSHOW="$("${BIN}" sessions show incident-gg1 --session-db="${DB}" 2>/dev/null)"
assert_has "sessions show reports the applied edit" "${GGSHOW}" "Operator edit applied"
assert_has "naming the call that actually ran" "${GGSHOW}" "apply_change(replicas=2)"
assert_has "and the call the model had proposed" "${GGSHOW}" "apply_change(replicas=10)"
assert_has "attributed to the authenticated approver" "${GGSHOW}" "shared-bearer-token"
stop_term

# ---- U-gate-edit leg H: an edit the schema rejects is refused --------
# The discriminating control for leg G. If the gate simply forwarded
# whatever the operator typed, leg G would pass exactly as it does now —
# so the claim under test is that the arguments are CHECKED, and that a
# refused edit runs neither the operator's call nor the model's.
#
# The execution count is NOT what discriminates here, and the leg does
# not pretend it is: the MCP server validates its own inputs, so a
# type-invalid call fails there too and the count stays 0 either way.
# Deleting mast's check (verified by neutralization) leaves the count
# untouched and trips the last two assertions instead — mast records the
# edit as APPLIED and never logs the refusal, i.e. tells the operator
# their arguments ran when the tool errored.
say "U-gate-edit/H: an edit the tool's schema rejects never reaches the tool"
DB="${WORK}/gate-h.db"
WL="${GATE_DEFAULT}"
LOG="${WORK}/gate-h.log"
reset_blocker
start_gate_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat gh1 ApplyChange)" 202
assert_state "session parked on the write gate" incident-gh1 paused
GHINT="$(show_field incident-gh1 Interrupt)"
assert_http "edit it to a replica count that is not a number -> 202" \
  "$(resume_verdict incident-gh1 "${GHINT}" '{"verdict":"edit","args":{"replicas":"two"}}')" 202
assert_state "the run finishes" incident-gh1 idle
assert_eq "neither the edited call nor the model's ran" "$(calls_count apply_change)" 0
assert_log_count "the refusal is on the audit trail" "${LOG}" 'edit_refused' 1
GHSHOW="$("${BIN}" sessions show incident-gh1 --session-db="${DB}" 2>/dev/null)"
assert_hasnt "and nothing is recorded as applied" "${GHSHOW}" "Operator edit applied"
stop_term

# ---- summary --------------------------------------------------------
say "Summary"
note "PASS=${PASS}  FAIL=${FAIL}"
note "logs + state under ${WORK}"
if [ "${FAIL}" -ne 0 ]; then
  echo "UAT FAILED" >&2
  exit 1
fi
echo "UAT PASSED"
