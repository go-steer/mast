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
#   U-notify-onchange (W4.5) — what the cycle says, and when it says
#   nothing.
#       A  a cycle whose classifier reported a change posts once; the
#          cycles after it, with the classifier reporting calm, make NO
#          request and spend NO model call — the strongest form of the
#          claim, and the only one worth making. Then calm closes the
#          timeline, so the next change is a new message rather than an
#          append onto yesterday's incident.
#       B  consecutive speaking cycles extend ONE message, however the
#          ingress answers: an append, then a 409 ("I no longer remember
#          that message") recovered by re-sending the whole text, then a
#          200-with-a-continuation ("that message is full") that every
#          later cycle targets instead.
#       C  a report the ingress refused fails the fire and is NOT
#          replayed. The classifier consumed the diff when it answered,
#          so the held message would describe a world that moved on.
#       D  a monitoring cycle that is BROKEN says so in the channel,
#          once on the edge rather than every cycle, and says when it
#          recovered.
#       E  silence has a deadline: with nothing changed for longer than
#          `digest_after`, the next cycle speaks anyway. A monitor that
#          has been quiet for a week is otherwise indistinguishable from
#          one that died a week ago.
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
# State goes under ${TMPDIR:-/tmp}/mast-uat-v05 (house rule #5); ports
# 7791 for the daemon and 7792 for the stub chat ingress.

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
INGPID=""
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
  [ -n "${INGPID}" ] && kill "${INGPID}" 2>/dev/null || true
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

# ---- the chat ingress -----------------------------------------------
# testdata/uat/ingress: a stub switchboard that records every request it
# is sent, one JSON object per line. The W4.5 legs read that ledger —
# and most of what they assert is that a line is NOT in it. "The cycle
# ran and told nobody" has no log line strong enough to stand for it;
# the only convincing evidence is a recording listener that received
# nothing.
INGRESS="${WORK}/ingress"
INGDIR="${WORK}/ingdir"
INGPORT="$((PORT + 1))"
INGURL="http://127.0.0.1:${INGPORT}"

# Deliberately not ${TOKEN}. The daemon refuses to start when the token
# it posts chat with is also one that drives it, so a harness that
# reused one would be measuring that refusal by accident on every leg.
NOTIFY_TOKEN="uat-v05-notify-token"

start_ingress() {
  rm -rf "${INGDIR}" && mkdir -p "${INGDIR}"
  "${INGRESS}" -addr="127.0.0.1:${INGPORT}" -dir="${INGDIR}" -token="${NOTIFY_TOKEN}" \
    >"${WORK}/ingress.log" 2>&1 &
  INGPID=$!
  local i
  for i in $(seq 1 100); do
    if ! kill -0 "${INGPID}" 2>/dev/null; then break; fi
    if curl -sf -m 1 "${INGURL}/healthz" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  echo "ingress failed to start; log:" >&2; cat "${WORK}/ingress.log" >&2; exit 1
}

stop_ingress() {
  [ -n "${INGPID}" ] && kill "${INGPID}" 2>/dev/null || true
  wait "${INGPID}" 2>/dev/null || true
  INGPID=""
}

# requests — how many requests the ingress has received. The number the
# whole workstream is about.
requests() {
  if [ -f "${INGDIR}/requests.jsonl" ]; then wc -l < "${INGDIR}/requests.jsonl" | tr -d ' '; else echo 0; fi
}
requests_atleast() { [ "$(requests)" -ge "$1" ]; }

# req <n> — the nth recorded request, raw. Prose assertions grep this
# line rather than extracting the field: a health notice has commas in
# it, and req_field stops at the first one.
req() { sed -n "$1p" "${INGDIR}/requests.jsonl" 2>/dev/null || true; }

# req_field <n> <field> — one scalar field off the nth request.
req_field() { req "$1" | sed -n "s/.*\"$2\":\"\{0,1\}\([^\",}]*\).*/\1/p"; }

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

# sched_fails_atleast — the other half. A fire that errors logs its own
# line and never the audit one, so a leg counting cycles through a
# failure has to read this counter instead.
sched_fails_atleast() {
  local n
  n="$(grep -c -- 'scheduled fire failed' "$1" || true)"
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
(cd "${REPO}" && go build -o "${INGRESS}" ./testdata/uat/ingress)
note "built ${BIN} + ${BLOCKER} + ${INGRESS} (toolactor model — no credentials, no network)"

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

# ---- the notify fixture: a cycle that speaks only when it must ------
# The classification fixture plus the half W4.5 adds. `digest_after` is
# a day — longer than any run of this harness — so the deadman is not
# what these legs measure: the only thing that can make this workload
# speak is the classifier saying something changed. Leg E uses the
# fixture below it to measure the deadman on its own.
BNOTIFY="${WORK}/notify"
cp -r "${REPO}/examples/workloads/bounded-triage" "${BNOTIFY}"
cp "${FIXTURE}/mcp.json" "${BNOTIFY}/mcp.json"
cat > "${BNOTIFY}/workload.yaml" <<'YAML'
# Harness fixture for scripts/uat-v0.5.sh's U-notify-onchange legs.
name: uat-notify
description: Fixture workload for the mast v0.5 notify-only-on-change legs.
mode: single_session

dispatch: bounded

tool_catalog:
  mcp:
    - server: uat-blocker
  tools:
    - name: findings_diff
      mutating: true

specialists:
  - incident-report

budget:
  max_wallclock_seconds: 120

hitl:
  on_mutation: require_approval

monitor:
  collect:
    - tool: findings_diff
      args: {transitions: "new,escalated,resolved"}
      as: transitions
  # Both halves are required for a cycle to be allowed to stay silent:
  # without transitions_from, "nothing changed" would be mast's guess
  # rather than the classifier's answer, and the cycle would run.
  transitions_from: transitions
  notify:
    conversation: "#uat-oncall"
    digest_after: 24h

edge_trigger:
  scheduled:
    # Four seconds: long enough that the harness can change the
    # classifier's answer between two cycles and know which cycle read
    # which answer, short enough that five cycles fit in a UAT.
    interval: 4s
    jitter: 0s
    prompt: 'A monitoring cycle woke you: {"reason":"CrashLoopBackOff"}. Report on what changed.'
YAML

# ---- the digest fixture: silence with a deadline --------------------
# The same workload with a deadman short enough to observe. Its own
# name, so leg E reads its own counters rather than the ones four
# earlier daemons moved.
BDIGEST="${WORK}/digest"
cp -r "${BNOTIFY}" "${BDIGEST}"
sed -e 's/^name: uat-notify$/name: uat-notify-digest/' \
    -e 's/^    digest_after: 24h$/    digest_after: 8s/' \
    -e 's/^    interval: 4s$/    interval: 3s/' \
    "${BNOTIFY}/workload.yaml" > "${BDIGEST}/workload.yaml"

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
# U-notify-onchange (W4.5) — the cycle speaks only when something moved
# ====================================================================
# The claim is a negative one, which is why these legs are driven
# against a recording ingress rather than a log line: what has to be
# true is that a quiet cycle produces no request and no model call. A
# monitor that posts "all quiet" every few minutes is one an operator
# mutes inside a week, and the mute costs them the incident report too.
export MAST_NOTIFY_TOKEN="${NOTIFY_TOKEN}"

# The two classifier answers these legs alternate between. Written
# straight into the blocker's canned output, so the change the daemon
# reacts to is the change a real run-to-run diff would report.
diff_changed() {
  cat > "${BLOCKDIR}/findings_diff.out" <<'DIFF'
transition=new subject_key=pod/prod/web-2f1/CrashLoopBackOff severity=critical message="back-off 5m0s restarting failed container"
scanned=412 findings=1 elapsed=1.4s
DIFF
}
diff_quiet() {
  cat > "${BLOCKDIR}/findings_diff.out" <<'DIFF'
scanned=412 findings=0 elapsed=1.1s
DIFF
}

# ---- leg A: nothing changed, so nothing happened --------------------
say "U-notify-onchange/A: a cycle with nothing to report makes no request and no model call"
WL="${BNOTIFY}"
DISPATCH=
DB="${WORK}/notify.db"
LOG="${WORK}/notify.log"
reset_blocker
diff_changed
start_ingress
start_daemon "${LOG}" --notify-url="${INGURL}"

NFARM="$(grep -- 'monitoring notifications armed' "${LOG}" || true)"
assert_has "the daemon armed the notify leg at startup" "${NFARM}" '#uat-oncall'
assert_has "and says up front what a quiet cycle will cost" "${NFARM}" 'will not wake the model'

if wait_for 60 requests_atleast 1; then
  ok "the cycle with something to report reached the ingress"
else
  bad "the changed cycle sent nothing"
fi
# The classifier reports calm from here. Written now rather than later
# because the request above lands at the END of the cycle that
# collected, which leaves nearly a full interval before the next read.
diff_quiet
assert_eq "it opened a message rather than editing one" "$(req_field 1 method)" POST
assert_eq "in the conversation the bundle named" "$(req_field 1 conversation)" '#uat-oncall'
assert_eq "and the ingress took it" "$(req_field 1 status)" 200
# The tick, not the wall clock: a send that timed out client-side after
# landing must present the key the original send used.
assert_has "the request carries a replay key derived from the tick" "$(req 1)" '"idem":"mast:uat-notify:'
# The daemon posts with a credential that cannot be used to drive it —
# the separation buildNotifyClient refuses to start without.
assert_has "and the egress credential, which is not an inbound one" "$(req 1)" "Bearer ${NOTIFY_TOKEN}"
assert_eq "the reporting cycle woke the model once" \
  "$(metric_value 'mast_model_calls_total{workload="uat-notify"}')" 1

# THE ASSERTION THE WORKSTREAM EXISTS FOR. Two more cycles run with the
# classifier reporting calm. Neither reaches the chat, and — the part
# that is not merely politeness — neither spends a model call: the turn
# does not run at all.
NFF="$(grep -c -- 'scheduled trigger fired' "${LOG}" || true)"
if wait_for 60 sched_fires_atleast "${LOG}" "$((NFF + 2))"; then
  ok "two more cycles ran with nothing to report"
else
  bad "the cadence stopped firing"
fi
assert_eq "the quiet cycles sent nothing at all" "$(requests)" 1
assert_eq "and woke no model" \
  "$(metric_value 'mast_model_calls_total{workload="uat-notify"}')" 1
# At info, not debug: "it ran and decided not to wake anyone" is the
# most common thing a healthy monitor does, and an operator asking
# whether it is still alive should not have to raise the log level of
# everything else to find out.
assert_has "the daemon says so plainly" \
  "$(grep -- 'monitoring cycle stayed quiet' "${LOG}" || true)" 'nothing changed'
NFQ="$(metric_value 'mast_monitor_notifications_total{outcome="quiet",workload="uat-notify"}')"
if [ "${NFQ:-0}" -ge 2 ]; then
  ok "and the meter counts them (quiet=${NFQ})"
else
  bad "quiet cycles were not counted (got '${NFQ:-<none>}')"
fi

# Calm closed the timeline, so the next incident is its own message
# rather than three cycles appended under yesterday's headline.
diff_changed
if wait_for 60 requests_atleast 2; then
  ok "the next change reported again"
else
  bad "the cycle after the quiet ones never reported"
fi
assert_eq "as a fresh message, because calm closed the last one" "$(req_field 2 method)" POST
stop_term
stop_ingress

# ---- leg B: one incident is one message -----------------------------
# Six cycles of an incident should read as one growing story, not six
# notifications an operator has to reassemble at 3am. The ingress is
# made to answer each of the three ways switchboard can, in the order
# that is hardest: extend, then "I no longer remember that message",
# then "that message is full".
say "U-notify-onchange/B: consecutive cycles extend one message, however the ingress answers"
WL="${BNOTIFY}"
DISPATCH=
DB="${WORK}/timeline.db"
LOG="${WORK}/timeline.log"
reset_blocker
diff_changed
start_ingress
start_daemon "${LOG}" --notify-url="${INGURL}"

if wait_for 60 requests_atleast 2; then
  ok "two speaking cycles reached the ingress"
else
  bad "the timeline never got past its first cycle (requests=$(requests))"
fi
assert_eq "the first cycle opened the message" "$(req_field 1 method)" POST
assert_eq "the second extended it instead of posting again" "$(req_field 2 method)" PATCH
assert_has "as an append, not a rewrite" "$(req 2)" '"append":'
assert_eq "targeting the message the first cycle opened" "$(req_field 2 id)" m1

# A restarted switchboard no longer holds the message body, so it
# refuses the append and asks for the whole text. Handled rather than
# surfaced: the operator reading the channel should not be able to tell
# that anything forgot anything.
: > "${INGDIR}/append.409"
if wait_for 60 requests_atleast 4; then
  ok "the refused append was followed by a second request in the same cycle"
else
  bad "the 409 was not recovered from (requests=$(requests))"
fi
assert_eq "the ingress refused the append" "$(req_field 3 status)" 409
assert_eq "and mast re-sent the story whole" "$(req_field 4 method)" PATCH
assert_has "as a text rewrite, not another append" "$(req 4)" '"text":'
assert_eq "into the same message" "$(req_field 4 id)" m1
# Different body, so a different key: switchboard fingerprints the body
# against the Idempotency-Key and answers 409 to a reused one, which
# would turn the fallback into a second failure.
NFK3="$(req_field 3 idem)"
NFK4="$(req_field 4 idem)"
if [ -n "${NFK4}" ] && [ "${NFK3}" != "${NFK4}" ]; then
  ok "the fallback carries a key of its own (${NFK4})"
else
  bad "the fallback reused the append's idempotency key (${NFK3:-<none>})"
fi

# The message is full: the ingress answers the append with a
# continuation, and every later cycle has to target that one.
rm -f "${INGDIR}/append.409"
: > "${INGDIR}/append.roll"
if wait_for 60 requests_atleast 5; then
  ok "the next cycle appended into a message that was full"
else
  bad "the rollover cycle never ran"
fi
assert_eq "which the ingress answered with a continuation" "$(req_field 5 status)" 200
if wait_for 60 requests_atleast 6; then
  ok "and the cycle after it kept talking"
else
  bad "nothing followed the rollover"
fi
assert_eq "into the continuation, not the message that was full" "$(req_field 6 id)" m2
assert_eq "the full-text fallback is counted as its own outcome" \
  "$(metric_value 'mast_monitor_notifications_total{outcome="replaced",workload="uat-notify"}')" 1
assert_eq "and so is the rollover" \
  "$(metric_value 'mast_monitor_notifications_total{outcome="rolled",workload="uat-notify"}')" 1
stop_term
stop_ingress

# ---- leg C: a failed report is not replayed -------------------------
say "U-notify-onchange/C: a report the ingress refused does not come back next cycle"
WL="${BNOTIFY}"
DISPATCH=
DB="${WORK}/notifyfail.db"
LOG="${WORK}/notifyfail.log"
reset_blocker
diff_changed
start_ingress
echo 503 > "${INGDIR}/status"
start_daemon "${LOG}" --notify-url="${INGURL}"

if wait_for 60 requests_atleast 1; then
  ok "the cycle tried to report"
else
  bad "the cycle never reached the ingress"
fi
assert_eq "and the ingress refused it" "$(req_field 1 status)" 503
diff_quiet
rm -f "${INGDIR}/status"
if wait_for 60 grep -q 'scheduled fire failed' "${LOG}"; then
  ok "the failed send failed the fire rather than passing quietly"
else
  bad "a report that never arrived was recorded as a completed cycle"
fi
assert_has "and the failure says which half of the cycle broke" \
  "$(grep -- 'scheduled fire failed' "${LOG}" || true)" 'could not report'

# THE ASSERTION THIS LEG EXISTS FOR. The classifier consumed the diff
# when it answered, so that finding is no longer new to it; a mast that
# held the undelivered message and re-sent it next cycle would be
# describing a world that has already moved on. Two more cycles run
# with the ingress healthy again, and the ledger does not grow.
NCF="$(grep -c -- 'scheduled trigger fired' "${LOG}" || true)"
if wait_for 60 sched_fires_atleast "${LOG}" "$((NCF + 2))"; then
  ok "two more cycles ran against a healthy ingress"
else
  bad "the cadence stopped after the failed send"
fi
assert_eq "the failed report was not replayed" "$(requests)" 1
assert_eq "and it is visible as its own outcome rather than as silence" \
  "$(metric_value 'mast_monitor_notifications_total{outcome="error",workload="uat-notify"}')" 1
stop_term
stop_ingress

# ---- leg D: a broken monitor says so, once --------------------------
# The gap W4.2 named and left open. A collection that fails every cycle
# is invisible to everyone who is not reading the daemon's logs, and
# "nobody is reading anything" is the operating assumption of the whole
# feature.
say "U-notify-onchange/D: a monitor that is failing says so once, and says when it recovers"
WL="${BNOTIFY}"
DISPATCH=
DB="${WORK}/notifyhealth.db"
LOG="${WORK}/notifyhealth.log"
reset_blocker
# Records with no summary line — the truncated classifier read
# U-transitions/B established fails the cycle before the model is woken.
# Here the question is what the CHANNEL learns about it.
cat > "${BLOCKDIR}/findings_diff.out" <<'DIFF'
transition=new subject_key=pod/prod/api-7d9/CrashLoopBackOff severity=critical
DIFF
start_ingress
start_daemon "${LOG}" --notify-url="${INGURL}"

if wait_for 60 requests_atleast 1; then
  ok "the broken cycle told the channel it was broken"
else
  bad "the monitoring failed silently"
fi
assert_eq "as a message of its own" "$(req_field 1 method)" POST
assert_has "naming the workload whose monitoring stopped" "$(req 1)" 'uat-notify'
assert_has "and saying plainly that nothing was assessed" "$(req 1)" 'Nothing was assessed'

# Edge-triggered, not rate-limited: a broken monitor that re-announces
# itself on the cadence is one an operator mutes.
NHF="$(grep -c -- 'scheduled fire failed' "${LOG}" || true)"
if wait_for 60 sched_fails_atleast "${LOG}" "$((NHF + 2))"; then
  ok "two more cycles failed the same way"
else
  bad "the cadence stopped after the failing cycle"
fi
assert_eq "and the channel was told once, not three times" "$(requests)" 1

# The way back matters as much: an operator who was told the monitor
# was broken is owed the message that it is not.
diff_changed
if wait_for 60 requests_atleast 3; then
  ok "recovery reached the channel"
else
  bad "the recovered monitor never said so (requests=$(requests))"
fi
assert_has "the notice says monitoring recovered" "$(req 2)" 'Monitoring has recovered'
assert_eq "and the recovered cycle then reported what changed" "$(req_field 3 method)" POST
assert_eq "both notices are counted as health, not as reports" \
  "$(metric_value 'mast_monitor_notifications_total{outcome="health",workload="uat-notify"}')" 2
stop_term
stop_ingress

# ---- leg E: silence has a deadline ----------------------------------
# A monitor that has been quiet for a week is indistinguishable from a
# monitor that died a week ago, and the whole value of leg A is that
# somebody trusts the silence. The deadman is wall-clock rather than a
# count of quiet cycles, because what an operator is owed is a sign of
# life on a schedule they can reason about.
say "U-notify-onchange/E: after long enough with no news, the cycle speaks anyway"
WL="${BDIGEST}"
DISPATCH=
DB="${WORK}/digest.db"
LOG="${WORK}/digest.log"
reset_blocker
diff_quiet
start_ingress
start_daemon "${LOG}" --notify-url="${INGURL}"

DGARM="$(grep -- 'monitoring notifications armed' "${LOG}" || true)"
assert_has "the daemon armed the deadman with the bundle's window" "${DGARM}" '"digest_after":"8s"'
if wait_for 60 sched_fires_atleast "${LOG}" 1; then
  ok "the first quiet cycle ran"
else
  bad "the cadence never fired"
fi
# The deadman counts from startup, not from zero: a daemon booting into
# a quiet world must not announce the quiet on its first cycle.
assert_eq "and said nothing, because the deadman had not expired" "$(requests)" 0
if wait_for 60 requests_atleast 1; then
  ok "the deadman then woke a cycle that had no news"
else
  bad "the workload went quiet indefinitely"
fi
assert_eq "which posted a message of its own" "$(req_field 1 method)" POST
assert_eq "counted as a digest wake rather than as a change" \
  "$(metric_value 'mast_monitor_digest_wakes_total{workload="uat-notify-digest"}')" 1
assert_eq "and it did wake the model, unlike the quiet cycles before it" \
  "$(metric_value 'mast_model_calls_total{workload="uat-notify-digest"}')" 1
stop_term
stop_ingress

# ====================================================================
say "Summary"
printf '   %d passed, %d failed\n' "${PASS}" "${FAIL}"
if [ "${FAIL}" -gt 0 ]; then exit 1; fi
