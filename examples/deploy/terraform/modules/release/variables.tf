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
  description = "GCP project. Substituted into the RBAC subject the GKE MCP path arrives as, which is why the chart refuses to render without it (#290)."
  type        = string

  validation {
    condition     = length(var.project_id) > 0
    error_message = "project_id is required: a placeholder in the RBAC subject renders a binding the API server accepts and that binds nobody."
  }
}

variable "namespace" {
  description = "Namespace to install into. Must be the namespace the WIF principal's subject names."
  type        = string
  default     = "mast-triage"
}

variable "release_name" {
  description = "Helm release name. Object names inside the chart are fixed and do not take this as a prefix, deliberately — two releases in one cluster is not a supported topology."
  type        = string
  default     = "mast"
}

variable "chart" {
  description = "Chart reference. The published OCI chart by default; point it at a local path to install a working copy."
  type        = string
  default     = "oci://ghcr.io/go-steer/charts/mast"
}

variable "chart_version" {
  description = <<-EOT
    Chart version to install. Null takes whatever the registry currently
    tags latest, which is fine for a first look and wrong for anything you
    intend to reproduce — a Terraform run that resolves a different chart
    every apply is not a declarative install.
  EOT
  type        = string
  default     = null
}

variable "location" {
  description = "Vertex AI location."
  type        = string
  default     = "global"
}

variable "model" {
  description = "Model the daemon runs its specialists on."
  type        = string
  default     = "gemini-2.5-flash"
}

variable "image_tag" {
  description = "Override the daemon image tag. Empty takes the chart's appVersion, which is the mast release the chart was published with."
  type        = string
  default     = ""
}

variable "remediation_namespaces" {
  description = <<-EOT
    The namespaces mast may CHANGE. Empty — the default — installs a mast
    that diagnoses the whole cluster and can write nowhere.

    Each entry becomes a namespaced Role and RoleBinding. Note that this is
    the half of the boundary the cluster enforces; it is only real while the
    IAM half is write_scope = "namespaced".
  EOT
  type        = list(string)
  default     = []
}

variable "cluster_name" {
  description = "Cluster name shown in triage context. Name it something you will recognise in a transcript."
  type        = string
  default     = "local"
}

variable "node_selector" {
  description = <<-EOT
    Node selector for the daemon pod.

    GKE Standard needs {"iam.gke.io/gke-metadata-server-enabled" = "true"} so
    the pod lands on a node that can serve Workload Identity Federation.
    Autopilot REJECTS that selector — it runs the metadata server
    everywhere — so the empty default is the Autopilot-safe one.
  EOT
  type        = map(string)
  default     = {}
}

variable "inject_token_secret" {
  description = <<-EOT
    Name of the Secret holding the shared bearer every write route requires.

    This module names it and does not create it, for a reason that is
    stronger here than it is for the chart: a Terraform-authored token —
    random_password, or a value passed in as a variable — is written to the
    state file in plaintext, and a state bucket is usually readable by more
    people than a Helm release is. Create it with kubectl, out of band.
  EOT
  type        = string
  default     = "mast-inject-token"
}

variable "watcher_enabled" {
  description = "Install the k8s-event-watcher sidecar that turns cluster events into injects."
  type        = bool
  default     = true
}

variable "watcher_token_secret" {
  description = "Name of the Secret holding the watcher's copy of the bearer. Must carry the SAME token as inject_token_secret."
  type        = string
  default     = "k8s-event-watcher-token"
}

variable "create_namespace" {
  description = "Let Helm create the namespace. Set false when it is managed elsewhere — and note the token Secrets have to exist in it before the daemon can become ready either way."
  type        = bool
  default     = true
}

variable "wait_for_rollout" {
  description = <<-EOT
    Block until the daemon reports ready.

    Leave this on: an install that reports success while nothing came up is
    the failure mode worth avoiding. The one timeout you are likely to meet
    is the documented one — the daemon does not become ready until the two
    token Secrets exist, so a first apply that hangs here almost always
    means that step was skipped rather than that anything is broken.
  EOT
  type        = bool
  default     = true
}

variable "rollout_timeout_seconds" {
  description = "How long to wait for the rollout. The daemon's own termination grace period is 330s, so an upgrade's teardown is inside this by design."
  type        = number
  default     = 600
}

variable "extra_values" {
  description = <<-EOT
    Raw YAML documents merged over everything above, last wins — the escape
    hatch for chart values this module does not name. Prefer it over a fork
    of the module: values.yaml is the chart's documented surface and this
    file deliberately mirrors only the subset with a footgun or a
    cross-module coupling.
  EOT
  type        = list(string)
  default     = []
}
