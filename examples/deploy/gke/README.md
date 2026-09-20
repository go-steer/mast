# GKE deployment — see `charts/mast/`

The GKE starter is not duplicated here: the canonical, working GKE
recipe is the Helm chart at the repo root under
[`charts/mast/`](../../../charts/mast/) — service accounts, watcher
RBAC, the daemon StatefulSet, the event-watcher Deployment and the
Service, running the [`gke-triage`](../../workloads/gke-triage/)
workload. Install it from the published OCI artifact, without cloning
this repo:

```sh
helm install mast oci://ghcr.io/go-steer/charts/mast \
  --namespace mast-triage --create-namespace \
  --set gcp.projectID=my-project
```

`gcp.projectID` is required and has no default; see
[`docs/deployment-design.md`](../../../docs/deployment-design.md)
§ "Chart, not kustomization" for why a placeholder would be worse.
Namespaces mast may *change* are opt-in — `--set
'remediationNamespaces={team-a}'`. A default install can change
nothing.

Shape notes against the v0.1 row in
[`docs/deployment-design.md`](../../../docs/deployment-design.md):

- **Single instance** (`replicas: 1`; the StatefulSet's sequential
  delete-then-create update preserves the old `Recreate` semantics) —
  GKE multi-instance (2-N replicas, Postgres store, advisory-lock
  handoff) is the v0.2 row.
- **Session durability — on by default:** the daemon runs as a
  **StatefulSet** with a `volumeClaimTemplate` mounted at
  `/var/lib/mast` and `--session-db=/var/lib/mast/sessions.db`, per
  the v0.1 GKE row in
  [`docs/deployment-design.md`](../../../docs/deployment-design.md) —
  a rescheduled bare-Deployment pod would lose the SQLite DB, and with
  it durable pauses, abort markers, and shutdown interruption markers.
  The claim uses the cluster's default StorageClass (1Gi; set
  `daemon.storage.storageClassName` / `daemon.storage.size`).
  Alternatively use
  `--session-db-driver=postgres` with a DSN, as in the
  [Cloud Run starter](../cloud-run/) — the right answer for
  multi-instance, which a shared PVC is not.
- **Rolling restarts / node drains:** on SIGTERM the daemon drains
  in-flight turns for up to the workload's
  `budget.max_wallclock_seconds` (30s without a budget), writing
  durable interruption markers *before* waiting — a SIGKILL at
  `terminationGracePeriodSeconds` still leaves the markers on disk
  (with a durable `--session-db`). Size the pod's
  `terminationGracePeriodSeconds` above the drain bound plus headroom;
  the chart defaults to 330 against the demo workload's 300s turn
  ceiling (`daemon.terminationGracePeriodSeconds`).
  Sessions cut short report `interrupted` in `mast sessions list`
  ([`docs/durable-execution-design.md`](../../../docs/durable-execution-design.md),
  "Shutdown contract").
