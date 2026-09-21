# Terraform: a mast install on GKE

One `terraform apply` for both halves of a mast deployment — the Google
Cloud IAM the daemon's identity needs, and the published Helm chart —
with the values that have to agree across them passed from one place.

```bash
cp terraform.tfvars.example terraform.tfvars   # edit project_id
terraform init
terraform apply
```

It does **not** create the GKE cluster. Point your kubeconfig at an
existing cluster with Workload Identity Federation enabled first; the
`helm` provider uses whatever context `kubectl config current-context`
reports.

## Why this exists when `scripts/setup-wif.sh` already binds the same IAM

Because of one thing the script structurally cannot do.

`setup-wif.sh` is idempotent forwards and not backwards. Re-run it with
`WRITE_SCOPE=namespaced` and the `roles/container.admin` binding an
earlier `WRITE_SCOPE=cluster-admin` run created is still there — its own
header says so, and tells you to remove it by hand with `gcloud`. While
it stands, the chart's per-namespace write Roles bind nothing on the path
mast actually uses: GKE authorizes a call if **either** IAM or RBAC
allows it, so a project-level `roles/container.admin` makes the namespace
boundary decorative. An operator who believes they narrowed the
deployment has not, and nothing they can see says otherwise.

Here, flipping `write_scope` back to `"namespaced"` destroys the binding.
That is the feature, and `modules/wif/tests/wif.tftest.hcl` is a
before-and-after that proves it.

Everything else the module does, the script also does. If you have
already run the script, you have a working install; this is for people
who would rather have the grant in state than in a shell history.

## What it creates

| | |
|---|---|
| `modules/wif` | Three APIs (`container`, `aiplatform`, `iamcredentials`), three project role bindings on the daemon's Workload Identity Federation principal, and `roles/iam.serviceAccountUser` on the node service account. |
| `modules/release` | The published chart, `oci://ghcr.io/go-steer/charts/mast`. |

The two are separate modules because they are usually separate people —
`modules/wif` needs project-level IAM admin, `modules/release` needs
cluster credentials — and either can be called on its own. What the root
buys is that `project_id` and `namespace` reach both from a single place.
They are not two settings that happen to match: the namespace is inside
the WIF principal's subject **and** inside the RBAC subject the chart
binds, and two copies that drift produce a daemon that is `Forbidden` on
every call with both halves looking correct in isolation
([#290](https://github.com/go-steer/mast/issues/290), measured on live
GKE).

## What it deliberately does not create

**The bearer token.** Every write route on the daemon requires a shared
bearer, held in two Secrets that must carry the same value. This
configuration names them and creates neither, because a token Terraform
authors is written to the state file in plaintext — and a state bucket is
usually readable by more people than a Helm release is. `terraform
output next_steps` prints the two `kubectl create secret` commands. The
daemon stays not-ready until they exist, so a first `apply` that appears
to hang on the rollout has almost always just skipped this step.

**Anything authoritative.** The role bindings are
`google_project_iam_member`, never `google_project_iam_binding` or
`_policy`. This module is a guest in somebody else's project: an
authoritative resource would delete bindings it never created, on the
first apply, silently. `charts.TestTerraformWifIsAdditivePerMember` in
the Go test suite keeps it that way.

**The project's API enablement, on the way out.** `disable_on_destroy`
and `disable_dependent_services` are both `false`, so destroying mast
cannot take `container.googleapis.com` down in a project running other
things on GKE. Terraform's defaults are the other way round.

## The two variables worth reading twice

**`write_scope`** — `"namespaced"` (the default) binds
`roles/container.viewer`, which is what makes the chart's RBAC split
real. `"cluster-admin"` binds `roles/container.admin`, which authorizes
every mutating call in every namespace regardless of what the chart
renders. Use it to unblock a remediation that is coming back `Forbidden`,
then narrow it again — which, unlike with the script, actually works.

**`remediation_namespaces`** — the namespaces mast may *change*. Empty,
the default, installs a mast that diagnoses the whole cluster and can
write nowhere. This is the cluster half of the boundary; it is only worth
anything while the IAM half is `"namespaced"`.

**`gke_standard`** — set it on GKE Standard, leave it off on Autopilot.
Standard needs the daemon pinned onto metadata-server-enabled nodes;
Autopilot runs the metadata server everywhere and *rejects* that node
selector, so setting it there fails admission rather than doing nothing.

## Checking the boundary is the one you think it is

```bash
terraform output container_role     # the IAM half
terraform output rbac_subject       # the username every chart binding must name
PROJECT_ID=my-project TARGET_NS=team-a ../../../scripts/rbac-matrix.sh
```

The matrix runs against **both** usernames the daemon can arrive as. A
missing per-namespace RoleBinding and a too-wide IAM binding look
identical from the agent's side; the matrix tells them apart.

## Tests

```bash
dev/ci/presubmits/terraform.sh
```

`terraform test` against mocked providers — nothing authenticates, reads
a real project, or creates anything — plus `fmt` and `validate` over the
root and both modules. CI runs the same script.

Six further guards live in `charts/terraform_test.go` rather than in HCL,
because they are claims that span two artifacts: that this module and
`scripts/setup-wif.sh` bind the same roles for both scopes, enable the
same APIs, and build the same WIF principal string; that the hardcoded
`mast-daemon` here is still the ServiceAccount the chart creates; and the
two properties an HCL assertion structurally cannot read, since they are
about which resource *type* was used — additive-per-member IAM, and no
resource that writes a secret into state.
