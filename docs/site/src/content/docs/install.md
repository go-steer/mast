---
title: Install
description: Install the mast binary from release tarballs or with go install.
---

Current release: **v0.2.0** — the durable-execution spine (recorded-effect
outbox, boot-time auto-resume, programmatic pause/abort) plus the ecosystem
interop surfaces: the A2A server, the hand-rolled AG-UI server with the full
HITL interrupt/resume lifecycle, and generic local/stdio MCP wiring — on the
v0.1.2 hardened-shutdown base, with an observability v0.2 fixed registry and a
teardown watchdog (see the [roadmap](/roadmap/)).

## Release tarballs

Each release ships cross-compiled tarballs plus a `checksums.txt`
(SHA-256). Assets for v0.2.0:

- `mast_0.2.0_linux_amd64.tar.gz`
- `mast_0.2.0_linux_arm64.tar.gz`
- `mast_0.2.0_darwin_amd64.tar.gz`
- `mast_0.2.0_darwin_arm64.tar.gz`
- `checksums.txt`

Download, verify, unpack (Linux amd64 shown — swap the asset name for your
platform):

```sh
curl -fsSLO https://github.com/go-steer/mast/releases/download/v0.2.0/mast_0.2.0_linux_amd64.tar.gz
curl -fsSLO https://github.com/go-steer/mast/releases/download/v0.2.0/checksums.txt
sha256sum --check --ignore-missing checksums.txt
tar -xzf mast_0.2.0_linux_amd64.tar.gz
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
