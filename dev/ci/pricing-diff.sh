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

# Prints which rates moved between two go-steer/core-models pins.
#
#   dev/ci/pricing-diff.sh [BASE_REF [HEAD_REF]]   (default: origin/main HEAD)
#
# core-models carries mast's price table (pricing.Builtin and
# BuiltinByBackend), so a version bump there can reprice every call a
# workload makes. CI cannot tell whether a new rate is right; this puts
# the rates that changed in front of the human who can. It reads the
# core-models requirement from go.mod at each ref, dumps the built-in
# table at each version with dev/pricing-diff, and prints the rows that
# were added, removed or repriced. When GITHUB_STEP_SUMMARY is set the
# report goes there as well.
#
# Informational: it exits 0 whatever moved, and non-zero only when it
# could not produce the report. Not in all.sh, because it needs the
# module proxy.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
base_ref="${1:-origin/main}"
head_ref="${2:-HEAD}"
module=github.com/go-steer/core-models

pin() {
  git show "$1:go.mod" | awk -v m="$module" '$1 == m { print $2; exit } $1 == "require" && $2 == m { print $3; exit }'
}

old="$(pin "$base_ref")"
new="$(pin "$head_ref")"
if [ -z "$old" ] || [ -z "$new" ]; then
  echo "pricing-diff: no ${module} requirement in go.mod at ${base_ref} or ${head_ref}" >&2
  exit 1
fi
if [ "$old" = "$new" ]; then
  echo "pricing-diff: ${module} pin unchanged (${old}); no rates moved"
  exit 0
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# dump VERSION OUT: the built-in table at VERSION, or an empty file when
# that version has no pricing package (core-models before v0.11.0).
dump() {
  local dir="$work/$1"
  mkdir -p "$dir"
  cp dev/pricing-diff/main.go "$dir/"
  (
    cd "$dir"
    go mod init pricingdiff >/dev/null 2>&1
    # Fetch the package, not just the module: a version without it fails
    # here instead of letting a later build resolve it from a newer one.
    if ! go get "${module}/pricing@$1" >"$dir/err" 2>&1; then
      if grep -q 'does not contain package' "$dir/err"; then
        echo "pricing-diff: ${module}@$1 has no pricing package; treating its table as empty" >&2
        : >"$2"
        exit 0
      fi
      cat "$dir/err" >&2
      exit 1
    fi
    got="$(go list -m -f '{{.Version}}' "$module")"
    if [ "$got" != "$1" ]; then
      echo "pricing-diff: asked for ${module}@$1, resolved ${got}" >&2
      exit 1
    fi
    GOFLAGS=-mod=readonly go run . >"$2"
  )
}

dump "$old" "$work/old.tsv"
dump "$new" "$work/new.tsv"

# Split the two tables into rows only one side has and rows whose
# rates differ. join wants both inputs sorted on the key, which the
# dumper already does; LC_ALL=C keeps sort and join agreeing on order.
export LC_ALL=C
join -t "$(printf '\t')" -v 2 "$work/old.tsv" "$work/new.tsv" >"$work/added"
join -t "$(printf '\t')" -v 1 "$work/old.tsv" "$work/new.tsv" >"$work/removed"
join -t "$(printf '\t')" "$work/old.tsv" "$work/new.tsv" | awk -F'\t' '$2 != $3' >"$work/changed"

report="$work/report.md"
{
  echo "### Price table: ${module} ${old} -> ${new}"
  echo
  echo "USD per million tokens. UpdatedAt is ignored; only rows whose rates differ are listed."
  echo
  if [ -s "$work/changed" ]; then
    echo "**Repriced ($(wc -l <"$work/changed"))**"
    echo
    awk -F'\t' '{ print "- `" $1 "`: " $2 " -> " $3 }' "$work/changed"
    echo
  fi
  for side in added removed; do
    if [ -s "$work/$side" ]; then
      echo "**${side^} ($(wc -l <"$work/$side"))**"
      echo
      awk -F'\t' '{ print "- `" $1 "`: " $2 }' "$work/$side"
      echo
    fi
  done
  if [ ! -s "$work/changed" ] && [ ! -s "$work/added" ] && [ ! -s "$work/removed" ]; then
    echo "No rates moved."
  fi
} >"$report"

cat "$report"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  cat "$report" >>"$GITHUB_STEP_SUMMARY"
fi
