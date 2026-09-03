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

# e2e.sh — presubmit: the end-to-end UAT harnesses. Both build mast and
# drive a real daemon process against an offline model + a real SQLite
# DB. Credential-free and network-free — CI-runnable in a couple of
# minutes.
#
#   scripts/uat-v0.2.sh — the durable-execution spine: session state,
#     /metrics, HTTP status, process exit codes, crash/drain/abort legs
#     against a blocking stdio MCP tool (docs/uat-v0.2-plan.md).
#   scripts/uat-v0.3.sh — the v0.3 parity work against the SHIPPED
#     examples/workloads/gke-triage bundle: what an operator actually
#     receives at the end of a run (docs/v0.3-plan.md §2, tier U).
#   scripts/uat-v0.4.sh — the v0.4 change-set work: a finding carries
#     the executable call, checked against the named tool's own input
#     schema, and the call an operator approves is the call that fires
#     (docs/v0.4-plan.md §3, W7.0). Plus W4.1's cadence: a workload
#     that wakes itself keeps its phase across a crash and does not pay
#     for the ticks it was down for.
#   scripts/uat-v0.5.sh — the v0.5 monitoring work: a scheduled cycle
#     gathers its own facts before the model is woken, for no model
#     calls of its own, and the daemon refuses a roster that could
#     reach those same tools through a specialist (docs/v0.5-plan.md,
#     W4.2).
#   scripts/uat-v0.6.sh — the v0.6 budget work: a ceiling stops a model
#     call BEFORE it is paid for, so a capped workload spends its cap
#     and not one call more; a specialist's ceiling closes that
#     specialist's path and the turn routes on rather than the session
#     ending; and a planner dispatch's mutating call — which no runner
#     plugin of the outer session ever sees — is on the effect ledger
#     after the process dies mid-call (docs/v0.6-plan.md, W9.x + W10.x).
#
# All five run, in order, and a failure in any fails the presubmit. The
# v0.2 harness is the spine and stays exactly as it is; each release's
# additions go in their own script rather than growing that one, so an
# acceptance pass can be read and re-run on its own.
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml →
# dev/ci/presubmits/all.sh); run all.sh locally before pushing.

set -euo pipefail
cd "$(dirname "$0")/../../.."

scripts/uat-v0.2.sh
scripts/uat-v0.3.sh
scripts/uat-v0.4.sh
scripts/uat-v0.5.sh
scripts/uat-v0.6.sh
