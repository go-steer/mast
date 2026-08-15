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

# live-kind-v0.4.sh — the change-set grant (W7), against a real cluster.
#
# scripts/uat-v0.4.sh proves the mechanism offline: an operator answers
# once, the rest of the set fires, and a grant is voided when a file the
# harness controls changes. Two claims survive that harness untested:
#
#   1. A precondition declared over a REAL read tool's REAL result. The
#      offline fixture returns a string the harness wrote; here the read
#      is `kubectl get deployment -o jsonpath={.spec.replicas}` through
#      an MCP server, and the declared field path (output.replicas) has
#      to be right about a shape mast does not control.
#   2. A grant voided by SOMEBODY ELSE. Offline, "the cluster moved" is
#      the harness writing a file. Here it is a second operator running
#      `kubectl scale` on the object between the approval and the call it
#      authorized — the case W7's precondition exists for.
#
# It also exercises what the offline fixture cannot: `args_from`, which
# takes each precondition read's arguments from the change's own. That is
# what makes a multi-call set checkable at all — a precondition over the
# field the set rewrites would have call 1 invalidate call 2 by doing its
# job, so each call is checked against its own object.
#
# Legs:
#   C-changeset/A  a two-Deployment change set, approved once: both
#       scales land on the cluster and the operator is asked exactly one
#       question
#   C-changeset/B  the same set, with `kubectl scale` moving the SECOND
#       Deployment while the first approved call is held open: the grant
#       is voided, the second call never fires, and the out-of-band value
#       stands
#
# "C" is the cluster tier — a fifth tier beside v0.3 §2's S/U/E/J, and
# the only one that needs a container runtime. It is not a gate.
#
# THIS SCRIPT WRITES TO A KUBERNETES CLUSTER. It is opt-in and it is not
# part of any presubmit. The pin is mechanical, following the rule the
# sibling core-sre-agent project settled on — fault injection must never
# touch a real cluster — enforced four ways:
#
#   1. The cluster is created by this script, named "mast-live-<pid>",
#      and deleted on the way out. An existing cluster of that name is
#      refused rather than adopted: it is by definition not one we made.
#   2. kind writes to a kubeconfig under ${TMPDIR}, never ~/.kube/config,
#      and refuses to start if that file already exists (kind MERGES).
#   3. After create, the kubeconfig is verified to describe exactly one
#      context, which is ours. No bug downstream can reach a cluster that
#      is not in the only file these processes read.
#   4. Every kubectl runs with --kubeconfig and --context, from an
#      environment with KUBECONFIG unset. The ambient current-context is
#      never resolved, here or in testdata/live/kubemcp.
#
# Usage:
#   MAST_LIVE_KIND=1 scripts/live-kind-v0.4.sh
#
# Needs: kind, kubectl, a working container runtime, Go. No credentials
# and no model provider — the run is driven by --model=toolactor.
# State goes under ${TMPDIR:-/tmp}/mast-live-v04 (house rule #5); port 7791.

set -euo pipefail

if [ "${MAST_LIVE_KIND:-}" != "1" ]; then
  cat >&2 <<'MSG'
live-kind-v0.4.sh is opt-in: it CREATES A KUBERNETES CLUSTER and writes to it.

  MAST_LIVE_KIND=1 scripts/live-kind-v0.4.sh

Nothing was done. The offline equivalent (no cluster, no container
runtime) is scripts/uat-v0.4.sh.
MSG
  exit 0
fi

PORT="${MAST_LIVE_V04_PORT:-7791}"
BASE="http://127.0.0.1:${PORT}"
WORK="${TMPDIR:-/tmp}/mast-live-v04"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${WORK}/mast"
KUBEMCP="${WORK}/kubemcp"
FIXTURE="${REPO}/testdata/live"
TOKEN="live-v04-token"
NS=default
PID=""

# The pin. CLUSTER carries the prefix every guard below checks; the pid
# suffix keeps two runs on one machine from colliding.
CLUSTER="mast-live-$$"
CONTEXT="kind-${CLUSTER}"
KUBECONFIG_FILE="${WORK}/kubeconfig.yaml"

PASS=0
FAIL=0

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
ok()   { PASS=$((PASS + 1)); printf '   \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { FAIL=$((FAIL + 1)); printf '   \033[31mFAIL\033[0m %s\n' "$*"; }
die()  { printf '\n\033[31m%s\033[0m\n' "$*" >&2; exit 1; }

assert_eq()   { if [ "$2" = "$3" ]; then ok "$1 (${2})"; else bad "$1 (got '${2:-<none>}', want '$3')"; fi; }
assert_http() { if [ "$2" = "$3" ]; then ok "$1 (HTTP $2)"; else bad "$1 (HTTP $2, want $3)"; fi; }
assert_has()  { if printf '%s' "$2" | grep -Fq -- "$3"; then ok "$1"; else bad "$1 — missing: $3"; fi; }
assert_hasnt() {
  if [ -z "$2" ]; then bad "$1 — nothing to search (empty)"; return 0; fi
  if printf '%s' "$2" | grep -Fq -- "$3"; then bad "$1 — present: $3"; else ok "$1"; fi
}
assert_log_count() {
  local got
  got="$(grep -c -- "$3" "$2" || true)"
  if [ "${got}" = "$4" ]; then ok "$1 (${got})"; else bad "$1 (${got}, want $4)"; fi
}

# ---- teardown -------------------------------------------------------
# Runs on every exit path including the failures. A cluster that outlives
# a failed run is both a resource leak and the thing the no-adopt rule
# will trip over next time.
teardown() {
  local code=$?
  [ -n "${PID}" ] && kill "${PID}" 2>/dev/null || true
  case "${CLUSTER}" in
    mast-live-*) ;;
    *) echo "refusing to delete cluster ${CLUSTER}: not one this script names" >&2; return "${code}" ;;
  esac
  if command -v kind >/dev/null 2>&1; then
    env -u KUBECONFIG kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
  fi
  rm -f "${KUBECONFIG_FILE}"
  return "${code}"
}
trap teardown EXIT

# ---- the cluster ----------------------------------------------------
# kc runs kubectl against our cluster and only our cluster: both flags,
# every time, with KUBECONFIG dropped from the environment so an ambient
# one cannot be merged in.
kc() {
  env -u KUBECONFIG kubectl --kubeconfig "${KUBECONFIG_FILE}" --context "${CONTEXT}" -n "${NS}" "$@"
}

replicas() { kc get deployment "$1" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "?"; }

# verify_isolation is the check that makes the others redundant: if the
# kubeconfig names exactly one context and that context is ours, then a
# dropped flag downstream cannot reach anything else, because nothing
# else is described in the only file these processes can read.
verify_isolation() {
  local current count
  current="$(grep -E '^current-context:' "${KUBECONFIG_FILE}" | head -1 | sed -e 's/^current-context:[[:space:]]*//' -e 's/["'\'']//g')"
  count="$(grep -c -- '- context:' "${KUBECONFIG_FILE}" || true)"
  [ "${current}" = "${CONTEXT}" ] || die "kubeconfig current-context is '${current}', want '${CONTEXT}'"
  [ "${count}" = "1" ] || die "kubeconfig describes ${count} contexts, want exactly 1 — refusing to run against a merged kubeconfig"
  note "isolation verified: ${KUBECONFIG_FILE} describes exactly one context (${CONTEXT})"
}

# ---- the daemon -----------------------------------------------------
start_daemon() {
  local log="$1"
  "${BIN}" --workload="${FIXTURE}" --dispatch=graph \
    --listen=":${PORT}" --model=toolactor --session-db="${DB}" \
    --log-level=info >"${log}" 2>&1 &
  PID=$!
  local i
  for i in $(seq 1 150); do
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

inject() {
  curl -s -m 90 -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/inject" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"kind\":\"live-event\",\"reason\":\"$2\",\"namespace\":\"${NS}\",\"name\":\"pod-$1\",\"uid\":\"$1\",\"message\":\"live\",\"cluster\":\"${CLUSTER}\"}"
}

resume_verdict() {
  curl -s -m 120 -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/resume" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"session_id\":\"$1\",\"interrupt_id\":\"$2\",\"response\":$3}"
}

# resume_bg / resume_bg_wait — the answer posted in the background, so a
# leg can act while the approved calls are running. resume_bg_wait sets
# ${RESUME_CODE} and must be called as a statement: inside `$(...)` the
# background post is not a child of the subshell, so `wait` would return
# before curl had written anything.
RESUME_BG_PID=""
RESUME_CODE=""
resume_bg() {
  rm -f "${WORK}/resume-code"
  ( resume_verdict "$1" "$2" "$3" > "${WORK}/resume-code" ) &
  RESUME_BG_PID=$!
}
resume_bg_wait() {
  wait "${RESUME_BG_PID}" 2>/dev/null || true
  RESUME_BG_PID=""
  RESUME_CODE="$(cat "${WORK}/resume-code" 2>/dev/null || true)"
}

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

assert_state() {
  if wait_for 60 state_is "$2" "$3"; then
    ok "$1 (state=$3)"
  else
    bad "$1 (state=$(show_field "$2" State), want $3)"
  fi
}

# ---- the fixture's call ledger and hold -----------------------------
reset_calls() {
  local hold="${1:-}"
  rm -rf "${MAST_LIVE_DIR}"
  mkdir -p "${MAST_LIVE_DIR}"
  [ "${hold}" = "scale_deployment" ] || : > "${MAST_LIVE_DIR}/scale_deployment.release"
}
release()    { : > "${MAST_LIVE_DIR}/$1.release"; }
calls_args() { [ -f "${MAST_LIVE_DIR}/$1.calls" ] && cat "${MAST_LIVE_DIR}/$1.calls" || true; }
wait_started() {
  local marker="${MAST_LIVE_DIR}/$1.started" i
  for ((i = 0; i < 900; i++)); do
    [ -f "${marker}" ] && return 0
    sleep 0.1
  done
  return 1
}

# ====================================================================
# preflight
# ====================================================================
say "Preflight"
for t in kind kubectl curl go; do
  command -v "${t}" >/dev/null 2>&1 || die "${t} is not on PATH"
done
env -u KUBECONFIG kind get clusters 2>/dev/null | grep -qx "${CLUSTER}" \
  && die "cluster ${CLUSTER} already exists — refusing to adopt a cluster this script did not create"
note "kind $(kind version 2>/dev/null | head -1), kubectl present"

rm -rf "${WORK}" && mkdir -p "${WORK}"
[ -e "${KUBECONFIG_FILE}" ] && die "${KUBECONFIG_FILE} exists — kind merges into an existing kubeconfig"

say "Build"
(cd "${REPO}" && go build -o "${BIN}" ./cmd/mast)
(cd "${REPO}" && go build -o "${KUBEMCP}" ./testdata/live/kubemcp)
note "built ${BIN} + ${KUBEMCP}"

say "Cluster"
note "creating kind cluster ${CLUSTER} (this takes a minute)"
env -u KUBECONFIG kind create cluster \
  --name "${CLUSTER}" --kubeconfig "${KUBECONFIG_FILE}" --wait 120s >"${WORK}/kind-create.log" 2>&1 \
  || { cat "${WORK}/kind-create.log" >&2; die "kind create failed"; }
verify_isolation
kc apply -f "${FIXTURE}/manifests.yaml" >/dev/null
assert_eq "api starts at 1 replica" "$(replicas api)" 1
assert_eq "worker starts at 1 replica" "$(replicas worker)" 1

# The fixture's MCP server is pinned to the same context, and refuses any
# other (testdata/live/kubemcp: configure).
export MAST_INJECT_TOKEN="${TOKEN}"
export MAST_LIVE_KUBEMCP="${KUBEMCP}"
export MAST_LIVE_KUBECONFIG="${KUBECONFIG_FILE}"
export MAST_LIVE_CONTEXT="${CONTEXT}"
export MAST_LIVE_NAMESPACE="${NS}"
export MAST_LIVE_DIR="${WORK}/calls"

# The set: one call per Deployment. Two objects rather than two calls on
# one, because each call's precondition re-reads its own object and a set
# that rewrote the field it was checked against would void itself.
SET_SCALE='[{"tool":"scale_deployment","arguments":"{\"deployment\":\"api\",\"replicas\":2}"},{"tool":"scale_deployment","arguments":"{\"deployment\":\"worker\",\"replicas\":2}"}]'
export MAST_FAKE_PROPOSED_CHANGE="${SET_SCALE}"

# ====================================================================
# C-changeset/A — one answer, two Deployments scaled
# ====================================================================
say "C-changeset/A: one approval scales both Deployments on the cluster"
DB="${WORK}/ca.db"
LOG="${WORK}/ca.log"
reset_calls
start_daemon "${LOG}"

assert_http "inject ScaleUp -> 202" "$(inject ca1 ScaleUp)" 202
assert_state "the first call parked" incident-ca1 paused
CAMSG="$(show_field incident-ca1 Message)"
note "the operator's question: ${CAMSG}"
assert_has "the question names the whole set" "${CAMSG}" "2 calls"
assert_has "and says how to answer for all of it" "${CAMSG}" "scope=change_set"

CAINT="$(show_field incident-ca1 Interrupt)"
assert_http "the operator approves the SET -> 202" \
  "$(resume_verdict incident-ca1 "${CAINT}" '{"verdict":"approve","scope":"change_set","note":"live"}')" 202
assert_state "the run finishes" incident-ca1 idle

# The headline, read off the cluster rather than off a log line.
assert_eq "api was scaled" "$(replicas api)" 2
assert_eq "worker was scaled by the grant alone" "$(replicas worker)" 2
assert_log_count "the operator was asked exactly once" "${LOG}" 'awaiting_approval' 1
assert_log_count "one grant was minted" "${LOG}" 'change-set grants minted' 1
assert_log_count "the second call ran on that grant" "${LOG}" \
  'mutating tool call authorized by an approved change set' 1
assert_log_count "the grant spend is on the audit trail" "${LOG}" 'approved_by_change_set' 1
stop_term

# ====================================================================
# C-changeset/B — somebody else moves the object, and the grant is void
# ====================================================================
# The claim the offline harness can only simulate. The first approved
# call is held inside the MCP server; while it waits, a second operator
# scales `worker` by hand; then it is released. The remaining call's
# precondition re-reads worker, finds 5 where 1 was approved against, and
# parks instead of stomping on a change nobody told mast about.
say "C-changeset/B: a grant is void once somebody else moves the object"
DB="${WORK}/cb.db"
LOG="${WORK}/cb.log"
kc scale deployment/api --replicas=1 >/dev/null
kc scale deployment/worker --replicas=1 >/dev/null
assert_eq "the cluster is back to 1/1" "$(replicas api)/$(replicas worker)" "1/1"
reset_calls scale_deployment
start_daemon "${LOG}"

assert_http "inject ScaleUp -> 202" "$(inject cb1 ScaleUp)" 202
assert_state "the first call parked" incident-cb1 paused
CBINT="$(show_field incident-cb1 Interrupt)"
resume_bg incident-cb1 "${CBINT}" '{"verdict":"approve","scope":"change_set","note":"live"}'
if wait_started scale_deployment; then
  ok "the approved call reached the cluster's tool"
else
  bad "the approved call never dispatched"
fi
# The other operator, with a terminal and no idea a change set is in
# flight.
kc scale deployment/worker --replicas=5 >/dev/null
note "out-of-band: kubectl scale deployment/worker --replicas=5"
release scale_deployment
resume_bg_wait
assert_http "the operator's answer was accepted -> 202" "${RESUME_CODE}" 202

assert_state "the rest of the set parked again" incident-cb1 paused
assert_eq "the approved call still landed" "$(replicas api)" 2
assert_eq "and the out-of-band change stands" "$(replicas worker)" 5
CBCALLS="$(calls_args scale_deployment)"
note "the tool's ledger: $(printf '%s' "${CBCALLS}" | tr '\n' '; ')"
assert_has "the approved call ran" "${CBCALLS}" "api=2"
assert_hasnt "the stale call never reached the cluster" "${CBCALLS}" "worker=2"
assert_log_count "the grant was voided" "${LOG}" \
  'a change-set grant was voided and its call re-parked' 1
assert_log_count "the operator was asked again" "${LOG}" 'awaiting_approval' 2

# What the second question says. An operator re-deciding needs the delta,
# not "please re-approve" — and the delta here is a real field of a real
# object, read back through the same tool the declaration names.
CBMSG="$(show_field incident-cb1 Message)"
note "the second question: ${CBMSG}"
assert_has "the question names the field that moved" "${CBMSG}" "output.replicas was 1"
assert_has "and what it is now" "${CBMSG}" "is 5"
assert_has "and names the read it made" "${CBMSG}" "get_deployment(deployment=worker)"
stop_term
unset MAST_FAKE_PROPOSED_CHANGE

# ====================================================================
say "Summary"
printf '   %d passed, %d failed\n' "${PASS}" "${FAIL}"
if [ "${FAIL}" -gt 0 ]; then exit 1; fi
