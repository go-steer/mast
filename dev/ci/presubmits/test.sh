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

# test.sh — presubmit: the whole module's unit tests, under the race
# detector. -race matches core-agent's CI bar (dev/tools/test-unit
# there) — the P1.3b ports brought real concurrency (vertexcache
# background init/refresh, the watchdog tap, the inject handlers'
# session pools) and their upstream regression tests were written
# against -race; running without it would silently weaken them.
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml →
# dev/ci/presubmits/all.sh); run all.sh locally before pushing.

set -euo pipefail
cd "$(dirname "$0")/../../.."

go test -race -timeout 5m ./...
