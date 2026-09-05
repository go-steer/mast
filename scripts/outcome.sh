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
#   the fixture images pulled, once — the cluster is deliberately never
#     allowed to reach a registry, so they are side-loaded from the host.
#     The list comes from the manifests, so ask rather than hardcode:
#       scripts/outcome.sh --print-images | xargs -rn1 docker pull
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
#
# BUILT AND EXECUTED, NOT `go run`, and the exit codes are the reason.
# `go run` does not propagate a non-zero child status: it prints
# "exit status 2" to stderr and exits 1 itself, which collapses "the tier
# could not run" into "the board is red" — the exact distinction the gate
# is built on. Caught by the first CI run of .github/workflows/outcome.yml,
# which reported a red board for a missing container image.

set -euo pipefail
cd "$(dirname "$0")/.."

bin="$(mktemp -d "${TMPDIR:-/tmp}/mast-outcome-bin.XXXXXX")"
trap 'rm -rf "${bin}"' EXIT
go build -o "${bin}/outcome" ./internal/evals/cmd/outcome
"${bin}/outcome" "$@"
