---
title: Install
description: Install the mast binary from release tarballs or with go install.
---

Current release: **v0.9.0** — the surfaces stop answering a question nobody
asked. `GET /perms` refuses rather than returning an empty 200, a session
parked on a human approval says so on the wire instead of reporting `idle`, an
out-of-process caller can read the change it is asking someone to approve, and
`/agui/agents.json` states what each workload will actually publish. **Upgrading
from v0.8.0 has one breaking change and two behaviour changes** — `.tmpl`
specialist files stop loading (the window v0.8 opened), authenticated AG-UI
threads opened before this release are not reachable after it, and a client
disconnect no longer cancels the run it was watching. Start with
[what changed](https://github.com/go-steer/mast/blob/main/CHANGELOG.md). On the
v0.8.0 correctness pass, the v0.7.0 route-back-from-a-change pass, the
v0.6.0 enforcement pass, the v0.5.0 monitoring cycle, the v0.4.0 change set,
the v0.3.0 write gate and the v0.2.0 durable-execution spine (see the
[roadmap](/roadmap/)).

## Release tarballs

Each release ships cross-compiled tarballs plus a `checksums.txt`
(SHA-256). Assets for v0.9.0:

- `mast_0.9.0_linux_amd64.tar.gz`
- `mast_0.9.0_linux_arm64.tar.gz`
- `mast_0.9.0_darwin_amd64.tar.gz`
- `mast_0.9.0_darwin_arm64.tar.gz`
- `checksums.txt`

Download, verify, unpack (Linux amd64 shown — swap the asset name for your
platform):

```sh
curl -fsSLO https://github.com/go-steer/mast/releases/download/v0.9.0/mast_0.9.0_linux_amd64.tar.gz
curl -fsSLO https://github.com/go-steer/mast/releases/download/v0.9.0/checksums.txt
sha256sum --check --ignore-missing checksums.txt
tar -xzf mast_0.9.0_linux_amd64.tar.gz
sudo install -m 0755 mast /usr/local/bin/mast
```

Each tarball contains the `mast` binary plus `LICENSE`, `README.md`, and
`CHANGELOG.md`.

## go install

```sh
go install github.com/go-steer/mast/cmd/mast@latest
```

## Verify

```sh
mast --version
```

Prints the release version (plus commit and date for tarball builds;
`mast dev` for a local `go install` build without ldflags stamping).

## Verify the signature

`checksums.txt` proves the bytes you downloaded match the bytes *something*
produced. The signature beside it proves that something was mast's release
workflow.

**v0.9.0, the current release, is not signed.** Signing landed with
[#342](https://github.com/go-steer/mast/issues/342) after it was cut, so v0.9.0
and everything before it ship `checksums.txt` alone and the rest of this
section does not apply to them. Every release from the next one onward also
ships `checksums.txt.sig` and `checksums.txt.pem`.

Those are a Sigstore **keyless** signature: there
is no mast public key to fetch and trust, because there is no mast private
key. The release job exchanges its GitHub OIDC token for a short-lived
certificate, and the identity written into that certificate is the workflow
file and the tag it ran on. That identity is what you pin:

```sh
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig \
  --certificate checksums.txt.pem \
  --certificate-identity "https://github.com/go-steer/mast/.github/workflows/release.yml@refs/tags/vX.Y.Z" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
```

`vX.Y.Z` is the release you downloaded — the tag is part of the identity, so it
is not a label and it is not optional.

cosign will not let you skip the question: `verify-blob` in keyless mode exits
with *"--certificate-identity or --certificate-identity-regexp is required"*
rather than verifying anything. What it *will* accept is an answer that means
nothing — `--certificate-identity-regexp '.*'` reports **Verified OK** against a
certificate from any workflow in any repository on GitHub, which is a fact about
Sigstore and not about mast. If you widen the identity to avoid editing a tag
into a script, widen it to a pattern that still pins the repository and the
workflow file:

```sh
--certificate-identity-regexp '^https://github\.com/go-steer/mast/\.github/workflows/release\.yml@refs/tags/v'
```

That says *some mast release* signed this — weaker than naming the tag, far
stronger than `.*`. The leading `^` is load-bearing: cosign matches the pattern
anywhere in the identity, so an unanchored `go-steer/mast` also accepts a
certificate issued to `github.com/attacker/go-steer/mast-lookalike`.

Then check the tarballs against the file you just verified — a signature over a
list you do not compare against is a signature over nothing:

```sh
sha256sum --check --ignore-missing checksums.txt
```

Both steps, plus fetching every asset, are what
[`dev/release/verify-signature.sh`](https://github.com/go-steer/mast/blob/main/dev/release/verify-signature.sh)
does; it needs `cosign` and `curl` and no credentials:

```sh
dev/release/verify-signature.sh vX.Y.Z
```

The release job runs that same script against the release it has just
published, so a release whose signature does not reach the assets does not
finish cutting. Verifying the step that signs is not the same as verifying the
thing it signed — mast's early releases published empty bodies while every step
in the job logged the right output, which is why this one asserts on the
download.

There is still no SLSA provenance attestation and no signed container image;
both stay on [#342](https://github.com/go-steer/mast/issues/342).

## Putting it in a cluster

There is no chart, no Terraform module and no Homebrew tap: the supported
path is the kustomize base in
[`deploy/`](https://github.com/go-steer/mast/tree/main/deploy) plus
[`scripts/setup-wif.sh`](https://github.com/go-steer/mast/blob/main/scripts/setup-wif.sh)
for Workload Identity, applied by you. That mast is a thing *you* install
rather than a service someone runs for you is a
[decision](https://github.com/go-steer/mast/blob/main/docs/positioning.md#who-installs-mast-answered-2026-09-11-closing-291),
and it comes with three things to know before the first apply — **run one
replica**, **one mast per tenant**, and **config drift is diagnosed rather
than reconciled**. Each is spelled out, with the issue tracking it, under
[what installing it costs you today](/roadmap/#what-installing-it-costs-you-today).
The third of those is the one that will surprise you after the first apply, so
it has its own section below.

## Config drift: diagnosed, not reconciled

**mast does not reload configuration, and editing a ConfigMap does not restart
it.** Both halves are deliberate. The consequence is the one worth
internalising before you rely on an edit: *an apply can succeed, the files on
disk can change, and the daemon can keep running the old configuration
indefinitely.*

The kustomize base sets `disableNameSuffixHash: true`, so `mast-workload`
keeps a stable name across applies. A hashed name is the usual way to make a
ConfigMap edit roll the pods, and it is the wrong trade here: this daemon
holds in-flight turns that are spending money, approvals parked waiting on a
human, and armed schedules. Rolling it on every `kubectl apply` — including
the ones that changed nothing it cares about — costs more than a stale read.

### What you see instead

At startup the daemon logs the identity of exactly what it loaded:

```json
{"level":"INFO","msg":"workload config identity",
 "root":"/etc/mast/workload","bundle":"/etc/mast/workload/workload.yaml",
 "digest":"sha256:20da24285cbc0b31","files":8,"bytes":17947,
 "newest_mtime":"2026-09-13T19:13:44Z"}
```

The `digest` is over file *contents* and relative paths only. mtime is
excluded on purpose — a ConfigMap remount rewrites every mtime without
changing a byte — and so is the absolute root, so the same bundle mounted at a
different path digests identically. Two daemons reporting the same digest are
running the same configuration, and that is a claim you can act on.

Once a minute the daemon re-hashes what is on disk and compares it to what it
loaded. When they diverge it says so, once, naming the file:

```json
{"level":"WARN","msg":"WORKLOAD CONFIG ON DISK NO LONGER MATCHES THE RUNNING CONFIG — the edit has not taken effect",
 "running_digest":"sha256:20da24285cbc0b31","on_disk_digest":"sha256:4e3a2d0b8eba8e6e",
 "changed":"specialists/storage-audit.specialist.md",
 "remedy":"mast does not reload configuration; restart the daemon (kubectl rollout restart) to pick this up"}
```

It is edge-triggered, not repeated every minute, so it will not bury your
logs — and if you revert the edit it says that too:

```json
{"level":"INFO","msg":"workload config on disk matches the running config again",
 "digest":"sha256:20da24285cbc0b31"}
```

### What to do about it

```sh
kubectl rollout restart statefulset/mast -n mast-triage
```

The new pod logs a fresh `workload config identity` line; compare its `digest`
to the `on_disk_digest` from the warning to confirm the edit took.

### The log line is the only surface

There is **no metric and no alert** for config drift. If nothing is reading
the daemon's logs, nothing will tell you the edit did not land. That is a real
limitation, not an oversight to read past: if you want to be paged on it, the
thing to alert on today is the `WARN` above, matched on its `msg`. Every other
`mast_*` metric is listed under [metrics](/reference/metrics/), and none of
them covers this.

### There is no CRD, and none is planned

mast is a workload you schedule, not a controller you extend. It does not
install a CRD, does not run a reconcile loop, and will not grow one — a second
control plane to version, support and freeze, aimed at configuration that is
already files you have GitOps for, is not a trade this project wants to make.
Drift detection is the answer to "is the daemon running what I applied", and
the answer is deliberately a diagnosis rather than a correction: mast tells
you, and you decide when the restart is safe. Given what an in-flight turn is
holding, that decision is not one a controller should be making for you.

## Next

- [Quickstart: unattended triage, fully offline](/quickstart/unattended-triage/)
- [Quickstart: embed the library](/quickstart/library-embed/)
- [Point it at a real provider](/concepts/providers/#credentials) — which
  environment variables each backend reads
