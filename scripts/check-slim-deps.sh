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

# check-slim-deps.sh — CI enforcement for the slim-embed guarantee
# (docs/library-api-design.md, "Slim-embed guarantee"): the reference
# slim consumer's transitive dependency graph must exclude the heavy
# subsystems. A PR that grows the slim graph fails here — the
# guarantee can't erode silently.
#
# Denylist notes (keep in sync with docs/library-api-design.md):
#
#   * mast-side packages: pkg/inject, pkg/observability, pkg/mcp,
#     pkg/graph, pkg/router, pkg/config are the subsystems a slim
#     embed must not pay for. The slim slice is pkg/agent,
#     pkg/specialists (+ optionally pkg/workload, pkg/budget,
#     pkg/transcript).
#
#   * go.opentelemetry.io is denylisted at the SDK/exporter level
#     only, NOT wholesale. Structural finding (2026-07-26): ADK v2's
#     model path depends on google.golang.org/genai, which imports
#     the OTel *API* (go.opentelemetry.io/otel, /trace, /metric) and
#     the otelhttp instrumentation directly. That API surface is
#     no-op stubs unless an SDK is installed, and mast cannot shed it
#     without shedding ADK itself. The heavy parts — the OTel SDK and
#     the OTLP exporters — enter the graph only via
#     pkg/observability, so those are what we deny.
#
#   * github.com/prometheus/ and github.com/modelcontextprotocol/
#     enter only via pkg/observability and pkg/mcp respectively;
#     denied wholesale.

set -euo pipefail

cd "$(dirname "$0")/.."

SLIM_PKG=./examples/deploy/slim

# Extended regexes, anchored at the start of the import path.
DENYLIST=(
  '^github\.com/go-steer/mast/pkg/inject(/|$)'
  '^github\.com/go-steer/mast/pkg/observability(/|$)'
  '^github\.com/go-steer/mast/pkg/mcp(/|$)'
  '^github\.com/go-steer/mast/pkg/graph(/|$)'
  '^github\.com/go-steer/mast/pkg/router(/|$)'
  '^github\.com/go-steer/mast/pkg/config(/|$)'
  '^github\.com/prometheus/'
  '^github\.com/modelcontextprotocol/'
  '^go\.opentelemetry\.io/otel/sdk(/|$)'
  '^go\.opentelemetry\.io/otel/exporters/'
)

deps="$(go list -deps "$SLIM_PKG")"

offenders=""
for pattern in "${DENYLIST[@]}"; do
  matches="$(grep -E "$pattern" <<<"$deps" || true)"
  if [[ -n "$matches" ]]; then
    offenders+="$matches"$'\n'
  fi
done

if [[ -n "$offenders" ]]; then
  echo "FAIL: slim-embed guarantee violated." >&2
  echo "" >&2
  echo "$SLIM_PKG transitively depends on denylisted packages:" >&2
  echo "" >&2
  sort -u <<<"$offenders" | sed '/^$/d; s/^/  - /' >&2
  echo "" >&2
  echo "The slim reference consumer must only pull the slim slice" >&2
  echo "(pkg/agent, pkg/specialists, optionally pkg/workload," >&2
  echo "pkg/budget, pkg/transcript) plus ADK and stdlib. See" >&2
  echo "docs/library-api-design.md, 'Slim-embed guarantee'." >&2
  exit 1
fi

echo "OK: $SLIM_PKG dependency graph is slim ($(wc -l <<<"$deps") packages, no denylisted deps)."
