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

# Fetches a previous board so this run can print a delta
# (.github/workflows/evals-nightly.yml and its Gemini sibling).
#
# Absence is normal, not an error: the first run has no predecessor, and
# artifacts expire. Every failure mode here ends with "no baseline", and
# the run continues without a delta section — losing a night's metered
# run because an artifact download 404'd would be the expensive way to
# handle a missing nicety.
#
# MAST_EVALS_WORKFLOW / MAST_EVALS_ARTIFACT name which history to read.
# There is one nightly per provider and their boards are not comparable.
# The defaults are the Claude nightly's, so a caller that says nothing
# gets the behavior this script had when it was the only one.
#
# ## A baseline is only a baseline if it measured the same thing
#
# "The last successful run of this workflow" is not enough on its own,
# because both nightlies take a `model` dispatch input. A hand-dispatched
# run against some other model uploads under the same artifact name and
# is then the newest success — so the next scheduled night deltas its
# board against a different model's. That is not a wrong-numbers bug:
# Summary.WriteDelta records the model pair on both boards and prints
# "the scores below are not comparable, they are two different
# measurements" before the rows (internal/evals/harness/delta.go). The
# cost is the trend — a night whose delta a reader has been told not to
# believe, which is the one thing the nightly exists to produce.
#
# So the run is chosen by what its board says it measured, not by being
# newest:
#
#   MAST_EVALS_BASELINE_MODEL   model the baseline board must have run
#                               (default: MAST_EVALS_MODEL)
#   MAST_EVALS_BASELINE_GRADER  grader it must have scored with
#                               (default: MAST_EVALS_GRADER)
#   MAST_EVALS_BASELINE_DEPTH   how many successful runs to walk back
#                               through looking for one (default 5)
#
# Set either to the empty string to compare across models deliberately,
# which is what .github/workflows/evals-candidate-gemini.yml does — a
# candidate model against the incumbent's board IS the measurement
# there, and the delta's "not comparable" line is the correct caption
# for it. Leave them unset and this run's own model/grader are what a
# baseline has to match. An empty expectation checks nothing, so a
# workflow that lets the harness pick the model gets the old behavior.
#
# Walking back also covers the duller cases the single-run lookup got
# wrong: a successful run that skipped (unconfigured), or one whose
# artifact has aged out while an older one has not.

set -euo pipefail

workflow="${MAST_EVALS_WORKFLOW:-evals-nightly.yml}"
artifact="${MAST_EVALS_ARTIFACT:-judge-board}"
depth="${MAST_EVALS_BASELINE_DEPTH:-5}"

# `${VAR-...}` and not `${VAR:-...}`: set-but-empty means "any model
# will do", and only unset falls back to what this run measures.
want_model="${MAST_EVALS_BASELINE_MODEL-${MAST_EVALS_MODEL:-}}"
want_grader="${MAST_EVALS_BASELINE_GRADER-${MAST_EVALS_GRADER:-}}"

scratch="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/mast-evals"
dest="${scratch}/baseline"
mkdir -p "${scratch}"
rm -rf "${dest}"

if [[ -n "${want_model}${want_grader}" ]] && ! command -v jq >/dev/null 2>&1; then
  # Fail closed. Taking the newest board unchecked is the behavior this
  # script was changed to stop doing.
  echo "jq is not installed and a baseline has to be checked against ${want_model:-any}/${want_grader:-any}; no baseline"
  exit 0
fi

# The most recent successful runs of this workflow. --json/-q rather
# than parsing text: the run list is machine-readable and its formatting
# is not a contract.
runs="$(gh run list \
  --workflow "${workflow}" \
  --status success \
  --limit "${depth}" \
  --json databaseId \
  -q '.[].databaseId' 2>/dev/null || true)"

if [[ -z "${runs}" ]]; then
  echo "no previous successful ${workflow} run; this board will have no delta"
  exit 0
fi

for prev in ${runs}; do
  rm -rf "${dest}"

  if ! gh run download "${prev}" --name "${artifact}" --dir "${dest}" 2>/dev/null; then
    echo "run ${prev}: no ${artifact} artifact (expired, or the run produced none) — looking further back"
    continue
  fi

  board="${dest}/board.json"
  if [[ ! -f "${board}" ]]; then
    echo "run ${prev}: ${artifact} has no board.json — looking further back"
    continue
  fi

  if [[ -n "${want_model}${want_grader}" ]]; then
    # One jq call, two fields; model names carry no spaces. A board
    # without a judge section (the deterministic tier's) reads as empty
    # and fails the match, which is the right answer.
    pair="$(jq -r '[(.judge.model // ""), (.judge.grader // "")] | join(" ")' "${board}" 2>/dev/null || true)"
    got_model="${pair%% *}"
    got_grader="${pair#* }"

    if [[ -n "${want_model}" && "${got_model}" != "${want_model}" ]] ||
      [[ -n "${want_grader}" && "${got_grader}" != "${want_grader}" ]]; then
      echo "run ${prev}: board measured ${got_model:-?}/${got_grader:-?}, this run needs a baseline that measured ${want_model:-any}/${want_grader:-any} — looking further back"
      continue
    fi
  fi

  echo "baseline: run ${prev} (measured ${want_model:-any}/${want_grader:-any})"
  exit 0
done

rm -rf "${dest}"
echo "none of the last ${depth} successful ${workflow} runs has a comparable board; this board will have no delta"
