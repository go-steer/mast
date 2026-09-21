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

# The root module owns exactly one thing the two halves cannot own
# separately: the values that have to agree across them. These runs are
# about that seam and nothing else — what each half does with a value it
# receives is tested inside that half.

mock_provider "google" {
  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }
}

mock_provider "helm" {}

variables {
  project_id = "example-project"
}

# #290, measured on live GKE 2026-09-06. The namespace is inside the WIF
# principal's subject AND inside the RBAC subject the chart binds. Two
# independent copies that drift produce a daemon that is Forbidden on every
# call while each half, read alone, looks right. So this run derives both
# ends from what was actually built and compares them.
run "one_namespace_reaches_the_identity_and_the_install" {
  command = apply

  variables {
    namespace = "sre-tools"
  }

  assert {
    condition     = output.install_namespace == "sre-tools"
    error_message = "the chart went into ${output.install_namespace}"
  }

  assert {
    condition     = strcontains(output.wif_principal, "/subject/ns/${output.install_namespace}/sa/mast-daemon")
    error_message = "the IAM principal names a different namespace than the install: ${output.wif_principal}"
  }

  assert {
    condition     = strcontains(output.rbac_subject, "[${output.install_namespace}/mast-daemon]")
    error_message = "the RBAC subject names a different namespace than the install: ${output.rbac_subject}"
  }
}

# The daemon's ServiceAccount name is fixed by the chart rather than
# prefixed with the release name, so that the identity strings above do not
# depend on what somebody typed after `helm install`. Both halves hardcode
# it; this asserts they hardcoded the same thing.
run "both_halves_name_the_same_service_account" {
  command = apply

  assert {
    condition     = strcontains(output.wif_principal, "/sa/mast-daemon") && strcontains(output.rbac_subject, "/mast-daemon]")
    error_message = "the two identity strings disagree about the daemon's ServiceAccount: ${output.wif_principal} vs ${output.rbac_subject}"
  }
}

# Autopilot runs the metadata server on every node and REJECTS this
# selector, so the default has to be the Autopilot-safe one: a wrong default
# here fails admission rather than doing nothing.
run "autopilot_is_the_default_and_gets_no_node_selector" {
  command = apply

  assert {
    condition     = length(yamldecode(output.chart_values).daemon.nodeSelector) == 0
    error_message = "the default install pinned the daemon onto selected nodes, which Autopilot refuses: ${jsonencode(yamldecode(output.chart_values).daemon.nodeSelector)}"
  }
}

run "gke_standard_pins_the_daemon_onto_metadata_server_nodes" {
  command = apply

  variables {
    gke_standard = true
  }

  assert {
    condition     = yamldecode(output.chart_values).daemon.nodeSelector["iam.gke.io/gke-metadata-server-enabled"] == "true"
    error_message = "GKE Standard did not get the metadata-server selector, so the daemon can land on a node where Workload Identity Federation is not served: ${jsonencode(yamldecode(output.chart_values).daemon.nodeSelector)}"
  }
}

# write_scope is the IAM half of the boundary and remediation_namespaces is
# the cluster half. GKE allows a call if IAM OR RBAC allows it, so a
# cluster-admin IAM binding makes the namespaced Roles decorative. The root
# passes them independently on purpose — a `terraform plan` has to be able
# to say "this install can change anything" out loud — and these two runs
# pin what each control is worth so the default cannot drift open.
run "the_shipped_default_can_change_nothing_on_either_side" {
  command = apply

  assert {
    condition     = output.container_role == "roles/container.viewer"
    error_message = "the default IAM grant is ${output.container_role}, which authorizes writes regardless of what the chart's RBAC says"
  }

  assert {
    condition     = length(yamldecode(output.chart_values).remediationNamespaces) == 0
    error_message = "the default install holds a write grant in ${jsonencode(yamldecode(output.chart_values).remediationNamespaces)}"
  }
}

run "asking_for_cluster_admin_says_so_in_the_plan" {
  command = apply

  variables {
    write_scope = "cluster-admin"
  }

  assert {
    condition     = output.container_role == "roles/container.admin"
    error_message = "cluster-admin resolved to ${output.container_role}"
  }
}

# The token is the one thing this configuration deliberately does not
# create, so the operator has to be told — by name, in the output, not in a
# README they have already closed.
run "the_apply_ends_by_naming_what_it_did_not_do" {
  command = apply

  assert {
    condition     = length(output.required_secrets) == 2
    error_message = "expected both bearer Secrets to be named as manual steps, got ${jsonencode(output.required_secrets)}"
  }

  assert {
    condition     = alltrue([for s in output.required_secrets : strcontains(output.next_steps, s)])
    error_message = "a Secret this install requires is missing from next_steps, which is where an operator reads it"
  }

  assert {
    condition     = strcontains(output.next_steps, "mast can change NOTHING right now")
    error_message = "a default install must say out loud that it holds no write grant; next_steps said: ${output.next_steps}"
  }
}
