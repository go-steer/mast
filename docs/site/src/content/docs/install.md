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

## Check what built it

The signature tells you *who signed* the release. It cannot tell you what the
release was built **from** — which repository, which commit, which workflow.
That is a separate claim, and every tag after v0.9.0 carries it as a
[SLSA build provenance](https://slsa.dev/spec/v1.0/provenance) attestation
generated by GitHub Actions and recorded in a public transparency log:

```sh
gh attestation verify mast_X.Y.Z_linux_amd64.tar.gz \
  --repo go-steer/mast \
  --signer-workflow go-steer/mast/.github/workflows/release.yml \
  --source-ref refs/tags/vX.Y.Z
```

Every asset is attested individually, including `checksums.txt`, so you run
that against the file you actually downloaded rather than against a list that
mentions it.

**Do not stop at `--repo`.** `gh attestation verify <file> --repo go-steer/mast`
prints a success line for an attestation made by *any* workflow in this
repository — the docs build, CI, anything. It is the same shape of answer as
cosign's `.*` above. `--signer-workflow` is what says *the release workflow*,
and `--source-ref` is what says *at this tag* rather than on somebody's branch.
Both are read from the certificate, which GitHub populates from the OIDC token;
the predicate body underneath is written by the build and is not where a
verifier should be looking.

The same script pattern applies, and the release job runs it against the
published assets:

```sh
dev/release/verify-provenance.sh vX.Y.Z
```

Unlike [`verify-signature.sh`](#verify-the-signature), this one needs `gh`
signed in: attestations for release files are served by the GitHub API rather
than published beside the asset.

The [container image](#putting-it-in-a-cluster) and the chart are attested the
same way, with the attestation stored in the registry next to the artifact
rather than in the API:

```sh
dev/release/verify-provenance.sh --oci ghcr.io/go-steer/mast@sha256:...
```

That mode wants a digest, not a tag. A tag can be re-pointed after it is
attested; `docker pull` and `helm pull` both print the digest they resolved,
and that is the string to paste.

## Putting it in a cluster

The supported path is the Helm chart, published as an OCI artifact beside
the image. You do not need to clone this repository:

```sh
helm install mast oci://ghcr.io/go-steer/charts/mast \
  --namespace mast-triage --create-namespace \
  --set gcp.projectID=my-project
```

On GKE, run
[`scripts/setup-wif.sh`](https://github.com/go-steer/mast/blob/main/scripts/setup-wif.sh)
first — it binds the Workload Identity Federation principal the daemon's
tools reach the cluster as, and prints these commands with your project
substituted in.

**`gcp.projectID` has no default, and `helm install` fails without it.** That
is deliberate. The value is substituted into the RBAC subject the GKE MCP
path arrives as, and a chart that shipped a placeholder there would render a
`ClusterRoleBinding` that `kubectl apply` accepts and that binds nobody — a
boundary that reads as if it exists, with no symptom until a tool call comes
back `Forbidden`
([#290](https://github.com/go-steer/mast/issues/290)).

**A default install can change nothing.** mast reads the whole cluster and
holds no write verb anywhere until you name the namespaces it may remediate:

```sh
helm upgrade mast oci://ghcr.io/go-steer/charts/mast \
  --namespace mast-triage \
  --set gcp.projectID=my-project \
  --set 'remediationNamespaces={team-a,team-b}'
```

Each namespace in that list gets its own `Role` and `RoleBinding`; nothing
cluster-scoped gains a write verb. See
[cluster permissions](/reference/cluster-permissions/) for the full grant and
for how to check it against a live cluster.

## Terraform

Both halves of the install — the Google Cloud IAM and the chart — in one
`apply`:
[`examples/deploy/terraform`](https://github.com/go-steer/mast/tree/main/examples/deploy/terraform).

```sh
git clone https://github.com/go-steer/mast
cd mast/examples/deploy/terraform
cp terraform.tfvars.example terraform.tfvars   # edit project_id
terraform init && terraform apply
```

It does not create the cluster. Point your kubeconfig at an existing GKE
cluster with Workload Identity Federation enabled first.

**Use it instead of `setup-wif.sh` if you expect to change your mind about
`write_scope`.** The script is idempotent forwards and not backwards: re-run
it with `WRITE_SCOPE=namespaced` and the `roles/container.admin` binding an
earlier run created is still there — its own header says so and tells you to
remove it by hand. While it stands, the per-namespace write `Role`s above bind
nothing on the path mast actually uses, because GKE authorizes a call if
*either* IAM or RBAC allows it. With Terraform, setting `write_scope` back to
`"namespaced"` destroys the binding.

**It creates no Secret.** The bearer token every write route requires is
yours to create; `terraform output next_steps` prints the two commands. A
token Terraform authors is stored in the state file in plaintext, and a state
bucket is usually readable by more people than a Helm release is. The daemon
stays not-ready until the Secrets exist, so a first `apply` that appears to
hang waiting for the rollout has usually just skipped this.

**It is additive.** The role bindings are `google_project_iam_member`, never
the authoritative `google_project_iam_binding` or `google_project_iam_policy`,
and destroying mast does not disable the project's APIs. A module dropped into
a project that already runs things must not delete grants it never created.

There is still no Homebrew tap and no apt repo. That mast is a thing
*you* install rather than a service someone runs for you is a
[decision](https://github.com/go-steer/mast/blob/main/docs/positioning.md#who-installs-mast-answered-2026-09-11-closing-291),
and it comes with three things to know before the first install — **run one
replica**, **one mast per tenant**, and **config drift is diagnosed rather
than reconciled**. Each is spelled out, with the issue tracking it, under
[what installing it costs you today](/roadmap/#what-installing-it-costs-you-today).
The third of those is the one that will surprise you afterwards, so it has
its own section below.

## Config drift: diagnosed, not reconciled

**mast does not reload configuration, and editing a ConfigMap does not restart
it.** Both halves are deliberate. The consequence is the one worth
internalising before you rely on an edit: *a `helm upgrade` can succeed, the
files on disk can change, and the daemon can keep running the old
configuration indefinitely.*

The chart carries no `checksum/config` pod annotation, so `mast-workload`
changing does not roll the daemon. Annotating the pod template with a hash of
the ConfigMap is the usual way to make a config edit restart the pods, and it
is the wrong trade here: this daemon
holds in-flight turns that are spending money, approvals parked waiting on a
human, and armed schedules. Rolling it on every `helm upgrade` — including
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
