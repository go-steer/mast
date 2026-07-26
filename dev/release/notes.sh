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

# notes.sh <tag> — extract the CHANGELOG.md section for <tag> to stdout.
#
# Matches a `## <tag> ...` heading (leading `v` optional in the heading)
# and prints everything up to the next `## ` heading. Falls back to the
# `## Unreleased` section — with a loud header — so a tag cut before the
# CHANGELOG was rolled still gets meaningful notes (same fallback idea
# as core-agent's compose-release-notes.sh).
set -euo pipefail

TAG="${1:?usage: notes.sh <tag>}"
CHANGELOG="$(dirname "$0")/../../CHANGELOG.md"

extract() { # extract <heading-regex>
  awk -v re="$1" '
    $0 ~ re { found=1; next }
    found && /^## / { exit }
    found { print }
  ' "$CHANGELOG"
}

BODY="$(extract "^## (v)?${TAG#v}([^0-9A-Za-z.-]|$)")"
if [ -z "${BODY//[[:space:]]/}" ]; then
  BODY="$(extract "^## Unreleased")"
  if [ -z "${BODY//[[:space:]]/}" ]; then
    echo "notes.sh: no CHANGELOG section for ${TAG} and no Unreleased section" >&2
    exit 1
  fi
  printf '> Notes drawn from the Unreleased section (no `## %s` heading found in CHANGELOG.md at tag time).\n' "$TAG"
fi
printf '%s\n' "$BODY"
