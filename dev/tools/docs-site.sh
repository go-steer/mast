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

# docs-site.sh — run/test the docs site (docs/site, Astro + Starlight)
# locally. THE way to work on the site; see docs/site/README.md.
#
#   dev/tools/docs-site.sh [dev|build|preview|check] [extra astro args...]
#
#   dev      (default) astro dev with --host, so the server is reachable
#            from containers/VMs, not just localhost
#   build    astro build — exactly what CI runs (.github/workflows/
#            ci-docs.yml calls this script, so local build == CI build)
#   preview  astro preview of the built site (run `build` first)
#   check    build with Starlight's link validation (Starlight validates
#            internal links at build time; no extra deps needed)
#
# Deliberately NOT part of dev/ci/presubmits/all.sh: the site build
# needs Node, and the Go presubmits must stay runnable by every Go
# contributor without a Node toolchain. The separation is by design —
# ci-docs.yml covers the site on paths that touch it.

set -euo pipefail

# Node major required by Astro 7 (>=22.12); keep in lockstep with
# NODE_VERSION in .github/workflows/ci-docs.yml.
NODE_MAJOR=22

site_dir="$(cd "$(dirname "$0")/../../docs/site" && pwd)"

cmd="${1:-dev}"
shift || true

if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
  echo "docs-site.sh: node + npm are required (Node ${NODE_MAJOR}.x)." >&2
  echo "  install hint: https://nodejs.org/ or 'nvm install ${NODE_MAJOR}'" >&2
  exit 1
fi

have_major="$(node --version | sed 's/^v\([0-9]*\).*/\1/')"
if [ "${have_major}" -lt "${NODE_MAJOR}" ]; then
  echo "docs-site.sh: Node ${NODE_MAJOR}.x required, found $(node --version)." >&2
  echo "  install hint: 'nvm install ${NODE_MAJOR}'" >&2
  exit 1
fi

cd "${site_dir}"

# Install deps when node_modules is missing or the lockfile is newer
# than the last install.
if [ ! -d node_modules ] || [ package-lock.json -nt node_modules ]; then
  echo "docs-site.sh: installing dependencies (npm ci) ..."
  npm ci
  touch node_modules
fi

case "${cmd}" in
  dev)
    exec npx astro dev --host "$@"
    ;;
  build)
    exec npx astro build "$@"
    ;;
  preview)
    exec npx astro preview "$@"
    ;;
  check)
    # Starlight validates internal links during `astro build`; a green
    # build is the link check. Kept as a named subcommand so a richer
    # checker can slot in without callers changing.
    exec npx astro build "$@"
    ;;
  *)
    echo "usage: dev/tools/docs-site.sh [dev|build|preview|check] [extra astro args...]" >&2
    exit 2
    ;;
esac
