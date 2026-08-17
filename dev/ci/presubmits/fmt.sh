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

# fmt.sh — presubmit: gofmt cleanliness over the Go source trees.
# Fails listing the offending files; fix with `gofmt -w <file>`.
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml →
# dev/ci/presubmits/all.sh); run all.sh locally before pushing.

set -euo pipefail
cd "$(dirname "$0")/../../.."

# dev is in the list because dev/ now holds real Go programs
# (regen-builtin-pricing, upstream-drift), not just shell. vet, lint
# and test already reach them via ./...; gofmt takes paths, so it has
# to be told.
offenders="$(gofmt -l ./*.go cmd internal pkg examples dev)"
if [[ -n "${offenders}" ]]; then
  echo "FAIL: the following files are not gofmt-clean:" >&2
  sed 's/^/  - /' <<<"${offenders}" >&2
  echo "Fix with: gofmt -w ${offenders//$'\n'/ }" >&2
  exit 1
fi

echo "OK: root, cmd, internal, pkg, examples, dev are gofmt-clean."
