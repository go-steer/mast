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

output "values_yaml" {
  description = "The values document this module renders, before extra_values are merged over it. Useful in a `terraform console` when reconciling an install against values.yaml."
  value       = local.values
}

output "namespace" {
  description = "Namespace the release installed into, read off the release rather than echoed back from the variable — it is half of the daemon's identity and the WIF principal embeds the other half."
  value       = helm_release.mast.namespace
}

output "release_name" {
  description = "Helm release name, for `helm -n <ns> get values <name>`."
  value       = var.release_name
}

output "required_secrets" {
  description = <<-EOT
    The Secrets this module names and does not create. Both must hold the
    SAME token, and neither exists until you create it:

      TOKEN=$(openssl rand -hex 32)
      kubectl -n <ns> create secret generic <name> --from-literal=token="$TOKEN"
  EOT
  value = compact([
    var.inject_token_secret,
    var.watcher_enabled ? var.watcher_token_secret : "",
  ])
}
