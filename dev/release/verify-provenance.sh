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

# Verifies the SLSA build provenance of a published mast artifact (#342).
#
#   dev/release/verify-provenance.sh v0.9.1
#   dev/release/verify-provenance.sh --oci ghcr.io/go-steer/mast@sha256:...
#   dev/release/verify-provenance.sh --oci ghcr.io/go-steer/charts/mast@sha256:...
#
# Both release workflows run this against what they have just published,
# for the reason the sibling verify-signature.sh gives: a step that
# attests exiting 0 says an attestation was generated on a runner, not
# that a verifier can find one.
#
# ## Provenance is not the signature over again
#
# `checksums.txt.sig` answers *who signed this*: mast's release workflow,
# at this tag. The provenance answers *what built it* — the source
# repository and commit, the workflow file and its ref, the runner
# environment, the invocation. Those are different questions, and the
# second is the one you need to check the artifact against the tree you
# are reading on GitHub. A signature cannot tell you a tarball was built
# from the commit the tag points at; provenance can.
#
# ## Naming the signer is the whole product, again
#
# `gh attestation verify <file> --repo go-steer/mast` accepts an
# attestation from *any* workflow in this repository — docs builds, CI,
# anything with `attestations: write`. That is the same shape as cosign's
# `--certificate-identity-regexp '.*'` and it fails the same way: the
# command prints a success line and has checked less than the reader
# thinks. So every invocation below pins:
#
#   --signer-workflow   the workflow file that is allowed to have
#                       attested this, and
#   --source-ref        the ref it ran on — refs/tags/<tag> for a
#                       release, so an attestation minted on a branch
#                       cannot stand in for one minted on the tag.
#
# `gh` populates both from the certificate, not from the predicate. The
# predicate body is written by the build and a compromised workflow can
# say anything in it; the certificate fields come from GitHub's OIDC
# token. Pin the fields that cannot be forged.
#
# ## What this needs
#
# `gh`, authenticated as any GitHub account. Unlike verify-signature.sh,
# this one is not credential-free: attestations for loose files are
# fetched from the GitHub API. The `--oci` mode reads the bundle from the
# registry instead (`--bundle-from-oci`), so it needs a pull credential
# for the registry rather than a GitHub account — for mast's public
# packages, none.

set -euo pipefail

REPO="${MAST_REPO:-go-steer/mast}"
WORKFLOW_RELEASE="${REPO}/.github/workflows/release.yml"
WORKFLOW_IMAGES="${REPO}/.github/workflows/release-images.yml"

usage() {
  cat >&2 <<EOF
usage: $(basename "$0") <tag>              # e.g. v0.9.1 — the release assets
       $(basename "$0") --oci <ref>        # e.g. ghcr.io/go-steer/mast@sha256:...
EOF
  exit 2
}

command -v gh >/dev/null 2>&1 || {
  echo "error: this needs the gh CLI (https://cli.github.com), authenticated." >&2
  exit 2
}

# ------------------------------------------------------------ --oci mode

if [[ "${1:-}" == "--oci" ]]; then
  REF="${2:-}"
  [[ -n "${REF}" ]] || usage
  # A tag is a moving pointer and provenance is about one build, so this
  # refuses a tag rather than resolving one: `helm pull`/`docker pull`
  # print the digest they resolved, and that is the thing to paste here.
  [[ "${REF}" == *"@sha256:"* ]] || {
    echo "error: --oci wants a digest reference, got: ${REF}" >&2
    echo "       A tag can be re-pointed after it is attested. Use <repo>@sha256:..." >&2
    exit 2
  }
  echo "verifying provenance for oci://${REF}"
  echo "  signer:   ${WORKFLOW_IMAGES}"
  echo
  gh attestation verify "oci://${REF}" \
    --repo "${REPO}" \
    --signer-workflow "${WORKFLOW_IMAGES}" \
    --bundle-from-oci
  echo
  echo "OK: ${REF} was built and attested by ${WORKFLOW_IMAGES}."
  exit 0
fi

# ------------------------------------------------------------- tag mode

TAG="${1:-}"
[[ -n "${TAG}" ]] || usage
if [[ ! "${TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.]+)?$ ]]; then
  echo "error: expected vX.Y.Z or vX.Y.Z-<pre>, got: ${TAG}" >&2
  exit 2
fi

BASE="https://github.com/${REPO}/releases/download/${TAG}"

workdir="$(mktemp -d "${TMPDIR:-/tmp}/mast-provenance-XXXXXX")"
trap 'rm -rf "${workdir}"' EXIT
cd "${workdir}"

echo "verifying provenance for ${REPO} ${TAG}"
echo "  signer:   ${WORKFLOW_RELEASE}"
echo "  ref:      refs/tags/${TAG}"
echo

if ! curl -fsSLO "${BASE}/checksums.txt"; then
  echo "error: ${TAG} has no checksums.txt." >&2
  exit 1
fi

# Every asset the release carries, not just the checksum file. The
# signature deliberately covers checksums.txt alone, because the file
# transitively covers the tarballs; provenance is attested per subject so
# that `gh attestation verify mast_X_linux_amd64.tar.gz` — the command
# someone actually runs on the thing they downloaded — answers without a
# second indirection through a checksum list.
assets=(checksums.txt)
while read -r asset; do
  [[ -n "${asset}" ]] && assets+=("${asset}")
done < <(awk '{print $2}' checksums.txt)

for asset in "${assets[@]}"; do
  if [[ "${asset}" != "checksums.txt" ]]; then
    echo "  fetching ${asset}"
    curl -fsSLO "${BASE}/${asset}"
  fi
done
echo

failed=0
for asset in "${assets[@]}"; do
  if gh attestation verify "${asset}" \
    --repo "${REPO}" \
    --signer-workflow "${WORKFLOW_RELEASE}" \
    --source-ref "refs/tags/${TAG}"; then
    :
  else
    echo "error: no usable provenance for ${asset}" >&2
    failed=1
  fi
done

if [[ ${failed} -ne 0 ]]; then
  echo >&2
  echo "FAILED: ${TAG} has assets with no provenance from ${WORKFLOW_RELEASE}." >&2
  echo "        Attestation landed with #342; v0.9.0 and everything before it" >&2
  echo "        have none and cannot be verified this way." >&2
  exit 1
fi

echo
echo "OK: all ${#assets[@]} assets of ${TAG} were built by ${WORKFLOW_RELEASE} at refs/tags/${TAG}."
