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

# The IAM half. Runs in one file share state, which is what lets the first
# two runs here be a before-and-after rather than two unrelated assertions.

mock_provider "google" {
  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }
}

variables {
  project_id = "example-project"
}

# The reason this module exists. scripts/setup-wif.sh cannot do this: its
# own header says a re-run with WRITE_SCOPE=namespaced leaves an earlier
# roles/container.admin in place and tells you to remove it with gcloud.
# While it stands, the chart's namespaced write Roles bound nothing on the
# MCP path, so an operator who believes they narrowed the deployment has
# not.
#
# What these two runs prove is the half that is this module's to get right:
# the bound role set is a function of write_scope, read off the resources.
# That the removed key is then DESTROYED rather than orphaned is for_each
# semantics, and it holds only because these are google_project_iam_member
# and not the authoritative _binding — a distinction no assertion here can
# see, so charts.TestTerraformWifIsAdditivePerMember pins it statically.
run "wide_open_first" {
  command = apply

  variables {
    write_scope = "cluster-admin"
  }

  assert {
    condition     = contains(output.bound_project_roles, "roles/container.admin")
    error_message = "cluster-admin did not bind roles/container.admin: ${join(", ", output.bound_project_roles)}"
  }
}

run "narrowing_removes_the_admin_binding" {
  command = apply

  variables {
    write_scope = "namespaced"
  }

  assert {
    condition     = !contains(output.bound_project_roles, "roles/container.admin")
    error_message = "roles/container.admin survived the narrowing — the whole point of this module is that it does not: ${join(", ", output.bound_project_roles)}"
  }

  assert {
    condition     = contains(output.bound_project_roles, "roles/container.viewer")
    error_message = "namespaced must still bind roles/container.viewer, or mast cannot read the cluster at all"
  }

  # Read off the resources, so this is the set that actually exists rather
  # than the set the locals block computed.
  assert {
    condition = output.bound_project_roles == tolist([
      "roles/aiplatform.user",
      "roles/container.viewer",
      "roles/mcp.toolUser",
    ])
    error_message = "the bound role set drifted from scripts/setup-wif.sh's: ${join(", ", output.bound_project_roles)}"
  }
}

# #290, measured on live GKE 2026-09-06: the API server sees the federation
# principal as an RBAC *User*, and a binding naming only the ServiceAccount
# matches nothing on the MCP path — every call Forbidden, including reads.
# Both strings are derived here so a reader can check the chart's bindings
# against a `terraform output`, and so the two cannot be independently
# edited into disagreement.
run "the_principal_and_the_rbac_subject_name_one_identity" {
  command = apply

  assert {
    condition     = output.principal == "principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/example-project.svc.id.goog/subject/ns/mast-triage/sa/mast-daemon"
    error_message = "the WIF principal is not the one scripts/setup-wif.sh builds: ${output.principal}"
  }

  assert {
    condition     = output.rbac_subject == "serviceAccount:example-project.svc.id.goog[mast-triage/mast-daemon]"
    error_message = "the RBAC subject is not the one the chart's mast.wifUser helper renders: ${output.rbac_subject}"
  }
}

run "the_namespace_reaches_both_strings" {
  command = apply

  variables {
    namespace = "sre-tools"
  }

  assert {
    condition     = strcontains(output.principal, "/subject/ns/sre-tools/sa/mast-daemon")
    error_message = "the principal ignored the namespace: ${output.principal}"
  }

  assert {
    condition     = strcontains(output.rbac_subject, "[sre-tools/mast-daemon]")
    error_message = "the RBAC subject ignored the namespace: ${output.rbac_subject}"
  }
}

run "an_unknown_write_scope_is_refused_rather_than_guessed" {
  command = plan

  variables {
    write_scope = "readonly"
  }

  expect_failures = [var.write_scope]
}

run "service_enablement_can_belong_to_someone_else" {
  command = apply

  variables {
    enable_apis = false
  }

  assert {
    condition     = length(output.enabled_apis) == 0
    error_message = "enable_apis = false still enabled: ${join(", ", output.enabled_apis)}"
  }

  assert {
    condition     = length(output.bound_project_roles) == 3
    error_message = "the bindings must not depend on this module having turned the APIs on"
  }
}

run "the_node_service_account_defaults_to_the_compute_default" {
  command = apply

  assert {
    condition     = output.node_service_account == "123456789012-compute@developer.gserviceaccount.com"
    error_message = "derived the wrong node SA: ${output.node_service_account}"
  }
}

run "a_supplied_node_service_account_wins" {
  command = apply

  variables {
    node_service_account = "gke-nodes@example-project.iam.gserviceaccount.com"
  }

  assert {
    condition     = output.node_service_account == "gke-nodes@example-project.iam.gserviceaccount.com"
    error_message = "an explicit node SA was overridden by the derived default: ${output.node_service_account}"
  }
}

# A module that is a guest in somebody else's project must not turn
# container.googleapis.com off on the way out.
run "destroying_mast_does_not_disable_the_projects_apis" {
  command = apply

  # alltrue([]) is true, so the floor comes first: this run asserts about
  # three services or it asserts about nothing.
  assert {
    condition     = length(google_project_service.required) == 3
    error_message = "expected the three APIs scripts/setup-wif.sh enables, got ${length(google_project_service.required)}"
  }

  assert {
    condition     = alltrue([for s in google_project_service.required : s.disable_on_destroy == false])
    error_message = "a google_project_service here has disable_on_destroy = true, which would take the project's GKE API down with the mast install"
  }

  assert {
    condition     = alltrue([for s in google_project_service.required : s.disable_dependent_services == false])
    error_message = "disable_dependent_services must stay false for the same reason"
  }
}
