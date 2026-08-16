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

# Runs the judge tier and posts the board
# (.github/workflows/evals-nightly.yml; docs/v0.3-plan.md W0.5).
#
# Runnable by hand against a Vertex project, which is the point of it
# being a script rather than inline YAML — the nightly and a local
# investigation of last night's board run the same thing:
#
#   ANTHROPIC_VERTEX_PROJECT_ID=<project> dev/ci/evals-nightly.sh
#
# Provider-neutral: MAST_EVALS_MODEL / MAST_EVALS_GRADER /
# MAST_EVALS_PROVIDER choose what runs, and the credentials come from
# the environment the caller set up, so the Gemini nightly
# (.github/workflows/evals-nightly-gemini.yml) runs this same script
# with a different env block rather than a forked copy of it.
# MAST_EVALS_LABEL only names the provider in the job summary heading,
# so two boards in one Actions run list are told apart at a glance.
#
# Exit status is the harness's: 0 for a complete board however low the
# scores, 1 for a board that is short a row, has a metric scoring
# nothing, or priced a tiered specialist at the wrong rate, 2 for a
# harness that could not run. The scores are never the reason this
# fails — the judge tier reports, S/U/E gate.

set -euo pipefail
cd "$(dirname "$0")/../.."

scratch="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/mast-evals"
mkdir -p "${scratch}"

board="${scratch}/board.json"
report="${scratch}/board.txt"
baseline="${scratch}/baseline/board.json"

args=(--tier=judge "--out=${board}")
if [[ -n "${MAST_EVALS_MODEL:-}" ]]; then
  args+=("--model=${MAST_EVALS_MODEL}")
fi
if [[ -n "${MAST_EVALS_GRADER:-}" ]]; then
  args+=("--grader=${MAST_EVALS_GRADER}")
fi
if [[ -n "${MAST_EVALS_PROVIDER:-}" ]]; then
  args+=("--provider=${MAST_EVALS_PROVIDER}")
fi
if [[ -f "${baseline}" ]]; then
  args+=("--baseline=${baseline}")
fi

# The report goes to the log and to a file in one pass, and the run's
# exit status survives the pipe (pipefail is on, and tee is last).
status=0
scripts/evals.sh "${args[@]}" | tee "${report}" || status=$?

if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    echo "## mast v0.3 parity evals — judge tier${MAST_EVALS_LABEL:+ (${MAST_EVALS_LABEL})}"
    echo ""
    echo '```'
    cat "${report}"
    echo '```'
  } >>"${GITHUB_STEP_SUMMARY}"
fi

exit "${status}"
