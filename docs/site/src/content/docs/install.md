---
title: Install
description: Install the mast binary from release tarballs or with go install.
---

Current release: **v0.7.0** — a change mast makes carries a route back, the
write gate's question is a measurement, and a real model's behaviour can red
the build. A mutating call records what it overwrote and the call that undoes
it *before* it fires, and never fires that call itself; a gated call now
carries its question, so an eval can assert that a change was put to a person;
and the outcome tier runs a real model against a real cluster on every pull
request, with a release refusing a commit that tier has not passed. On the
v0.6.0 enforcement pass, the v0.5.0 monitoring cycle, the v0.4.0 change set,
the v0.3.0 write gate and the v0.2.0 durable-execution spine (see the
[roadmap](/roadmap/)).

## Release tarballs

Each release ships cross-compiled tarballs plus a `checksums.txt`
(SHA-256). Assets for v0.7.0:

- `mast_0.7.0_linux_amd64.tar.gz`
- `mast_0.7.0_linux_arm64.tar.gz`
- `mast_0.7.0_darwin_amd64.tar.gz`
- `mast_0.7.0_darwin_arm64.tar.gz`
- `checksums.txt`

Download, verify, unpack (Linux amd64 shown — swap the asset name for your
platform):

```sh
curl -fsSLO https://github.com/go-steer/mast/releases/download/v0.7.0/mast_0.7.0_linux_amd64.tar.gz
curl -fsSLO https://github.com/go-steer/mast/releases/download/v0.7.0/checksums.txt
sha256sum --check --ignore-missing checksums.txt
tar -xzf mast_0.7.0_linux_amd64.tar.gz
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
- [Point it at a real provider](/concepts/providers/#credentials) — which
  environment variables each backend reads
