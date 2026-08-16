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

# uat-v0.4.sh — end-to-end acceptance pass for mast v0.4
# (docs/v0.4-plan.md §3). Most of it has one subject:
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
# The last leg subject is W4.1's, and it is a different claim:
#
#   a workload that wakes itself up keeps its cadence across a crash,
#   and does not pay for the ticks it was down for.
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
#   U-changeset (W7, the grant) — the same roster over a two-call set,
#     with a change-set TTL and a declared precondition. One operator
#     answer, N calls, and every way that could become "N calls nobody
#     approved" closed:
#       A  `scope: change_set` runs both calls off ONE question
#       B  the default `scope: once` still asks per call — the control
#          that keeps leg A from passing on a runtime that stopped
#          parking altogether
#       C  the cluster moves while the first approved call is held open
#          at the blocker: the second call's grant is voided and it
#          parks again, which is the check a wall clock cannot make
#       D  the window runs out with the cluster unchanged: the same
#          re-park, on the other clock
#       E  the daemon is SIGKILLed while the question is open; the
#          approval arrives at a process that never saw the diagnoser,
#          and still covers the whole set
#
#   U-bounded-cost (W4.3 `/steps`; W4.2 `/collect`, v0.5) — what one
#     cycle costs. The other legs above are about what a run DOES; this
#     one is about what it SPENDS, which is the question an operator
#     answers before letting a workload run unattended on a timer:
#       /steps  the shipped examples/workloads/bounded-triage bundle
#               answers one incident in exactly ONE model call, with a
#               report forced to the finding schema — asserted off the
#               meter, on both surfaces that publish it, and paired
#               with the refusal that keeps the number honest (a roster
#               that would need an orchestrator will not start)
#       /collect (W4.2, v0.5) zero-token collection — cluster data
#               gathered with no model call at all. Stubbed here rather
#               than left unwritten so the row's other half has a home
#               the day it lands
#
#   U-decisions (W8, the harvest) — the same two-call set, exported
#     with the daemon stopped: one record per adjudicated CALL (so one
#     approval cannot stand in for two mutations), the approver digested
#     by default, and --include-approver naming them with the file
#     saying which mode produced it.
#
#   U-scheduled (W4.1, the cadence) — the v0.2 coordinator fixture with
#     `edge_trigger.scheduled` on a 3s interval, which is the whole of
#     what the workload declares: nothing posts to this daemon and no
#     external cron exists.
#       A  two ticks fire, the daemon is SIGKILLed, and it comes back
#          after a gap spanning two more. The restarted process resumes
#          the ORIGINAL anchor rather than re-anchoring on boot, reports
#          the ticks it was down for in one line, and never runs them —
#          the sessions those ticks would have owned do not exist. That
#          pairing is the leg: catch-up on boot would show up here as a
#          crash-looping daemon buying a backlog of model runs, and a
#          reset clock would show up as a nightly sweep that drifts into
#          the afternoon.
#       B  a mutating call inside a scheduled run still parks for a real
#          approver, and the run says who it ran as (mast:scheduler).
#          Unattended is not unsupervised: the write gate does not know
#          or care that nobody asked for this turn.
#
# The observation points are the two an operator already has: `mast
# sessions show`, which reads the parked question back out of SQLite,
# and the blocker's own call ledger, which records the arguments each
# call actually ran with. U-bounded-cost adds the third, the meter: the
# daemon's `session_model_calls` log field and `mast_model_calls_total`
# on /metrics. Step count is read there and nowhere else — wallclock and
# token totals move with the model, the prompt and the machine, so a
# cost claim inferred from either is a claim about the afternoon it was
# measured. The scheduled legs read a fourth the daemon already emits —
# its own JSON log — because a cadence's evidence is when things
# happened, and a counter kept by the harness could drift from what the
# daemon did.
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

# reset_blocker [hold-tool] — fresh ledger and, by default, both tools
# pre-released: most legs are about which call fires, not about holding
# one open. Naming a tool holds it blocked instead, which is how the
# freshness and crash legs get a deterministic window between the calls
# one approval authorized.
reset_blocker() {
  local hold="${1:-}"
  rm -rf "${BLOCKDIR}"
  mkdir -p "${BLOCKDIR}"
  [ "${hold}" = "apply_change" ] || : > "${BLOCKDIR}/apply_change.release"
  [ "${hold}" = "read_status" ]  || : > "${BLOCKDIR}/read_status.release"
}

release() { : > "${BLOCKDIR}/$1.release"; }

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

# start_refused <logfile> <workload> <dispatch> — run the daemon
# expecting it to REFUSE the roster and exit. Returns 0 when it did.
#
# A daemon that comes up instead is killed and the caller reports a
# failure: the value of a build-time refusal is precisely that it lands
# before the workload is serving, so "it errored later" is not the same
# result.
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

# kill9 — the crash the change-set legs need: no drain, no flush, and
# nothing written on the way out that a restart could lean on.
kill9() {
  kill -9 "${PID}" 2>/dev/null || true
  wait "${PID}" 2>/dev/null || true
  PID=""
}

# wait_started <tool> — block until the blocker reports the tool has
# dispatched. The change-set legs move the world (or kill the daemon)
# while a call is in flight, and a sleep would make that a race.
wait_started() {
  local marker="${BLOCKDIR}/$1.started" i
  for ((i = 0; i < 300; i++)); do
    [ -f "${marker}" ] && return 0
    sleep 0.1
  done
  return 1
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

# resume_bg — the same answer, posted in the background. /resume runs
# the turn synchronously, so a leg that has to act WHILE the approved
# calls are running (move the world, kill the daemon) cannot wait for
# the response first. The code lands in ${RESUME_CODE_FILE}.
RESUME_BG_PID=""
resume_bg() {
  RESUME_CODE_FILE="${WORK}/resume-code.$$"
  rm -f "${RESUME_CODE_FILE}"
  ( resume_verdict "$1" "$2" "$3" > "${RESUME_CODE_FILE}" ) &
  RESUME_BG_PID=$!
}

# resume_bg_wait sets ${RESUME_CODE}. It is a STATEMENT, not something to
# call in `$(...)`: a command substitution runs in a subshell, where the
# background post is not a child, so `wait` there returns instantly and
# reads the code file before curl has written it.
RESUME_CODE=""
resume_bg_wait() {
  wait "${RESUME_BG_PID}" 2>/dev/null || true
  RESUME_BG_PID=""
  RESUME_CODE="$(cat "${RESUME_CODE_FILE}" 2>/dev/null || true)"
}

# move_cluster <state> — write what read_status will report from now on.
# This is the whole mechanism behind the freshness legs: the grant's
# snapshot was taken against the previous answer.
move_cluster() { printf '%s\n' "$1" > "${BLOCKDIR}/state"; }

# show_field <session-id> <label> — one labelled line from `mast sessions
# show`. awk reads to EOF rather than exiting early: an early exit
# SIGPIPEs the CLI, which under pipefail kills the run with 141.
show_field() {
  "${BIN}" sessions show "$1" --session-db="${DB}" 2>/dev/null \
    | awk -v k="$2:" '$1 == k && !seen { sub(/^[[:space:]]*[^:]*:[[:space:]]*/, ""); print; seen = 1 }'
}

state_is() { [ "$(show_field "$1" State)" = "$2" ]; }

# model_calls <logfile> — the meter's own count for the last completed
# turn, off the daemon's `turn complete` record (budget.Meter.Snapshot).
#
# The count is read here rather than derived from anything else on
# purpose. Wallclock says how long the turn took, token totals say how
# much text moved; neither says how many times the provider was called,
# and both drift with the model, the prompt and the machine. A cost
# claim has to come from the thing that counts calls.
model_calls() {
  grep -o '"session_model_calls":[0-9]*' "$1" | tail -n 1 | cut -d: -f2
}

# metric_value <metric-with-labels> — one gauge/counter sample from the
# daemon's /metrics, exactly as an operator's scrape would read it. The
# second, independently computed view of the same number.
metric_value() {
  curl -sf -m 5 "${BASE}/metrics" 2>/dev/null | awk -v k="$1" '$1 == k { print $2 }'
}

# log_field <logfile> <message> <field> — one field off the FIRST JSON
# log line carrying <message>. The daemon logs JSON, so the cadence's
# own record — its anchor, the ticks it dropped, who it ran as — is
# readable straight out of the log; a fire counter kept by the harness
# would be a second account of the same events, free to disagree with
# the daemon's.
log_field() {
  { grep -F -- "$2" "$1" || true; } \
    | sed -n "1s/.*\"$3\":\"\{0,1\}\([^\",}]*\).*/\1/p"
}

# sched_ticks <logfile> — the cadence points a process actually fired,
# sorted, one per line, so two logs can be compared as sets.
sched_ticks() {
  { grep -F -- 'scheduled trigger fired' "$1" || true; } \
    | sed -n 's/.*"tick":"\([^"]*\)".*/\1/p' | sort -u
}

sched_fires_atleast() {
  local n
  n="$(grep -c -- 'scheduled trigger fired' "$1" || true)"
  [ "${n}" -ge "$2" ]
}

# sched_session <workload> <tick> — the session ID a tick's run owns,
# composed here by the same rule cmd/mast composes it with. A tick the
# daemon skipped has no session, which is how the harness asks `mast
# sessions list` whether a missed tick was quietly caught up.
sched_session() {
  printf 'scheduled-%s-%s\n' "$1" "$(printf '%s' "$2" | sed 's/[-:]//g; s/\.[0-9]*Z$/Z/')"
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

# ---- the change-set fixture: the handoff roster + freshness rules ---
# Same three specialists and the same two tools; the workload adds what
# W7 needs an operator to be able to answer for: a freshness window and
# a declared precondition for the write.
CHANGESET="${WORK}/changeset"
cp -r "${HANDOFF}" "${CHANGESET}"

# mk_changeset_workload <path> <ttl> — the fixture's workload with the
# change-set TTL as a parameter, so the expiry leg differs from the
# others in exactly one line.
mk_changeset_workload() {
  cat > "$1" <<YAML
# Harness fixture for scripts/uat-v0.4.sh's U-changeset legs. The
# U-handoff roster plus the two things a change-set grant is bounded by:
# a wall-clock window, and a read that says whether the world an
# operator approved against is still the world.
name: uat-changeset
description: Fixture workload for the mast v0.4 change-set grant legs.
mode: single_session

tool_catalog:
  mcp:
    - server: uat-blocker
  tools:
    - name: read_status
      mutating: false
    - name: apply_change
      mutating: true
      precondition:
        # No args and no args_from: read_status takes none. The declared
        # field is where the blocker reports the harness-controlled state
        # file (ADK wraps an MCP server's structured result under
        # "output"), which is how a leg moves the world between an
        # operator's approval and the calls it covers.
        read: read_status
        fields:
          - output.state

specialists:
  - classify
  - ApplyChange
  - _fallback
  - change-executor

budget:
  max_wallclock_seconds: 300

hitl:
  require_approval: false
  change_set_ttl: $2

edge_trigger:
  http:
    path: /inject
    auth: bearer
YAML
}
mk_changeset_workload "${CHANGESET}/workload.yaml" 10m

# The expiry leg's copy: identical but for a window shorter than the
# time the harness holds the first call open.
EXPIRING="${WORK}/expiring"
cp -r "${CHANGESET}" "${EXPIRING}"

# ---- the scheduled fixture: the v0.2 roster, on a cadence -----------
# Deliberately the plainest roster in the file — one coordinator, one
# worker, the same two blocker tools. W4.1 is about what wakes the
# workload, so anything else the roster did would be noise in the one
# leg that measures time.
SCHEDNAME=uat-sched
SCHED="${WORK}/scheduled"
cp -r "${FIXTURE}" "${SCHED}"

# mk_scheduled_workload <path> <on_mutation> <reason> — testdata/uat's
# workload with a cadence declared and nothing else changed.
#
# The prompt carries an incident-shaped reason because that is the lever
# the offline fake reads to pick a tool (pkg/agent/toolactor.go), the
# same one inject_uat pulls from the other side. It doubles as a check
# on the wake-up message itself: the prompt reaches the model as the
# author wrote it, braces and quotes intact, rather than escaped into a
# field of the envelope.
mk_scheduled_workload() {
  cat > "$1" <<YAML
# Harness fixture for scripts/uat-v0.4.sh's U-scheduled legs.
name: ${SCHEDNAME}
description: Fixture workload for the mast v0.4 scheduled-trigger legs.
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
  on_mutation: $2

edge_trigger:
  # The HTTP door stays open alongside the cadence: a scheduled workload
  # is still injectable, and a fixture that dropped the trigger could
  # not show it.
  http:
    path: /inject
    auth: bearer
  scheduled:
    interval: 3s
    # Zero, so a tick is predictable to the nanosecond. A real bundle
    # leaves this unset and gets a tenth of its interval; that the
    # offset is bounded and never accumulates into drift is a unit
    # test's claim (cmd/mast/schedtrigger_test.go), because a harness
    # that waited out a random delay would only be measuring the delay.
    jitter: 0s
    prompt: 'Sweep the fixture cluster: {"reason":"$3"} and remediate what you find.'
YAML
}
mk_scheduled_workload "${SCHED}/workload.yaml" apply ReadStatus

# The write-gate leg's copy: the same cadence, with the write gate on.
# require_approval is mast's default for a workload that says nothing;
# it is spelled out here because it is what the leg varies.
SCHEDGATE="${WORK}/scheduled-gate"
cp -r "${SCHED}" "${SCHEDGATE}"
mk_scheduled_workload "${SCHEDGATE}/workload.yaml" require_approval ApplyChange
mk_changeset_workload "${EXPIRING}/workload.yaml" 1s

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
# U-changeset (W7) — one answer authorizes the rest of the set
# ====================================================================
# Everything above parks one call at a time, which is W7.0's contract
# and an operator's night at 03:00: a five-call remediation is five
# questions, and the fifth arrives after the world has moved. W7 lets
# one answer carry the whole set — bounded by a clock AND by the cluster
# itself, because a wall-clock window cannot tell that a Deployment was
# scaled by someone else in the meantime.
#
# The set is two calls to the same tool with different arguments
# (replicas 2 then 3), which is the smallest thing "the REST of the set"
# can mean. The blocker's ledger distinguishes them, so every leg below
# can say which calls fired, not merely how many.
WL="${CHANGESET}"
DISPATCH=graph
SET_CHANGE='[{"tool":"apply_change","arguments":"{\"replicas\":2}"},{"tool":"apply_change","arguments":"{\"replicas\":3}"}]'

# ---- leg A: one question, the whole set -----------------------------
say "U-changeset/A: approving with scope=change_set authorizes the rest of the set"
DB="${WORK}/c-a.db"
LOG="${WORK}/c-a.log"
reset_blocker
export MAST_FAKE_PROPOSED_CHANGE="${SET_CHANGE}"
start_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat ca1 ApplyChange)" 202
assert_state "the first call parked" incident-ca1 paused

CAMSG="$(show_field incident-ca1 Message)"
note "the operator's question: ${CAMSG}"
# The question has to disclose what a change_set answer would cover.
# Approving a set whose extent the question never stated is the failure
# this leg's assertion exists to prevent.
assert_has "the question names the whole set" "${CAMSG}" "2 calls"
assert_has "and says how to answer for all of it" "${CAMSG}" "scope=change_set"

CAINT="$(show_field incident-ca1 Interrupt)"
assert_http "the operator approves the SET -> 202" \
  "$(resume_verdict incident-ca1 "${CAINT}" '{"verdict":"approve","scope":"change_set","note":"uat"}')" 202
assert_state "the run finishes" incident-ca1 idle

# The headline: both calls ran, and the operator was asked once.
assert_eq "both calls fired" "$(calls_count apply_change)" 2
CACALLS="$(calls_args apply_change)"
assert_has "the approved first call" "${CACALLS}" "replicas=2"
assert_has "and the one the grant authorized" "${CACALLS}" "replicas=3"
assert_log_count "the operator was asked exactly once" "${LOG}" 'awaiting_approval' 1
assert_log_count "one grant was minted" "${LOG}" 'change-set grants minted' 1
assert_log_count "the second call ran on that grant" "${LOG}" \
  'mutating tool call authorized by an approved change set' 1
# A grant is not a bypass: the policy check still runs in front of it,
# and spending one is recorded like any other authorized mutation.
assert_log_count "the grant spend is on the audit trail" "${LOG}" 'approved_by_change_set' 1
stop_term

# ---- leg B: the control — scope=once still asks per call ------------
# Same fixture, same set, one word different in the verdict. Without
# this leg, leg A could pass on a runtime that had simply stopped
# parking the second call for everyone.
say "U-changeset/B: the default scope still asks per call"
DB="${WORK}/c-b.db"
LOG="${WORK}/c-b.log"
reset_blocker
start_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat cb1 ApplyChange)" 202
assert_state "the first call parked" incident-cb1 paused
CBINT="$(show_field incident-cb1 Interrupt)"
assert_http "the operator approves just this call -> 202" \
  "$(resume_verdict incident-cb1 "${CBINT}" '{"verdict":"approve","note":"uat"}')" 202

assert_state "the second call parks in its own right" incident-cb1 paused
assert_eq "only the approved call fired" "$(calls_count apply_change)" 1
assert_has "and it was the first one" "$(calls_args apply_change)" "replicas=2"
assert_log_count "the operator was asked twice" "${LOG}" 'awaiting_approval' 2
assert_no_log "no grant was minted" "${LOG}" 'change-set grants minted'
stop_term

# ---- leg C: the world moves, the grant is void ----------------------
# The claim a TTL cannot make. The harness holds the first approved call
# open at the blocker, moves the cluster underneath it, and releases:
# the second call's precondition re-read no longer matches the snapshot
# taken when the operator answered, so mast re-asks instead of firing a
# call whose premise is gone.
say "U-changeset/C: a grant is void once the cluster moves"
DB="${WORK}/c-c.db"
LOG="${WORK}/c-c.log"
reset_blocker apply_change
start_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat cc1 ApplyChange)" 202
assert_state "the first call parked" incident-cc1 paused
CCINT="$(show_field incident-cc1 Interrupt)"
# Posted in the background: /resume runs the approved calls, and this
# leg has to act while they are running.
resume_bg incident-cc1 "${CCINT}" '{"verdict":"approve","scope":"change_set","note":"uat"}'
if wait_started apply_change; then
  ok "the approved call reached the tool"
else
  bad "the approved call never dispatched"
fi
move_cluster moved
release apply_change
resume_bg_wait
assert_http "the operator's answer was accepted -> 202" "${RESUME_CODE}" 202

assert_state "the rest of the set parked again" incident-cc1 paused
assert_eq "only the approved call fired" "$(calls_count apply_change)" 1
assert_has "and it was the first one" "$(calls_args apply_change)" "replicas=2"
assert_hasnt "the stale call never ran" "$(calls_args apply_change)" "replicas=3"
assert_log_count "the grant was voided" "${LOG}" \
  'a change-set grant was voided and its call re-parked' 1
assert_log_count "the operator was asked again" "${LOG}" 'awaiting_approval' 2
# The re-park says what moved, in the declared field's own terms. A
# question that just said "re-approve this" would leave an operator
# re-deciding with no more information than the first time.
CCMSG="$(show_field incident-cc1 Message)"
note "the second question: ${CCMSG}"
assert_has "the question says the approval stopped covering it" "${CCMSG}" "change set"
assert_has "and names the field that moved" "${CCMSG}" 'output.state was "steady"'
stop_term

# ---- leg D: the wall-clock backstop ---------------------------------
# The other clock. Same shape as leg C, except the world does NOT move —
# the window simply runs out while the first call is held open. A tool
# with no precondition has only this bound, so it has to work on its
# own, not merely as a second opinion on the read.
say "U-changeset/D: a grant expires on its own clock"
WL="${EXPIRING}"
DB="${WORK}/c-d.db"
LOG="${WORK}/c-d.log"
reset_blocker apply_change
start_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat cd1 ApplyChange)" 202
assert_state "the first call parked" incident-cd1 paused
CDINT="$(show_field incident-cd1 Interrupt)"
resume_bg incident-cd1 "${CDINT}" '{"verdict":"approve","scope":"change_set","note":"uat"}'
if wait_started apply_change; then
  ok "the approved call reached the tool"
else
  bad "the approved call never dispatched"
fi
# Outlast the fixture's 1s window while the first call is held.
sleep 2
release apply_change
resume_bg_wait
assert_http "the operator's answer was accepted -> 202" "${RESUME_CODE}" 202

assert_state "the expired call parked again" incident-cd1 paused
assert_eq "only the approved call fired" "$(calls_count apply_change)" 1
assert_log_count "the operator was asked again" "${LOG}" 'awaiting_approval' 2
assert_log_count "the grant was voided" "${LOG}" \
  'a change-set grant was voided and its call re-parked' 1
# Both bounds void a grant through the same path, so the leg is only
# about the clock if the RECORDED REASON is the clock. The cluster never
# moved here; a void blamed on the read would mean the TTL never fired
# and this leg was passing on leg C's mechanism.
assert_log_atleast "on its own clock" "${LOG}" 'that approval expired at' 1
assert_no_log "and not because the cluster moved" "${LOG}" \
  'the cluster moved since this was approved'
CDMSG="$(show_field incident-cd1 Message)"
note "the second question: ${CDMSG}"
assert_has "the question says the window ran out" "${CDMSG}" "expired at"
stop_term

# ---- leg E: the approval outlives the process -----------------------
# mast's whole shape is unattended: the daemon that asked the question
# is not necessarily the one that hears the answer. The crash window
# here is the park itself — nothing is in flight, the session is durable
# — so what has to survive is the recorded change set and the operator's
# ability to authorize all of it on a process that never saw the
# diagnoser's turn.
#
# (A crash BETWEEN two granted calls is a different subject: it leaves a
# dangling mutating intent, which the recorded-effect outbox declines to
# replay — scripts/uat-v0.2.sh S1.)
say "U-changeset/E: an approval that arrives after a restart still covers the set"
WL="${CHANGESET}"
DB="${WORK}/c-e.db"
LOG="${WORK}/c-e.log"
reset_blocker
start_daemon "${WORK}/c-e-boot.log"

assert_http "inject ApplyChange -> 202" "$(inject_uat ce1 ApplyChange)" 202
assert_state "the first call parked" incident-ce1 paused
kill9

start_daemon "${LOG}"
assert_state "the park survived the crash" incident-ce1 paused
CEINT="$(show_field incident-ce1 Interrupt)"
assert_http "the new process accepts the SET approval -> 202" \
  "$(resume_verdict incident-ce1 "${CEINT}" '{"verdict":"approve","scope":"change_set","note":"uat"}')" 202
assert_state "the run finishes" incident-ce1 idle
assert_eq "both calls fired" "$(calls_count apply_change)" 2
assert_has "including the one only the grant authorized" "$(calls_args apply_change)" "replicas=3"
assert_log_count "the restarted process asked nothing new" "${LOG}" 'awaiting_approval' 0
stop_term
unset MAST_FAKE_PROPOSED_CHANGE

# ---- U-decisions: the verdict outlives the incident -----------------
# W8. Everything above proves mast obeyed the operator. This leg asks
# the other question: a week later, can anyone read what the operator
# decided? The export runs against the same SQLite file with NO daemon
# alive, because a harvest that needs the process that made the
# decisions is a harvest nobody will ever run.
say "U-decisions: every adjudication exports as a labelled record"
WL="${CHANGESET}"
DB="${WORK}/dec.db"
LOG="${WORK}/dec.log"
reset_blocker
export MAST_FAKE_PROPOSED_CHANGE="${SET_CHANGE}"
start_daemon "${LOG}"

assert_http "inject ApplyChange -> 202" "$(inject_uat d1 ApplyChange)" 202
assert_state "the first call parked" incident-d1 paused
DINT="$(show_field incident-d1 Interrupt)"
assert_http "the operator approves the SET -> 202" \
  "$(resume_verdict incident-d1 "${DINT}" '{"verdict":"approve","scope":"change_set","note":"uat"}')" 202
assert_state "the run finishes" incident-d1 idle
stop_term
unset MAST_FAKE_PROPOSED_CHANGE

DEC="$("${BIN}" sessions export-decisions incident-d1 --session-db="${DB}")"
# One row per CALL, not per question. The set was approved once and ran
# twice; a file showing one approval where two mutations happened is the
# dataset defect this assertion exists to catch.
assert_eq "one record per adjudicated call" \
  "$(printf '%s\n' "${DEC}" | grep -c '"decided_at"' || true)" 2
assert_has "the header counts them" "${DEC}" '"records":2'
assert_has "the operator's own verdict is one record" "${DEC}" '"authority":"operator_verdict"'
assert_has "the granted call is the other" "${DEC}" '"authority":"change_set_grant"'
assert_has "both are recorded as authorized" "${DEC}" '"disposition":"authorized"'
assert_has "the arguments are the label" "${DEC}" '"replicas":2'
# Redaction is the default, not an option to remember.
assert_has "the file says how it was redacted" "${DEC}" '"redaction":"approver_digest"'
assert_hasnt "the default export does not name the approver" "${DEC}" 'shared-bearer-token'
assert_has "it carries a stable digest instead" "${DEC}" '"approver":"sha256:'

RAW="$("${BIN}" sessions export-decisions incident-d1 --session-db="${DB}" --include-approver)"
assert_has "--include-approver names the approver" "${RAW}" 'shared-bearer-token'
assert_has "and the header says the file is raw" "${RAW}" '"redaction":"none"'

# ====================================================================
# U-bounded-cost (W4.3 /steps) — what one cycle costs
# ====================================================================
# The subject is the SHIPPED bundle, examples/workloads/bounded-triage,
# not a derived fixture. The claim is about what an operator gets when
# they run what mast ships; a fixture would only show that this harness
# can write a one-specialist roster.
BOUNDED="${REPO}/examples/workloads/bounded-triage"

# ---- /steps: the cycle is one model call ----------------------------
say "U-bounded-cost/steps: one incident costs exactly one model call"
WL="${BOUNDED}"
# Deliberately unset: the bundle's own `dispatch: bounded` has to be
# what selects the shape, or this leg would be testing the flag.
DISPATCH=
DB="${WORK}/b-steps.db"
LOG="${WORK}/b-steps.log"
start_daemon "${LOG}"

assert_http "inject CrashLoopBackOff -> 202" "$(inject_uat bs1 CrashLoopBackOff)" 202
assert_state "the run finished on its own" incident-bs1 idle
assert_has "the bundle's own dispatch: selected the shape" \
  "$(grep -- 'root agent constructed' "${LOG}" || true)" 'bounded-triage_bounded'

# The assertion the workstream exists for, read off the meter.
assert_eq "the cycle cost one model call" "$(model_calls "${LOG}")" 1
# And the other surface. The two are computed independently — the log
# field from budget.Meter.Snapshot() when the turn ends, the counter
# from the observability registry as events stream — so their agreeing
# is what makes the number a fact about the run rather than about one
# accounting path.
assert_eq "and the exported counter agrees" \
  "$(metric_value 'mast_model_calls_total{workload="bounded-triage"}')" 1

# One call, and it answered: a shape that spends nothing because it did
# nothing would pass the count assertion on its own.
BSEV="$(grep -- 'runner event' "${LOG}" || true)"
assert_has "the one call returned the finding contract" "${BSEV}" 'severity'
assert_has "with a value from the enum the schema declares" "${BSEV}" 'critical'
assert_has "and the change-set field the contract carries" "${BSEV}" 'proposed_change'
# No router turn in front of it and no tool loop under it. Both would be
# extra model calls, so both are asserted absent by their traces and not
# only by the count.
assert_has "the report came from the one specialist" "${BSEV}" '"author":"incident-report"'
assert_no_log "no finish_task loop ran" "${LOG}" 'function_call:finish_task'
stop_term

# ---- /steps: the count is a promise, so it is enforced --------------
# A number is only a guarantee if the rosters that could not keep it are
# refused. Same binary, same flag, pointed at the shipped fourteen-
# specialist gke-triage roster: the daemon must not come up at all.
say "U-bounded-cost/steps: a roster that would need an orchestrator will not start"
DB="${WORK}/b-refuse.db"
LOG="${WORK}/b-refuse.log"
if start_refused "${LOG}" "${REPO}/examples/workloads/gke-triage" bounded; then
  ok "the daemon refused the roster instead of serving it"
else
  bad "the daemon came up on a roster the bounded shape cannot keep its promise for"
fi
BREF="$(grep -- 'failed to construct root agent' "${LOG}" || true)"
# The daemon can exit nonzero for reasons that have nothing to do with
# the roster — this leg runs under --model=toolactor, which unlike echo
# does wire the workload's MCP servers — so say what it actually
# reported when the expected refusal is absent. Reading that off three
# "missing: <substring>" lines cost a CI round trip once already, and
# the roster check moving ahead of MCP (compose.CheckRoster) is what
# makes the leg credential-free rather than merely credential-free
# here.
[ -n "${BREF}" ] || note "no refusal logged; last line was: $(tail -n 1 "${LOG}")"
assert_has "the refusal counts what it found" "${BREF}" '14 specialists'
assert_has "and names them" "${BREF}" 'triage-classifier'
assert_has "and says what the shape takes instead" "${BREF}" 'takes exactly one'

# ---- /collect: zero-token collection (W4.2, v0.5) -------------------
# Stubbed rather than left unwritten: `U-bounded-cost` is the proof for
# two scoreboard rows, and the halves flip on different workstreams. A
# leg that exists and says it is empty is harder to forget than a name
# in a table.
say "U-bounded-cost/collect: zero-token collection (W4.2 — not shipped yet)"
note "SKIPPED: nothing gathers cluster data without a model call yet."
note "  When W4.2 lands this leg asserts the collection step's own count"
note "  is ZERO on the same two meter surfaces /steps reads. It flips"
note "  scoreboard row 9 (docs/v0.3-plan.md §1); /steps flips row 10."

# ====================================================================
# U-scheduled (W4.1) — the workload wakes itself, and keeps its phase
# ====================================================================
# Every leg above needs something to call the daemon. This one needs
# nothing to: the bundle declares an interval, and the runs happen
# because time passed. What has to survive a restart is therefore not a
# session but a CADENCE — the anchor the ticks are counted from — and
# the two ways to get that wrong are opposites. Re-anchoring on boot
# re-phases the schedule (a 02:00 sweep becomes a 14:00 sweep after an
# afternoon rollout); catching up on boot fires every tick the outage
# spanned, which is a crash-looping daemon buying a backlog of model
# runs about the crash.
WL="${SCHED}"
DISPATCH=coordinator

# ---- leg A: the cadence survives a crash, the missed ticks do not ---
say "U-scheduled/A: a restarted daemon resumes the cadence and skips what it missed"
DB="${WORK}/s-a.db"
LOG="${WORK}/s-a-boot.log"
LOG2="${WORK}/s-a.log"
reset_blocker
start_daemon "${LOG}"

if wait_for 60 sched_fires_atleast "${LOG}" 2; then
  ok "the workload woke itself twice with nothing calling it"
else
  bad "the cadence did not fire twice (got $(grep -c 'scheduled trigger fired' "${LOG}" || true))"
fi
ANCHOR="$(log_field "${LOG}" 'scheduled trigger anchored' anchor)"
if [ -n "${ANCHOR}" ]; then ok "the first process anchored the cadence (${ANCHOR})"; else bad "no anchor was logged"; fi
kill9

# Down across two ticks, at least. The gap is what leg A is about: a
# crash the schedule has to be read back out of SQLite to survive, with
# ticks coming due while nothing is running.
sleep 7
start_daemon "${LOG2}"

assert_no_log "the restart did not re-anchor" "${LOG2}" 'scheduled trigger anchored'
assert_eq "it resumed the original anchor" \
  "$(log_field "${LOG2}" 'resumed its persisted cadence' anchor)" "${ANCHOR}"
assert_log_count "the missed ticks are reported once, not once each" "${LOG2}" \
  'scheduled ticks skipped rather than caught up' 1

SKIPPED="$(log_field "${LOG2}" 'ticks skipped rather than caught up' ticks)"
if [ "${SKIPPED:-0}" -ge 2 ]; then
  ok "the outage's ticks were counted (${SKIPPED})"
else
  bad "the daemon reported ${SKIPPED:-no} skipped ticks over an outage spanning at least 2"
fi

# The claim the count alone cannot make: a skipped tick has no run. Each
# fire owns a session named for its tick, so the ticks the daemon says
# it dropped must be absent from the session list — a catch-up would put
# them there, backdated and all.
SKIPFROM="$(log_field "${LOG2}" 'ticks skipped rather than caught up' from)"
SKIPTHRU="$(log_field "${LOG2}" 'ticks skipped rather than caught up' through)"
SESSIONS="$("${BIN}" sessions list --session-db="${DB}" 2>/dev/null || true)"
if [ -z "${SKIPFROM}" ] || [ -z "${SKIPTHRU}" ]; then
  # Fail closed: with no window to name, the two assertions below would
  # be searching the session list for a bare prefix.
  bad "the daemon did not say which ticks it skipped"
else
  assert_hasnt "the first missed tick never ran" "${SESSIONS}" "$(sched_session "${SCHEDNAME}" "${SKIPFROM}")"
  assert_hasnt "nor the last one" "${SESSIONS}" "$(sched_session "${SCHEDNAME}" "${SKIPTHRU}")"
fi

if wait_for 60 sched_fires_atleast "${LOG2}" 1; then
  ok "the restarted process fires again on its own"
else
  bad "the cadence never resumed after the restart"
fi

# On the ORIGINAL lattice, not a new one. Ticks are anchor + k×interval
# exactly, so every fire carries the anchor's sub-second remainder; a
# daemon that had re-anchored would be firing on a fresh wall-clock
# instant whose nanoseconds are its own.
FRAC="${ANCHOR##*.}"
sched_ticks "${LOG2}" > "${WORK}/s-a-ticks2"
assert_eq "every fire after the restart lands on the original lattice" \
  "$(grep -vc -- "\.${FRAC}\$" "${WORK}/s-a-ticks2" || true)" 0

# And no tick was run twice. This is the assertion two separate log
# files exist for: the restarted process's fires can be compared as a
# set against the dead process's.
sched_ticks "${LOG}" > "${WORK}/s-a-ticks1"
assert_eq "no tick fired in both processes" \
  "$(comm -12 "${WORK}/s-a-ticks1" "${WORK}/s-a-ticks2" | wc -l | tr -d ' ')" 0
stop_term

# ---- leg B: unattended is not unsupervised --------------------------
say "U-scheduled/B: a mutating call in a scheduled run still parks"
WL="${SCHEDGATE}"
DB="${WORK}/s-b.db"
LOG="${WORK}/s-b.log"
reset_blocker
start_daemon "${LOG}"

if wait_for 60 sched_fires_atleast "${LOG}" 1; then
  ok "the cadence fired"
else
  bad "the cadence never fired"
fi
# Who the run belongs to, in the daemon's own words. A scheduled turn
# has no request to take a caller from, and "mast:scheduler" is a name
# no human identity can take — so an approval attributed to it could
# never be read as somebody's.
assert_eq "the run identified itself as the scheduler" \
  "$(log_field "${LOG}" 'scheduled trigger fired' caller)" 'mast:scheduler'

SBSID="$(log_field "${LOG}" 'scheduled trigger fired' session)"
assert_state "the scheduled run's write parked for an operator" "${SBSID}" paused
assert_eq "nothing was applied on the scheduler's own authority" "$(calls_count apply_change)" 0
SBMSG="$(show_field "${SBSID}" Message)"
note "the operator's question: ${SBMSG}"
assert_has "the question is the write itself" "${SBMSG}" "apply_change"

# The other half: a scheduled run is an ordinary citizen, so a real
# operator can answer it and the call then fires, exactly as it would
# have from an injected run.
SBINT="$(show_field "${SBSID}" Interrupt)"
assert_http "an operator can approve it -> 202" \
  "$(resume_verdict "${SBSID}" "${SBINT}" '{"verdict":"approve","note":"uat"}')" 202
assert_state "the answered run finishes" "${SBSID}" idle
assert_eq "the approved call fired, exactly once" "$(calls_count apply_change)" 1
stop_term

# ====================================================================
say "Summary"
printf '   %d passed, %d failed\n' "${PASS}" "${FAIL}"
if [ "${FAIL}" -gt 0 ]; then exit 1; fi
