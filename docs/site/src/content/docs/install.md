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

`checksums.txt` proves the bytes you downloaded match the bytes the release
job produced. It is not a signature, and there is no provenance attestation
yet — [#342](https://github.com/go-steer/mast/issues/342).

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

## Next

- [Quickstart: unattended triage, fully offline](/quickstart/unattended-triage/)
- [Quickstart: embed the library](/quickstart/library-embed/)
- [Point it at a real provider](/concepts/providers/#credentials) — which
  environment variables each backend reads
