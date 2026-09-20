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

### The scheduling lease — shipped 2026-09-18 ([#345](https://github.com/go-steer/mast/issues/345))

Everything above this line is still a target. What ships is the half of it that makes the trap loud rather than the half that makes the fleet work, and the split is deliberate: a second replica used to fire every schedule a second time with nothing in any log to say so, and that is a worse failure than a second replica that declines to fire at all.

**Scope.** One lease, `scheduling/<workload>`, over every loop in `serve` that starts a turn nobody asked for — the scheduled trigger, the timed-pause scheduler, and the boot auto-resume scan. One lease rather than three because "is this replica the one that acts on its own?" is one question, and three leases would let a deployment be half-leader. The lease governs *unrequested* work only; inject, AG-UI and A2A are unaffected on every replica.

**Mechanism: (b) claim-based, not (a) a Kubernetes Lease.** Re-checked rather than inherited, and the recommendation holds for a reason the original text does not give: a claim over the session store works in every topology mast is installed into, including the Compose and bare-binary ones, whereas a Kubernetes Lease works in one of them. It also reuses `pkg/eventlog`'s shipped session-lock primitive — same row, same heartbeat loop, same steal-on-stale rule — rather than adding a second coordination mechanism with its own failure modes. `AcquireInstanceLease` is that primitive at the fleet grain; before #345 the primitive had no caller anywhere in the module.

**The timings are the instance lease's own, and they are tighter than the session lock's.** 2s heartbeat / 8s staleness, against the session lock's 5s / 30s. The two leases are waited on by different things and so want different numbers: a session lock's staleness window is only ever waited out by a process that has decided to steal a stuck turn, which is rare and not latency-sensitive, while the instance lease's window is waited out on **every contested boot** — see the crash case below, where it is the difference between a restarted daemon resuming its schedules in ten seconds and in thirty-five. Tight is affordable here because the holder is one process per workload, not one per session, so the heartbeat traffic does not scale with anything.

**What it deliberately is not.** It is not leader election and it does not make mast multi-replica. A replica that loses the race at boot stays passive for its whole life: it does not watch the lease, and it does not take over when the leader dies later. Kubernetes restores the leader instead: once the lease goes stale it is taken by whichever instance *boots* next, and that reclaim has no identity check — a redeploy or a scale-up claims it as readily as a restart of the pod that died. What never claims it is an instance already running, which asks once at boot and never again; that asymmetry, not the lease's expiry, is what "stays passive for its whole life" means. Takeover by a live passive replica is a genuine feature and is not this one; it needs session-ownership handoff (above) to be real first, or two replicas will hand the *lease* over while still both believing they own the sessions.

**The three outcomes, and the one that is a judgement call.**

| At boot | Decision | Why |
|---|---|---|
| Lease acquired | Drives scheduled work | The single-replica case, which is almost everyone. |
| `ErrSessionLocked`, still held after the retry window | Passive, logged at ERROR naming the holder and all three loops it stops | This is a deployment mistake an operator fixes in one command, and the symptom it replaces is one they would otherwise debug from duplicate side effects at the far end. |
| Store could not answer | **Drives scheduled work**, logged at ERROR | Fails open on purpose. Duplicate work needs a second replica to exist; failing closed would turn a database hiccup at boot into a workload that never fires again and says nothing. Losing the coordination is a smaller loss than losing the function being coordinated. |

**The crash case, and why a contested boot waits.** A daemon that exits cleanly releases its lease, so the contested boot is nearly always the *other* case: the previous process was SIGKILLed — OOM, node eviction, `kill -9`, the exact events the durability pillar is about — and left a lease nothing released. Refusing there would refuse the dead process's own replacement, which then stays passive for life; an OOM kill would silently end scheduled work until somebody restarted the pod a second time. That is strictly worse than the duplicate-fire bug the lease was added to fix, and it is not hypothetical: it is what the UAT's `kill -9`-and-restart step produced on the first cut of this change.

So a contested boot polls for `InstanceLeaseStaleAfter` plus two seconds (10s) before concluding that the holder is alive, logging the wait so the pause does not read as a hang. A crashed daemon's restart therefore has its schedules back in about ten seconds. The cost lands on the genuine second replica, which spends those ten seconds at boot before it can say it is second — once per replica, against a crash cost paid on every crash. The window is deliberately **bounded**: retrying forever would be takeover by another name, and that needs session-ownership handoff first.

**Losing the lease mid-life.** The daemon watches `Lost()` and cancels the scheduling loops — not the daemon. It keeps serving requests and does not take the role back without a restart. Both halves matter: a lease lost means another instance is already firing, so continuing would be the split-brain the lease exists to prevent; and a replica that stopped firing is still a replica that should answer HTTP.

**The passive replica's timers still fire.** A `mast sessions pause --resume-at` can land on any replica, and the record is durable wherever it lands — but the scheduler that would arm it is the leader's. The leader therefore rescans the pause table every minute in addition to its boot scan, which bounds the lateness of a timer minted elsewhere at a minute instead of at the leader's next restart. The rescan is idempotent by construction: pending timers are keyed by token and the fire path re-fetches the record, so re-arming a consumed token is a silent drop rather than a second fire.

**Still open.** Leader takeover — a healthy passive replica picking up the cadence when the leader dies, rather than waiting for the leader's own pod to come back — is **[#403](https://github.com/go-steer/mast/issues/403)**, and it is filed rather than listed because the interesting part is the dependency: it cannot be built before session-ownership handoff, or a promoted replica runs turns against sessions the old leader still believes it owns and kills them with `stale session error`. Also open, and on the other axis: the per-pause claim described above, which would let *every* replica fire *some* timers rather than one replica fire all of them, and autonomous-loop assignment.

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

### Sequencing against the v1.0 freeze — decided 2026-09-19 ([#344](https://github.com/go-steer/mast/issues/344))

**`per_tenant` is explicitly post-v1.0.** No part of it needs to land before the freeze, and the freeze does not make it harder afterwards, because each of the three frozen surfaces it touches takes the change additively. The useful half of writing that down is the prohibition at the end: there is exactly one way to implement per-tenant isolation that breaks the freeze, and it is the shortest one.

| Frozen surface | What per-tenant needs from it | Additive after the freeze? |
|---|---|---|
| `pkg/workload` | an `isolation:` block on `Bundle` | Yes — a new block is a new struct field and a new YAML key |
| `pkg/transcript` | a tenant filter on every read | The parameter is already there, unpopulated |
| root `github.com/go-steer/mast` | a caller-supplied tenant id | Yes — via `Config`, which every entry point already takes |

**The bundle key.** The rule is already written in the tree: `AGUIRunQueue`'s doc comment in [`../pkg/workload/bundle.go`](../pkg/workload/bundle.go) states that adding a key to a block is free after v1.0 while changing a key's *shape* is a `schema_version` bump — and `isolation:` is a block for the same reason `run_queue:` is. The loader's behaviour for a key of this kind is already right too: `Load` probes `schema_version` leniently and only then unmarshals with `KnownFields(true)` ([`../pkg/workload/loader.go`](../pkg/workload/loader.go)), so a v1.0 binary handed a bundle written for a later schema refuses it by version, naming both numbers, rather than ignoring an isolation block it cannot read. For the file that declares who may not read whom, refusing is the correct failure.

**The store.** Every read and write on `transcript.Store` already takes a `userID` — `List`, `Get`, `Decisions`, `Acks`, `Schedule`, `RecordAck`, `AckEffects`. The tenant axis does not have to be *added* to the frozen signatures; it has to be populated and enforced. Two calls sit outside it deliberately, and both are operator-plane rather than tenant-plane: `ScanInterrupted(ctx)` sweeps every user by design (it is the auto-resume sweeper), and `ExportDecisions` carries `UserID` inside `ExportOptions`, where empty means auto-discover. Whether those two become tenant-scoped or admin-only is a question for whoever implements this; neither needs a new parameter, so neither is a freeze question.

**The library API.** Today the root package hardcodes `const userID = "mast-library"` — every library-run session lands under one user, so the library surface has no tenant axis at all. All seven frozen root entry points (`Run`, `RunWorkload`, `ListSessions`, `ResumeSession`, `ResumeByToken`, `AckEffects`, `Pause`) take `cfg Config`, so a tenant field on `Config` reaches all of them without changing a signature. That is the same seam the `WithIsolationScope(tenantID)` sketch above describes, arrived at from the other direction.

**What the freeze forbids: re-using `userID` to mean tenant.** It is the obvious shortcut — the parameter is on every call already, and nothing would fail to compile. It is also a semantic change to a frozen key, which is precisely what [#300](https://github.com/go-steer/mast/issues/300) promised not to do. A tenant is not a user; a tenant *has* users. Taking the shortcut breaks the freeze without changing one signature, which is the kind of break a signature diff cannot catch — so it is written here rather than left to be noticed.

None of this makes deferral free. [#344](https://github.com/go-steer/mast/issues/344)'s argument for doing the work — that the second team to install mast discovers the missing boundary by looking at a session store rather than by reading a design doc — is untouched by which side of the freeze it lands on. What is settled is only the ordering: nothing here blocks v1.0, and v1.0 does not block this.

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

> **Status, 2026-09-11 ([#291](https://github.com/go-steer/mast/issues/291)):** this section is a *target*, and most of it is unbuilt. Shipped today: the container image, the GitHub Release binaries, and — since 2026-09-19 — cosign signatures over the checksum file (see [Signed release artifacts](#signed-release-artifacts--shipped-2026-09-19-342)). **Not shipped:** the Homebrew tap, the Debian/apt repo, `examples/deploy/gke-helm/`, `examples/deploy/terraform/`, and the Cloud Build config — `examples/deploy/gke/` is a README. Read the paragraphs below as the shape being aimed at, not as an inventory; the work is tracked in [#342](https://github.com/go-steer/mast/issues/342) and the lapsed schedule is struck through under [Phasing](#phasing).

### Container images

- **`ghcr.io/go-steer/mast:v0.X.Y`** — official binary image. Distroless base; ~30MB image. Multi-arch (linux/amd64 + linux/arm64).
- **`ghcr.io/go-steer/mast:v0.X.Y-debug`** — debug variant with shell + common tools; not for production.
- **Base for library-embedded consumers.** Not applicable — library consumers ship their own images with mast as a Go dep.

### Binaries

- **GitHub Releases** for each tag: `mast-linux-amd64`, `mast-linux-arm64`, `mast-darwin-amd64`, `mast-darwin-arm64`, `mast-windows-amd64.exe`. Signed release artifacts (cosign). *What ships is `mast_<version>_<os>_<arch>.tar.gz` for linux/darwin x amd64/arm64 — no Windows build and no bare binaries — plus `checksums.txt` and, since 2026-09-19, its signature. See below.*
- **Homebrew formula** in `homebrew-tap` for developer laptop install.
- **Debian packages** in a public apt repo for Debian/Ubuntu server operators. v0.3+.

### Signed release artifacts — shipped 2026-09-19 ([#342](https://github.com/go-steer/mast/issues/342))

Every tag after v0.9.0 publishes `checksums.txt.sig` and `checksums.txt.pem` beside `checksums.txt`. Three decisions are worth recording, because the line above ("Signed release artifacts (cosign)") settles none of them.

**Keyless, not a project key.** cosign exchanges the release job's GitHub OIDC token for a short-lived Fulcio certificate; the identity written into that certificate is `https://github.com/go-steer/mast/.github/workflows/release.yml@refs/tags/<tag>`. There is no mast public key for a verifier to obtain out of band and no private key for the project to hold, rotate, or lose. What is being attested is not "a human at go-steer vouches for this" but "mast's release workflow, at this tag, produced this" — which is the claim an installer actually wants and the one a transparency log can back.

**The checksum file only.** `checksums.txt` covers every tarball byte-for-byte, so a signature over it transitively covers the release. `artifacts: all` would mint four more signatures asserting nothing the first does not, and would give a verifier three more opportunities to check the wrong one and believe they were done.

**The identity is the whole product, and the failure mode is a wildcard rather than an omission.** This was measured against the first signed artifact (cosign v2.6.5) rather than reasoned about, and the reasoning would have been wrong: `verify-blob` in keyless mode *refuses* to run without `--certificate-identity` or `--certificate-identity-regexp`, so nobody accidentally verifies nothing by leaving the flag off. What does verify nothing is `--certificate-identity-regexp '.*'`, which reports **Verified OK** against a certificate minted by any workflow in any repository on GitHub — and a wildcard is exactly what a reader writes when the exact identity carries a tag they would otherwise have to edit into a script. So the install page warns about the pattern cosign accepts, not about the flag cosign already requires. The same run confirmed the pin is load-bearing in the direction that matters: a wrong ref, a wrong workflow file, a wrong repository and a wrong OIDC issuer are each refused. `dev/release/verify-signature.sh <tag>` writes the exact identity itself so no reader has to choose, fetches the tarballs the signed file names, and runs `sha256sum --check`, because a verified signature over a list that is then ignored proves nothing about the bytes on disk.

The release workflow runs that same script **against the published release**, not against its own `dist/`. GoReleaser exiting 0 means cosign exited 0 on a runner; it says nothing about whether the `.sig` and `.pem` became release assets. That is the same failure shape as the empty release bodies of v0.1.0-pre through v0.3.0, where every step logged correct output and the artifact was wrong ([#125](https://github.com/go-steer/mast/issues/125)), so it gets the same answer: assert on the download. The dry run signs and verifies too — against `refs/heads/<branch>` rather than `refs/tags/<tag>`, and it asserts that identity rather than accepting any — because a signing step that has never executed is not evidence that signing works.

Still open on #342: SLSA provenance attestation, and a signature on the container image.

### The install page is checked against the release — shipped 2026-09-19 ([#342](https://github.com/go-steer/mast/issues/342))

`docs/site/src/content/docs/install.md` sat at v0.4.0 for three releases, copy-paste download URLs included, and every check was green ([#306](https://github.com/go-steer/mast/issues/306)). The rule that closed it reads the *version* in those URLs. It does not read the *filenames*, and a page whose every version is right can still list four tarballs where five ship, or offer a `checksums.txt.sig` the release does not carry.

[`dev/release/check-install-page.sh`](../dev/release/check-install-page.sh) compares the page's asset list, its download URLs and its tarball-contents sentence against the assets of the release. It asks two different oracles at two different times, and the split is deliberate:

- **On every PR**, the expected set comes from `.goreleaser.yaml` at the version CHANGELOG.md's newest heading names. Offline, deterministic, and correct on the release-prep PR — where the tag does not exist yet and the heading is already right, which is why docs-lint reads the changelog rather than `git describe`.
- **After each tag**, from the assets the published release actually carries. A config is what someone intends to ship; the release is what an operator can download, and the gap between those two is the whole reason the notes check and the signature check above assert on the download.

Two things the check deliberately does *not* do. Signature artifacts are required to be **documented on the page**, not **listed under the release** — signing landed after v0.9.0 was cut, so demanding `.sig` inside a list headed "Assets for v0.9.0" would demand a falsehood, while the failure worth catching is a release that signs and a page that never mentions it. And an input the parser cannot understand — an archive name template it has no evaluator for — is fatal rather than a smaller expected set quietly reported as OK.

This is the *checked-by-CI* half of #342's third item. The other half, "an install page that does not assume the reader is us", is waiting on the [chart-or-kustomization choice](#packaging): what the page should tell an operator to run depends on what there is to run.

### The image an operator pulls — shipped 2026-09-20 ([#342](https://github.com/go-steer/mast/issues/342))

Item 1 of #342 asks for "one composition an outsider can run — Helm chart or a self-sufficient kustomization, pick one and say why". Attempting to answer it surfaced a prerequisite nobody had written down: **there was no mast image**, and there had not been one at any point.

Three facts, each checkable:

- `deploy/base/50-statefulset-daemon.yaml` pulls `ghcr.io/go-steer/mast:latest`. The org publishes nine container packages — `core-agent`, `core-agent-slim`, `core-agent-tui`, `k8s-event-watcher`, `lookout`, `switchboard`, `cogo`, `simian-agent`, `charts/lookout` — and `mast` has never been one of them.
- `deploy/overlays/example` pinned `newTag: spike-0`, a tag that was never pushed. Dropped 2026-09-20.
- The `Dockerfile` that would produce the image had not built since go.mod moved to `go 1.26.6`: its `GO_VERSION` pin stayed at 1.26.3 and the golang images set `GOTOOLCHAIN=local`, so `go mod download` fails outright. Nothing in CI compiled it, which is why six weeks passed without anyone finding out. Fixed, and `.github/workflows/ci-image.yml` now builds the image, runs the binary inside it, and cross-compiles arm64 on every push to `main`.

So the chart-versus-kustomization question was choosing a wrapper for an artifact that did not exist, and the second half of item 2 ("cosign on release binaries **and the container image**") was blocked on the same gap. Publishing comes first; that is the reordering, and it is the only part of #342's suggested sequence that changed.

[`../.github/workflows/release-images.yml`](../.github/workflows/release-images.yml) publishes `ghcr.io/go-steer/mast` for `linux/amd64` and `linux/arm64`: `:X.Y.Z`, `:X.Y`, `:X` and `:latest` on a release tag — `:latest` guarded off any tag containing a `-`, semver's pre-release marker, for the reason release.yml already marks those releases Pre-release — and `:main` plus `:main-<sha>` on a main push. It is adapted from core-agent's file of the same name (see [`./sibling-sync.md`](./sibling-sync.md)), with three deliberate differences:

- **It verifies what it published.** `cosign sign` exiting 0 says the runner signed something; it does not say a verifier pulling the tag finds a signature. The workflow re-verifies against the registry with the identity pinned exactly, then pulls the image back and runs it. Same [#125](https://github.com/go-steer/mast/issues/125) discipline as the release notes, the checksum signature and the install page.
- **One image, no variant matrix.** mast has no `no_tui` build tag and no separate client binary.
- **No O-tier gate.** `release.yml` already refuses to publish a release for a commit the tier has not passed; a second gate on the same commit adds a failure mode, not a guarantee.

The main-push tags are not a convenience. They are what continuously rehearses push → sign → verify, so that path is never first exercised on a release tag — the same argument that makes release.yml's dry run sign for real.

The image also now knows what it is. `VERSION`, `COMMIT` and `BUILD_DATE` build args feed the same three `-X` symbols `.goreleaser.yaml` injects into the released binaries, so `mast --version` inside the image answers with the release rather than with `dev`. A bare `docker build .` passes none of them and still reports `dev`, which is the honest answer for a build with no release to claim.

Publishing does not answer chart-versus-kustomization; it makes the question answerable. The daemon manifest's `:latest` starts resolving at the next release tag, and `:main` is pullable from this change onward. The answer is the next section.

### Chart, not kustomization — decided and shipped 2026-09-20 ([#342](https://github.com/go-steer/mast/issues/342))

Item 1 asks for one composition an outsider can run without editing it first, and says to pick a form and argue it rather than ship both. **It is a Helm chart**, and it is in `charts/mast/`. The `deploy/` kustomize tree was deleted in the same change; keeping both would leave two compositions that can disagree about what mast deploys, and "one of them already existed" is not the reason #342 asks for.

The argument is not that charts are conventional. It is that three of the four things this deployment needs are things kustomize either cannot do or can only do by having the operator edit files in this repo.

**1. There is no parameter surface, and the placeholders survive the render.** `kustomize build` takes no values. Measured on the tree while it still existed: `deploy/base` rendered two unreplaced placeholders into its output, `deploy/overlays/example` three, and `deploy/remediation-target` three across two distinct names. Being fair to kustomize, a `replacements:` block can source a project ID from a ConfigMap and splice it into the RBAC subject string with a delimiter and index — that removes the `sed` the remediation-target header documents. It does not remove the `kustomization.yaml` the operator has to author against our base, and authoring an overlay against a base is reading the base. The "done when" clause is *installs without reading `deploy/` source*.

**2. One list, N remediable namespaces.** The write grant is per-namespace by design, and that design is right. Its expression is not: today an operator applies the same kustomization once per target namespace with a different `namespace:` each time. A chart takes `remediationNamespaces: [team-a, team-b]` and emits the pairs. Verified against a throwaway chart before this was written — two `RoleBinding`s, each carrying both subjects with the project ID substituted into the Workload Identity Federation username.

**3. A missing project ID becomes a refusal to install instead of a successful apply that grants nothing.** This is the argument that decided it. `REPLACE_ME_PROJECT` left unreplaced still renders a syntactically valid `ClusterRoleBinding`; `kubectl apply` accepts it, and the daemon then reaches the cluster over the MCP path as a User that no binding names. That is precisely the [#290](https://github.com/go-steer/mast/issues/290) failure — a boundary that reads as if it exists and grants nothing — and it is the failure mode with no symptom until a tool call comes back `Forbidden`. Helm's `required` makes the same omission a render-time error (`gcp.projectID is required`) with nothing sent to the API server. A placeholder that installs cleanly is worse than no default at all.

**4. The chart rides the pipeline the previous section just built.** Charts publish as OCI artifacts, so `ghcr.io/go-steer/charts/mast` sits beside the image, versions with the release, and is signed and verified by the same keyless cosign identity — the org already publishes `charts/lookout` this way. kustomize's remote form is a git ref against a repo an outside reader currently cannot resolve.

**What it cost, which is what was predicted.** `deploy/projection_test.go` and `deploy/rbac_test.go` were 741 lines that parsed the manifests as YAML directly, and templated files are not YAML. They are now `charts/{rbac,projection,render}_test.go`, asserting against `helm template` output, which does parse. That puts `helm` in the Go test path, under the rule the decision set: the tests **fail** when helm is missing rather than skipping, because an RBAC test that skips is indistinguishable from no RBAC test. `MAST_SKIP_CHART_TESTS` is the deliberate opt-out for someone who genuinely has no helm, and CI installs helm so the opt-out can never be why a run is green. The ConfigMap-drift check against `examples/workloads/gke-triage/` survives unchanged in shape — `.Files.Glob` reads the chart's own copy of the bundle, so the copy still needs pinning to its source.

**Two silent-downgrade tests became structurally unnecessary, which is a better outcome than porting them.** `deploy/projection_test.go` existed because the ConfigMap generator's `files:` and the StatefulSet's `items:` were two hand-written enumerations that could disagree, and had ([v0.3 W1.3 finding (b)](./v0.3-plan.md): nine of thirteen specialists were generated into the ConfigMap and never projected into the pod, so those failure modes routed to `_fallback` with nothing in the logs). In the chart both lists come from one `.Files.Glob "files/**"`, so they cannot drift; the test that remains pins the chart's copy against `examples/workloads/gke-triage/` and asserts every ConfigMap key reaches the pod, which is the part a glob cannot guarantee on its own. Likewise, the default install now renders **zero** write verbs anywhere — `remediationNamespaces` is empty — so "a fresh install can change nothing" is a property one test states (`TestDefaultInstallGrantsNoWrite`) rather than a claim about which files an operator remembered not to apply.

Not decided here: the chart covers the GKE daemon topology only. Cloud Run and Terraform stay separate artifacts, and Homebrew and apt remain behind item 3.

**What an install looks like.** One command, no file in this repo read or edited:

```bash
helm install mast oci://ghcr.io/go-steer/charts/mast \
  --namespace mast-triage --create-namespace \
  --set gcp.projectID=my-project \
  --set 'remediationNamespaces={team-a,team-b}'
```

Object names are fixed rather than release-prefixed, against Helm convention and on purpose: `scripts/setup-wif.sh` derives an IAM principal from the daemon's ServiceAccount name, `scripts/rbac-matrix.sh` checks grants by subject name, and the WIF username itself embeds the namespace and the ServiceAccount. A release-name prefix would make all three depend on what the operator typed after `helm install`. Two releases in one cluster is not a supported topology regardless — the ClusterRole names are cluster-scoped and would collide under any prefix.

### Kubernetes manifests

`examples/deploy/gke/` — canonical GKE manifests: Deployment, Service, HPA, ConfigMap (for `.agents/*`), Secrets (for provider creds), NetworkPolicy, PodDisruptionBudget. Kustomize-friendly (base + overlays for common variations).

`examples/deploy/gke-helm/` — *superseded 2026-09-20. The chart is not an example: it is `charts/mast/`, the one shipped composition, published as an OCI artifact. See "Chart, not kustomization" above.*

The shipped composition lives in `charts/mast/`, not `examples/deploy/` — see "Cluster permissions" below for the RBAC layout.

### Cluster permissions

*(Shipped v0.3 W2.6, 2026-08-14.)* The agent's structural read/write split — read-only diagnosers, one `change-executor` (`internal/compose.CheckCapabilitySplit`) — is mirrored on the cluster side, so that what the agent will *propose* and what the cluster will *accept* are two independent boundaries rather than one:

| Grant | Kind | Scope | Where |
|---|---|---|---|
| Diagnosis | `ClusterRole mast-daemon-read` | every namespace, `get`/`list`/`watch`, **no secrets** | `charts/mast/templates/clusterrole{,binding}-daemon-read.yaml` |
| Change | `Role mast-daemon-write` | one namespace per `remediationNamespaces` entry, none by default | `charts/mast/templates/rbac-daemon-write.yaml` |

Three properties are deliberate and are pinned by `charts/rbac_test.go`:

- **The write grant is narrower than the tools.** `apply_k8s_manifest` can name any kind; the Role lets it create workload objects and ConfigMaps only, in one namespace, and lets nothing delete a Deployment. A call outside the grant fails at the API server as a `Forbidden` the specialist sees as a tool error.
- **The write grant is opt-in and namespaced.** `remediationNamespaces` is empty by default, and a default install therefore renders no write verb anywhere: mast diagnoses the whole cluster and can change nothing until someone names the namespaces. Each named namespace gets its own `Role` + `RoleBinding` pair, so widening is a value an operator can read back with `helm get values`, not a directory they applied N times and have to remember.
- **The lint walks from the subject.** Any binding naming the daemon — under either of the two usernames below — is checked, not just the file called "read", because this boundary erodes by someone adding a cluster-scoped grant for one tool.

**The IAM caveat, which is the load-bearing part on GKE.** GKE authorizes a Kubernetes API call if **either** IAM or Kubernetes RBAC allows it, and the daemon reaches the cluster through the GKE MCP server as its Workload Identity Federation principal — not through the pod's KSA token. Bind `roles/container.admin` to that principal and the namespaced write Role bounds the in-cluster API path and *nothing at all* on the MCP path. So `scripts/setup-wif.sh` binds `roles/container.viewer` by default (`WRITE_SCOPE=namespaced`) and leaves the writes to RBAC, which is the configuration the split describes; `WRITE_SCOPE=cluster-admin` is the escape hatch. `scripts/rbac-matrix.sh` checks both halves — the RBAC cells via `kubectl auth can-i --as=`, and, when `PROJECT_ID` is set, whether the principal still holds a cluster-write IAM role.

**Two subjects, because two usernames (#290, measured on live GKE 2026-09-06).** The narrowed mode shipped as an opt-in for four releases on the stated grounds that nobody had run it. Running it produced a result and a reason the result had been unreachable: GKE does **not** resolve the WIF principal to the KSA's RBAC ServiceAccount subject. The API server sees an RBAC *User* named `serviceAccount:<project>.svc.id.goog[mast-triage/mast-daemon]`, which a `kind: ServiceAccount` subject does not match. Under the old default that was invisible — cluster-write IAM allowed the call anyway, and the RBAC file read as if it were the boundary. Under the narrowing it would have been a daemon that could not remediate anything, for reasons no manifest explained. Both bindings now name both subjects, `charts/rbac_test.go` fails a binding that names only one, and with the pair in place the matrix is green over the MCP path in both directions: patch allowed in the remediable namespace, refused one namespace over, Deployment delete refused, cluster-wide secret list refused. The IAM role and the RBAC subject are one change, not two: either alone leaves the split decorative.

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
| ~~**v0.2**~~ | ~~Session-ownership handoff (advisory-lock based). Multi-instance GKE deployments (2-N replicas; Postgres store). Attach-mode redirect-based affinity. Helm chart.~~ **Did not ship.** |
| ~~**v0.3**~~ | ~~Spanner adapter (via community contribution or Google-team direct). Timed-pause scheduler (claim-based). Multi-tenant deployment starter. Attach-mode proxy-based affinity. Custom-Kubernetes-metric HPA guide.~~ **Did not ship.** |
| ~~**v0.4+**~~ | ~~Firestore adapter; multi-region active-active (with Spanner); explicit autonomous-loop load balancing; Debian package.~~ **Did not ship.** |

**The v0.2–v0.4 rows lapsed, and are struck through rather than re-dated (2026-09-11, [#291](https://github.com/go-steer/mast/issues/291)).** Every version row after v0.1 above went unshipped through v0.7.0: there is no Helm chart, no session-ownership handoff, no claim-based scheduler, no multi-tenant starter, no Debian package. The packaging section's Homebrew tap, apt repo, `examples/deploy/gke-helm/` and `examples/deploy/terraform/` do not exist either (cosign signatures did not either, and shipped 2026-09-19 — see [Signed release artifacts](#signed-release-artifacts--shipped-2026-09-19-342)), and `examples/deploy/gke/` is a README.

They are struck rather than moved because a schedule that slips five releases without anyone noticing is not a schedule, and re-dating it to v0.8 would produce the same artifact — a table that reads like a plan and enforces nothing. This is the failure shape [#300](https://github.com/go-steer/mast/issues/300) found in the stability promise: a corpus commitment repeated for releases, with no mechanism that could ever fail because of it.

The work that survived the review is now tracked as issues, which can be closed and can go stale visibly: **[#342](https://github.com/go-steer/mast/issues/342)** packaged installation (chart, signed artifacts, an install page CI checks), **[#343](https://github.com/go-steer/mast/issues/343)** the reconciliation answer (diagnose, do not reconcile — the CRD stays out of scope), **[#344](https://github.com/go-steer/mast/issues/344)** `isolation.scope`, and **[#345](https://github.com/go-steer/mast/issues/345)** the scheduler's two-replicas problem (**closed 2026-09-18** — see [The scheduling lease](#the-scheduling-lease--shipped-2026-09-18-345); the duplication is gone, the fleet is still not multi-replica). The designs above are unchanged and still what those issues should be built from; what changed is that nothing in this document claims a version for them any more. See [`./positioning.md`](./positioning.md) § "Who installs mast" for the decision that made them scope rather than observations.

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
- **Kubernetes operator (CRD-based mast management).** *Reaffirmed 2026-09-11 ([#291](https://github.com/go-steer/mast/issues/291), [#343](https://github.com/go-steer/mast/issues/343)) and no longer dated "v1.0+".* A CRD is a second control plane to version, support and eventually freeze, aimed at config that is already files an operator has GitOps for. Deciding that mast is a thing others install does **not** reopen it — what that decision does oblige is an answer to "is the daemon running the manifests I applied", and mast's answer is **diagnose, do not reconcile** ([#289](https://github.com/go-steer/mast/issues/289): stable ConfigMap name, no hot reload, a startup identity line and a warning when the mounted files stop matching). Operators use standard Deployment + ConfigMap patterns. Written out for an installing operator — the actual log lines, the `kubectl rollout restart` that fixes it, and the fact that the warning has no metric or alert behind it — under [config drift](https://go-steer.github.io/mast/install/#config-drift-diagnosed-not-reconciled) on the docs site (#343).

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
