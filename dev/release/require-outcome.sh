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

# Refuses to release a commit the O tier has not passed
# (.github/workflows/release.yml; docs/outcome-evals-design.md §7).
#
#   dev/release/require-outcome.sh [ref]     # default: the current tag,
#                                            # else HEAD
#
# ## Why this exists rather than a branch-protection setting
#
# Both were available and they make different promises. Adding `outcome`
# to `main`'s required checks says *every merge* passed it — a claim
# about a process, enforced on the pull request's merge ref, bypassable
# by an admin, and one that makes a fork PR unmergeable, since GitHub
# withholds the credentials the tier needs. This says *the commit being
# released* passed it: a claim about the artifact, checked against the
# artifact, at the one moment the answer is load-bearing.
#
# It is the same discipline as the release notes check further down
# release.yml. Composing the notes correctly proved nothing for six
# releases; only reading the published body did. A green `outcome` on
# some ancestor proves nothing about this tag; only the tag's own check
# run does.
#
# The two are not exclusive. If branch protection is turned on later,
# this stays: it is what makes the release *say* the tier passed, and
# what catches the tag that was cut somewhere protection does not reach.
#
# ## Why it can fire
#
# .github/workflows/outcome.yml runs on `push: branches: [main]` as well
# as on pull requests, so every commit on main carries an `outcome`
# check run against its own SHA. Without that trigger this gate would be
# §7's rung that cannot fire — the PR's check run is attached to the
# ephemeral merge ref, not to the squash commit a tag points at.
#
# ## Exit status is two-valued on purpose
#
# 0 released, 1 refused. There is deliberately no third code for "could
# not determine": for a gate on an artifact, not knowing and knowing it
# is red are the same answer, and #297 is the standing lesson about exit
# codes nobody branches on.

set -euo pipefail

readonly CHECK="outcome"

# How long to wait for a run that is still going. A pass is ~6 minutes
# and starts when the commit lands on main, so a tag pushed any time
# after that finds it finished; the wait is for the case where the two
# happen together.
wait_for="${MAST_RELEASE_OUTCOME_WAIT:-900}"
poll_every="${MAST_RELEASE_OUTCOME_POLL:-30}"

summary="${GITHUB_STEP_SUMMARY:-/dev/stderr}"

# refuse <one-line reason> <<'EOF' … EOF
#
# The long form goes to the job summary and the one-liner to stderr,
# because in CI those are different archives and #297's lesson is that
# the run which reads only one of them reads nothing on the exact
# occasion there is nothing else to go on.
refuse() {
    {
        echo "## release refused: the ${CHECK} tier"
        echo ""
        cat
    } >>"${summary}"
    echo "::error::${1}" >&2
    exit 1
}

if ! command -v gh >/dev/null 2>&1; then
    refuse "gh is not on PATH, so the ${CHECK} verdict cannot be read" <<EOF
\`gh\` is not on PATH, so the tier's verdict for this commit cannot be
read. A release gate that cannot reach its evidence refuses.
EOF
fi

repo="${GITHUB_REPOSITORY:-}"
if [[ -z "${repo}" ]]; then
    repo="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"
fi

ref="${1:-${GITHUB_REF_NAME:-HEAD}}"
if [[ "${ref}" == "HEAD" ]]; then
    sha="$(git rev-parse HEAD)"
else
    sha="$(gh api "repos/${repo}/commits/${ref}" --jq .sha)"
fi

echo "${CHECK}: reading the tier's verdict for ${ref} (${sha})"

# Newest first. Re-running the tier on a red commit is legitimate — it
# is a re-measurement, not an override — but it must not be silent, so
# every run found is printed and the release log carries the history.
runs_for() {
    gh api \
        "repos/${repo}/commits/${sha}/check-runs?check_name=${CHECK}&per_page=100" \
        --jq '.check_runs | sort_by(.started_at) | reverse | .[]
              | [.status, (.conclusion // "-"), .started_at, .html_url] | @tsv'
}

deadline=$(($(date +%s) + wait_for))
while :; do
    runs="$(runs_for)"

    if [[ -z "${runs}" ]]; then
        refuse "no ${CHECK} check run exists for ${sha}" <<EOF
No \`${CHECK}\` check run exists for \`${sha}\`, so nothing says a real
model was measured against this commit.

Two ways to get here. The commit predates the O tier (merged 2026-09-05,
[#311](https://github.com/go-steer/mast/pull/311)) — such a tag cannot be
gated on it, and the honest fix is to say so in the notes rather than to
weaken this check. Or the run was never dispatched for this SHA:

    gh workflow run outcome.yml --ref ${ref}

then re-run the release workflow once it is green.
EOF
    fi

    IFS=$'\t' read -r status conclusion _ url <<<"${runs}"

    if [[ "${status}" == "completed" ]]; then
        break
    fi

    now="$(date +%s)"
    if ((now >= deadline)); then
        refuse "${CHECK} is still ${status} on ${sha} after ${wait_for}s" <<EOF
The \`${CHECK}\` run for \`${sha}\` is still \`${status}\` after
${wait_for}s: <${url}>

Not a verdict, so not a release. Wait for it and re-run this workflow,
or raise \`MAST_RELEASE_OUTCOME_WAIT\`.
EOF
    fi

    echo "${CHECK}: ${status}, waiting ${poll_every}s ($(((deadline - now) / 60))m left)"
    sleep "${poll_every}"
done

echo "${CHECK}: runs on this commit, newest first"
echo "${runs}"

if [[ "${conclusion}" != "success" ]]; then
    refuse "${CHECK} concluded ${conclusion} on ${sha}" <<EOF
The most recent \`${CHECK}\` run for \`${sha}\` concluded
\`${conclusion}\`: <${url}>

The tier gates on the pull request for a reason, and a tag is not the
place to overrule it. Either fix what it found and tag the commit that
fixes it, or — if the red is the tier's own flakiness rather than the
agent's — demote the case in a committed diff (§7) and tag that.
EOF
fi

echo "${CHECK}: success on ${sha} — ${url}"
