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

# The cluster half of a mast install: the published Helm chart, with the
# values that have a footgun or a coupling behind them named as typed
# variables and the rest left to extra_values.

locals {
  # One YAML document rather than a pile of `set` entries, for two reasons.
  # It reads in the plan as the thing the operator is actually installing;
  # and `helm --set` accepts a key the chart does not define, silently,
  # applying it to nothing — so a renamed values.yaml key turns a documented
  # override into a no-op rather than an error (charts/installpage_test.go
  # exists because that happened to the install page). A values document is
  # no stricter, but it keeps every override in one place a reader can diff
  # against values.yaml.
  values = yamlencode({
    gcp = {
      projectID = var.project_id
      location  = var.location
    }
    model = var.model
    image = {
      tag = var.image_tag
    }
    daemon = {
      nodeSelector = var.node_selector
    }
    auth = {
      injectTokenSecret = var.inject_token_secret
    }
    watcher = {
      enabled     = var.watcher_enabled
      clusterName = var.cluster_name
      tokenSecret = var.watcher_token_secret
    }
    remediationNamespaces = var.remediation_namespaces
  })
}

resource "helm_release" "mast" {
  name      = var.release_name
  chart     = var.chart
  version   = var.chart_version
  namespace = var.namespace

  create_namespace = var.create_namespace

  values = concat([local.values], var.extra_values)

  wait    = var.wait_for_rollout
  timeout = var.rollout_timeout_seconds
}
