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
#
# rbac-matrix.sh — check what the mast daemon's Kubernetes RBAC actually
# permits, cell by cell, against a live cluster (v0.3 W2.6).
#
# Diagnosis is cluster-wide; change is confined to the namespaces an
# operator instantiated deploy/remediation-target/ for. This script
# asserts both halves — including the cells that must say **no**, which
# are the ones a widened Role breaks silently.
#
# Usage:
#   TARGET_NS=team-a ./scripts/rbac-matrix.sh
#
# Environment:
#   TARGET_NS   — a namespace mast MAY change (deploy/remediation-target
#                 applied there). Default: mast-demo.
#   CONTROL_NS  — a namespace mast may NOT change. Default: kube-system.
#   NAMESPACE   — where the daemon's ServiceAccount lives. Default: mast-triage.
#   KSA_NAME    — the ServiceAccount. Default: mast-daemon.
#   PROJECT_ID  — optional; when set (and gcloud is present) the IAM
#                 caveat below is checked rather than just printed.
#
# Requires kubectl with cluster access and permission to impersonate
# (`kubectl auth can-i --as=...` is a SubjectAccessReview, so the caller
# needs impersonate or cluster-admin). It changes nothing.
#
# THE CAVEAT THIS SCRIPT CANNOT CHECK WITH kubectl ALONE. `--as=` asks
# the Kubernetes authorizer about the KSA subject. mast reaches a GKE
# cluster through the GKE MCP server as its WIF principal, and GKE
# allows a call if EITHER IAM or RBAC allows it. So a green matrix means
# "RBAC bounds the in-cluster path"; it means "RBAC bounds mast" only
# when the principal's IAM is read-only too (WRITE_SCOPE=namespaced in
# scripts/setup-wif.sh). Pass PROJECT_ID to have that checked here.

set -uo pipefail

if [[ -t 1 ]]; then
    GREEN=$'\033[0;32m'
    RED=$'\033[0;31m'
    YELLOW=$'\033[1;33m'
    BLUE=$'\033[0;34m'
    RESET=$'\033[0m'
else
    GREEN=""; RED=""; YELLOW=""; BLUE=""; RESET=""
fi

TARGET_NS="${TARGET_NS:-mast-demo}"
CONTROL_NS="${CONTROL_NS:-kube-system}"
NAMESPACE="${NAMESPACE:-mast-triage}"
KSA_NAME="${KSA_NAME:-mast-daemon}"
SUBJECT="system:serviceaccount:${NAMESPACE}:${KSA_NAME}"

if ! command -v kubectl >/dev/null 2>&1; then
    printf '%s[ERROR]%s kubectl is not installed.\n' "${RED}" "${RESET}" >&2
    exit 1
fi

printf '%sSubject:%s        %s\n' "${BLUE}" "${RESET}" "${SUBJECT}"
printf '%sRemediable ns:%s  %s\n' "${BLUE}" "${RESET}" "${TARGET_NS}"
printf '%sControl ns:%s     %s\n\n' "${BLUE}" "${RESET}" "${CONTROL_NS}"

pass=0
fail=0

# cell WANT VERB RESOURCE SCOPE...
#   WANT  — yes | no, what RBAC must answer
#   SCOPE — the remaining kubectl args (-n NS, or --all-namespaces)
cell() {
    local want="$1" verb="$2" resource="$3"; shift 3
    local got
    got=$(kubectl auth can-i "${verb}" "${resource}" --as="${SUBJECT}" "$@" 2>/dev/null)
    if [[ "${got}" != "yes" ]]; then
        got="no"
    fi
    local where="$*"
    if [[ "${got}" == "${want}" ]]; then
        pass=$((pass + 1))
        printf '%s  ok  %s %-6s %-28s %-22s -> %s\n' "${GREEN}" "${RESET}" "${verb}" "${resource}" "${where}" "${got}"
    else
        fail=$((fail + 1))
        printf '%sFAIL  %s %-6s %-28s %-22s -> %s (want %s)\n' "${RED}" "${RESET}" "${verb}" "${resource}" "${where}" "${got}" "${want}"
    fi
}

echo "Diagnosis is cluster-wide:"
cell yes get   pods                     --all-namespaces
cell yes get   pods/log                 --all-namespaces
cell yes list  events                   --all-namespaces
cell yes get   nodes                    --all-namespaces
cell yes list  deployments.apps         --all-namespaces
cell yes get   persistentvolumeclaims   --all-namespaces
# Cluster-wide ConfigMap read is intended (diagnosis reads workload
# config), and it is an exposure worth seeing in the matrix rather than
# discovering later.
cell yes get   configmaps               -n "${CONTROL_NS}"

echo
echo "...but never secrets:"
cell no  get   secrets                  --all-namespaces
cell no  get   secrets                  -n "${TARGET_NS}"

echo
echo "Change is confined to ${TARGET_NS}:"
cell yes patch deployments.apps         -n "${TARGET_NS}"
cell yes patch deployments.apps/scale   -n "${TARGET_NS}"
cell yes delete pods                    -n "${TARGET_NS}"
cell yes create configmaps              -n "${TARGET_NS}"

echo
echo "...and stops at its edges:"
cell no  patch deployments.apps         -n "${CONTROL_NS}"
cell no  patch deployments.apps         --all-namespaces
cell no  delete deployments.apps        -n "${TARGET_NS}"
cell no  create secrets                 -n "${TARGET_NS}"
cell no  create clusterrolebindings.rbac.authorization.k8s.io --all-namespaces
cell no  patch nodes                    --all-namespaces
cell no  delete namespaces              --all-namespaces

echo
if [[ -n "${PROJECT_ID:-}" ]] && command -v gcloud >/dev/null 2>&1; then
    echo "IAM path (the one RBAC does not bound):"
    principal_suffix="/subject/ns/${NAMESPACE}/sa/${KSA_NAME}"
    wide=$(gcloud projects get-iam-policy "${PROJECT_ID}" \
        --flatten="bindings[].members" \
        --filter="bindings.members~${principal_suffix}$ AND (bindings.role=roles/container.admin OR bindings.role=roles/container.developer OR bindings.role=roles/container.clusterAdmin OR bindings.role=roles/editor OR bindings.role=roles/owner)" \
        --format="value(bindings.role)" 2>/dev/null)
    if [[ -n "${wide}" ]]; then
        fail=$((fail + 1))
        printf '%sFAIL  %s the KSA principal holds cluster-write IAM: %s\n' "${RED}" "${RESET}" "$(echo "${wide}" | tr '\n' ' ')"
        printf '      the namespaced write Role above does not bound the GKE MCP path.\n'
        printf '      Narrow it with: WRITE_SCOPE=namespaced ./scripts/setup-wif.sh %s\n' "${PROJECT_ID}"
    else
        pass=$((pass + 1))
        printf '%s  ok  %s no cluster-write IAM role on the KSA principal\n' "${GREEN}" "${RESET}"
    fi
else
    printf '%s[WARN]%s   PROJECT_ID unset or gcloud missing — the IAM half is UNCHECKED.\n' "${YELLOW}" "${RESET}"
    printf '         While the KSA principal holds roles/container.admin, the GKE MCP\n'
    printf '         path can change any namespace no matter what this matrix says.\n'
fi

echo
if (( fail > 0 )); then
    printf '%s%d cell(s) FAILED%s, %d passed.\n' "${RED}" "${fail}" "${RESET}" "${pass}"
    exit 1
fi
printf '%sall %d cells as expected%s\n' "${GREEN}" "${pass}" "${RESET}"
