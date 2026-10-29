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

# go-toolchain.sh — presubmit: every Dockerfile's `ARG GO_VERSION` default
# equals the Go that CI builds and scans with.
#
# CI reads go.mod through setup-go's go-version-file, which prefers the
# `toolchain` line, and that line sits ahead of `go` only for a stdlib
# security fix (#507, #568). The golang images set GOTOOLCHAIN=local, so a
# Dockerfile pin that is lower than `toolchain` but still >= `go` does not
# fail: the image builds, and ships the stdlib CI just scanned as
# vulnerable. ci-image.yml cannot see that, because nothing breaks. This
# check is the guard. It keeps the literal pin (so `docker build .` works
# with no workflow around it) and makes drift from go.mod a red build.
#
# AX_GO_VERSION and other *_GO_VERSION args build other modules and are not
# checked.

set -euo pipefail
cd "$(dirname "$0")/../../.."

want="$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit }' go.mod)"
source_line="toolchain"
if [[ -z "${want}" ]]; then
  want="$(awk '$1 == "go" { print $2; exit }' go.mod)"
  source_line="go"
fi
if [[ -z "${want}" ]]; then
  echo "FAIL: go.mod has neither a toolchain nor a go directive" >&2
  exit 1
fi

mapfile -t dockerfiles < <(find . -path ./.git -prune -o -path '*/node_modules' -prune -o \
  -type f -name 'Dockerfile*' -print | sort)

bad=()
checked=0
for f in "${dockerfiles[@]}"; do
  while IFS= read -r line; do
    checked=$((checked + 1))
    got="${line#*GO_VERSION=}"
    got="${got%%[[:space:]]*}"
    got="${got//\"/}"
    if [[ "${line}" != *"GO_VERSION="* || -z "${got}" ]]; then
      bad+=("${f}: '${line}' has no default (want ${want})")
    elif [[ "${got}" != "${want}" ]]; then
      bad+=("${f}: GO_VERSION=${got} (want ${want})")
    fi
  done < <(grep -E '^[[:space:]]*ARG[[:space:]]+GO_VERSION([=[:space:]]|$)' "${f}" || true)
done

if ((${#bad[@]} > 0)); then
  echo "FAIL: Dockerfile GO_VERSION pins disagree with go.mod's ${source_line} line (${want}):" >&2
  printf '  - %s\n' "${bad[@]}" >&2
  echo "Set each to ARG GO_VERSION=${want}." >&2
  exit 1
fi

if ((checked == 0)); then
  echo "FAIL: found no 'ARG GO_VERSION' in any Dockerfile; the check would pass vacuously" >&2
  exit 1
fi

echo "OK: ${checked} Dockerfile GO_VERSION pin(s) match go.mod's ${source_line} line (${want})."
