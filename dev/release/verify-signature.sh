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

# Verifies a published mast release's Sigstore signature, end to end,
# from the artifacts as an outsider downloads them.
#
#   dev/release/verify-signature.sh v0.9.1
#
# Anyone can run this — it needs `cosign` and `curl` and nothing else,
# no GitHub credentials and no checkout. .github/workflows/release.yml
# runs it against the release it just cut, which is the whole reason it
# is a script rather than a `run:` block: a verifier the project cannot
# hand to a user is a verifier that only proves the project's own step
# did not error.
#
# ## What is actually being checked
#
# Three things, and it is worth being precise about which:
#
#   1. checksums.txt carries a valid Sigstore signature.
#   2. That signature was made by a certificate whose *identity* is
#      mast's release workflow at this tag — not merely "some
#      certificate from some GitHub Action". A signature that verifies
#      against any identity proves only that somebody signed something.
#   3. The tarballs on the release hash to what that signed file says.
#
# Skip (3) and you have verified a signature over a list you then
# ignored. You cannot skip (2) by omission — cosign refuses keyless
# verify-blob with no identity flag at all — but you can skip it by
# widening: `--certificate-identity-regexp '.*'` reports Verified OK
# against any workflow in any repository, and an unanchored pattern
# matches anywhere in the identity, so it is a substring match rather
# than the repository check it looks like. Both measured, cosign v2.6.5.
#
# ## Why the identity is the workflow file and not a key
#
# Keyless signing means there is no long-lived private key: cosign
# exchanges the workflow's GitHub OIDC token for a short-lived Fulcio
# certificate, and the identity that certificate carries is the
# workflow path plus the ref it ran on. So the thing a verifier pins is
# "mast's release.yml, running on this tag" — which is checkable
# against a public transparency log and is not something a leaked
# secret can forge.

set -euo pipefail

TAG="${1:-}"
if [[ -z "${TAG}" ]]; then
  echo "usage: $(basename "$0") <tag>    # e.g. v0.9.1" >&2
  exit 2
fi
if [[ ! "${TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.]+)?$ ]]; then
  echo "error: expected vX.Y.Z or vX.Y.Z-<pre>, got: ${TAG}" >&2
  exit 2
fi

REPO="${MAST_REPO:-go-steer/mast}"
BASE="https://github.com/${REPO}/releases/download/${TAG}"
IDENTITY="https://github.com/${REPO}/.github/workflows/release.yml@refs/tags/${TAG}"
ISSUER="https://token.actions.githubusercontent.com"

workdir="$(mktemp -d "${TMPDIR:-/tmp}/mast-verify-XXXXXX")"
trap 'rm -rf "${workdir}"' EXIT
cd "${workdir}"

echo "verifying ${REPO} ${TAG}"
echo "  identity: ${IDENTITY}"
echo "  issuer:   ${ISSUER}"
echo

# (1) + (2). Fetch the signed file and its two detached companions.
for asset in checksums.txt checksums.txt.sig checksums.txt.pem; do
  if ! curl -fsSLO "${BASE}/${asset}"; then
    echo "error: ${TAG} has no ${asset}." >&2
    echo "       Signing landed with #342; v0.9.0 and everything before it ship" >&2
    echo "       checksums.txt alone and cannot be verified this way." >&2
    exit 1
  fi
done

cosign verify-blob checksums.txt \
  --signature checksums.txt.sig \
  --certificate checksums.txt.pem \
  --certificate-identity "${IDENTITY}" \
  --certificate-oidc-issuer "${ISSUER}"

# (3). A signature over a checksum file is worth nothing until the
# checksums are checked against the bytes. Fetch every tarball the
# signed file names and let sha256sum have the last word.
echo
awk '{print $2}' checksums.txt | while read -r asset; do
  [[ -n "${asset}" ]] || continue
  echo "  fetching ${asset}"
  curl -fsSLO "${BASE}/${asset}"
done

sha256sum --check checksums.txt

echo
echo "OK: ${TAG} checksums.txt is signed by ${IDENTITY}, and every asset it names matches."
