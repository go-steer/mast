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

# Reports whether the judge tier has credentials to run with
# (.github/workflows/evals-nightly.yml). Not a presubmit — nothing on
# the PR path runs the metered tier.
#
# Missing configuration is a skip, not a failure: a nightly that goes
# red the day someone rotates a secret trains its readers to ignore it,
# and the state it would be reporting ("nobody set this up") is not a
# fact about mast. The skip is loud in the job summary so it cannot be
# mistaken for a clean run.

set -euo pipefail

if [[ -n "${PROJECT:-}" && -n "${WIF:-}" ]]; then
  echo "configured=true" >>"${GITHUB_OUTPUT}"
  exit 0
fi

echo "configured=false" >>"${GITHUB_OUTPUT}"

# Written as ifs rather than `[[ ... ]] && missing+=(...)`: under
# `set -e` a false test makes the and-list the failing command, which
# would exit before the summary is written.
missing=()
if [[ -z "${PROJECT:-}" ]]; then
  missing+=("vars.MAST_EVALS_VERTEX_PROJECT")
fi
if [[ -z "${WIF:-}" ]]; then
  missing+=("secrets.MAST_EVALS_WIF_PROVIDER")
fi

{
  echo "## judge tier skipped"
  echo ""
  echo "The metered eval tier needs live provider credentials, and this"
  echo "repository has not set: ${missing[*]}."
  echo ""
  echo "See the header of \`.github/workflows/evals-nightly.yml\` for the full list."
} >>"${GITHUB_STEP_SUMMARY}"

echo "judge tier skipped: missing ${missing[*]}"
