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

# The cluster half. These assert on the values document rather than on the
# rendered manifests — what the chart does with these values is
# charts/render_test.go's job, and duplicating it here would be a second
# copy of the chart's behaviour to keep in sync.

mock_provider "helm" {}

variables {
  project_id = "example-project"
}

run "the_required_value_reaches_the_chart" {
  command = apply

  assert {
    condition     = yamldecode(output.values_yaml).gcp.projectID == "example-project"
    error_message = "gcp.projectID did not reach the values document, which is the one value the chart refuses to render without (#290)"
  }

  assert {
    condition     = helm_release.mast.namespace == "mast-triage"
    error_message = "installed into ${helm_release.mast.namespace}, not the namespace the WIF principal's subject names"
  }
}

run "an_empty_project_id_is_refused_at_plan_time" {
  command = plan

  variables {
    project_id = ""
  }

  expect_failures = [var.project_id]
}

# The default install diagnoses everything and can change nothing. That is
# the shipped posture and it should not be possible to acquire write access
# by leaving a variable unset.
run "a_default_install_can_change_nothing" {
  command = apply

  assert {
    condition     = length(yamldecode(output.values_yaml).remediationNamespaces) == 0
    error_message = "the default install named remediable namespaces: ${join(", ", yamldecode(output.values_yaml).remediationNamespaces)}"
  }
}

run "named_namespaces_reach_the_chart_as_a_list" {
  command = apply

  variables {
    remediation_namespaces = ["team-a", "team-b"]
  }

  # A list and not a comma-joined string: the chart renders one Role and
  # RoleBinding per entry, and a single entry named "team-a,team-b" would
  # produce one Role in a namespace that does not exist.
  assert {
    condition     = jsonencode(yamldecode(output.values_yaml).remediationNamespaces) == jsonencode(["team-a", "team-b"])
    error_message = "remediationNamespaces arrived as ${jsonencode(yamldecode(output.values_yaml).remediationNamespaces)}"
  }
}

# The module names the Secrets and creates neither. A Terraform-authored
# token is written to the state file in plaintext, and a state bucket is
# usually readable by more people than a Helm release is — the same argument
# the chart makes about release state, one storage layer worse.
run "the_bearer_secrets_are_named_and_not_created" {
  command = apply

  assert {
    condition     = output.required_secrets == tolist(["mast-inject-token", "k8s-event-watcher-token"])
    error_message = "the two Secrets an operator must create are not what this module tells them to create: ${jsonencode(output.required_secrets)}"
  }

  assert {
    condition     = yamldecode(output.values_yaml).auth.injectTokenSecret == "mast-inject-token"
    error_message = "the chart was pointed at a different Secret than the one the output names"
  }
}

run "disabling_the_watcher_drops_its_secret_from_the_list" {
  command = apply

  variables {
    watcher_enabled = false
  }

  assert {
    condition     = output.required_secrets == tolist(["mast-inject-token"])
    error_message = "an operator with no watcher should not be told to create the watcher's token: ${jsonencode(output.required_secrets)}"
  }
}

run "extra_values_are_merged_last_so_they_can_win" {
  command = apply

  variables {
    extra_values = [
      "logLevel: debug\n",
    ]
  }

  assert {
    condition     = length(helm_release.mast.values) == 2
    error_message = "extra_values did not reach the release as its own document"
  }

  assert {
    condition     = helm_release.mast.values[1] == "logLevel: debug\n"
    error_message = "extra_values must come last, or the escape hatch cannot override anything"
  }
}

# The mock provider fabricates a value for every attribute the configuration
# leaves unset, so version cannot be read back as null off the resource here.
# What this run establishes is the pair of properties that are the module's:
# an unpinned apply is ALLOWED — the module does not require a version — and
# the default is not some pin an operator inherits without choosing it.
run "an_unpinned_chart_version_is_allowed" {
  command = apply

  assert {
    condition     = var.chart_version == null
    error_message = "chart_version has a default pin, which is a chart version nobody in the install chose"
  }
}

run "a_pinned_chart_version_is_what_gets_installed" {
  command = apply

  variables {
    chart_version = "0.9.0"
  }

  assert {
    condition     = helm_release.mast.version == "0.9.0"
    error_message = "the pin did not reach the release: ${helm_release.mast.version}"
  }
}

# wait = true is the default on purpose: an install that reports success
# while nothing came up is the failure worth avoiding, and the timeout an
# operator is most likely to meet is the documented one (the daemon stays
# not-ready until the two Secrets exist).
run "the_install_waits_for_the_rollout_by_default" {
  command = apply

  assert {
    condition     = helm_release.mast.wait
    error_message = "wait defaulted to false, so a broken install would report success"
  }

  assert {
    condition     = helm_release.mast.timeout >= 330
    error_message = "the rollout timeout is below the daemon's own 330s termination grace period, so an upgrade's teardown cannot finish inside it"
  }
}
