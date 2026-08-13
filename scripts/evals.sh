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

# evals.sh — the v0.3 parity eval suite (docs/v0.3-plan.md W0.4). Runs
# two things and prints one board:
#
#   1. The corpus check. Loads the 31 ported LangChain SRE scenarios and
#      the intent table and verifies each metric can score something. It
#      does NOT score the corpus — scoring a trajectory needs a model
#      that chooses, and the free tier has none, so replaying a fixture
#      would only assert the script equals itself. That is the judge
#      tier's job (W0.5). What this gates is that no metric has become a
#      constant function, which is how both of upstream's custom-code
#      evaluators are broken today.
#
#   2. The five differentiator scenarios — the parity claims upstream's
#      harness structurally cannot express — driven against the composed
#      mast runtime with a scripted model and a real SQLite session DB.
#      Each is held to the outcome it declares in Go, in both
#      directions: a red one that goes green fails the suite too, so
#      landing a capability forces its declaration to be updated.
#
# The expected-fail allowlist it prints is v0.3's progress metric. Every
# entry names the workstream that removes it; the list should only ever
# shrink.
#
# Credential-free and network-free. Runs in a couple of seconds.
#
# Usage: scripts/evals.sh [--tier=deterministic|judge] [--format=text|json]
# Scratch state goes under ${TMPDIR:-/tmp} (house rule #5).
#
# Exit codes: 0 green; 1 a scenario missed its declared outcome or a
# metric scores nothing; 2 the harness could not run.

set -euo pipefail
cd "$(dirname "$0")/.."

exec go run ./internal/evals/cmd/evals "$@"
