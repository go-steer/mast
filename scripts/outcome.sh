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

# outcome.sh — the O tier (#297, docs/outcome-evals-design.md).
#
# The one tier that needs all three of a real model, a real cluster, and
# a verdict that blocks a merge. Everything the S/U/E tiers gate on is a
# property of how mast is built; scoring a trajectory needs a model that
# chooses, and a scripted provider does not choose.
#
# THIS CREATES A KUBERNETES CLUSTER and deletes it on the way out,
# including on every failure path. It is deliberately not a presubmit:
# `dev/ci/presubmits/all.sh` stays credential-free, network-free and
# fast, which is what makes it something people actually run.
#
# What it needs:
#
#   kind, kubectl, a container runtime
#   lookout, pinned — `go install github.com/go-steer/k8s-lookout/cmd/lookout@<pin>`
#     (the pin is outcome.PinnedLookout; the runner refuses a build whose
#     surface does not advertise every tool the intent table names)
#   provider credentials for the model under test
#   docker pull busybox:1.36, once — the fixture image is side-loaded
#
# Usage:
#   scripts/outcome.sh
#   scripts/outcome.sh --model=claude-opus-5
#   scripts/outcome.sh --keep        # leave the cluster up to read a red cell
#
# Scratch state — kubeconfig, session databases — goes under
# ${TMPDIR:-/tmp}/mast-outcome-<pid> (house rule #5).
#
# Exit codes: 0 the board is green; 1 the board is red; 2 the tier could
# not run. The middle one is the only one that is a finding about mast.

set -euo pipefail
cd "$(dirname "$0")/.."

exec go run ./internal/evals/cmd/outcome "$@"
