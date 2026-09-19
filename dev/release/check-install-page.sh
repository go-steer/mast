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

# check-install-page.sh — the install page must name the assets the
# release actually publishes.
#
# Usage:
#   dev/release/check-install-page.sh              # against .goreleaser.yaml
#   dev/release/check-install-page.sh --release v1.0.0   # against the published release
#   dev/release/check-install-page.sh --self-test
#
# ## Why this exists
#
# docs-lint's rule 5 (#306) checks the *version* in install.md's
# download URLs, because the page sat at v0.4.0 for three releases and
# a reader following it got a 404. It does not check the *filenames*.
# A page listing four tarballs is equally wrong if the build matrix
# grew a fifth, or if the archive name template changed, or if it
# promises a `checksums.txt.sig` the release does not carry — and all
# three of those are invisible to a version comparison, because every
# version on the page is correct while the page as a whole is a lie.
#
# #342's "done when" is that the page telling an operator how to
# install mast is "checked against the release by CI rather than by
# memory". This is that check.
#
# ## The two modes are the same comparison against two sources
#
# The page's claim is parsed once. What it is compared against differs:
#
#   default        the asset set `.goreleaser.yaml` will produce, at
#                  the version CHANGELOG.md's newest heading names.
#                  Offline, deterministic, and correct on the
#                  release-prep PR — where the tag does not exist yet
#                  and the changelog heading is already right. This is
#                  the mode CI's docs-lint job runs on every PR.
#
#   --release TAG  the asset set the published release actually
#                  carries, read with `gh`. This is the mode
#                  .github/workflows/release.yml runs after publishing,
#                  and it is the one that closes the loop: the default
#                  mode proves the page matches the config, and a
#                  config is a prediction. Same discipline as
#                  verify-signature.sh beside it — GoReleaser exiting 0
#                  says what happened on a runner, not what reached the
#                  release.
#
# ## Two kinds of claim, compared in two places
#
# The page makes two different claims about the assets, and they are
# not interchangeable:
#
#   the "Assets for vX.Y.Z" list  — what you can download for the
#                                   release named there. Compared by
#                                   set equality; a name missing from
#                                   it or invented in it is an error.
#
#   the signature artifacts       — `checksums.txt.sig` / `.pem`.
#                                   Required to be named *somewhere on
#                                   the page*, not in that list.
#
# The split is not a loophole, it is the transition #342 created:
# signing landed after v0.9.0 was cut, so the published v0.9.0 carries
# no `.sig` while `.goreleaser.yaml` at HEAD produces one. Demanding
# `.sig` in a list headed "Assets for v0.9.0" would demand the page
# state a falsehood. What both modes do require is that a configured
# signature is documented at all — the failure worth catching is a
# release that signs and a page that never tells anyone, which is the
# same as not signing.
#
# ## Failure is loud, never silent
#
# Every field this script derives from `.goreleaser.yaml` is required.
# If the config grows a shape the parser does not understand — a second
# archive, a name template that is not the one below — this exits 2 and
# says which field, rather than deriving a smaller set and passing.
# A check that cannot find its inputs and reports OK is the #306
# failure mode with extra steps.

set -euo pipefail

# The archive name template this script knows how to evaluate. Pinned
# rather than interpreted: a bash reimplementation of GoReleaser's
# template language is a second thing to keep correct, and the useful
# property is that changing the template in .goreleaser.yaml fails here
# loudly and makes someone update both.
readonly KNOWN_NAME_TEMPLATE='{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}'

PAGE="docs/site/src/content/docs/install.md"
CONFIG=".goreleaser.yaml"
CHANGELOG="CHANGELOG.md"
RELEASE_TAG=""
SELF_TEST=0

die() { echo "check-install-page: $*" >&2; exit 2; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --release) RELEASE_TAG="${2:-}"; [[ -n "$RELEASE_TAG" ]] || die "--release needs a tag"; shift 2 ;;
    --page)    PAGE="${2:-}"; shift 2 ;;
    --config)  CONFIG="${2:-}"; shift 2 ;;
    --changelog) CHANGELOG="${2:-}"; shift 2 ;;
    --self-test) SELF_TEST=1; shift ;;
    -h|--help) sed -n '16,24p' "$0"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

# ---------------------------------------------------------------- page

# The version the page claims, from its "Assets for vX.Y.Z" line.
page_version() {
  local v
  v=$(awk 'match($0, /[Aa]ssets for v[0-9][0-9.]*/) {
             s = substr($0, RSTART, RLENGTH); sub(/.*v/, "", s); print s; exit }' "$PAGE")
  [[ -n "$v" ]] || die "$PAGE has no 'Assets for vX.Y.Z' line — that line is what anchors the asset list"
  printf '%s\n' "$v"
}

# The asset names the page lists: the bullet list of backticked names
# immediately under the "Assets for" line, up to the first line that is
# not such a bullet.
page_assets() {
  awk '
    /[Aa]ssets for v[0-9]/ { inlist = 1; next }
    inlist && /^- `[^`]+`$/ { gsub(/^- `|`$/, ""); print; next }
    inlist && /^[[:space:]]*$/ { next }
    inlist { exit }
  ' "$PAGE"
}

# Every asset name a download URL on the page points at.
page_download_urls() {
  grep -oE 'releases/download/v[0-9][0-9.]*/[A-Za-z0-9._-]+' "$PAGE" | sed 's|.*/||' || true
}

# ---------------------------------------------------------- goreleaser

# Emit key=value for the fields we need, tracking which top-level
# block each line is under. Anything absent is caught by the caller.
goreleaser_facts() {
  awk '
    function list(s,   inner) {
      if (match(s, /\[[^]]*\]/)) {
        inner = substr(s, RSTART + 1, RLENGTH - 2)
        gsub(/[[:space:]]/, "", inner)
        return inner
      }
      return ""
    }
    /^[a-z_]+:/ { section = $1; sub(/:.*/, "", section); infiles = 0 }
    section == "project_name" && /^project_name:/ { print "project=" $2; next }
    section == "builds"   && /^[[:space:]]+binary:/   { print "binary=" $2; next }
    section == "builds"   && /^[[:space:]]+goos:/     { print "goos=" list($0); next }
    section == "builds"   && /^[[:space:]]+goarch:/   { print "goarch=" list($0); next }
    section == "archives" && /^[[:space:]]+formats:/  { print "formats=" list($0); next }
    section == "archives" && /^[[:space:]]+name_template:/ {
      line = $0
      sub(/^[[:space:]]*name_template:[[:space:]]*/, "", line)
      gsub(/^"|"$/, "", line)
      print "archive_name_template=" line
      next
    }
    section == "archives" && /^[[:space:]]+files:/ { infiles = 1; next }
    section == "archives" && infiles && /^[[:space:]]+- / {
      line = $0
      sub(/^[[:space:]]*-[[:space:]]*/, "", line)
      print "archive_file=" line
      next
    }
    section == "archives" && infiles { infiles = 0 }
    section == "checksum" && /^[[:space:]]+name_template:/ {
      line = $0
      sub(/^[[:space:]]*name_template:[[:space:]]*/, "", line)
      gsub(/^"|"$/, "", line)
      print "checksum_name=" line
      next
    }
    section == "signs" && /^[[:space:]]+artifacts:[[:space:]]*checksum/ { print "signs_checksum=yes"; next }
  ' "$CONFIG"
}

fact() {
  local key="$1" out
  out=$(printf '%s\n' "$FACTS" | sed -n "s/^${key}=//p")
  printf '%s\n' "$out"
}

# The release version, read from the newest CHANGELOG heading rather
# than from `git describe`, for the reason docs-lint's current_release
# documents: the release-prep PR makes these claims true in the same
# commit the tag is cut on, and a shallow CI checkout has no tags.
changelog_release() {
  local v
  v=$(awk 'match($0, /^## v[0-9][0-9.]*/) {
             s = substr($0, RSTART, RLENGTH); sub(/.*v/, "", s); print s; exit }' "$CHANGELOG")
  [[ -n "$v" ]] || die "no '## vX.Y.Z' heading in $CHANGELOG"
  printf '%s\n' "$v"
}

expected_from_config() {
  local ver="$1" project binary goos goarch formats tmpl checksum
  project=$(fact project);  [[ -n "$project" ]]  || die "$CONFIG: no project_name"
  goos=$(fact goos);        [[ -n "$goos" ]]     || die "$CONFIG: no builds.goos list"
  goarch=$(fact goarch);    [[ -n "$goarch" ]]   || die "$CONFIG: no builds.goarch list"
  formats=$(fact formats);  [[ -n "$formats" ]]  || die "$CONFIG: no archives.formats list"
  checksum=$(fact checksum_name); [[ -n "$checksum" ]] || die "$CONFIG: no checksum.name_template"
  tmpl=$(fact archive_name_template)
  [[ "$tmpl" == "$KNOWN_NAME_TEMPLATE" ]] || die \
    "$CONFIG: archives.name_template is '$tmpl', but this script only knows how to evaluate '$KNOWN_NAME_TEMPLATE'. Update both."
  [[ "$formats" == "tar.gz" ]] || die \
    "$CONFIG: archives.formats is '$formats'; this script assumes a single tar.gz. Update both."

  local os arch
  for os in ${goos//,/ }; do
    for arch in ${goarch//,/ }; do
      printf '%s_%s_%s_%s.%s\n' "$project" "$ver" "$os" "$arch" "$formats"
    done
  done
  printf '%s\n' "$checksum"
  if [[ -n "$(fact signs_checksum)" ]]; then
    printf '%s.sig\n%s.pem\n' "$checksum" "$checksum"
  fi
}

expected_from_release() {
  local tag="$1"
  command -v gh >/dev/null 2>&1 || die "--release needs the gh CLI"
  gh release view "$tag" --json assets --jq '.assets[].name' \
    || die "could not read the assets of release $tag"
}

# --------------------------------------------------------------- check

run_check() {
  FACTS=$(goreleaser_facts)
  local ver claimed expected want_desc

  if [[ -n "$RELEASE_TAG" ]]; then
    ver="${RELEASE_TAG#v}"
    expected=$(expected_from_release "$RELEASE_TAG")
    want_desc="the assets published on $RELEASE_TAG"
  else
    ver=$(changelog_release)
    expected=$(expected_from_config "$ver")
    want_desc="the assets $CONFIG produces at v$ver"
  fi

  local pv rc=0
  pv=$(page_version)
  if [[ "$pv" != "$ver" ]]; then
    echo "$PAGE: the asset list is headed 'Assets for v$pv', but $want_desc are v$ver" >&2
    rc=1
  fi

  claimed=$(page_assets)
  [[ -n "$claimed" ]] || die "$PAGE lists no assets under its 'Assets for' line"

  # Signature artifacts are documented, not listed — see the header.
  local sigs downloads
  sigs=$(printf '%s\n' "$expected" | grep -E '\.(sig|pem)$' || true)
  downloads=$(printf '%s\n' "$expected" | grep -Ev '\.(sig|pem)$' || true)

  local missing extra a
  missing=$(comm -23 <(printf '%s\n' "$downloads" | sort -u) <(printf '%s\n' "$claimed" | sort -u))
  extra=$(comm -13 <(printf '%s\n' "$downloads" | sort -u) <(printf '%s\n' "$claimed" | sort -u))

  for a in $missing; do
    echo "$PAGE: does not list '$a', which is one of $want_desc" >&2
    rc=1
  done
  for a in $extra; do
    echo "$PAGE: lists '$a', which is not among $want_desc" >&2
    rc=1
  done
  for a in $sigs; do
    if ! grep -Fq "$a" "$PAGE"; then
      echo "$PAGE: never mentions '$a', which is one of $want_desc — a release that signs and a page that does not say so is a release nobody verifies" >&2
      rc=1
    fi
  done

  # A download URL naming an asset that does not exist is the failure
  # a reader hits first, so it is checked separately from the list:
  # the two can disagree.
  for a in $(page_download_urls | sort -u); do
    if ! grep -Fxq -- "$a" <<<"$expected"; then
      echo "$PAGE: a download URL points at '$a', which is not among $want_desc" >&2
      rc=1
    fi
  done

  # What the tarball contains is also a claim about the release. Every
  # file GoReleaser packs beside the binary must be named on the page;
  # otherwise the page's "contains X, Y and Z" sentence rots the first
  # time the archive grows a file.
  local f
  for f in $(fact archive_file); do
    if ! grep -Fq "$f" "$PAGE"; then
      echo "$PAGE: the tarball contains '$f' (archives.files) and the page never mentions it" >&2
      rc=1
    fi
  done
  local bin
  bin=$(fact binary); [[ -n "$bin" ]] || die "$CONFIG: no builds.binary"
  if ! grep -Fq "$bin" "$PAGE"; then
    echo "$PAGE: never mentions the binary name '$bin'" >&2
    rc=1
  fi

  if [[ $rc -ne 0 ]]; then
    echo "check-install-page: FAILED — the install page and $want_desc disagree" >&2
    return 1
  fi
  echo "check-install-page: OK ($(printf '%s\n' "$expected" | wc -l | tr -d ' ') assets, page agrees with $want_desc)"
}

# ----------------------------------------------------------- self-test

# The rule has to be shown to fire. A checker whose only evidence is a
# green run on a correct page is indistinguishable from `exit 0`.
self_test() {
  local tmp rc
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/mast-check-install-page.XXXXXX")
  trap 'rm -rf "$tmp"' RETURN

  cp "$CONFIG" "$tmp/config.yaml"
  cp "$CHANGELOG" "$tmp/CHANGELOG.md"

  local fails=0 checks=0
  probe() { # probe <description> <expect-fire|expect-silent> <page-file>
    local desc="$1" want="$2" page="$3" out
    checks=$((checks + 1))
    if out=$("$0" --page "$page" --config "$tmp/config.yaml" --changelog "$tmp/CHANGELOG.md" 2>&1); then
      rc=0
    else
      rc=$?
    fi
    if [[ "$want" == "fire" && $rc -eq 0 ]]; then
      echo "self-test: '$desc' did NOT fire — the check is not load-bearing" >&2
      fails=$((fails + 1))
    elif [[ "$want" == "silent" && $rc -ne 0 ]]; then
      echo "self-test: '$desc' fired on a correct page: $out" >&2
      fails=$((fails + 1))
    fi
  }

  # The real page must pass, or every fixture below proves nothing.
  probe "the real install page" silent "$PAGE"

  # A renamed asset: every version on the page is still right.
  sed 's/_linux_amd64/_linux_x86_64/' "$PAGE" > "$tmp/renamed.md"
  probe "an asset renamed on the page" fire "$tmp/renamed.md"

  # A dropped asset: the page lists three tarballs where four ship.
  grep -v '^- `mast_[0-9.]*_darwin_arm64.tar.gz`$' "$PAGE" > "$tmp/dropped.md"
  probe "an asset the page forgot" fire "$tmp/dropped.md"

  # An invented asset: the page promises a Windows build.
  awk '{ print } /^- `mast_[0-9.]*_linux_amd64\.tar\.gz`$/ { sub(/_linux_amd64/, "_windows_amd64"); print }' \
    "$PAGE" > "$tmp/invented.md"
  probe "an asset the release does not carry" fire "$tmp/invented.md"

  # A build matrix that grew: the config changes, the page does not.
  sed 's/^    goos: \[linux, darwin\]$/    goos: [linux, darwin, windows]/' "$CONFIG" > "$tmp/config-win.yaml"
  checks=$((checks + 1))
  if "$0" --page "$PAGE" --config "$tmp/config-win.yaml" --changelog "$tmp/CHANGELOG.md" >/dev/null 2>&1; then
    echo "self-test: a new goos in the config did NOT fire" >&2
    fails=$((fails + 1))
  fi

  # An unparseable template must be fatal, not a smaller expected set.
  sed 's|^    name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"$|    name_template: "{{ .Binary }}-{{ .Version }}"|' \
    "$CONFIG" > "$tmp/config-tmpl.yaml"
  checks=$((checks + 1))
  if "$0" --page "$PAGE" --config "$tmp/config-tmpl.yaml" --changelog "$tmp/CHANGELOG.md" >/dev/null 2>&1; then
    echo "self-test: an unknown name_template did NOT fire — the parser derived a set it cannot justify" >&2
    fails=$((fails + 1))
  fi

  if [[ $fails -ne 0 ]]; then
    echo "check-install-page self-test: FAILED ($fails of $checks)" >&2
    return 1
  fi
  echo "check-install-page self-test: OK (all $checks probes behaved)"
}

cd "$(git rev-parse --show-toplevel)"
if [[ $SELF_TEST -eq 1 ]]; then
  self_test
else
  run_check
fi
