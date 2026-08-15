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

# uat-v0.4.sh — end-to-end acceptance pass for the mast v0.4 change-set
# work (docs/v0.4-plan.md §3, W7.0). Its subject is one claim:
#
#   the call an operator approves is the call that fires.
#
# Before W7.0 a finding recommended a remediation in prose, an operator
# agreed with the prose, and the change executor composed a tool call
# from it on a later turn — so what reached the cluster was a call
# nobody had looked at. A change set closes that gap by making the
# finding carry the executable call, checked against the named tool's
# own input schema before the report is accepted, and handed to the
# executor verbatim once approved.
#
# Legs (all offline: no credentials, no network):
#
#   U-proposed-change (W7.0, the producer contract) — over a fixture
#     derived from testdata/uat, whose stdio MCP blocker is the only
#     offline source of a REAL tool with a REAL declared input schema.
#     A contract checked against a fake schema would be a claim about
#     the harness:
#       A  a change naming a wired tool with valid arguments is
#          ACCEPTED, recorded, and the report lands on the first call
#       B  no change set (the default, and what every pre-W7.0 roster
#          produces) is a complete report — nothing recorded, nothing
#          refused, one call
#       C  a change naming a tool the workload does not declare is
#          REFUSED back to the specialist, which retries
#       D  a change whose arguments the tool would reject is REFUSED
#          the same way, so leg A is shown to check the arguments and
#          not merely the name
#
#   U-handoff (W7.0, the structural predicate) — the diagnoser→executor
#     edge, over a graph-dispatch roster derived from the same fixture:
#     a classifier, a diagnoser holding only the read tool, and a
#     change executor holding the write tool.
#       A  the diagnoser proposes apply_change({"replicas":2}) and the
#          executor's call parks at the write gate quoting replicas=2 —
#          NOT the replicas=10 this same fake proposes when it picks a
#          call for itself. That contrast is the whole assertion: the
#          parked question distinguishes the approved call from a
#          merely plausible one, which prose never could. Approving it
#          then fires apply_change exactly once, with replicas=2 in the
#          blocker's own ledger — the round trip, end to end.
#       B  the same roster with nothing proposed: the executor never
#          runs and the write tool is never called, so leg A's handoff
#          is shown to be the change set's doing and not the roster's
#
# The observation points are the two an operator already has: `mast
# sessions show`, which reads the parked question back out of SQLite,
# and the blocker's own call ledger, which records the arguments each
# call actually ran with.
#
# Usage: scripts/uat-v0.4.sh
# State goes under ${TMPDIR:-/tmp}/mast-uat-v04 (house rule #5); port 7790.

set -euo pipefail

PORT="${MAST_UAT_V04_PORT:-7790}"
BASE="http://127.0.0.1:${PORT}"
WORK="${TMPDIR:-/tmp}/mast-uat-v04"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${WORK}/mast"
FIXTURE="${REPO}/testdata/uat"
TOKEN="uat-v04-token"
export MAST_INJECT_TOKEN="${TOKEN}"
PID=""

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
# The same stdio MCP server the v0.2 and v0.3 harnesses drive. It is the
# only offline tool that both declares a real input schema (so the
# producer contract has something to check against) and records the
# arguments it was actually called with (so the handoff has something to
# prove).
BLOCKER="${WORK}/blocker"
BLOCKDIR="${WORK}/blockdir"
export MAST_UAT_BLOCKER="${BLOCKER}"
export UAT_BLOCKER_DIR="${BLOCKDIR}"

reset_blocker() {
  rm -rf "${BLOCKDIR}"
  mkdir -p "${BLOCKDIR}"
  # These legs are about which call fires, not about holding one open.
  : > "${BLOCKDIR}/apply_change.release"
  : > "${BLOCKDIR}/read_status.release"
}

calls_count() {
  local f="${BLOCKDIR}/$1.calls"
  if [ -f "${f}" ]; then wc -l < "${f}" | tr -d ' '; else echo 0; fi
}

calls_args() {
  local f="${BLOCKDIR}/$1.calls"
  if [ -f "${f}" ]; then cat "${f}"; fi
}

# ---- lifecycle ------------------------------------------------------
# start_daemon <logfile> — launch the daemon over ${WL} under ${DISPATCH}
# with --model=toolactor. toolactor rather than echo is forced, not a
# preference: wireMCPToolsets skips MCP entirely under the echo model, so
# an echo daemon has no tool schemas to resolve and the producer contract
# would fail closed on every leg — green for the wrong reason.
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

stop_term() {
  kill -TERM "${PID}" 2>/dev/null || true
  wait "${PID}" 2>/dev/null || true
  PID=""
}

# ---- drivers --------------------------------------------------------
inject_uat() {
  curl -s -m 90 -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/inject" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"kind\":\"uat-event\",\"reason\":\"$2\",\"namespace\":\"default\",\"name\":\"pod-$1\",\"uid\":\"$1\",\"message\":\"uat\",\"cluster\":\"uat\"}"
}

resume_verdict() {
  curl -s -m 60 -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/resume" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"session_id\":\"$1\",\"interrupt_id\":\"$2\",\"response\":$3}"
}

# show_field <session-id> <label> — one labelled line from `mast sessions
# show`. awk reads to EOF rather than exiting early: an early exit
# SIGPIPEs the CLI, which under pipefail kills the run with 141.
show_field() {
  "${BIN}" sessions show "$1" --session-db="${DB}" 2>/dev/null \
    | awk -v k="$2:" '$1 == k && !seen { sub(/^[[:space:]]*[^:]*:[[:space:]]*/, ""); print; seen = 1 }'
}

state_is() { [ "$(show_field "$1" State)" = "$2" ]; }

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

assert_has() {
  if printf '%s' "$2" | grep -Fq -- "$3"; then ok "$1"; else bad "$1 — missing: $3"; fi
}

# assert_hasnt fails CLOSED on an empty haystack: absence proves nothing
# when there is nothing to search.
assert_hasnt() {
  if [ -z "$2" ]; then bad "$1 — nothing to search (empty)"; return 0; fi
  if printf '%s' "$2" | grep -Fq -- "$3"; then bad "$1 — present: $3"; else ok "$1"; fi
}

assert_state() {
  if wait_for 30 state_is "$2" "$3"; then
    ok "$1 (state=$3)"
  else
    bad "$1 (state=$(show_field "$2" State), want $3)"
  fi
}

assert_log_count() {
  local got
  got="$(grep -c -- "$3" "$2" || true)"
  if [ "${got}" = "$4" ]; then ok "$1 (${got})"; else bad "$1 (${got}, want $4)"; fi
}

# assert_log_atleast <label> <logfile> <pattern> <n> — for the refusal
# legs, where the specialist retries until its turn budget runs out and
# the exact retry count is the fake's business, not the contract's.
assert_log_atleast() {
  local got
  got="$(grep -c -- "$3" "$2" || true)"
  if [ "${got}" -ge "$4" ]; then ok "$1 (${got})"; else bad "$1 (${got}, want >= $4)"; fi
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

# The report contract the derived rosters declare. Small on purpose: the
# subject is proposed_change, and every other property is scenery the
# offline fake would have to fill.
mk_schema() {
  cat > "$1" <<'JSON'
{
  "title": "Finding",
  "type": "object",
  "description": "A UAT finding. See scripts/uat-v0.4.sh — this is a harness fixture, not a shipped contract.",
  "properties": {
    "summary": {
      "type": "string",
      "description": "One line naming what was found."
    },
    "proposed_change": {
      "type": "array",
      "description": "The executable form of the recommendation: the exact calls an operator would approve.",
      "items": {
        "type": "object",
        "description": "One executable call.",
        "properties": {
          "tool": {"type": "string", "description": "Name of the tool to call."},
          "arguments": {"type": "string", "description": "That tool's arguments as a JSON object, encoded in a string."}
        },
        "required": ["tool", "arguments"]
      }
    }
  },
  "required": ["summary"]
}
JSON
}

# ---- the producer fixture: testdata/uat + a report contract ---------
PRODUCER="${WORK}/producer"
cp -r "${FIXTURE}" "${PRODUCER}"
mkdir -p "${PRODUCER}/schemas"
mk_schema "${PRODUCER}/schemas/finding.json"
# The one edit: the worker now returns a structured report, so it has a
# proposed_change field to fill. Everything else about the fixture —
# its tool catalog, its capability declaration, its on_mutation: apply —
# is left exactly as the v0.2 harness has it.
awk '/^mode: Task$/ { print; print "output_schema: ../schemas/finding.json"; next } { print }' \
  "${FIXTURE}/specialists/uat-worker.tmpl" > "${PRODUCER}/specialists/uat-worker.tmpl"
grep -q '^output_schema:' "${PRODUCER}/specialists/uat-worker.tmpl" \
  || { echo "fixture derivation failed: no output_schema in the derived worker" >&2; exit 1; }

# ---- the handoff fixture: a graph roster over the same tools --------
# Three specialists and a fallback, the smallest roster the structural
# predicate needs: something to classify, something to diagnose that
# CANNOT write, and something that can.
HANDOFF="${WORK}/handoff"
mkdir -p "${HANDOFF}/specialists" "${HANDOFF}/schemas"
cp "${FIXTURE}/mcp.json" "${HANDOFF}/mcp.json"
mk_schema "${HANDOFF}/schemas/finding.json"

cat > "${HANDOFF}/workload.yaml" <<'YAML'
# Harness fixture for scripts/uat-v0.4.sh's U-handoff legs. Derived from
# testdata/uat: same stdio MCP blocker, same two tools, split across a
# diagnoser and a change executor so the handoff has two ends.
name: uat-handoff
description: Fixture workload for the mast v0.4 diagnoser-to-executor handoff legs.
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
  - classify
  - ApplyChange
  - _fallback
  - change-executor

budget:
  max_wallclock_seconds: 300

hitl:
  # Deliberately off. These legs are about the write gate parking the
  # EXECUTOR's call — the one composed from the approved change set —
  # and a per-specialist approval pause in front of it would park the
  # finding instead, which is uat-v0.3.sh's subject. on_mutation is left
  # at its default (require_approval), which is what does the parking.
  require_approval: false

edge_trigger:
  http:
    path: /inject
    auth: bearer
YAML

cat > "${HANDOFF}/specialists/classify.tmpl" <<'TMPL'
---
name: classify
description: Routes a UAT incident envelope to a specialist by its reason.
mode: SingleTurn
---

Read the JSON payload's `reason` field and emit it as a single token,
with no explanation or punctuation.
TMPL

cat > "${HANDOFF}/specialists/ApplyChange.tmpl" <<'TMPL'
---
name: ApplyChange
description: Diagnoses a UAT incident and proposes an executable remediation.
mode: Task
output_schema: ../schemas/finding.json
budget:
  max_turns: 5
tools:
  mcp:
    - server: uat-blocker
      tools:
        - read_status
---

Diagnose the incident and return a finding. Name the remediation as an
exact call in `proposed_change`; you cannot make it yourself.
TMPL

cat > "${HANDOFF}/specialists/change-executor.tmpl" <<'TMPL'
---
name: change-executor
description: Carries out an approved change against the UAT fixture.
mode: Task
capability: change_executor
budget:
  max_turns: 5
tools:
  mcp:
    - server: uat-blocker
      tools:
        - apply_change
---

Make the approved calls exactly as written, in order. Do not re-derive
them and do not add any others.
TMPL

cat > "${HANDOFF}/specialists/_fallback.tmpl" <<'TMPL'
---
name: _fallback
description: Fallback specialist for UAT incidents with no dedicated handler.
mode: Task
output_schema: ../schemas/finding.json
budget:
  max_turns: 5
tools:
  # Explicitly none. An absent allowlist would grant the fallback the
  # whole catalog including apply_change, which mast refuses for a
  # specialist that has not declared change_executor (W2.4).
  mcp: []
---

Report what you were given.
TMPL

# ====================================================================
# U-proposed-change (W7.0) — the producer contract
# ====================================================================
WL="${PRODUCER}"
DISPATCH=coordinator

# The change the fixture's tool would actually accept: apply_change
# declares one required integer argument (testdata/uat/blocker).
GOOD_CHANGE='[{"tool":"apply_change","arguments":"{\"replicas\":2}"}]'

# ---- leg A: a valid change is accepted and recorded -----------------
say "U-proposed-change/A: an executable change is accepted and recorded"
DB="${WORK}/p-a.db"
LOG="${WORK}/p-a.log"
reset_blocker
export MAST_FAKE_PROPOSED_CHANGE="${GOOD_CHANGE}"
start_daemon "${LOG}"

assert_http "inject ReadStatus -> 202" "$(inject_uat pa1 ReadStatus)" 202
assert_state "the run finished" incident-pa1 idle
# Accepted on the first call. A refused report comes straight back to the
# specialist, so "one attempt" is what separates an accepted contract
# from a retry loop that happened to end somewhere.
assert_log_atleast "the change set was recorded" "${LOG}" 'change set proposed' 1
# The daemon logs JSON, so the recorded signature appears escaped. That
# is what makes this a byte-level assertion rather than a "mentions the
# tool" one: the recorded call is the normalized signature, arguments
# and all.
assert_has "the record names the exact call" \
  "$(grep -- 'change set proposed' "${LOG}" || true)" 'apply_change({\"replicas\":2})'
assert_no_log "nothing was refused" "${LOG}" 'change_set_refused'
stop_term

# ---- leg B: no change set is a complete report ----------------------
# The regression control for every pre-W7.0 roster: a finding that
# proposes nothing must pass through the contract untouched.
say "U-proposed-change/B: an empty change set is a finished report"
DB="${WORK}/p-b.db"
LOG="${WORK}/p-b.log"
reset_blocker
unset MAST_FAKE_PROPOSED_CHANGE
start_daemon "${LOG}"

assert_http "inject ReadStatus -> 202" "$(inject_uat pb1 ReadStatus)" 202
assert_state "the run finished" incident-pb1 idle
assert_no_log "nothing was recorded" "${LOG}" 'change set proposed'
assert_no_log "nothing was refused" "${LOG}" 'change_set_refused'
assert_log_count "finish_task called exactly once" "${LOG}" 'function_call:finish_task' 1
stop_term

# ---- leg C: an invented tool is refused -----------------------------
say "U-proposed-change/C: a change naming a tool the workload does not have is refused"
DB="${WORK}/p-c.db"
LOG="${WORK}/p-c.log"
reset_blocker
export MAST_FAKE_PROPOSED_CHANGE=kubectl_scale
start_daemon "${LOG}"

# 500, and that is the right answer. A refusal comes back to the
# specialist to fix, and this one cannot fix it — the invented tool is
# all the fake has to offer — so it retries until its turn budget runs
# out and the incident fails loudly. The alternative, accepting a report
# mast could not check, is the failure this workstream exists to prevent.
assert_http "the incident fails rather than shipping the proposal" "$(inject_uat pc1 ReadStatus)" 500
assert_state "the run ended" incident-pc1 idle
assert_log_atleast "the report was refused" "${LOG}" 'change_set_refused' 1
assert_has "the refusal names the invented tool" \
  "$(grep -- 'change_set_refused' "${LOG}" || true)" 'kubectl_scale'
assert_no_log "nothing was recorded" "${LOG}" 'change set proposed'
# Refused back to the specialist, not fatal to the turn: the run has to
# reach an end, because a contract that kills the incident is worse than
# the prose it replaced.
assert_log_atleast "the specialist got another turn" "${LOG}" 'function_call:finish_task' 2
stop_term

# ---- leg D: arguments the tool would reject are refused -------------
# The discriminator for leg A: same real tool, same wiring, arguments
# apply_change's own schema does not declare. Without this leg, leg A
# would only show that the NAME was checked.
say "U-proposed-change/D: arguments the tool would reject are refused"
DB="${WORK}/p-d.db"
LOG="${WORK}/p-d.log"
reset_blocker
export MAST_FAKE_PROPOSED_CHANGE='[{"tool":"apply_change","arguments":"{\"nope\":1}"}]'
start_daemon "${LOG}"

assert_http "the incident fails rather than shipping the proposal" "$(inject_uat pd1 ReadStatus)" 500
assert_state "the run ended" incident-pd1 idle
assert_log_atleast "the report was refused" "${LOG}" 'change_set_refused' 1
# The refusal is about the ARGUMENTS: it names the one the specialist
# sent and the one the tool actually declares. Leg C's refusal could not
# say this — there was no tool to read a schema off.
DREF="$(grep -- 'change_set_refused' "${LOG}" || true)"
assert_has "the refusal names the offending argument" "${DREF}" 'nope'
assert_has "the refusal names what the tool does declare" "${DREF}" 'replicas'
assert_no_log "nothing was recorded" "${LOG}" 'change set proposed'
assert_eq "the tool never ran" "$(calls_count apply_change)" 0
stop_term
unset MAST_FAKE_PROPOSED_CHANGE

# ====================================================================
# U-handoff (W7.0) — the approved call is the call that fires
# ====================================================================
WL="${HANDOFF}"
DISPATCH=graph

# ---- leg A: the approved call reaches the cluster unchanged ---------
say "U-handoff/A: the approved call is the call that fires"
DB="${WORK}/h-a.db"
LOG="${WORK}/h-a.log"
reset_blocker
export MAST_FAKE_PROPOSED_CHANGE="${GOOD_CHANGE}"
start_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat ha1 ApplyChange)" 202
assert_state "the executor's call parked at the write gate" incident-ha1 paused
assert_eq "nothing has been applied yet" "$(calls_count apply_change)" 0

HAMSG="$(show_field incident-ha1 Message)"
note "the operator's question: ${HAMSG}"
assert_has "the parked question is the executor's write call" "${HAMSG}" "apply_change"
assert_has "it carries the arguments the diagnoser proposed" "${HAMSG}" "replicas=2"
assert_hasnt "not the ones the executor would have picked for itself" "${HAMSG}" "replicas=10"

HAINT="$(show_field incident-ha1 Interrupt)"
if [ -n "${HAINT}" ]; then ok "the park has an interrupt id (${HAINT})"; else bad "no interrupt id to answer"; fi
assert_http "the operator can answer it -> 202" "$(resume_verdict incident-ha1 "${HAINT}" '{"verdict":"approve","note":"uat"}')" 202
assert_state "the answered run finishes" incident-ha1 idle
# The route survives the resume: without it the re-run classifier reads
# the operator's answer, finds no incident in it, and the run lands on
# _fallback — a different specialist from the one that asked.
assert_no_log "the resume did not fall through to _fallback" "${LOG}" 'run__fallback'

# The whole point of the handoff: the call an operator approved is the
# call the cluster gets. This is the leg that regressed while the graph
# announced the approved change set as a USER-authored event — ADK's
# confirmation resume scans back to the most recent user event and gives
# up when it holds no FunctionResponse, so the announcement shadowed the
# operator's confirmation and the parked call was silently abandoned:
# state=idle, no error, nothing applied. Count and arguments both, since
# a resume that re-dispatches twice is as wrong as one that never does.
assert_eq "the approved call fired, exactly once" "$(calls_count apply_change)" 1
HACALLS="$(calls_args apply_change)"
assert_has "with the arguments the operator approved" "${HACALLS}" "replicas=2"
assert_hasnt "not the ones the executor would have picked" "${HACALLS}" "replicas=10"
stop_term

# ---- leg B: no change set, no handoff -------------------------------
# The discriminating control. Same roster, same incident, same executor
# holding the same write tool — only the change set is gone.
say "U-handoff/B: with nothing proposed, the executor never runs"
DB="${WORK}/h-b.db"
LOG="${WORK}/h-b.log"
reset_blocker
unset MAST_FAKE_PROPOSED_CHANGE
start_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat hb1 ApplyChange)" 202
assert_state "the run ends on the finding" incident-hb1 idle
assert_eq "the write tool was never called" "$(calls_count apply_change)" 0
assert_no_log "nothing was recorded" "${LOG}" 'change set proposed'
stop_term

# ====================================================================
say "Summary"
printf '   %d passed, %d failed\n' "${PASS}" "${FAIL}"
if [ "${FAIL}" -gt 0 ]; then exit 1; fi
