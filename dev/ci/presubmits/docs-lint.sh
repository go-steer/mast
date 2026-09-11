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

# docs-lint.sh — presubmit: prose-drift checks over README, DESIGN.md
# and the site content (self-test first, so a defanged regex fails
# loudly).
#
# Note for anyone touching the version rule: this job checks out at
# actions/checkout@v4's default depth, which fetches no tags, so the
# current release is read from CHANGELOG.md rather than from
# `git describe`.
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml →
# dev/ci/presubmits/all.sh); run all.sh locally before pushing.

set -euo pipefail
"$(dirname "$0")/../../tools/docs-lint" --self-test
exec "$(dirname "$0")/../../tools/docs-lint"
