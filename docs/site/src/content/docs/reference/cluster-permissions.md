---
title: Cluster permissions
description: The RBAC mirror for a mast deployment on Kubernetes — cluster-wide read, namespace-scoped write, the two usernames mast arrives as, and the IAM binding that decides whether the split is real.
sidebar:
  order: 7
---

The [write gate](/reference/write-gate/) decides whether an operator approved
a change. This page is the other boundary: what the cluster will accept from
mast **even when they did**.

The two are independent on purpose. The gate lives inside the agent, so a bug
in it is a bug in the thing being gated. RBAC lives in the API server, so a
call outside the grant fails as a `Forbidden` that the specialist sees as a
tool error — no matter what the model proposed or the operator approved.

## The split

| Grant | Kind | Scope | Template |
|---|---|---|---|
| Diagnosis | `ClusterRole mast-daemon-read` | every namespace, `get` / `list` / `watch` | `charts/mast/templates/clusterrole-daemon-read.yaml` |
| Change | `Role mast-daemon-write` | **one namespace per `remediationNamespaces` entry** | `charts/mast/templates/rbac-daemon-write.yaml` |

Read comes with the install:

```sh
helm install mast oci://ghcr.io/go-steer/charts/mast \
  --namespace mast-triage --create-namespace \
  --set gcp.projectID=my-project
```

**Write is opt-in, and a default install has none of it.** With
`remediationNamespaces` empty — the default — the chart renders no write verb
anywhere in the cluster. Naming namespaces is what grants it:

```sh
helm upgrade mast oci://ghcr.io/go-steer/charts/mast \
  --namespace mast-triage \
  --set gcp.projectID=my-project \
  --set 'remediationNamespaces={team-a,team-b}'
```

That emits one `Role` + `RoleBinding` pair per entry. The daemon's
ServiceAccount stays in `mast-triage`; each `RoleBinding` reaches across
namespaces to it. So "which namespaces may mast change" is one list you can
read back with `helm get values mast -n mast-triage`, or confirm against the
cluster with `kubectl get rolebinding -A -l app.kubernetes.io/name=mast` —
not a field somebody can widen in one edit.

### Two subjects, because mast arrives under two usernames

Both bindings name two subjects, and both are load-bearing:

| Path | Subject | Username the API server sees |
|---|---|---|
| In-cluster | `kind: ServiceAccount` | `system:serviceaccount:mast-triage:mast-daemon` |
| GKE MCP | `kind: User` | `serviceAccount:PROJECT.svc.id.goog[mast-triage/mast-daemon]` |

The second is the one the agent's tools take. mast never presents the pod's
ServiceAccount token to a GKE API server: it calls
`container.googleapis.com/mcp` with a Google credential derived from the KSA,
and the API server sees the Workload Identity Federation principal — an RBAC
**User**, not a ServiceAccount. GKE does not resolve one to the other.

This is why `gcp.projectID` is a **required** chart value rather than one with
a placeholder default: it is what names that `User`, and a chart that guessed
would render a binding `kubectl apply` accepts and that binds nobody. Omit it
and `helm install` stops before anything reaches the API server. Were the
placeholder to survive instead, mast would read and write nothing over the
path it actually uses — a patch in the namespace you granted comes back

```
deployments.apps "checkout" is forbidden: User
"serviceAccount:my-project.svc.id.goog[mast-triage/mast-daemon]" cannot patch
resource "deployments" in the namespace "team-a"
```

which names a `User` nothing has bound.

### What the write grant deliberately does not include

- **No secrets.** Not readable cluster-wide, not writable in the target
  namespace. Diagnosis never needs their contents, and an agent that hands
  what it reads to a model should not hold them.
- **No deleting a workload.** `patch`, `update` and `create` on Deployments,
  StatefulSets, DaemonSets and their `scale` subresources; `delete` on Pods
  (that is how a restart happens) and nothing else.
- **Narrower than the tools.** `apply_k8s_manifest` can name any kind. Under this
  Role it lands only for workload objects and ConfigMaps, in one namespace.
  That gap is the point.

## Verifying it

`scripts/rbac-matrix.sh` is the `kubectl auth can-i` matrix as something you
run, not a table to read:

```sh
TARGET_NS=team-a PROJECT_ID=my-project ./scripts/rbac-matrix.sh
```

It runs its 20 cells **once per username** — 40 in all, plus the IAM cell —
and exits non-zero on any surprise. Nine cells per path must answer **no**:
patching `kube-system`, deleting a Deployment, reading a secret, creating a
ClusterRoleBinding. Those are the answers that change silently when a Role is
widened.

`PROJECT_ID` is what names the MCP path's subject, so without it the run is
half a measurement and the script says so and fails. Set `IN_CLUSTER_ONLY=true`
on a cluster that has no MCP path at all (kind, minikube) — and then the run
is not evidence about a GKE deployment.

You need cluster access and permission to impersonate (`--as=` is a
SubjectAccessReview). It changes nothing. Its `--as=` answers for the MCP
subject were checked against the real thing: all nine matched what the GKE MCP
server returned for the same calls on the same cluster.

## The GKE caveat

**On GKE, Kubernetes RBAC is not on its own the boundary.** GKE authorizes an
API call if **either** IAM or RBAC allows it, and a mast deployment reaches
the cluster through the GKE MCP server as its Workload Identity Federation
principal — not through the pod's ServiceAccount token. Bind
`roles/container.admin` to that principal and every mutating call in every
namespace is permitted regardless of the Role above.

So `scripts/setup-wif.sh` binds `roles/container.viewer` by default and leaves
the writes to RBAC. That role carries no write verb and no `container.secrets.*`,
which is what makes the split load-bearing on the path mast uses:

```sh
./scripts/setup-wif.sh my-project                     # namespaced, the default
WRITE_SCOPE=cluster-admin ./scripts/setup-wif.sh ...  # the escape hatch
```

The narrowed mode was run against a live GKE cluster on 2026-09-06. Over the
MCP path it authorized a patch in the remediable namespace, refused the same
patch one namespace over, refused deleting a Deployment, and refused listing
secrets cluster-wide — but only once the bindings named the `User` subject
above. With the `ServiceAccount` subject alone it made mast read-only
everywhere, which is why the two changes ship together.

Upgrading an existing deployment takes both halves, and neither happens by
itself:

- `helm upgrade` with `gcp.projectID` set, or the MCP path stays unbound;
- **remove the old `roles/container.admin`** — re-running `setup-wif.sh` adds
  bindings and never takes one away.

```sh
gcloud projects remove-iam-policy-binding my-project \
  --role=roles/container.admin --condition=None \
  --member=principal://iam.googleapis.com/projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/my-project.svc.id.goog/subject/ns/mast-triage/sa/mast-daemon
```

`rbac-matrix.sh` fails a cell while that binding stands — for
`roles/container.admin`, `container.developer`, `container.clusterAdmin`,
`editor` or `owner`.

## What keeps the chart honest

`charts/rbac_test.go` runs on every PR, against the output of `helm template`
rather than against the templates — what an operator installs is the rendered
object, not the file. It walks from the *subject* rather than the filename —
every binding that names the daemon under **either** username — because the
way this boundary erodes is a cluster-scoped grant added for one tool, not an
edit to the file called "read". It fails if:

- a ClusterRole bound to the daemon gains a write verb, a wildcard, or secrets;
- a default install (no `remediationNamespaces`) renders *any* write verb
  bound to the daemon, anywhere;
- the write grant stops being a namespaced `Role` and `RoleBinding` in each
  namespace the operator named, or starts landing in the daemon's own
  namespace — the one namespace where it is useless;
- a binding names the daemon's ServiceAccount and not its WIF `User`, so it
  grants the in-cluster path only — the failure that made the narrowed IAM
  mode look broken for four releases;
- the chart renders at all without `gcp.projectID`, since a rendered
  placeholder is a binding that applies cleanly and grants nothing;
- the IAM role `setup-wif.sh` binds **by default** stops matching what the
  daemon's ServiceAccount template tells operators it binds. Which arm is the
  default is read from the script, not assumed — it has changed once already.

Alongside it, `charts/projection_test.go` pins the chart's copy of the
workload bundle byte-identical to
[`examples/workloads/gke-triage/`](https://github.com/go-steer/mast/tree/main/examples/workloads/gke-triage),
checks that every key in the workload ConfigMap actually reaches the pod, and
pins the exact set of objects a default install creates. These tests **fail**
rather than skip when `helm` is not installed: an RBAC test that skips is
indistinguishable from no RBAC test.
