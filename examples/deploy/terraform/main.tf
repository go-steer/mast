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

# One `terraform apply` for a mast install on GKE: the IAM bindings the
# daemon's Workload Identity Federation principal needs, and the published
# Helm chart.
#
# The two halves are separate modules because they are often separate
# people — modules/wif needs project-level IAM admin, modules/release needs
# cluster credentials — and an operator who only wants one can call one.
# What the root buys is that project_id and namespace reach both from a
# single place. They are not two settings that happen to match: the
# namespace is inside the WIF principal's subject AND inside the RBAC
# subject the chart binds, so two independent copies that drift produce a
# daemon that is Forbidden on every call with both halves looking correct
# in isolation (#290, measured on live GKE 2026-09-06).

module "wif" {
  source = "./modules/wif"

  project_id           = var.project_id
  namespace            = var.namespace
  write_scope          = var.write_scope
  enable_apis          = var.enable_apis
  node_service_account = var.node_service_account
}

module "release" {
  source = "./modules/release"

  project_id             = var.project_id
  namespace              = var.namespace
  chart_version          = var.chart_version
  cluster_name           = var.cluster_name
  remediation_namespaces = var.remediation_namespaces
  extra_values           = var.extra_values

  node_selector = var.gke_standard ? {
    "iam.gke.io/gke-metadata-server-enabled" = "true"
  } : {}

  # The chart's RBAC is what authorizes a write once write_scope is
  # "namespaced", so the bindings have to exist before the daemon starts
  # reaching for them. Terraform sees no dependency between an IAM member
  # and a Helm release, so it is stated.
  depends_on = [module.wif]
}
