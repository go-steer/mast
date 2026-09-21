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

# terraform.sh — presubmit: fmt, validate and `terraform test` over
# examples/deploy/terraform.
#
# The tests run against mocked providers, so nothing here authenticates,
# reads a real project or creates anything. `terraform init` does reach
# registry.terraform.io to fetch the google and helm providers.
#
# This FAILS rather than skips when terraform is absent, for the same
# reason charts/*_test.go fails without helm: a check that skips is
# indistinguishable from a check that passes, and the thing under test —
# whether an operator's write_scope narrowing actually removes an IAM
# binding — is not something to discover from a green build that never
# ran. MAST_SKIP_TERRAFORM_TESTS=1 is the local opt-out; CI ignores it.
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml →
# dev/ci/presubmits/all.sh); run all.sh locally before pushing.

set -euo pipefail

root="$(cd "$(dirname "$0")/../../.." && pwd)"
tf_root="${root}/examples/deploy/terraform"

if ! command -v terraform >/dev/null 2>&1; then
  if [[ "${CI:-}" != "true" && "${MAST_SKIP_TERRAFORM_TESTS:-}" == "1" ]]; then
    echo "terraform not installed and MAST_SKIP_TERRAFORM_TESTS=1 — skipping locally." >&2
    echo "CI runs this for real; do not push on the strength of this line." >&2
    exit 0
  fi
  echo "terraform is not installed: https://developer.hashicorp.com/terraform/install" >&2
  echo "Set MAST_SKIP_TERRAFORM_TESTS=1 to skip locally (CI ignores it)." >&2
  exit 1
fi

echo "terraform $(terraform version -json | sed -n 's/.*"terraform_version": "\([^"]*\)".*/\1/p')"

# fmt over the whole tree at once: canonical formatting is a property of
# the source, not of any one module.
terraform -chdir="${tf_root}" fmt -recursive -check -diff

# The root and each module are initialized and tested separately —
# `terraform test` only collects tests/ under the directory it runs in,
# so a module tested only through the root would have its own runs
# silently never execute.
for dir in "." "modules/wif" "modules/release"; do
  echo ""
  echo "--- ${dir}"
  terraform -chdir="${tf_root}/${dir}" init -backend=false -input=false -no-color >/dev/null
  terraform -chdir="${tf_root}/${dir}" validate -no-color
  terraform -chdir="${tf_root}/${dir}" test -no-color
done
