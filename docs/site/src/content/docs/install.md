---
title: Install
description: Install the mast binary from release tarballs or with go install.
---

Current release: **v0.4.0** — the operator approves the exact call that will
fire, the loop runs on a schedule without an orchestrator, and every verdict
becomes a labelled eval row. A finding carries a typed proposed change rather
than a sentence; one answer can cover a whole change set, bounded by a
freshness re-read rather than only a clock; a bundle wakes itself on a durable
cadence; and `dispatch: bounded` spends exactly one cheap-tier call on a
schema-forced report. On the v0.3.0 write gate and the v0.2.0
durable-execution spine (see the [roadmap](/roadmap/)).

## Release tarballs

Each release ships cross-compiled tarballs plus a `checksums.txt`
(SHA-256). Assets for v0.4.0:

- `mast_0.4.0_linux_amd64.tar.gz`
- `mast_0.4.0_linux_arm64.tar.gz`
- `mast_0.4.0_darwin_amd64.tar.gz`
- `mast_0.4.0_darwin_arm64.tar.gz`
- `checksums.txt`

Download, verify, unpack (Linux amd64 shown — swap the asset name for your
platform):

```sh
curl -fsSLO https://github.com/go-steer/mast/releases/download/v0.4.0/mast_0.4.0_linux_amd64.tar.gz
curl -fsSLO https://github.com/go-steer/mast/releases/download/v0.4.0/checksums.txt
sha256sum --check --ignore-missing checksums.txt
tar -xzf mast_0.4.0_linux_amd64.tar.gz
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
