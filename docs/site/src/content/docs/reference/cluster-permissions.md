---
title: Cluster permissions
description: The RBAC mirror for a mast deployment on Kubernetes — cluster-wide read, namespace-scoped write, and the IAM binding that decides whether the split is real.
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

It checks 20 cells and exits non-zero on any surprise. Nine of them must
answer **no** — patching `kube-system`, deleting a Deployment, reading a
secret, creating a ClusterRoleBinding — because those are the answers that
change silently when a Role is widened.

You need cluster access and permission to impersonate (`--as=` is a
SubjectAccessReview). It changes nothing.

## The GKE caveat

**On GKE, Kubernetes RBAC is not on its own the boundary.** GKE authorizes an
API call if **either** IAM or RBAC allows it, and a mast deployment reaches
the cluster through the GKE MCP server as its Workload Identity Federation
principal — not through the pod's ServiceAccount token. `scripts/setup-wif.sh`
binds `roles/container.admin` to that principal by default, which permits
every mutating call in every namespace regardless of the Role above.

To make the split load-bearing on that path, narrow the IAM binding and let
RBAC carry the writes:

```sh
WRITE_SCOPE=namespaced ./scripts/setup-wif.sh my-project
```

That binds `roles/container.viewer` instead. It is not the default because it
depends on GKE resolving the WIF principal to the RBAC subject the RoleBinding
names, which has not been verified against a live cluster — try it on a
non-production project, run the matrix, and fall back with
`WRITE_SCOPE=cluster-admin` if a remediation comes back `Forbidden`.

Passing `PROJECT_ID` to `rbac-matrix.sh` makes this a checked cell rather than
a caveat you have to remember: it fails the run if the principal still holds
`roles/container.admin`, `container.developer`, `container.clusterAdmin`,
`editor` or `owner`.

## What keeps the manifests honest

`deploy/rbac_test.go` runs on every PR. It walks from the *subject* rather
than the filename — every `ClusterRoleBinding` that names the daemon's
ServiceAccount — because the way this boundary erodes is a cluster-scoped
grant added for one tool, not an edit to the file called "read". It fails if:

- a ClusterRole bound to the daemon gains a write verb, a wildcard, or secrets;
- the write grant stops being a namespaced `Role`, or becomes reachable from
  `deploy/base`'s `resources:` (where the base's namespace transformer would
  pin it to `mast-triage`);
- a manifest exists in a directory but is missing from its kustomization, so
  it reviews as shipped and deploys as absent;
- the IAM role `setup-wif.sh` binds by default stops matching what
  `10-serviceaccount-daemon.yaml` tells operators it binds.
