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

# Runs the O tier as a merge gate and posts the board
# (.github/workflows/outcome.yml; docs/outcome-evals-design.md §5.4).
#
# Runnable by hand against a Vertex project, which is the point of it
# being a script rather than inline YAML — the gate and a local
# investigation of a red run the same thing:
#
#   ANTHROPIC_VERTEX_PROJECT_ID=<project> dev/ci/outcome-gate.sh
#
# THE UNCONFIGURED CASE FAILS RATHER THAN SKIPS, and that is the one way
# this differs from dev/ci/evals-nightly-configured.sh. The nightly
# reports, so a missing secret there is "nobody set this up" and skipping
# is honest. This gates, and a gate that reports green because it could
# not run is §7's rung that cannot fire — the failure mode the whole
# design is written against. So a missing credential is red, with the
# reason and the remedy in the job summary.
#
# Exit status is the tier's: 0 green, 1 red, 2 could not run.

set -euo pipefail
cd "$(dirname "$0")/../.."

if [[ -z "${ANTHROPIC_VERTEX_PROJECT_ID:-}${ANTHROPIC_API_KEY:-}${GOOGLE_API_KEY:-}${GEMINI_API_KEY:-}" ]]; then
  {
    echo "## outcome tier: no credentials"
    echo ""
    echo "The O tier grades a real model against a real cluster, so it cannot"
    echo "run without provider credentials — and it fails rather than skipping,"
    echo "because a gate that reports green when it could not run is worse than"
    echo "no gate."
    echo ""
    echo "**On a pull request from a fork this is expected.** GitHub withholds"
    echo "secrets from fork workflows. A maintainer runs this tier on a branch"
    echo "in \`go-steer/mast\` before merging."
    echo ""
    echo "**On a branch in this repo** it means the tier's configuration is"
    echo "missing: see the header of \`.github/workflows/outcome.yml\`."
  } >>"${GITHUB_STEP_SUMMARY:-/dev/stderr}"
  echo "outcome: no provider credentials in the environment" >&2
  exit 2
fi

scratch="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/mast-outcome-ci"
mkdir -p "${scratch}"
report="${scratch}/board.txt"

args=()
if [[ -n "${MAST_OUTCOME_MODEL:-}" ]]; then
  args+=("--model=${MAST_OUTCOME_MODEL}")
fi
if [[ -n "${MAST_OUTCOME_PROVIDER:-}" ]]; then
  args+=("--provider=${MAST_OUTCOME_PROVIDER}")
fi
if [[ -n "${MAST_OUTCOME_CEILING:-}" ]]; then
  args+=("--ceiling=${MAST_OUTCOME_CEILING}")
fi

# The board goes to the log and to a file in one pass, and the run's exit
# status survives the pipe: pipefail is on and tee is last, so the only
# way tee could mask the status is by failing itself. See PR #196 and
# dev/tools/shell-lint for the flake this shape was written after.
status=0
scripts/outcome.sh "${args[@]}" | tee "${report}" || status=$?

if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    echo "## mast outcome evals — the O tier"
    echo ""
    echo '```'
    cat "${report}"
    echo '```'
  } >>"${GITHUB_STEP_SUMMARY}"
fi

exit "${status}"
