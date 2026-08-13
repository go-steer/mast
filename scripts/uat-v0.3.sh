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

# start_graph <logfile> — launch the daemon over ${WL} under graph
# dispatch (the roster's classifier -> per-failure-mode specialist ->
# approval gate path) and spin until it answers.
start_graph() {
  local log="$1"; shift
  "${BIN}" --workload="${WL}" --dispatch=graph \
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

# ---- assertions -----------------------------------------------------
assert_eq() {
  if [ "$2" = "$3" ]; then ok "$1 (${2})"; else bad "$1 (got '${2:-<none>}', want '$3')"; fi
}

assert_http() {
  if [ "$2" = "$3" ]; then ok "$1 (HTTP $2)"; else bad "$1 (HTTP $2, want $3)"; fi
}

# assert_has <label> <haystack> <needle> — literal substring must appear.
assert_has() {
  if printf '%s' "$2" | grep -Fq -- "$3"; then ok "$1"; else bad "$1 — missing: $3"; fi
}

# assert_hasnt <label> <haystack> <needle> — literal substring must not.
# Fails CLOSED on an empty haystack: absence proves nothing when there is
# nothing to search.
assert_hasnt() {
  if [ -z "$2" ]; then bad "$1 — nothing to search (empty)"; return 0; fi
  if printf '%s' "$2" | grep -Fq -- "$3"; then bad "$1 — present: $3"; else ok "$1"; fi
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
start_graph "${LOG}"

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
start_graph "${LOG}"

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
start_graph "${LOG}"

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

# ---- summary --------------------------------------------------------
say "Summary"
note "PASS=${PASS}  FAIL=${FAIL}"
note "logs + state under ${WORK}"
if [ "${FAIL}" -ne 0 ]; then
  echo "UAT FAILED" >&2
  exit 1
fi
echo "UAT PASSED"
