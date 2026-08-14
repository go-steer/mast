---
title: Install
description: Install the mast binary from release tarballs or with go install.
---

Current release: **v0.3.0** — the write gate (a mutating tool call parks for
a scoped, authenticated, durably-recorded operator verdict, one call at a
time) and the structural read/write split across specialists, dispatch, and
the deploy manifests, plus parallel fan-out, per-specialist budgets and model
selection, and typed specialist reports — on the v0.2.0 durable-execution
spine (see the [roadmap](/roadmap/)).

## Release tarballs

Each release ships cross-compiled tarballs plus a `checksums.txt`
(SHA-256). Assets for v0.3.0:

- `mast_0.3.0_linux_amd64.tar.gz`
- `mast_0.3.0_linux_arm64.tar.gz`
- `mast_0.3.0_darwin_amd64.tar.gz`
- `mast_0.3.0_darwin_arm64.tar.gz`
- `checksums.txt`

Download, verify, unpack (Linux amd64 shown — swap the asset name for your
platform):

```sh
curl -fsSLO https://github.com/go-steer/mast/releases/download/v0.3.0/mast_0.3.0_linux_amd64.tar.gz
curl -fsSLO https://github.com/go-steer/mast/releases/download/v0.3.0/checksums.txt
sha256sum --check --ignore-missing checksums.txt
tar -xzf mast_0.3.0_linux_amd64.tar.gz
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

## Next

- [Quickstart: unattended triage, fully offline](/quickstart/unattended-triage/)
- [Quickstart: embed the library](/quickstart/library-embed/)
