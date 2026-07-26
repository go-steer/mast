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
# Originally derived from
# go-steer/core-agent@c5efbb9e:examples/gke-troubleshoot-agent/scripts/setup-wif.sh.
# The only real deltas are the defaults (KSA_NAME=mast-daemon,
# NAMESPACE=mast-triage) and the summary text. The four IAM bindings
# are identical — mast talks to the same Vertex AI + GKE MCP surface as
# core-agent's recipe does.

# setup-wif.sh — configure GKE Workload Identity Federation direct-binding
# IAM for the mast daemon's KSA in the GKE triage spike deploy.
#
# What it does (in order):
#
#   1. Enables the three GCP APIs the recipe requires:
#        - container.googleapis.com        (GKE + GKE MCP)
#        - aiplatform.googleapis.com       (Vertex AI / Gemini)
#        - iamcredentials.googleapis.com   (WIF token-exchange path)
#
#   2. Binds four IAM roles to the KSA principal:
#        - roles/aiplatform.user
#        - roles/mcp.toolUser
#        - roles/container.admin
#        - roles/iam.serviceAccountUser (on the NODE service account)
#
# All bindings use WIF-for-GKE direct binding — no Google Service Account
# impersonation for the KSA itself. See:
#   https://docs.cloud.google.com/kubernetes-engine/docs/how-to/workload-identity#authenticating_to
#
# Usage:
#   ./scripts/setup-wif.sh [PROJECT_ID]
#
# Environment overrides:
#   PROJECT_ID     — GCP project ID. Falls back to `gcloud config get-value project`.
#   NAMESPACE      — K8s namespace. Default: mast-triage.
#   KSA_NAME       — Kubernetes ServiceAccount name. Default: mast-daemon.
#   NODE_SA        — Node service account (default: Compute Engine default).
#   DRY_RUN        — "true" to print gcloud commands without executing.
#
# Idempotent: re-runs are safe. Existing bindings are left in place.
#
# Prereqs on the operator: roles/container.admin + roles/iam.serviceAccountAdmin
# on the project.

set -euo pipefail

if [[ -t 1 ]]; then
    GREEN=$'\033[0;32m'
    RED=$'\033[0;31m'
    YELLOW=$'\033[1;33m'
    BLUE=$'\033[0;34m'
    RESET=$'\033[0m'
else
    GREEN=""; RED=""; YELLOW=""; BLUE=""; RESET=""
fi

log_info()    { printf '%s[INFO]%s    %s\n' "${BLUE}"   "${RESET}" "$1"; }
log_success() { printf '%s[SUCCESS]%s %s\n' "${GREEN}"  "${RESET}" "$1"; }
log_warn()    { printf '%s[WARN]%s    %s\n' "${YELLOW}" "${RESET}" "$1"; }
log_error()   { printf '%s[ERROR]%s   %s\n' "${RED}"    "${RESET}" "$1" >&2; }

if ! command -v gcloud >/dev/null 2>&1; then
    log_error "gcloud CLI is not installed. Install the Google Cloud SDK and try again."
    exit 1
fi

PROJECT_ID="${1:-${PROJECT_ID:-}}"
if [[ -z "${PROJECT_ID}" ]]; then
    log_info "No PROJECT_ID specified; attempting to detect from active gcloud config..."
    PROJECT_ID=$(gcloud config get-value project 2>/dev/null || true)
    if [[ -z "${PROJECT_ID}" ]]; then
        log_error "Could not detect active gcloud project. Pass PROJECT_ID as arg 1 or set PROJECT_ID env var."
        echo "Usage: $0 [PROJECT_ID]" >&2
        exit 1
    fi
fi

NAMESPACE="${NAMESPACE:-mast-triage}"
KSA_NAME="${KSA_NAME:-mast-daemon}"
DRY_RUN="${DRY_RUN:-false}"

log_info "Configuring GKE Workload Identity Federation for the mast daemon KSA:"
echo "  GCP Project:      ${PROJECT_ID}"
echo "  K8s Namespace:    ${NAMESPACE}"
echo "  K8s SA Name:      ${KSA_NAME}"

if [[ -z "${PROJECT_NUMBER:-}" ]]; then
    log_info "Fetching project number for '${PROJECT_ID}'..."
    PROJECT_NUMBER=$(gcloud projects describe "${PROJECT_ID}" --format='value(projectNumber)' 2>/dev/null || true)
    if [[ -z "${PROJECT_NUMBER}" ]]; then
        log_error "Failed to retrieve project number for '${PROJECT_ID}'. Verify the project ID and your gcloud permissions, or set PROJECT_NUMBER explicitly (useful with DRY_RUN=true)."
        exit 1
    fi
else
    log_info "Using PROJECT_NUMBER override: ${PROJECT_NUMBER}"
fi
echo "  Project Number:   ${PROJECT_NUMBER}"

NODE_SA="${NODE_SA:-${PROJECT_NUMBER}-compute@developer.gserviceaccount.com}"
echo "  Node SA:          ${NODE_SA}"

KSA_PRINCIPAL="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/${NAMESPACE}/sa/${KSA_NAME}"
echo "  KSA Principal:"
echo "    ${KSA_PRINCIPAL}"
echo

if [[ "${DRY_RUN}" == "true" ]]; then
    log_warn "=== DRY RUN MODE: no changes will be applied ==="
    echo
fi

enable_api() {
    local api="$1"
    log_info "Enabling API: ${api}"
    local cmd="gcloud services enable ${api} --project=${PROJECT_ID}"
    if [[ "${DRY_RUN}" == "true" ]]; then
        echo "  [DRY RUN] Would run: ${cmd}"
    else
        eval "${cmd}" >/dev/null
        log_success "API enabled: ${api}"
    fi
}

bind_project_role() {
    local role="$1"
    log_info "Binding project role: ${role}"
    local cmd="gcloud projects add-iam-policy-binding ${PROJECT_ID} \
--role=${role} \
--member=${KSA_PRINCIPAL} \
--condition=None \
--quiet"
    if [[ "${DRY_RUN}" == "true" ]]; then
        echo "  [DRY RUN] Would run: ${cmd}"
    else
        eval "${cmd}" >/dev/null
        log_success "Bound: ${role}"
    fi
}

bind_sa_role() {
    local sa="$1"
    local role="roles/iam.serviceAccountUser"
    log_info "Binding ${role} on service account: ${sa}"
    local cmd="gcloud iam service-accounts add-iam-policy-binding ${sa} \
--role=${role} \
--member=${KSA_PRINCIPAL} \
--quiet"
    if [[ "${DRY_RUN}" == "true" ]]; then
        echo "  [DRY RUN] Would run: ${cmd}"
    else
        eval "${cmd}" >/dev/null
        log_success "Bound ${role} on ${sa}"
    fi
}

log_info "=== Phase 1: enabling required GCP APIs ==="
enable_api "container.googleapis.com"
enable_api "aiplatform.googleapis.com"
enable_api "iamcredentials.googleapis.com"
echo

log_info "=== Phase 2: binding IAM roles to the KSA principal ==="
bind_project_role "roles/aiplatform.user"
bind_project_role "roles/mcp.toolUser"
bind_project_role "roles/container.admin"
bind_sa_role "${NODE_SA}"
echo

if [[ "${DRY_RUN}" == "true" ]]; then
    log_warn "=== DRY RUN complete: no changes were applied ==="
else
    log_success "=== Setup complete: WIF bindings are active ==="
    echo
    echo "The mast daemon's KSA can now:"
    echo "  - Call Gemini via the Vertex AI API"
    echo "  - Call the GKE MCP server + its tools"
    echo "  - Administer GKE clusters + workloads"
    echo "  - Impersonate the node SA (required by GKE MCP)"
    echo
    echo "Next steps:"
    echo "  1. Create the inject-token Secret (must match watcher-token):"
    echo "     TOKEN=\$(openssl rand -hex 32)"
    echo "     kubectl create ns ${NAMESPACE}"
    echo "     kubectl -n ${NAMESPACE} create secret generic mast-inject-token --from-literal=token=\"\$TOKEN\""
    echo "     kubectl -n ${NAMESPACE} create secret generic k8s-event-watcher-token --from-literal=token=\"\$TOKEN\""
    echo "  2. Edit deploy/overlays/example/kustomization.yaml (REPLACE_ME → project id, image tag)."
    echo "  3. kubectl apply -k deploy/overlays/example"
    echo
    echo "Bindings applied:"
    echo "  - roles/aiplatform.user        on projects/${PROJECT_ID}"
    echo "  - roles/mcp.toolUser           on projects/${PROJECT_ID}"
    echo "  - roles/container.admin        on projects/${PROJECT_ID}"
    echo "  - roles/iam.serviceAccountUser on ${NODE_SA}"
    echo "  All bound to member: ${KSA_PRINCIPAL}"
fi
