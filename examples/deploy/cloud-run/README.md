# Cloud Run deployment — single instance, Postgres sessions

The Cloud Run shape from
[`docs/deployment-design.md`](../../../docs/deployment-design.md):
mast as a single-instance Cloud Run service, webhook-style ingress,
scale-to-zero friendly. File here: [`service.yaml`](./service.yaml)
(Knative service sketch with `REPLACE_ME` markers).

## Postgres is required here — not optional

Cloud Run's filesystem is ephemeral: a SQLite `--session-db` would
vanish on every instance recycle, silently dropping paused sessions
and falsifying the v0.1 durability exit criterion. Per the 2026-07-25
revision of deployment-design, the Cloud Run session store is **Cloud
SQL Postgres from v0.1**.

The runtime side is one flag pair — ADK's `session/database` service
takes a Postgres dialector exactly as it takes SQLite's
([`gorm.io/driver/postgres`](https://pkg.go.dev/gorm.io/driver/postgres),
wired in `cmd/mast`):

```sh
mast --session-db-driver=postgres \
     --session-db="host=/cloudsql/PROJECT:REGION:mast-sessions user=mast password=... dbname=mast sslmode=disable"
```

`--session-db-driver` defaults to `sqlite`, so the other starters are
unchanged. An explicit `--session-db-driver=postgres` with an empty
`--session-db` is a startup error, never a silent in-memory downgrade.

## Setup sketch

1. **Cloud SQL Postgres instance + DB + user** (smallest tier is fine
   for v0.1's single instance):

   ```sh
   gcloud sql instances create mast-sessions --database-version=POSTGRES_16 \
       --region=REGION --tier=db-f1-micro
   gcloud sql databases create mast --instance=mast-sessions
   gcloud sql users create mast --instance=mast-sessions --password=...
   ```

   mast auto-migrates its session schema on startup; no manual DDL.

2. **Secrets** (bearer token + DSN):

   ```sh
   openssl rand -hex 32 | gcloud secrets create mast-inject-token --data-file=-
   printf 'host=/cloudsql/PROJECT:REGION:mast-sessions user=mast password=... dbname=mast sslmode=disable' \
     | gcloud secrets create mast-session-db-dsn --data-file=-
   ```

   Grant the service's runtime SA `roles/secretmanager.secretAccessor`
   on both, plus `roles/cloudsql.client` and `roles/aiplatform.user`.

3. **Image with your workload baked in.** Cloud Run has no ConfigMap
   mounts, so the simplest v0.1 path is a derived image:

   ```dockerfile
   FROM ghcr.io/go-steer/mast:latest
   COPY my-workload/ /workspace/workload/
   ```

   (`/workspace/workload` matches the base image's `--workload`
   convention — `workload.yaml`, `mcp.json`, `specialists/*.tmpl`.)

4. **Deploy:** edit the `REPLACE_ME` markers in `service.yaml`, then

   ```sh
   gcloud run services replace service.yaml --region=REGION
   ```

## Operational notes

- **Single instance in v0.1.** `maxScale: "1"` is deliberate:
  multi-instance session-ownership handoff (advisory locks in the
  store) is the v0.2 row. Don't raise it yet.
- **Scale-to-zero vs. autonomous work.** `minScale: "0"` suits
  webhook-triggered workloads (cold start 1-3s). Long-running or
  autonomous loops want `minScale: "1"`, or GKE instead. Sessions that
  outlive the request timeout pause durably and resume via Cloud
  Scheduler → the `/resume` endpoint
  ([`docs/durable-execution-design.md`](../../../docs/durable-execution-design.md)).
- **`mast sessions list/show` against Postgres:** the operator
  read-path CLI opens SQLite paths only in v0.1 — inspect Cloud Run
  sessions via the daemon's endpoints (`resume`/`abort` already go
  through the daemon) or `psql` until the CLI grows the driver flag.
- **Port:** mast doesn't read Cloud Run's `$PORT` in v0.1; the starter
  pins `--listen=:8080` and `containerPort: 8080` to match.
