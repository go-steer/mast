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

# evals.sh — presubmit: the v0.3 parity eval suite, deterministic tier.
# Checks that the ported LangChain corpus is still measurable (no metric
# has degenerated into a constant function) and runs the five
# differentiator scenarios against the composed runtime, holding each to
# its declared outcome in both directions (scripts/evals.sh;
# docs/v0.3-plan.md W0.4). Credential-free and network-free.
#
# The judge tier is deliberately not run here: it needs live provider
# credentials and costs money, so it reports nightly rather than gating
# (W0.5).
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml →
# dev/ci/presubmits/all.sh); run all.sh locally before pushing.

set -euo pipefail
cd "$(dirname "$0")/../../.."

exec scripts/evals.sh --tier=deterministic
