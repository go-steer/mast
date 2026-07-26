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

# demo-spike2.sh — runs the three spike-2 scenarios end to end against
# the offline echo model (no credentials, no network). See FINDINGS.md
# for what each scenario proves.
#
#   1. Graph dispatch: LLM-as-router workflow graph as the runner root
#      (three reasons each hit their own specialist; unknown reason ->
#      Default -> _fallback).
#   2. Durable HITL: pause persisted to SQLite, process killed with -9,
#      fresh process resumes with an operator approval.
#   3. Budget enforcement: a $0.01 cost cap trips mid-turn and aborts.
#
# Usage: scripts/demo-spike2.sh
# State goes under ${TMPDIR:-/tmp}/mast-spike2-demo; port 7799.

set -euo pipefail

PORT=7799
BASE="http://localhost:${PORT}"
WORK="${TMPDIR:-/tmp}/mast-spike2-demo"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${WORK}/mast"
PID=""

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }

cleanup() { [ -n "${PID}" ] && kill "${PID}" 2>/dev/null || true; }
trap cleanup EXIT

start() { # start <logfile> <extra flags...>
  local log="$1"; shift
  "${BIN}" --workload="$1" --dispatch=graph --listen=":${PORT}" "${@:2}" >"${log}" 2>&1 &
  PID=$!
  for _ in $(seq 1 50); do
    curl -sf -m 1 "${BASE}/" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  echo "server failed to start; log:" >&2; cat "${log}" >&2; exit 1
}

stop() { kill "${PID}" 2>/dev/null || true; wait "${PID}" 2>/dev/null || true; PID=""; }

inject() { # inject <uid> <reason>
  curl -s -m 30 -X POST "${BASE}/inject" -H 'Content-Type: application/json' -d "{
    \"kind\":\"Pod\",\"reason\":\"$2\",\"namespace\":\"default\",\"name\":\"web-1\",
    \"uid\":\"$1\",\"message\":\"demo event\",\"cluster\":\"demo\"}"
}

rm -rf "${WORK}" && mkdir -p "${WORK}"

say "Build"
(cd "${REPO}" && go build -o "${BIN}" ./cmd/mast)
note "built ${BIN} (echo model — no credentials needed)"

# ---------------------------------------------------------------- 1 --
say "Scenario 1: graph dispatch (LLM-as-router as runner root)"
cp -r "${REPO}/examples/workloads/gke-triage" "${WORK}/wl-graph"
# No approval gate in this scenario; keep it pure routing.
sed -i.bak 's/require_approval: true/require_approval: false/' "${WORK}/wl-graph/workload.yaml"
start "${WORK}/graph.log" "${WORK}/wl-graph"
inject demo-a ImagePullBackOff  >/dev/null
inject demo-b CrashLoopBackOff  >/dev/null
inject demo-c NodeNotReady      >/dev/null
inject demo-d SomeVendorReason  >/dev/null  # not in roster -> Default route
sleep 1
stop
note "route hit   -> $(grep -c '"author":"ImagePullBackOff"' "${WORK}/graph.log") events from the ImagePullBackOff specialist"
note "route hit   -> $(grep -c '"author":"CrashLoopBackOff"' "${WORK}/graph.log") events from the CrashLoopBackOff specialist"
note "route hit   -> $(grep -c '"author":"NodeNotReady"' "${WORK}/graph.log") events from the NodeNotReady specialist"
note "fallback    -> $(grep -c '"author":"_fallback"' "${WORK}/graph.log") events from _fallback (SomeVendorReason has no specialist)"
grep -E 'runner event' "${WORK}/graph.log" | sed 's/^/   | /'

# ---------------------------------------------------------------- 2 --
say "Scenario 2: durable HITL across kill -9"
DB="${WORK}/sessions.db"
start "${WORK}/hitl-1.log" "${REPO}/examples/workloads/gke-triage" --session-db="${DB}"
inject demo-hitl ImagePullBackOff >/dev/null
sleep 1
note "paused: $(grep -o '"interrupt_id":"[^"]*"' "${WORK}/hitl-1.log" | head -1)"
note "killing the process with -9 ..."
kill -9 "${PID}"; wait "${PID}" 2>/dev/null || true; PID=""
note "restarting a FRESH process on the same SQLite DB ..."
start "${WORK}/hitl-2.log" "${REPO}/examples/workloads/gke-triage" --session-db="${DB}"
curl -s -m 30 -X POST "${BASE}/resume" -H 'Content-Type: application/json' -d '{
  "session_id":"incident-demo-hitl",
  "interrupt_id":"approve-ImagePullBackOff",
  "response":{"approved":true,"note":"rollback approved by oncall"}}' >/dev/null
sleep 1
stop
note "post-restart resume output:"
grep -E '"output"' "${WORK}/hitl-2.log" | sed 's/^/   | /'

# ---------------------------------------------------------------- 3 --
say "Scenario 3: budget enforcement (\$0.01 cap)"
cp -r "${REPO}/examples/workloads/gke-triage" "${WORK}/wl-budget"
sed -i.bak -e 's/max_wallclock_seconds: 300/max_wallclock_seconds: 300\n  max_cost_usd: 0.01/' \
           -e 's/require_approval: true/require_approval: false/' "${WORK}/wl-budget/workload.yaml"
start "${WORK}/budget.log" "${WORK}/wl-budget"
for i in 1 2 3; do
  note "inject ${i}: $(inject demo-budget ImagePullBackOff)"
done
sleep 1
stop
grep -E 'BUDGET EXCEEDED|session_cost_usd' "${WORK}/budget.log" | sed 's/^/   | /'

say "Done"
note "logs + state under ${WORK}"
note "see FINDINGS.md for what each scenario demonstrates"
