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

| Grant | Kind | Scope | Manifest |
|---|---|---|---|
| Diagnosis | `ClusterRole mast-daemon-read` | every namespace, `get` / `list` / `watch` | `deploy/base/14-clusterrole-daemon-read.yaml` |
| Change | `Role mast-daemon-write` | **one namespace per apply** | `deploy/remediation-target/20-role-daemon-write.yaml` |

Read ships with the base:

```sh
kubectl apply -k deploy/overlays/example
```

Write is a separate, per-namespace act. Edit the target namespace and apply it
once for each namespace mast may change:

```sh
kustomize build deploy/remediation-target |
  sed 's/REPLACE_ME_TARGET_NAMESPACE/team-a/' | kubectl apply -f -
```

The daemon's ServiceAccount stays in `mast-triage`; the `RoleBinding` reaches
across namespaces to it. So "which namespaces may mast change" is a list of
applies you can enumerate with `kubectl get rolebinding -A -l
app.kubernetes.io/name=mast`, not a field somebody can widen in one edit.

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

So **replace `REPLACE_ME_PROJECT` with your project ID** in
`deploy/base/15-clusterrolebinding-daemon-read.yaml` and
`deploy/remediation-target/21-rolebinding-daemon-write.yaml`, alongside the
namespace. Leave it and mast reads and writes nothing over the path it uses:
a patch in the namespace you granted comes back

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

- re-apply `deploy/base` and `deploy/remediation-target` with your project ID
  substituted, or the MCP path stays unbound;
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

## What keeps the manifests honest

`deploy/rbac_test.go` runs on every PR. It walks from the *subject* rather
than the filename — every binding that names the daemon under **either**
username — because the way this boundary erodes is a cluster-scoped grant
added for one tool, not an edit to the file called "read". It fails if:

- a ClusterRole bound to the daemon gains a write verb, a wildcard, or secrets;
- the write grant stops being a namespaced `Role`, or becomes reachable from
  `deploy/base`'s `resources:` (where the base's namespace transformer would
  pin it to `mast-triage`);
- a manifest exists in a directory but is missing from its kustomization, so
  it reviews as shipped and deploys as absent;
- a binding names the daemon's ServiceAccount and not its WIF `User`, so it
  grants the in-cluster path only — the failure that made the narrowed IAM
  mode look broken for four releases;
- that `User` subject loses its `REPLACE_ME_PROJECT` placeholder and gets
  pinned to one project;
- the IAM role `setup-wif.sh` binds **by default** stops matching what
  `10-serviceaccount-daemon.yaml` tells operators it binds. Which arm is the
  default is read from the script, not assumed — it has changed once already.
