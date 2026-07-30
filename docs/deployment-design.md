# mast deployment: design

**Status:** draft, 2026-07-01 (updated 2026-07-25 — Cloud Run v0.1 durability contradiction fixed: Postgres pulled into v0.1 via ADK's `session/database` service, which spike 2 verified makes SQLite and Postgres the same one-call surface; GKE v0.1 single-instance pinned to a StatefulSet+PVC when using SQLite). Companion to [`./positioning.md`](./positioning.md) (multi-session deployment story is priority #5), [`./durable-execution-design.md`](./durable-execution-design.md) (multi-instance coordination is a durability concern), [`./library-api-design.md`](./library-api-design.md) (library-embedded is a distinct deployment shape), [`./orchestration-design.md`](./orchestration-design.md) (`isolation.scope` on bundles maps to deployment tenancy), and [`./observability-design.md`](./observability-design.md) (multi-instance metric aggregation). Covers how mast actually runs in production — topologies, multi-instance coordination, multi-tenant tenancy, and packaging.

## Deployment topologies

Mast targets four production topologies. Each has different consequences for session storage, coordination, and configuration.

### 1. Cloud Run (single-binary, single-region, autoscaling)

**Shape.** Mast binary as a Cloud Run service. Ingress via HTTPS (attach + webhook + metrics on separate ports or path prefixes). Autoscales 0-to-N based on request load. Statelessness on the request path; state in Cloud SQL / Firestore / Spanner via the session store adapter.

**Pros.** Simplest ops story; horizontal scaling handled by the platform; per-request billing aligns with agent-shaped costs; managed HTTPS + IAM.

**Cons.** Cold-start latency on scale-from-zero (acceptable for webhook workloads, painful for interactive attach). No persistent local disk (session store must be external). Long-running sessions (autonomous loops, planner spanning hours) don't fit Cloud Run's per-request model — need scheduled resume triggers.

**Session store.** Cloud SQL Postgres **from v0.1** (revised 2026-07-25 — the earlier "v0.2 adapter" phasing contradicted this very paragraph plus the v0.1 exit criterion that pauses survive restart: SQLite cannot live on Cloud Run's ephemeral filesystem. The fix is cheap because there is no adapter to write: ADK's `session/database.NewSessionService` takes a Postgres dialector exactly as it takes SQLite's — see [`./durable-execution-design.md`](./durable-execution-design.md) storage table). Firestore remains v0.3+.

**Long-running workloads pattern.** Sessions that would exceed Cloud Run's request timeout pause durably (per `./durable-execution-design.md`), resume via external scheduler (Cloud Scheduler → HTTP resume endpoint).

**Fit.** Best for event-driven unattended workloads (webhooks, queue triggers) that complete in seconds-to-minutes. Awkward for autonomous loops or long HITL waits — those want GKE.

### 2. GKE (multi-replica, always-on, controller-like)

**Shape.** Mast binary as a Deployment (or StatefulSet if session-affinity matters), with a Service in front. Ingress via GKE Gateway or Ingress. Session store is Postgres / Spanner. Replicas coordinate via advisory locks in the session store.

**Pros.** Long-running processes; autonomous loops native; local storage for hot caches; native Kubernetes primitives (ConfigMaps for `.agents/*` configs, Secrets for provider credentials, HPA for scaling). Composes with existing platform infrastructure (Prometheus, cert-manager, service mesh).

**Cons.** More ops surface than Cloud Run; scaling is manual-ish (HPA works but requires custom metrics for agent-shaped load); shared state coordination is the operator's job (see multi-instance section below).

**Session store.** Postgres (v0.2), Spanner (v0.3+), or CockroachDB (community adapter). Multi-region deployments want Spanner; single-region want Postgres for cost.

**Sidecar variant.** Mast running as a sidecar to another workload (e.g., alongside a GKE-native platform controller) — same binary, scoped-to-one-pod session store (SQLite acceptable), attach mode disabled or bound to loopback. Composes with library-embedded deployment (below).

**Fit.** Best for platform-team workloads that need to run continuously: incident triage across a cluster, drift detection loops, cost monitors, SLO reporters. The main mast target.

### 3. Library-embedded (mast inside a larger Go service)

**Shape.** Mast imported as a Go library (`import "github.com/go-steer/mast"`) into a host service. Host owns the process lifecycle; mast is a subsystem. Host can expose mast's attach mode on its own HTTP mux via `mast.RegisterAttachHandlers` (per [`./library-api-design.md`](./library-api-design.md)).

**Pros.** No separate deployment; agent execution shares host's observability + auth + config; no serialization overhead for programmatic invocations; host can enforce host-native policies via mast lifecycle hooks.

**Cons.** Host is responsible for session state persistence choice; host's crash cycle == mast's crash cycle (both pods restart together); mast release cadence must fit host's dependency-bump discipline.

**Session store.** Whatever the host uses. Common patterns:
- Host already has Postgres → mast uses `eventlog.Postgres` against a mast-owned schema.
- Host has no persistent state → in-memory session store (`session.NewMemoryStore()`) for ephemeral sessions; SQLite for local persistent.
- Host runs on a platform where mast's chosen store makes sense inherently (Cloud Run + Firestore).

**Deployment shape.** Whatever the host's is. If host is Cloud Run, mast is Cloud-Run-embedded. If host is GKE, mast is GKE-embedded.

**Fit.** Best for any Go service that wants to add agent capabilities without operating a separate mast fleet. Also the primary shape for testing (a Go test can spin up an in-process mast trivially).

### 4. Standalone (`mast` binary on a VM / bare metal)

**Shape.** Single mast binary running on a Linux host — a systemd unit, a VM in GCE/AWS/on-prem, a developer's laptop. Session store is SQLite local file.

**Pros.** Simplest possible; no dependencies; suitable for developer workflows + one-off scripts + air-gapped environments.

**Cons.** No horizontal scaling; local durability only (backups are the operator's job); no built-in HA.

**Session store.** SQLite (default, v0.1).

**Fit.** Development, testing, air-gapped, single-operator experimental deployments. Not a production topology for team use.

## Multi-instance coordination

Both GKE and Cloud Run (>1 instance) require coordination across replicas. Concrete concerns:

### Session ownership handoff

**Concern.** When mast instance A receives a resume request for session X that was previously handled by instance B (now dead), A must claim X, replay from the last durable event, and continue.

**Mechanism.** Session store carries an `owner_instance_id` + `owner_lease_expiry` per session. Claim = compare-and-swap on `(owner_instance_id, owner_lease_expiry)`. Lease renews periodically while owner is running; expires on crash. Any instance can claim a session with an expired lease.

**Detail.** Lease TTL default 30s; renewal every 10s while active. On resume attempt, instance polls until claim succeeds or times out. On timed-pause resume, whichever instance the scheduler picks races to claim; failed claims retry or defer to the winner.

**Session store adapters** implement the lease semantics. Built-in Postgres adapter uses `SELECT ... FOR UPDATE SKIP LOCKED` + a `sessions_leases` table; built-in Spanner adapter uses transaction primitives directly.

### Timed-pause scheduler

**Concern.** A timed pause with `ResumeAt = T` should fire at T once, on one instance.

**Options considered:**
- (a) **Leader-elected scheduler.** One instance elected via leader-election (Kubernetes lease); it runs the scheduler; all others idle. Simple but leader failover has downtime.
- (b) **Claim-based.** Every instance polls the pause table every N seconds; instances race to claim eligible pauses; first-claimer fires. No leader; naturally fault-tolerant.
- (c) **External scheduler.** Cloud Scheduler / K8s CronJob fires HTTP calls into the mast fleet; any instance responds and claims.

**Recommendation:** (b) claim-based. Matches session-ownership handoff mechanism (both use compare-and-swap on session records); no leader-election infrastructure needed; polling is cheap on a mast-owned table.

**Detail.** Polling interval configurable (default 5s). Pause table indexed on `ResumeAt`; poll query is `SELECT ... WHERE ResumeAt <= NOW() AND lease_expired LIMIT 100`. Claim + fire + release in one transaction.

### Autonomous-loop assignment

**Concern.** A cyclic-graph autonomous loop (autonomous monitor, inbox drainer) needs to be running on exactly one instance at a time; failover happens on that instance's death.

**Mechanism.** Autonomous loops are sessions with a distinct type flag (`session.TypeAutonomous`). Session ownership handoff (above) applies; but for autonomous loops specifically, ownership renewal is more aggressive (lease renewal every 3s; TTL 10s) because failover latency matters more for continuous loops.

**Distribution.** Multiple autonomous loops across the fleet are distributed by claim — no explicit load-balancing. Even distribution assumed statistically over N loops on M instances. Uneven distribution acceptable for v0.3; explicit balancing is v0.4+ (deploy scheduler picks least-loaded instance for new autonomous starts).

### Attach-mode session affinity

**Concern.** An operator connecting via attach to session X should reach the instance that currently owns X (or be transparently routed).

**Options:**
- **Header-based affinity.** Attach clients pass `X-Mast-Session-Id: X`; ingress routes based on a consistent-hash of the session ID. Requires ingress support (GKE Gateway ✓; Cloud Run has limited support).
- **Redirect.** Any instance can accept the attach request; if it's not the owner, it responds with `307 Location: <owner-instance-url>`. Requires each instance to know the owner-instance URL (from session record's owner-metadata).
- **Proxy.** Any instance can accept; if not the owner, it proxies the attach stream to the owner instance internally. Best UX; ops overhead of instance-to-instance auth.

**Recommendation.** Redirect (v0.3) for simplicity; proxy (v0.4+) for UX when the redirect story creates friction. Header-based affinity as a supported deployment pattern for GKE Gateway consumers who prefer it.

## Multi-tenant tenancy

`isolation.scope` on workload bundles (per [`./orchestration-design.md`](./orchestration-design.md)) can be `per_request`, `per_tenant`, or `global`. This maps to deployment tenancy:

### Per-request isolation

Each session gets its own isolation scope; no cross-session state. Trivially safe; the default. Session-store rows carry the session's scope; queries always include scope filter.

### Per-tenant isolation

Sessions grouped by tenant share isolation scope (state-bound nodes see tenant-scoped values; audit-derived memory learns per tenant). Cross-tenant queries impossible via mast API.

**Requirements:**
- **Tenant ID at session start.** Passed via workload input, envelope header, or `WithIsolationScope(tenantID)` at library API.
- **Session store schema.** Sessions table has `tenant_id` column; indexed; every read query includes tenant filter.
- **Permission gate integration.** Custom permission checkers (per [`./library-api-design.md`](./library-api-design.md)) can enforce per-tenant tool allowlists.
- **Metric labels.** `mast.tenant.scope` on metrics (opt-in per observability doc cardinality guardrail).
- **Log correlation.** All log lines carry tenant ID.

**Cross-tenant leakage prevention.** Bundle-learning (per `./orchestration-design.md`) respects `isolation.scope`: same-tenant only unless operator explicitly opts in to cross-tenant aggregation. Audit-derived memory (per `./memory-design.md`) reads scoped state only.

### Global isolation

All sessions share one scope. Explicit opt-in for single-tenant deployments where isolation overhead isn't worth it. Not the default.

## Configuration surface for deployment

Deployment-specific config lives in the runtime config, injected via env / config file / command line:

```yaml
# .mast/mast.yaml (partial, deployment-relevant sections)
deployment:
  instance_id: ${HOSTNAME}       # unique per replica; env-derived typically
  role: server                    # server | worker | autonomous | scheduler
  session_store:
    type: postgres                # sqlite | postgres | spanner | firestore | custom
    dsn: ${MAST_SESSION_STORE_DSN}
  coordination:
    lease_ttl_seconds: 30
    lease_renew_interval_seconds: 10
    autonomous_lease_ttl_seconds: 10
    autonomous_lease_renew_interval_seconds: 3
    scheduler_poll_interval_seconds: 5
  attach:
    session_affinity: redirect    # redirect | proxy | header
```

## Packaging

### Container images

- **`ghcr.io/go-steer/mast:v0.X.Y`** — official binary image. Distroless base; ~30MB image. Multi-arch (linux/amd64 + linux/arm64).
- **`ghcr.io/go-steer/mast:v0.X.Y-debug`** — debug variant with shell + common tools; not for production.
- **Base for library-embedded consumers.** Not applicable — library consumers ship their own images with mast as a Go dep.

### Binaries

- **GitHub Releases** for each tag: `mast-linux-amd64`, `mast-linux-arm64`, `mast-darwin-amd64`, `mast-darwin-arm64`, `mast-windows-amd64.exe`. Signed release artifacts (cosign).
- **Homebrew formula** in `homebrew-tap` for developer laptop install.
- **Debian packages** in a public apt repo for Debian/Ubuntu server operators. v0.3+.

### Kubernetes manifests

`examples/deploy/gke/` — canonical GKE manifests: Deployment, Service, HPA, ConfigMap (for `.agents/*`), Secrets (for provider creds), NetworkPolicy, PodDisruptionBudget. Kustomize-friendly (base + overlays for common variations).

`examples/deploy/gke-helm/` — Helm chart. v0.2.

### Cloud Run

`examples/deploy/cloud-run/` — canonical Cloud Run deployment: service YAML, terraform module, Cloud Build config for CI-driven deploys.

### Terraform modules

`examples/deploy/terraform/` — reusable modules for the common cloud shapes (GKE, Cloud Run, EKS-if-community-contributes).

## Deployment starter examples

Match the reference-graph library pattern — ship runnable canonical examples that operators copy and adapt:

```
examples/deploy/
  gke/                    # canonical GKE Deployment + supporting resources
  gke-multi-tenant/       # GKE with per-tenant isolation + observability
  gke-helm/               # Helm chart (v0.2)
  cloud-run/              # Cloud Run service
  cloud-run-scheduled/    # Cloud Run + Cloud Scheduler for long-running work
  library-embedded/       # Go host service embedding mast
  standalone/             # systemd unit + config for VM deployment
  compose/                # docker-compose for local multi-tenant dev
```

Each has a `README.md` explaining what it demonstrates, when to reach for it vs. alternatives, and the customization points.

## Composition with other subsystems

| Subsystem | Deployment consideration |
|---|---|
| **Durable execution** | Session store MUST be portable across instances for multi-replica deployments. Timed-pause scheduler + session-ownership handoff are the coordination primitives. |
| **Orchestration** | `isolation.scope` on bundles maps to deployment tenancy. Bundles + specialists are file-loaded from ConfigMaps (GKE) or bundled in the container image; env-var overrides for per-environment (dev/staging/prod) variations. |
| **Library API** | Library-embedded is its own deployment topology; extension points (session store, permission gate, providers) are how the host injects platform-specific bits. |
| **Observability** | Multi-instance metric aggregation via Prometheus scrape (each pod exposes `/metrics`); trace correlation across pods via distributed tracing headers. |
| **Memory** | Audit-derived memory reads from the session store; respects tenancy scope. |
| **Attach mode + mast-web** | Session affinity via redirect/proxy/header; mast-web consumer authenticates against the fleet (any instance), gets routed to the owning instance for session-specific operations. |
| **MCP** | MCP servers can be per-mast-instance (in-cluster deployments) or shared. Credential resolution per bundle context (per positioning priority #6). |
| **Watchdog + signal routing** | Watchdog runs per instance; signals emit into that instance's session event stream (per core-agent issue #159 pattern). Cross-instance watchdog aggregation is a metric-layer concern (Prometheus alert firing → mast bundle trigger). |
| **A2A** ([`./a2a-design.md`](./a2a-design.md)) | A2A server endpoint fronted by Ingress + TLS (GKE Gateway with cert-manager typical). In-cluster Google Agent Registry / kagent registry auto-registration on startup. Per-topology starter (`examples/deploy/gcp-agent-runtime/`, `examples/deploy/kagent/`) shows the wiring. |
| **AG-UI** ([`./ag-ui-design.md`](./ag-ui-design.md)) | AG-UI server endpoint fronted by Ingress + TLS; SSE-friendly (long-lived connections; ensure Ingress + service-mesh timeouts allow). CopilotKit chat-platform bots (`@copilotkit/bot-*` for Slack / Teams / Discord / Telegram / WhatsApp) deploy as sidecar workloads to mast — same pod, same cluster, or as external processes; auth via bearer tokens. Deployment starters `examples/deploy/{slack,teams,discord,telegram,whatsapp}-via-copilotkit/` (v0.2+) include bot process wiring + auth + Kubernetes Secrets. Bedrock AgentCore native AG-UI runtime deployment starter (`examples/deploy/bedrock-agentcore/`) in v0.3+ for AWS-shaped audiences. |
| **Federation** ([`./federation-design.md`](./federation-design.md)) | Federation topology maps to deployment topology — star ↔ single supervisor + worker fleet; mesh ↔ multi-region peer fleets; hierarchical ↔ multi-region with regional supervisors. Mast-native adapter routes via Kubernetes Service + label selector for GKE. Cross-cluster federation uses A2A adapter (higher-trust boundary requires stronger auth). |

## Cost considerations

Deployment-cost knobs operators tune:

- **Provider cost is dominant.** Session-level cost dominates infra cost for typical workloads. Reference: a single Gemini Pro turn is ~$0.01-0.10; infra per-session is ~$0.0001. This shapes decisions.
- **Autoscale on session queue depth**, not CPU. CPU under-utilizes when sessions are provider-bound (mast pod idles waiting on Gemini). Custom Kubernetes metric: `mast_sessions_active` gauge → HPA.
- **Idle-scale-to-zero pattern** for Cloud Run. Session store must be external so scaling to zero doesn't lose state. Cold-start latency 1-3s is fine for webhook workloads.
- **Autonomous loops keep at least one instance alive.** Scaling to zero + a resume-from-scheduler pattern reintroduces cold-start on every iteration. For autonomous work, `min_instances=1` (or run on GKE, not Cloud Run).
- **Multi-region considerations.** Session store latency dominates cross-region agent cost; strongly-consistent stores (Spanner) required for multi-region active-active; regional Postgres with active-passive failover for single-region.

## Phasing

| Version | Scope |
|---|---|
| **v0.1** | Standalone binary; library-embedded; Cloud Run single-instance; GKE single-instance. Session stores via ADK `session/database`: SQLite (standalone / library / GKE-with-PVC) and **Postgres (Cloud Run — required for durability there; revised 2026-07-25)**. GKE single-instance with SQLite runs as StatefulSet + PVC, not a bare Deployment (a rescheduled pod otherwise loses sessions and falsifies the durability exit criterion). *(Shipped 2026-07-30, issue #40: the `deploy/` kustomize base itself now carries this shape — StatefulSet, 1Gi claim at `/var/lib/mast`, `--session-db` on by default; in-memory is a deliberate opt-out, not a deploy default.)* Base `examples/deploy/{gke,cloud-run,standalone,library-embedded}/` starters. |
| **v0.2** | Session-ownership handoff (advisory-lock based). Multi-instance GKE deployments (2-N replicas; Postgres store). Attach-mode redirect-based affinity. Helm chart. |
| **v0.3** | Spanner adapter (via community contribution or Google-team direct). Timed-pause scheduler (claim-based). Multi-tenant deployment starter. Attach-mode proxy-based affinity. Custom-Kubernetes-metric HPA guide. |
| **v0.4+** | Firestore adapter; multi-region active-active (with Spanner); explicit autonomous-loop load balancing; Debian package. |

## Open questions

1. **Deployment topology auto-detection.** Should mast detect its topology (Cloud Run vs. GKE vs. standalone) via env and default accordingly? Bias: partial — detect Cloud Run (`K_SERVICE` env) and Kubernetes (`KUBERNETES_SERVICE_HOST` env) to pick session-store default (external in both cases). Full topology config still explicit.
2. **Instance ID collision risk.** Two instances with the same `instance_id` would race for the same session claims. Enforce uniqueness at start-up (query session store for existing lease under same ID)? Bias: yes — fail-fast on collision; instance ID must be genuinely unique across the fleet.
3. **Rolling update coordination.** During a rolling update, old and new instance versions coexist briefly. Session-format changes must be forward-compat (new instance reads old-instance sessions); event schema changes need a migration path. Bias: rolling-update-safety is a hard requirement; every event-schema change must be additive within a minor version.
4. **Cross-region for GKE.** Multi-region GKE + regional session store (Postgres in one region) means cross-region reads for sessions handled in other regions. Bias: document as an anti-pattern; operators wanting multi-region want Spanner. v0.3 or later.
5. **Air-gapped deployments.** Mast should run in air-gapped environments (no external provider access; local model serving via ollama or similar). Provider extension point covers this; deployment guide needed. Bias: v0.3 as air-gapped adopters surface.
6. **Deployment testing.** How do we test deployment topologies in CI? Bias: `examples/deploy/*` have smoke tests that spin up the topology in a kind cluster + verify a canonical workload runs end-to-end. v0.2.
7. **Migration between topologies.** Moving from single-instance SQLite to multi-instance Postgres — is there a one-time export tool? Bias: yes for v0.2 (`mast sessions export/import` command).
8. **In-cluster mast-web instance.** If `mast-web` runs in-cluster alongside mast (same GKE), does it authenticate as a peer service or as a proxy on behalf of the operator? Design deferred to mast-web's doc.

## Out of scope

- **Managed hosting.** We don't sell mast-as-a-service; operators run their own.
- **Deployment orchestration UI.** No mast-web feature to deploy mast itself; standard tooling (kubectl, terraform, Cloud Build) handles that.
- **Backup and disaster recovery for session stores.** Session store's ecosystem tooling (Postgres backup + PITR, Spanner backups) handles this; mast doesn't add a layer.
- **Cross-cloud portability guarantees.** Mast runs on any cloud with Go binary support; portability of the session state depends on operator's choice of session store adapter.
- **Serverless anywhere-but-Cloud-Run.** Lambda + Azure Functions + Cloud Functions: possible if community contributes adapters; not shipped by us.
- **Kubernetes operator (CRD-based mast management).** Interesting but v1.0+; operators use standard Deployment + ConfigMap patterns today.

## Related

- [`./positioning.md`](./positioning.md) — priority #5 (multi-session deployment story) lands here
- [`./durable-execution-design.md`](./durable-execution-design.md) — session storage requirements + coordination primitives
- [`./library-api-design.md`](./library-api-design.md) — library-embedded topology + extension points
- [`./orchestration-design.md`](./orchestration-design.md) — `isolation.scope` → deployment tenancy
- [`./observability-design.md`](./observability-design.md) — multi-instance metric aggregation
- [`./memory-design.md`](./memory-design.md) — audit-derived memory in multi-tenant deployments
- [`./mcp-catalog-design.md`](./mcp-catalog-design.md) — MCP server placement decisions in cluster
- [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) — attach client's deployment interaction
- Cloud Run / GKE / Cloud Scheduler / Cloud SQL / Spanner docs — the substrates we ride on
