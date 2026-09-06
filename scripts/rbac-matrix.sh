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
# permits, cell by cell, against a live cluster (v0.3 W2.6, #290).
#
# Diagnosis is cluster-wide; change is confined to the namespaces an
# operator instantiated deploy/remediation-target/ for. This script
# asserts both halves — including the cells that must say **no**, which
# are the ones a widened Role breaks silently.
#
# TWO SUBJECTS, because mast reaches a cluster by two paths and the API
# server gives them different usernames:
#
#   in-cluster path   system:serviceaccount:NAMESPACE:KSA
#                     anything presenting the pod's projected KSA token
#                     to the API server directly.
#
#   GKE MCP path      serviceAccount:PROJECT.svc.id.goog[NAMESPACE/KSA]
#                     the path the agent's tools take. mast never
#                     presents the KSA token there; it calls
#                     container.googleapis.com/mcp with a Google
#                     credential derived from the KSA, and the API
#                     server sees the Workload Identity Federation
#                     principal — an RBAC *User*, not a ServiceAccount.
#
# Measuring only the first is worse than measuring nothing, because it
# goes green on a cluster where mast cannot write at all: with only the
# ServiceAccount subject bound, every MCP-path write is Forbidden even
# in a namespace deploy/remediation-target was applied to (measured on
# live GKE 2026-09-06, #290). So the MCP subject needs PROJECT_ID, and
# without it this script reports what it measured and exits non-zero
# rather than calling a half-measured cluster good.
#
# Usage:
#   TARGET_NS=team-a PROJECT_ID=my-project ./scripts/rbac-matrix.sh
#
# Environment:
#   PROJECT_ID  — GCP project the cluster lives in. Required: it is what
#                 names the MCP path's subject. IN_CLUSTER_ONLY=true
#                 drops that half for a cluster with no MCP path (kind,
#                 minikube, non-GKE) — and then the run is not evidence
#                 about a GKE deployment.
#   TARGET_NS   — a namespace mast MAY change (deploy/remediation-target
#                 applied there). Default: mast-demo.
#   CONTROL_NS  — a namespace mast may NOT change. Default: kube-system.
#   NAMESPACE   — where the daemon's ServiceAccount lives. Default: mast-triage.
#   KSA_NAME    — the ServiceAccount. Default: mast-daemon.
#   IN_CLUSTER_ONLY — "true" to skip the MCP-path subject. See above.
#
# Requires kubectl with cluster access and permission to impersonate
# (`kubectl auth can-i --as=...` is a SubjectAccessReview, so the caller
# needs impersonate or cluster-admin). It changes nothing.
#
# `kubectl auth can-i --as=` against the MCP subject was checked against
# the real thing: all nine of its answers match what the GKE MCP server
# returned for the same calls on the same cluster (#290). No pod needed.
#
# WHAT THIS STILL CANNOT SEE. GKE allows a call if EITHER IAM or RBAC
# allows it, and `--as=` asks only the Kubernetes authorizer. So green
# here means "RBAC bounds both paths"; it means "mast is bounded" only
# when the principal's IAM is read-only too (WRITE_SCOPE=namespaced in
# scripts/setup-wif.sh). That is the last section below, and it is
# checked, not printed, whenever gcloud is on PATH.

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
IN_CLUSTER_ONLY="${IN_CLUSTER_ONLY:-false}"

KSA_SUBJECT="system:serviceaccount:${NAMESPACE}:${KSA_NAME}"
WIF_SUBJECT="serviceAccount:${PROJECT_ID:-}.svc.id.goog[${NAMESPACE}/${KSA_NAME}]"

if ! command -v kubectl >/dev/null 2>&1; then
    printf '%s[ERROR]%s kubectl is not installed.\n' "${RED}" "${RESET}" >&2
    exit 1
fi

printf '%sRemediable ns:%s  %s\n' "${BLUE}" "${RESET}" "${TARGET_NS}"
printf '%sControl ns:%s     %s\n' "${BLUE}" "${RESET}" "${CONTROL_NS}"

pass=0
fail=0
SUBJECT="${KSA_SUBJECT}"

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

# matrix runs every cell against whatever SUBJECT is currently set to.
# The same 20 answers are required of both paths: an operator who reads
# the manifests expects one boundary, not one per credential.
matrix() {
    echo
    printf '%s=== %s%s\n' "${BLUE}" "$1" "${RESET}"
    printf '    subject: %s\n' "${SUBJECT}"

    echo
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
}

SUBJECT="${KSA_SUBJECT}"
matrix "in-cluster path (the pod's KSA token)"

if [[ "${IN_CLUSTER_ONLY}" == "true" ]]; then
    echo
    printf '%s[WARN]%s   IN_CLUSTER_ONLY=true — the GKE MCP path was NOT measured.\n' "${YELLOW}" "${RESET}"
    printf '         That is the path mast'"'"'s tools take. This run says nothing\n'
    printf '         about a GKE deployment.\n'
elif [[ -z "${PROJECT_ID:-}" ]]; then
    fail=$((fail + 1))
    echo
    printf '%sFAIL  %s PROJECT_ID unset — the GKE MCP path was NOT measured.\n' "${RED}" "${RESET}"
    printf '      The cells above are the in-cluster path, and they go green on a\n'
    printf '      cluster where mast cannot write anything: the MCP path arrives as\n'
    printf '      User %s,\n' "serviceAccount:PROJECT.svc.id.goog[${NAMESPACE}/${KSA_NAME}]"
    printf '      which the ServiceAccount subject in the RoleBinding does not match.\n'
    printf '      Re-run with PROJECT_ID set, or IN_CLUSTER_ONLY=true if this cluster\n'
    printf '      has no MCP path at all.\n'
else
    SUBJECT="${WIF_SUBJECT}"
    matrix "GKE MCP path (the KSA's WIF principal) — the one mast uses"
fi

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
