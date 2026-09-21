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

variable "project_id" {
  description = "GCP project holding the GKE cluster and the Vertex AI quota. The only value with no default."
  type        = string
}

variable "namespace" {
  description = <<-EOT
    Namespace mast runs in. Passed to BOTH halves from here, which is the
    point of the root module: the namespace is part of the daemon's
    identity, and an IAM principal bound for one namespace against a chart
    installed into another produces a deployment that is Forbidden
    everywhere with nothing obviously wrong in either half (#290).
  EOT
  type        = string
  default     = "mast-triage"
}

variable "write_scope" {
  description = "\"namespaced\" (default) or \"cluster-admin\". See modules/wif/variables.tf — flipping this back to namespaced REMOVES the admin binding, which is the thing scripts/setup-wif.sh cannot do."
  type        = string
  default     = "namespaced"
}

variable "remediation_namespaces" {
  description = "Namespaces mast may CHANGE. Empty installs a mast that diagnoses everything and writes nowhere."
  type        = list(string)
  default     = []
}

variable "chart_version" {
  description = "Chart version to install. Pin it for anything you intend to reproduce."
  type        = string
  default     = null
}

variable "cluster_name" {
  description = "Cluster name shown in triage context."
  type        = string
  default     = "local"
}

variable "gke_standard" {
  description = <<-EOT
    True for a GKE Standard cluster, which needs the daemon pinned onto
    metadata-server-enabled nodes for Workload Identity Federation. Leave
    false on Autopilot: it ships the metadata server everywhere and REJECTS
    that selector, so setting this on Autopilot fails the admission rather
    than doing nothing.
  EOT
  type        = bool
  default     = false
}

variable "enable_apis" {
  description = "Enable container/aiplatform/iamcredentials. False where service enablement is somebody else's pipeline."
  type        = bool
  default     = true
}

variable "node_service_account" {
  description = "Node service account the daemon may impersonate. Empty derives the Compute Engine default."
  type        = string
  default     = ""
}

variable "extra_values" {
  description = "Raw YAML documents merged over the chart values this root sets, last wins."
  type        = list(string)
  default     = []
}
