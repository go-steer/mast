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

output "wif_principal" {
  description = "The IAM principal every binding names."
  value       = module.wif.principal
}

output "rbac_subject" {
  description = "The RBAC username the GKE API server gives that principal. Every daemon binding the chart renders must name it; grep for it when a tool call comes back Forbidden."
  value       = module.wif.rbac_subject
}

output "container_role" {
  description = "Which container role write_scope resolved to."
  value       = module.wif.container_role
}

output "install_namespace" {
  description = "Namespace the chart installed into, read off the Helm release. It must be the namespace inside rbac_subject above; if those two ever disagree, that is the #290 failure and every tool call will be Forbidden."
  value       = module.release.namespace
}

output "chart_values" {
  description = "The values document the chart was installed with, before extra_values. Diff it against the chart's values.yaml when an override appears to have done nothing."
  value       = module.release.values_yaml
}

output "required_secrets" {
  description = "Secrets this configuration names and deliberately does not create. The daemon stays not-ready until they exist."
  value       = module.release.required_secrets
}

output "next_steps" {
  description = "What is left to do by hand, in order."
  value       = <<-EOT
    1. Create the bearer token, once, in both Secrets:

         TOKEN=$(openssl rand -hex 32)
         kubectl -n ${var.namespace} create secret generic ${join(" --from-literal=token=\"$TOKEN\"\n         kubectl -n ${var.namespace} create secret generic ", module.release.required_secrets)} --from-literal=token="$TOKEN"

       Not created here: a Terraform-authored token is written to the state
       file in plaintext, and a state bucket is usually readable by more
       people than a Helm release is.

    2. Check the boundary is the one you think it is:

         PROJECT_ID=${var.project_id} TARGET_NS=<ns> scripts/rbac-matrix.sh

       It runs the matrix against BOTH usernames the daemon can arrive as.
       A missing per-namespace RoleBinding and a too-wide IAM binding look
       identical from the agent's side; the matrix tells them apart.

    ${length(var.remediation_namespaces) == 0 ? "3. mast can change NOTHING right now — it diagnoses cluster-wide and holds\n       no write grant anywhere, which is the default. Set remediation_namespaces\n       when you are ready." : "3. mast may change: ${join(", ", var.remediation_namespaces)}"}
  EOT
}
