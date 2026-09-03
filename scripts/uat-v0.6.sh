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

# uat-v0.6.sh — end-to-end acceptance pass for mast v0.6
# (docs/v0.6-plan.md). One subject:
#
#   a ceiling stops a call before it is paid for, and a specialist's
#   ceiling is not the workload's.
#
# Legs (all offline: no credentials, no network):
#
#   U-precall (W10.1, W10.2) — the ceiling moved in front of the call.
#       A  the counterfactual, and the reason this leg self-calibrates:
#          an UNCAPPED incident runs to the end and the harness reads
#          what the cycle cost. Nothing is asserted about the number
#          itself — it is the fake's arithmetic, and pinning it here
#          would make every future change to the fixture look like a
#          budget regression.
#       B  the same incident under a cap of exactly one call fewer. It
#          spends the cap and NOT ONE CALL MORE, on both surfaces. This
#          is the whole of W10.2 in one number: `Observe` checks turns
#          with `>`, so through v0.5 a workload capped at N always made
#          N+1 — it had to spend the call to discover it could not
#          afford it. The counter reading N here, and never N+1, is that
#          behaviour's absence.
#       C  and the refusal is a REPORT. It carries the marker an operator
#          reads, the daemon answers the incident with a budget failure,
#          and no runner error is logged: a cap that fires must not
#          arrive looking like a crashed tool.
#
#   U-scoped (W10.3) — one path closed is not the workload over.
#       A  a roster of two change executors, the first capped at one
#          turn. The capped path is refused mid-way; the coordinator
#          routes around it; the incident FINISHES, through the other
#          specialist, and the tool ledger proves the second path really
#          ran rather than the first being retried. Through v0.5 this
#          session was dead — not by decision, but because cancelling
#          the run was the only lever the post-hoc fold had.
#       B  a quiet ending is the failure mode this creates, so the loss
#          is loud: a WARN naming the specialist, and the same trip
#          counter an operator already alerts on.
#       C  the operator surface says which path is out — `cost_ceiling`
#          reports the session NOT tripped and the specialist tripped in
#          the same body. A view with one `tripped` flag would have to
#          pick, and either answer is a lie.
#       D  and a reset the operator will actually type: raising the
#          SESSION's ceiling reports honestly that it cleared nothing
#          and names who is still out, with the argument to raise them.
#
#   U-dispatch (W9.1, W9.2, W9.3) — what crosses the dispatch boundary.
#       A  a planner roster whose change executor would write through a
#          gate that cannot reach it does not start. The refusal names
#          all three ways out, because a permanent refusal that leaves
#          the operator guessing is a worse answer than the hole.
#       B  under `on_mutation: apply` — the one policy the refusal
#          exempts, and what an unattended workload actually sets — the
#          daemon is SIGKILLed with a dispatched mutating call in
#          flight. On restart the scan sees TWO dangling intents: the
#          planner's own dispatch, which is in the session log, and the
#          specialist's `apply_change`, which is in no event of it. The
#          second one is v0.6's; before W9.3 a dispatched write left no
#          trace at all, and the session came back looking clean enough
#          to resume over a mutation that may or may not have landed.
#
# What this harness does NOT re-measure: the pre-call gate's effect on a
# retry loop above the model. scripts/uat-v0.4.sh's producer-contract
# legs own that one, and they had to change contract for it (a spent
# specialist stopped being a 500) — moving the assertion here would
# leave those legs green against nothing.
#
# The meters are the two an operator already scrapes: the daemon's
# `session_model_calls` log field (budget.Meter.Snapshot) and
# `mast_model_calls_total` on /metrics, computed independently of each
# other. The blocker's own call ledger is the third witness, and the only
# one that answers "did this tool actually run" rather than "did the
# daemon think it ran".
#
# Usage: scripts/uat-v0.6.sh
# State goes under ${TMPDIR:-/tmp}/mast-uat-v06 (house rule #5); ports
# 7793 for the daemon and 7794 for the operator attach surface.

set -euo pipefail

PORT="${MAST_UAT_V06_PORT:-7793}"
BASE="http://127.0.0.1:${PORT}"
ATTACH_PORT="$((PORT + 1))"
ATTACH="http://127.0.0.1:${ATTACH_PORT}"
WORK="${TMPDIR:-/tmp}/mast-uat-v06"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${WORK}/mast"
FIXTURE="${REPO}/testdata/uat"
TOKEN="uat-v06-token"
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
# The same stdio MCP server the v0.2 … v0.5 harnesses drive: it declares
# a real input schema, records every call it receives, and can be held
# open on demand — which is what gives U-dispatch/B a deterministic
# window to kill the daemon inside a dispatched mutation.
BLOCKER="${WORK}/blocker"
BLOCKDIR="${WORK}/blockdir"
export MAST_UAT_BLOCKER="${BLOCKER}"
export UAT_BLOCKER_DIR="${BLOCKDIR}"

# reset_blocker [hold-tool] — fresh ledger, both tools pre-released
# unless one is named to be held.
reset_blocker() {
  local hold="${1:-}"
  rm -rf "${BLOCKDIR}"
  mkdir -p "${BLOCKDIR}"
  [ "${hold}" = "apply_change" ] || : > "${BLOCKDIR}/apply_change.release"
  [ "${hold}" = "read_status" ]  || : > "${BLOCKDIR}/read_status.release"
}

calls_count() {
  local f="${BLOCKDIR}/$1.calls"
  if [ -f "${f}" ]; then wc -l < "${f}" | tr -d ' '; else echo 0; fi
}

# ---- lifecycle ------------------------------------------------------
# --model=toolactor rather than echo for the reason the v0.4 and v0.5
# harnesses record: wireMCPToolsets skips MCP entirely under echo, so an
# echo daemon has no tool schemas at all and every leg here would be
# green or red for the wrong reason.
DISPATCH=coordinator
ATTACH_ON=""
start_daemon() {
  local log="$1"; shift
  local attach=()
  [ -n "${ATTACH_ON}" ] && attach=(--attach-listen="127.0.0.1:${ATTACH_PORT}")
  "${BIN}" --workload="${WL}" --dispatch="${DISPATCH}" \
    --listen=":${PORT}" --model=toolactor --session-db="${DB}" \
    --log-level=info "${attach[@]}" "$@" >"${log}" 2>&1 &
  PID=$!
  local i
  for i in $(seq 1 100); do
    if ! kill -0 "${PID}" 2>/dev/null; then break; fi
    if curl -sf -m 1 "${BASE}/" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  echo "daemon failed to start; log:" >&2; cat "${log}" >&2; exit 1
}

# start_refused <logfile> <workload> — run the daemon expecting it to
# REFUSE the roster and exit. Returns 0 when it did. A daemon that comes
# up instead is killed and the caller reports a failure: the whole value
# of a startup refusal is that it lands before the workload is serving,
# so "it errored later" is a different result.
start_refused() {
  local log="$1" wl="$2" i rc=0
  "${BIN}" --workload="${wl}" --dispatch=coordinator \
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

# term_nowait — SIGTERM and DO NOT reap. beginDrain durably marks every
# in-flight session interrupted BEFORE it waits on the turn, and that
# mark is what the next boot's scan finds; a bare SIGKILL leaves none,
# so there is no candidate and the whole restart measures nothing. Not
# reaped because the turn here is wedged on a held tool and the daemon
# would otherwise sit out its entire drain bound.
term_nowait() { kill -TERM "${PID}" 2>/dev/null || true; }

# kill9 — no drain, no flush, nothing written on the way out that a
# restart could lean on. The crash U-dispatch/B needs.
kill9() {
  kill -9 "${PID}" 2>/dev/null || true
  wait "${PID}" 2>/dev/null || true
  PID=""
}

# wait_started <tool> — block until the blocker reports the tool has
# dispatched. A sleep here would make the kill a race.
wait_started() {
  local marker="${BLOCKDIR}/$1.started" i
  for ((i = 0; i < 400; i++)); do
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

# inject_bg <uid> <reason> — the same POST, backgrounded. /inject runs the
# turn on the request context and writes 202 only when it finishes, so a
# leg that acts WHILE the turn is running (kill the daemon mid-tool)
# cannot wait for the response first. Sets BGPID; must not be called in a
# command substitution, or curl would be a child of the subshell and
# unreapable here.
BGPID=""
inject_bg() {
  ( inject_uat "$1" "$2" >"${WORK}/inject-$1.code" 2>/dev/null || true ) &
  BGPID=$!
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

# model_calls <logfile> — the meter's count for the last COMPLETED turn.
# A turn a ceiling stopped never logs `turn complete`, which is why the
# capped arm reads the counter and the refusal record instead.
model_calls() {
  grep -o '"session_model_calls":[0-9]*' "$1" | tail -n 1 | cut -d: -f2
}

# metric_value <metric-with-labels> — one sample off /metrics, exactly as
# an operator's scrape reads it. Computed independently of the log field
# above (registry vs. meter snapshot), which is what makes the two
# agreeing a fact about the run rather than about one accounting path.
metric_value() {
  curl -sf -m 5 "${BASE}/metrics" 2>/dev/null | awk -v k="$1" '$1 == k { print $2 }'
}

# log_field <logfile> <message> <field> — one field off the FIRST JSON log
# line carrying <message>.
log_field() {
  { grep -F -- "$2" "$1" || true; } \
    | sed -n "1s/.*\"$3\":\"\{0,1\}\([^\",}]*\).*/\1/p"
}

# guardrails <session-id> — GET the operator's guardrail view, raw JSON.
guardrails() { curl -sf -m 5 "${ATTACH}/sessions/$1/guardrails" 2>/dev/null || true; }

# guardrails_reset <session-id> <json-body> — POST a reset, raw JSON back.
guardrails_reset() {
  curl -s -m 5 -X POST "${ATTACH}/sessions/$1/guardrails/reset" \
    -H 'Content-Type: application/json' -d "$2" 2>/dev/null || true
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

# Scrape first, match second. `curl | grep -Fq` is the PR #196 shape:
# grep exits on its first match, curl takes SIGPIPE, and pipefail
# promotes 141 over grep's 0 — so a match reports as a miss and a
# wait_for on it spins out its whole budget.
metric_has() {
  local body
  body="$(curl -sf -m 5 "${BASE}/metrics" 2>/dev/null || true)"
  grep -Fq -- "$1" <<<"${body}"
}

# ---- assertions -----------------------------------------------------
assert_eq() {
  if [ "$2" = "$3" ]; then ok "$1 (${2})"; else bad "$1 (got '${2:-<none>}', want '$3')"; fi
}

assert_http() {
  if [ "$2" = "$3" ]; then ok "$1 (HTTP $2)"; else bad "$1 (HTTP $2, want $3)"; fi
}

# Here-strings, not `printf ... | grep -Fq`: this script runs under
# `pipefail`, `grep -q` exits on its first match, and the writer's SIGPIPE
# (141) is then promoted over grep's 0 — so a match can read as a miss
# (PR #196).
assert_has() {
  if grep -Fq -- "$3" <<<"$2"; then ok "$1"; else bad "$1 — missing: $3"; fi
}

# assert_hasnt fails CLOSED on an empty haystack: absence proves nothing
# when there was nothing to search.
assert_hasnt() {
  if [ -z "$2" ]; then bad "$1 — nothing to search (empty)"; return 0; fi
  if grep -Fq -- "$3" <<<"$2"; then bad "$1 — present: $3"; else ok "$1"; fi
}

assert_state() {
  if wait_for 30 state_is "$2" "$3"; then
    ok "$1 (state=$3)"
  else
    bad "$1 (state=$(show_field "$2" State), want $3)"
  fi
}

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

# mk_specialist <dir> <name> <max-turns> — one change-executor template.
#
# change_executor on all of them because every roster here can reach
# apply_change, and mast refuses a read_only specialist that can
# (internal/compose.CheckCapabilitySplit). None declares an
# `output_schema`, which is load-bearing for U-scoped: a schemad
# specialist's refusal resolves the delegation to nothing (pkg/agent's
# UnreportableRefusal, W10.3), and there would be nothing for a
# coordinator to recognize as a closed path. The unschemad case is the
# one where routing around is even possible, so it is the one to measure.
mk_specialist() {
  local dir="$1" name="$2" turns="${3:-}"
  mkdir -p "${dir}/specialists"
  {
    echo "---"
    echo "name: ${name}"
    echo "description: |"
    echo "  UAT change executor for scripts/uat-v0.6.sh. Drives the"
    echo "  reason-selected fixture tool and finishes."
    echo "mode: Task"
    echo "capability: change_executor"
    if [ -n "${turns}" ]; then
      echo "budget:"
      echo "  max_turns: ${turns}"
    fi
    echo "tools:"
    echo "  mcp:"
    echo "    - server: uat-blocker"
    echo "      tools:"
    echo "        - read_status"
    echo "        - apply_change"
    echo "---"
    echo
    echo "You are a UAT change executor. Handle the work item you are"
    echo "handed, then call finish_task. This is a test fixture."
  } > "${dir}/specialists/${name}.tmpl"
}

# ---- U-precall: one incident, with and without a ceiling ------------
# Two bundles that differ in exactly one line. Written as two directories
# rather than one edited in place so both are on disk when a leg fails
# and someone has to read them.
PRECALL="${WORK}/precall"
CAPPED="${WORK}/capped"
for d in "${PRECALL}" "${CAPPED}"; do
  mkdir -p "${d}"
  cp "${FIXTURE}/mcp.json" "${d}/mcp.json"
  mk_specialist "${d}" uat-v06-exec
done

cat > "${PRECALL}/workload.yaml" <<'YAML'
# Harness fixture for scripts/uat-v0.6.sh's U-precall/A leg — the
# uncapped arm, whose only job is to say what the cycle costs.
name: uat-v06-precall
description: Fixture workload for the mast v0.6 pre-call ceiling legs (uncapped arm).
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
  - uat-v06-exec

budget:
  # Wallclock only. Any other ceiling here would make this arm a second
  # measurement of a cap rather than the baseline the capped arm is
  # measured against.
  max_wallclock_seconds: 120

hitl:
  on_mutation: apply

edge_trigger:
  http:
    path: /inject
    auth: bearer
YAML

# ---- U-scoped: two paths, one of them spent -------------------------
# The capped specialist is FIRST in the roster, so the coordinator
# reaches it first and the route-around is the thing being measured
# rather than an ordering accident. One turn is the smallest cap that
# still lets the specialist do something before it is stopped: it drives
# read_status, and is refused on the call that would have reported.
SCOPED="${WORK}/scoped"
mkdir -p "${SCOPED}"
cp "${FIXTURE}/mcp.json" "${SCOPED}/mcp.json"
mk_specialist "${SCOPED}" uat-v06-capped 1
mk_specialist "${SCOPED}" uat-v06-spare

cat > "${SCOPED}/workload.yaml" <<'YAML'
# Harness fixture for scripts/uat-v0.6.sh's U-scoped legs.
name: uat-v06-scoped
description: Fixture workload for the mast v0.6 scoped-refusal legs.
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
  - uat-v06-capped
  - uat-v06-spare

budget:
  # No session ceiling on purpose: the only ceiling in this workload is
  # one specialist's, so a session that stops has stopped for the wrong
  # reason and the leg says so instead of passing quietly.
  max_wallclock_seconds: 120

hitl:
  on_mutation: apply

edge_trigger:
  http:
    path: /inject
    auth: bearer
YAML

# ---- U-dispatch: the planner, gated and ungated ---------------------
# Same roster and the same one mutating tool, twice, differing only in
# hitl.on_mutation — which is the entire subject. Under
# `require_approval` the daemon must not start; under `apply` it must,
# and must record what it dispatched.
DGATED="${WORK}/dispatch-gated"
DAPPLY="${WORK}/dispatch-apply"
for d in "${DGATED}" "${DAPPLY}"; do
  mkdir -p "${d}"
  cp "${FIXTURE}/mcp.json" "${d}/mcp.json"
  mk_specialist "${d}" uat-v06-exec
done

mk_planner_bundle() {
  cat > "$1/workload.yaml" <<YAML
# Harness fixture for scripts/uat-v0.6.sh's U-dispatch legs.
name: $2
description: Fixture workload for the mast v0.6 dispatch-boundary legs, on_mutation $3.
mode: single_session

planner:
  enabled: true

tool_catalog:
  mcp:
    - server: uat-blocker
  tools:
    - name: read_status
      mutating: false
    - name: apply_change
      mutating: true

specialists:
  - uat-v06-exec

budget:
  max_wallclock_seconds: 120

hitl:
  on_mutation: $3

edge_trigger:
  http:
    path: /inject
    auth: bearer
YAML
}
mk_planner_bundle "${DGATED}" uat-v06-dispatch-gated require_approval
mk_planner_bundle "${DAPPLY}" uat-v06-dispatch-apply apply

# ====================================================================
# U-precall (W10.1, W10.2) — the ceiling moved in front of the call
# ====================================================================

# ---- leg A: what one incident costs when nothing stops it -----------
say "U-precall/A: an uncapped incident runs to the end, and says what it cost"
WL="${PRECALL}"
DB="${WORK}/pre-a.db"
LOG="${WORK}/pre-a.log"
reset_blocker
start_daemon "${LOG}"

assert_http "the incident is accepted and finishes" "$(inject_uat pa1 ReadStatus)" 202
assert_state "the run ended" incident-pa1 idle
BASELINE="$(model_calls "${LOG}")"
assert_eq "the two meters agree on the cost" \
  "$(metric_value 'mast_model_calls_total{workload="uat-v06-precall"}')" "${BASELINE}"
assert_eq "nothing was refused" \
  "$(metric_value 'mast_budget_trips_total{workload="uat-v06-precall"}')" 0
stop_term

# The cap is derived, not written down. A hard-coded number would turn
# any future change to the fake's turn sequence into a budget failure,
# which is the one thing this leg must not be able to report.
if [ -z "${BASELINE}" ] || [ "${BASELINE}" -lt 2 ]; then
  bad "the uncapped arm reported '${BASELINE:-<none>}' model calls; a cap of one fewer is not a measurable thing"
  CAP=1
else
  CAP=$((BASELINE - 1))
  note "baseline ${BASELINE} model calls; the capped arm runs at max_turns: ${CAP}"
fi

sed "s/^name: uat-v06-precall$/name: uat-v06-capped/; \
     s/^  max_wallclock_seconds: 120$/  max_wallclock_seconds: 120\n  max_turns: ${CAP}/" \
  "${PRECALL}/workload.yaml" > "${CAPPED}/workload.yaml"

# ---- leg B: the cap is spent, and not exceeded ----------------------
say "U-precall/B: a capped incident spends its cap and not one call more"
WL="${CAPPED}"
DB="${WORK}/pre-b.db"
LOG="${WORK}/pre-b.log"
reset_blocker
start_daemon "${LOG}"

# 500, and 500 is the point: the pre-call refusal leaves the stream
# ending cleanly and the session holding an answer, so without the
# refusal delta the daemon would report a turn that never did its work as
# OK (W10.2). The status code is the caller-visible half of that fix.
assert_http "the incident fails rather than silently under-running" "$(inject_uat pb1 ReadStatus)" 500

# The assertion the workstream exists for. `Observe` checks turns with
# `>`, so v0.5 spent this call, priced it, wrote it to the ledger and
# only then aborted; the number here would have been ${BASELINE}.
assert_eq "the workload made exactly its cap of model calls" \
  "$(metric_value "mast_model_calls_total{workload=\"uat-v06-capped\"}")" "${CAP}"
assert_eq "and the meter's own count agrees" \
  "$(log_field "${LOG}" 'refused a model call before it was made' model_calls)" "${CAP}"
assert_eq "the trip is on the counter an operator alerts on" \
  "$(metric_value 'mast_budget_trips_total{workload="uat-v06-capped"}')" 1

# ---- leg C: the refusal is a report, not a crash --------------------
say "U-precall/C: the refusal reaches the transcript as a report"
assert_log_atleast "the ceiling said so at ERROR" "${LOG}" \
  'BUDGET CEILING — refused a model call before it was made' 1
assert_has "the record names the ceiling that was reached" \
  "$(grep -- 'refused a model call' "${LOG}" || true)" 'of a cap of'
# The marker pkg/agent exports for exactly this: an operator reading the
# transcript finds a sentence, not an absence.
assert_log_atleast "and the transcript carries the operator-readable refusal" "${LOG}" \
  'STOPPED — this agent reached a cost ceiling.' 1
# A refusal is not an error and must not arrive as one. ADK puts an
# error in the field it reserves for a broken tool, and pkg/planner's
# dispatch already refused to emit that shape for a crossed cap; a
# pre-call ceiling one layer down does not get to contradict it.
assert_no_log "the run reported no error — nothing crashed" "${LOG}" 'runner emitted error'
assert_no_log "and nothing was cancelled out from under a call" "${LOG}" 'BUDGET EXCEEDED'
stop_term

# ====================================================================
# U-scoped (W10.3) — one path closed is not the workload over
# ====================================================================
WL="${SCOPED}"
ATTACH_ON=1

# ---- leg A: the workload finishes through the other path ------------
say "U-scoped/A: a specialist's cap closes one path; the workload finishes through another"
DB="${WORK}/sc-a.db"
LOG="${WORK}/sc-a.log"
reset_blocker
start_daemon "${LOG}"

assert_http "the incident completes" "$(inject_uat sa1 ReadStatus)" 202
assert_state "the run ended" incident-sa1 idle
# Two calls to the fixture's tool: one from the capped specialist before
# it was stopped, one from the specialist the coordinator routed to. This
# is the assertion that the SECOND PATH ran — the log alone cannot tell a
# route-around from the same specialist being retried, and the tool
# ledger can.
assert_eq "both paths reached the cluster" "$(calls_count read_status)" 2
SCEV="$(grep -- 'runner event' "${LOG}" || true)"
assert_has "the spare specialist answered" "${SCEV}" 'uat-v06-spare'

# ---- leg B: the loss is loud ----------------------------------------
say "U-scoped/B: the closed path is on the record, not just absent from it"
# A run that quietly loses half its roster returns the same 202 as one
# that did not. That is the failure mode W10.3 creates, so these two are
# not decoration: they are the whole reason a scoped refusal is allowed
# to be quiet at the HTTP layer.
assert_log_atleast "a WARN says a path was closed" "${LOG}" \
  'BUDGET CEILING — a specialist was refused; the turn routed on' 1
# That line specifically, not any BUDGET CEILING line: the post-hoc fold
# one over logs a specialist name too, so a looser grep here would stay
# green on a build with no pre-call gate at all.
assert_has "and names which one" \
  "$(grep -- 'the turn routed on' "${LOG}" || true)" 'uat-v06-capped'
assert_eq "the trip counter moved" \
  "$(metric_value 'mast_budget_trips_total{workload="uat-v06-scoped"}')" 1
# The other half of the claim, and the one v0.5 could not make: the
# session itself was never stopped.
assert_no_log "the session was never aborted for it" "${LOG}" 'BUDGET EXCEEDED — aborting session turn'
assert_log_atleast "the turn completed" "${LOG}" 'turn complete' 1

# ---- leg C: the operator surface distinguishes the two --------------
say "U-scoped/C: GET /guardrails reports the session up and the specialist out"
GR="$(guardrails incident-sa1)"
assert_has "the specialist appears as its own scope" "${GR}" '"name":"uat-v06-capped"'
assert_has "and is reported tripped" "${GR}" '"tripped":true'
# The discriminator. One `tripped` flag for the whole session would have
# to answer either "yes" (and send an operator to raise a ceiling that is
# not in the way) or "no" (and hide the path that stopped).
assert_has "while the session's own ceiling is not tripped" "${GR}" '"session_cost_usd"'
assert_hasnt "the session is not reported halted" "${GR}" '"halted":true'

# ---- leg D: the reset an operator would actually type ----------------
say "U-scoped/D: a session-scoped reset says honestly that it cleared nothing"
# The bug W10.3 had to fix twice: this grant used to report
# `Reset: [cost_ceiling]`, claiming to have cleared a trip it never
# touched — and once that was fixed it reported a cheerful "nothing was
# tripped" while a specialist sat spent. Both are the same mistake, which
# is judging one holder's ceiling by another's.
RS="$(guardrails_reset incident-sa1 '{"guardrail":"cost_ceiling","additional_turns":5}')"
assert_hasnt "it does not claim to have cleared the specialist's trip" "${RS}" '"reset":["cost_ceiling"]'
assert_has "it names who is still out" "${RS}" 'uat-v06-capped'
assert_has "and the argument that would raise them" "${RS}" 'scope='
stop_term
ATTACH_ON=""

# ====================================================================
# U-dispatch (W9.1, W9.2, W9.3) — what crosses the dispatch boundary
# ====================================================================

# ---- leg A: the roster that would bypass the gate will not start ----
say "U-dispatch/A: a planner roster whose writes would skip the gate does not start"
DB="${WORK}/dg.db"
LOG="${WORK}/dg.log"
reset_blocker
if start_refused "${LOG}" "${DGATED}"; then
  ok "the daemon refused the roster instead of serving it"
else
  bad "the daemon came up on a roster whose mutating calls the write gate cannot reach"
fi
DREF="$(grep -- 'change executor' "${LOG}" || true)"
[ -n "${DREF}" ] || note "no refusal logged; last line was: $(tail -n 1 "${LOG}")"
assert_has "the refusal names the executor it found" "${DREF}" 'uat-v06-exec'
assert_has "and the policy that is not being kept" "${DREF}" 'require_approval'
# W9.2's exit criterion, in the harness's own terms. The refusal is
# permanent, so it has to be actionable: an escape it names loosely costs
# the operator the hour they spend discovering it does not compose.
assert_has "it offers coordinator as a way out" "${DREF}" 'coordinator'
assert_has "and graph" "${DREF}" 'graph'
assert_has "and apply, for writes genuinely meant to fire unattended" "${DREF}" 'hitl.on_mutation: apply'

# ---- leg B: an interrupted dispatch leaves a record -----------------
say "U-dispatch/B: a dispatched mutation is scannable after the process dies"
WL="${DAPPLY}"
DB="${WORK}/da.db"
LOG="${WORK}/da.log"
# Hold apply_change open so the kill lands with the dispatched call in
# flight — the exact window where an effect may or may not have
# committed, and the only window this record exists for.
reset_blocker apply_change
start_daemon "${LOG}"

inject_bg db1 ApplyChange
if wait_started apply_change; then
  ok "the planner dispatched the specialist and its mutating call is in flight"
else
  bad "the dispatched mutating call never started"
fi
# SIGTERM to get the durable interruption mark written, then SIGKILL so
# nothing else is: the mark is the only thing the restart is allowed to
# inherit through the ordinary shutdown path, and everything this leg
# asserts on has to come out of the ops row instead.
term_nowait
assert_state "the crashed session is marked interrupted" incident-db1 interrupted
kill9
wait "${BGPID}" 2>/dev/null || true

assert_eq "the mutation ran exactly once before the crash" "$(calls_count apply_change)" 1
# The negative that makes this leg W9.3's: the OUTER session's own stream
# never carried the call. The gate and the outbox are runner plugins and
# the dispatch runs on a runner of its own, which is the whole of #235.
assert_no_log "the outer turn's event stream never saw it" "${LOG}" 'function_call:apply_change'

# Restart on the same DB. Both tools pre-released now: the point is what
# the boot scan SEES, and a still-blocked tool would only add a way for
# the restart to hang.
: > "${BLOCKDIR}/apply_change.release"
LOG2="${WORK}/da-restart.log"
start_daemon "${LOG2}"

if wait_for 30 metric_has 'mast_autoresume_total{outcome="skipped_ambiguous",workload="uat-v06-dispatch-apply"} 1'; then
  ok "auto-resume declined the session rather than resuming over an ambiguous effect"
else
  bad "auto-resume did not decline the interrupted dispatch"
fi
# TWO dangling intents, and the second one is the release. The first is
# the planner's own invoke_specialist, which is in the session log
# because the outer runner appended it. The second is the specialist's
# apply_change, which is in no event of that log and never will be — it
# is in the companion ops row, written out-of-band by the recorder on the
# #226 observer seam. Before W9.3 this count was 1, and a workload under
# `on_mutation: apply` came back from a crash looking clean enough to
# resume over a write that may already have landed.
assert_eq "the scan counted the dispatched call as well as the dispatch" \
  "$(log_field "${LOG2}" 'dangling mutating intent' dangling_mutating)" 2
# And the guarantee the count is in service of.
assert_eq "the mutation was not replayed" "$(calls_count apply_change)" 1
stop_term

# ====================================================================
say "Summary"
printf '   \033[32m%d passed\033[0m, \033[31m%d failed\033[0m\n' "${PASS}" "${FAIL}"
[ "${FAIL}" -eq 0 ] || exit 1
