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

# Fetches the previous nightly's board so tonight's run can print a
# delta (.github/workflows/evals-nightly.yml).
#
# Absence is normal, not an error: the first run has no predecessor, and
# artifacts expire. Every failure mode here ends with "no baseline", and
# the run continues without a delta section — losing a night's metered
# run because an artifact download 404'd would be the expensive way to
# handle a missing nicety.

set -euo pipefail

scratch="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/mast-evals"
mkdir -p "${scratch}"

# The most recent successful run of this workflow that produced a board.
# --json/-q rather than parsing text: the run list is machine-readable
# and its formatting is not a contract.
prev="$(gh run list \
  --workflow evals-nightly.yml \
  --status success \
  --limit 1 \
  --json databaseId \
  -q '.[0].databaseId' 2>/dev/null || true)"

if [[ -z "${prev}" ]]; then
  echo "no previous successful nightly; tonight's board will have no delta"
  exit 0
fi

if ! gh run download "${prev}" --name judge-board --dir "${scratch}/baseline" 2>/dev/null; then
  echo "run ${prev} has no judge-board artifact (expired?); tonight's board will have no delta"
  exit 0
fi

if [[ -f "${scratch}/baseline/board.json" ]]; then
  echo "baseline: run ${prev}"
else
  echo "run ${prev} produced no board.json; tonight's board will have no delta"
fi
