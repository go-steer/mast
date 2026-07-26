# mast v0.1 anchor use case: GKE triage demo

**Status:** draft, 2026-07-16 (updated 2026-07-25 — spike-2 pass: graph shape verified end-to-end, sessions revised to per-incident, prototype-location open question resolved, spike-status section added). Companion to [`./fork-design.md`](./fork-design.md) (Phase 1 exit criteria this demo augments), [`./adk-v2-usage.md`](./adk-v2-usage.md) (the v2 constructs it exercises), [`./specialists-design.md`](./specialists-design.md) (schema for the specialist roster), [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) (the LLM-as-router graph shape), [`./orchestration-design.md`](./orchestration-design.md) (workload bundle schema), and [`./skills-design.md`](./skills-design.md) (coexistence: the mast-native reshape does not preclude publishing/consuming a skill against the same problem later). Reference use case: [`core-agent/examples/gke-troubleshoot-agent`](https://github.com/go-steer/core-agent/tree/main/examples/gke-troubleshoot-agent) plus its [`k8s-event-watcher`](https://github.com/go-steer/core-agent/tree/main/cmd/k8s-event-watcher) sidecar.

## Why this doc exists

Phase 1 of the fork ships v0.1 of the `mast` runtime ([`./fork-design.md`](./fork-design.md) P1.6). The exit criteria in that doc are correctness-shaped ("`mast --workload=... --provider=... <envelope>` runs end-to-end", "HITL round-trip … pause survives process restart", etc.) but do not name a concrete platform-team use case that ties the subsystems together. This doc fixes that: **GKE incident triage — the same job the core-agent recipe already covers — is v0.1's anchor use case, expressed in a mast-native decomposition that exercises the subsystems that are mast's actual differentiators.**

The demo's job is (i) *showcase mast's differentiators* against a real platform-team problem, not (ii) *prove parity with the current core-agent recipe*. Anyone who wants the current recipe can use core-agent today — that recipe exists, deploys, and works. What does not exist yet is a mast-native answer to the same problem that demonstrates specialists, workload bundles, classifier-first dispatch, workflow graphs, and durable HITL doing useful work end-to-end. Landing this demo is landing that answer.

## Reference: what the current core-agent recipe does

Captured verbatim from an inventory pass, not reasoned about — this is the ground truth the reshape reacts to.

- **Shape.** One long-lived Task-mode agent daemon per pod, one session per incident. No sub-agents, no workflow graph. Routing lives inside a single skill (`k8s-triage/SKILL.md`) that picks one of eleven reference files by the k8s event `reason` (`CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `OOMKilled`, `FailedMount`, `FailedScheduling`, `BackOff`, `Unhealthy`, `NetworkNotReady`, `NodeNotReady`, `Evicted`, plus `_fallback`). Each reference has Budget / Diagnose / Common-fixes sections.
- **MCP.** One server: `gke` at `https://container.googleapis.com/mcp`, transport HTTP, auth `google_oauth` scope `cloud-platform`. Auth via Workload Identity Federation for GKE, direct binding (no GSA impersonation). Setup scripted (`scripts/setup-wif.sh`). Tools called: `apply_manifest`, `patch_resource`, `scale_deployment`, `rollout_undo`, plus generic diagnostics (`get pod`, `logs --previous`, `describe pod`, `list events`).
- **Edge trigger.** `k8s-event-watcher` sidecar with a client-go informer, allow-list of 11 reasons, dedup on `(uid, reason)`, `per-incident` mode by default. HTTP POST `/sessions` then `/sessions/<sid>/inject` with a `InjectPayload` envelope (`kind, reason, namespace, kind_of_object, name, container, uid, message, count, first_seen, last_seen, cluster, context:{controller_ref, node, labels}`). Bearer token + `X-Asserted-Caller` proxy-identity mechanism.
- **HITL.** None in-band. Escalation is out-of-band via a structured `INCIDENT SUMMARY` block into Cloud Logging → Pub/Sub → Slack.
- **Substrate.** Multi-session daemon + session DB on a RWO PVC (single-replica, HA out of scope). Distroless container (no bash/curl/coreutils). Kustomize base + overlay. `X-Asserted-Caller` requires `admin_identities` + `proxy_identities` config. Prometheus `/metrics` on the sidecar; W3C traceparent propagation across the inject POST.

## The mast-native reshape

### Workload bundle

`.agents/workloads/gke-triage.yaml` — the operator-authored operational profile per [`./orchestration-design.md`](./orchestration-design.md). Exact schema is that doc's; the sketch below is the shape this demo commits to (field names subject to that doc's authoritative naming):

```yaml
name: gke-triage
description: Autonomous triage of GKE cluster incidents surfaced by k8s-event-watcher.
mode: single_session          # explicit; multi_session substrate deferred to v0.2

tool_catalog:
  mcp:
    - server: gke
      auth: google_oauth
      scope: cloud-platform    # WIF direct binding; scripted setup

specialists:
  - triage-classifier          # SingleTurn LLM-as-router
  - CrashLoopBackOff
  - ImagePullBackOff
  - ErrImagePull
  - OOMKilled
  - FailedMount
  - FailedScheduling
  - BackOff
  - Unhealthy
  - NetworkNotReady
  - NodeNotReady
  - Evicted
  - change-safety-gate         # HITL escalation for high-blast-radius suggestions

budget:
  max_wallclock_seconds: 300
  max_cost_usd: 0.50

edge_trigger:
  http:
    path: /workloads/gke-triage/inject
    auth: bearer
    envelope: k8s-event        # matches k8s-event-watcher's InjectPayload
```

### Specialist roster

Thirteen `.tmpl` files under `.agents/specialists/`. Per [`./specialists-design.md`](./specialists-design.md) schema:

| Specialist | Mode | Purpose |
|---|---|---|
| `triage-classifier` | `SingleTurn` | Reads parsed `InjectPayload`; emits the reason bucket to route to. Cheap tier. |
| `CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `OOMKilled`, `FailedMount`, `FailedScheduling`, `BackOff`, `Unhealthy`, `NetworkNotReady`, `NodeNotReady`, `Evicted` | `Task` | Per-failure-mode diagnostic specialists. Own tool allowlist scoped to the gke-MCP subset each failure mode uses. Own budget. `finish_task` argument is the structured triage result. |
| `change-safety-gate` | `Task` | Consulted before any suggested mutation with blast radius above a threshold (`rollout_undo`, `scale_deployment` to 0, `apply_manifest` on shared infrastructure). Emits `session.RequestInput{Message, ResponseSchema}` for operator approval; resumes via attach → [`mast-web`](https://github.com/go-steer/mast-web). |

Prompt-content seed: each per-failure-mode specialist starts from the corresponding reference file in the current recipe (`deploy/base/config/skills/k8s-triage/<reason>.md`). Adapted to the specialist framing — mast-authored, budget-bounded, tool-allowlisted — not merely copy-pasted.

### Workflow graph

LLM-as-router shape per [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) shape #7, wrapped with parse-input and emit-summary nodes:

```
[NewFunctionNode: parse InjectPayload → typed event]
        │
        ▼
[SingleTurn agent node: triage-classifier → route key]
        │
        ▼
[EdgeBuilder.AddRoutes: StringRoute per reason + Default (→ fallback)]
        │
        ├────── CrashLoopBackOff  ──┐
        ├────── ImagePullBackOff  ──┤
        ├────── ...                 │
        └────── Default (fallback)  │
                                    ▼
        [RunNode[TriageResult]: selected Task-mode specialist]
                                    │
                                    ▼
        [optional: RunNode[ApprovalResult]: change-safety-gate if blast-radius > threshold]
                                    │
                                    ▼
        [NewFunctionNode: emit structured INCIDENT SUMMARY into session events]
```

The change-safety-gate is a conditional edge, not always-on — the parent specialist decides (via a `needs_approval` bit in its `finish_task` argument) whether to route through it.

### Edge trigger

- `k8s-event-watcher` reused unchanged. The `InjectPayload` envelope schema is preserved so the existing sidecar binary can be pointed at mast without recompilation.
- Endpoint: `POST /workloads/gke-triage/inject`, bearer-auth. Single token; no `X-Asserted-Caller`.
- ~~Sidecar's `per-incident` mode not used... `shared` mode funnels every inject into one long-lived session.~~ *Revised 2026-07-25 (spike 2): sessions are **per-incident** (`incident-<uid>`, derived from the envelope UID). Two forcing functions surfaced in the spike: durable HITL is per-session, so a shared session commingles pauses from unrelated incidents; and deterministic InterruptIDs (`approve-<specialist>`) are only collision-free with one incident per session. The sidecar's `per-incident` mode is therefore the natural pairing; multi-session **substrate** (per-user auth, cross-session isolation) remains deferred — per-incident session IDs are just an ID-derivation rule, not that substrate.*

### What is deliberately cut for v0.1

- **Multi-session substrate + session-DB PVC.** Deferred to v0.2 alongside the fleet-scale story.
- **`X-Asserted-Caller` proxy-identity mechanism.** With single-session, per-session Owner is moot. Simplifies the auth model materially (drops `admin_identities`, `proxy_identities`, bearer-table config).
- **Slack / PagerDuty / webhook alert fan-out.** The workload still emits the structured `INCIDENT SUMMARY`; the sink is out of scope. Sinks re-enter with the v0.2 observability push per [`./observability-design.md`](./observability-design.md).
- **Multi-cluster fan-in.** Single cluster, single daemon.
- **HA / multi-replica.** Single replica per the current recipe.
- **Cross-runtime resume (Python ADK).** Wire compat preserved per [`./durable-execution-design.md`](./durable-execution-design.md); active dispatch not built.

### What is added vs. the current recipe

- **Specialists in place of skill references.** Eleven per-failure-mode `Task`-mode specialists (with per-specialist tool allowlist and budget) instead of eleven markdown references inside one skill. Trade-off: more moving parts, tighter per-mode budget control, per-mode tool scoping.
- **Classifier in place of skill-internal `reason` switch.** A `SingleTurn` classifier specialist replaces the skill's `reason`-to-file-picker. Exercises the LLM-as-router shape and the small-tier-parent-classifier pattern ([`./positioning.md`](./positioning.md) priority #4, resolved).
- **Workflow graph in place of long-lived single agent.** The graph engine surfaces execution shape at a level attach mode + [`mast-web`](https://github.com/go-steer/mast-web) can render.
- **In-band HITL via `change-safety-gate`.** Divergence from the current recipe's fully-unattended posture. Intentional: HITL is on the v0.1 exit criteria ([`./fork-design.md`](./fork-design.md) item 6), and gating high-blast-radius suggestions is the natural first surface for it in this use case.
- **Workload bundle in place of `AGENTS.md` + `config.json` + `mcp.json` + ConfigMap-flattening.** One declarative operational profile per [`./orchestration-design.md`](./orchestration-design.md).

## v2 subsystems exercised

Cross-reference [`./adk-v2-usage.md`](./adk-v2-usage.md):

| Subsystem | Exercised? | By what in this demo |
|---|---|---|
| Runner + agent modes | Yes (`Task` + `SingleTurn`); `Chat` n/a | Per-failure-mode specialists (`Task`), classifier + `change-safety-gate` response (`SingleTurn`/`Task`) |
| Auto-installed helper tools | `finish_task` yes; `single_turn`/`task` implicit | Every Task-mode specialist returns via `finish_task` argument |
| Unified `agent.Context` | Yes (passively) | Providers, MCP, attach, eventlog all use it |
| Graph engine + node types | Yes | `NewFunctionNode` (parse, emit), `RunNode[TriageResult]` (specialists), `EdgeBuilder.AddRoutes` (classifier routing), `StringRoute` + `Default` |
| `agenttool` wrapping | Yes | Twelve specialists wrapped as tools; one classifier as `SingleTurn` agent node |
| HITL primitives | Yes | `change-safety-gate` emits `RequestInputEvent` with `ResponseSchema`; resume via attach + `mast-web` |
| Session events + durability | Yes | Session persists; HITL pause survives process restart per [`./durable-execution-design.md`](./durable-execution-design.md) |
| Unified telemetry span tree | Yes | Every node + agent + tool call is one span attribute vocabulary |
| `WithIsolationScope` | No | Single-session; multi-tenant story is v0.2 |
| Cyclic graphs | No | Autonomous loop shape not needed for this use case |
| `EmittingFunctionNode` | No | Streaming partial results not required for this demo |

Six of seven v2 subsystems from the inventory get real exercise; the untouched surfaces are all justifiable v0.1 deferrals.

## Sequencing

**Timing relative to the fork trigger.** *Updated 2026-07-26:* per [`./fork-design.md`](./fork-design.md)'s revised trigger, Phase 1's rebuild work starts immediately (this demo's prototype graduates into it); only the P1.3 adapter ports wait on core-agent's code cleanup milestones.

**Sanctioned pre-trigger work.** [`./fork-design.md`](./fork-design.md) risks table explicitly calls for prototyping bucket 1 in a scratch worktree during the trigger-wait period. The GKE triage demo is the honest scope for that prototype: it exercises the substrate against a real use case, not a synthetic toy. Prototype work lives in the standalone `mast-prototype` repo (see resolved open Q #1 below) and validates the ~1500 LOC bucket-1 estimate.

**Spike status (2026-07-25).** Two spikes have run against this plan in the `mast-prototype` repo (tags `spike1`, `spike2`; findings in its `FINDINGS.md`, folded into the corpus in the 2026-07-25 docs pass):

- *Spike 1 (ADK v2.0.0):* loaders (bundle + specialists incl. `mode: SingleTurn`), Chat-coordinator SubAgents dispatch, inject endpoint speaking the sidecar's `InjectPayload`, GKE MCP toolset wiring, offline echo model.
- *Spike 2 (ADK v2.1.0):* the workflow-graph shape from this doc runs end-to-end as the runner root (classifier → `StringRoute`/`Default` → specialist via `DynamicNode`+`RunNode`); durable HITL round-trips across `kill -9` on ADK's SQLite session service (change-safety-gate stand-in: ResumedInput-first + state-stash pattern); per-incident sessions; budget metering from event `UsageMetadata` trips `max_cost_usd` mid-turn; per-specialist MCP tool allowlists via stock `FilterToolset`. A `scripts/demo-spike2.sh` in that repo reproduces all three scenarios offline.

The remaining substrate this plan needs that no spike has touched: multi-cluster/multi-replica anything (deliberately cut), real-model runs against a live GKE MCP endpoint, and the full 13-specialist roster (spike ran 3).

**PR drop-order within Phase 1.**

- After P1.4 (specialists + workload bundle + HITL primitive + edge trigger land): the demo's authoring surfaces exist. Content authoring (13 `.tmpl` files, the workload bundle YAML, the workflow-graph wiring code) can begin.
- During P1.5 (smoke examples + observability + presubmits): the demo becomes part of the shipped `examples/` set — probably `examples/workloads/gke-triage/` with the bundle, specialists, and a stub `k8s-event-watcher` harness.
- P1.6 (v0.1.0 tag): demo running end-to-end is a hard gate. This doc suggests augmenting [`./fork-design.md`](./fork-design.md)'s Phase 1 exit criteria with an additional item: **"The GKE triage workload bundle accepts a synthetic `InjectPayload` for each of the eleven failure modes and produces a structured `INCIDENT SUMMARY`; the `change-safety-gate` specialist round-trips HITL for a `rollout_undo` suggestion."**

**Deployment.** The v0.1 demo runs on a laptop against a stub sidecar, not on GKE. GKE deployment (WIF setup, distroless container, Kustomize base + overlay) enters via [`./deployment-design.md`](./deployment-design.md)'s `examples/deploy/gke/` starter in P1.5, with the triage workload as one of the reference bundles shipped.

## Open questions

1. ~~**Where does prototype code live pre-trigger?**~~ *Resolved 2026-07-25: a standalone `mast-prototype` git repo (local; candidate for private `go-steer/mast-prototype`), not an uncommitted scratch worktree. Rationale: spike findings are worth versioning (spike-1's wrong root-agent conclusion lived only in a code comment until the repo was initialized), tags (`spike1`/`spike2`) give diffable spike boundaries, and a worktree of the mast repo would put prototype code into mast's branch namespace against the docs-only-pre-fork rule.*
2. **`change-safety-gate` trigger conditions.** Does the gate fire on specific tool names (`rollout_undo`, `scale_deployment` where replicas=0, `apply_manifest` targeting shared infrastructure) or on a `needs_approval` bit set by the parent specialist? Bias: latter — parent specialist knows the semantics better than a mechanical name-match. But needs a concrete `ResponseSchema` sketch before Phase 1. *2026-07-25 partial: the substrate is proven (spike 2 gates unconditionally via a bundle-level `hitl.require_approval` flag, with `ResponseSchema: {approved: bool, note?: string}` as `*jsonschema.Schema` and schema-validated resume verified across restart); the trigger-condition decision itself — always-on vs. `needs_approval` bit — is still open, and the gate body must follow the ResumedInput-first + state-stash contract per [`./durable-execution-design.md`](./durable-execution-design.md).*
3. **Bundle `mode: single_session` field.** Is this an explicit field, or the default when `mode` is absent? Depends on [`./orchestration-design.md`](./orchestration-design.md)'s authoritative schema. Flag if that doc doesn't yet specify.
4. **Fallback specialist.** When the classifier returns `_fallback` (unknown reason, or reason outside the eleven), what runs? Options: (a) a generic diagnostic specialist that loads the current recipe's `_fallback.md` content, (b) reject the inject with a structured error, (c) escalate directly to `change-safety-gate` for operator triage. Bias: (a).
5. **Skill coexistence.** Per [`./skills-design.md`](./skills-design.md), skills and specialists coexist. Should this demo *also* expose the equivalent k8s-triage skill as a bundled artifact so consumers who want the "one skill, one agent" shape can use it? Bias: not v0.1 — one authoring model at a time; skill packaging is a separate exercise for later.
6. **Prompt-content reuse from the current recipe.** How much of the current recipe's reference-file content gets adapted verbatim into per-specialist prompts? Bias: substantial reuse — the eleven references are field-tested content, and rewriting from scratch is wasteful. Attribution should mirror bucket-2 port headers ("Originally derived from go-steer/core-agent@<SHA>:deploy/base/config/skills/k8s-triage/<reason>.md").

## Related

- [`./fork-design.md`](./fork-design.md) — Phase 1 exit criteria this demo augments; sanctioned pre-trigger prototyping
- [`./adk-v2-usage.md`](./adk-v2-usage.md) — the v2 substrate this demo exercises
- [`./specialists-design.md`](./specialists-design.md) — schema for the thirteen `.tmpl` files
- [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) — LLM-as-router shape (#7) the graph instantiates
- [`./orchestration-design.md`](./orchestration-design.md) — workload bundle schema
- [`./durable-execution-design.md`](./durable-execution-design.md) — HITL pause/resume the `change-safety-gate` relies on
- [`./skills-design.md`](./skills-design.md) — coexistence framing; open question #5
- [`./deployment-design.md`](./deployment-design.md) — GKE deployment starter that hosts the workload in P1.5
- [`./observability-design.md`](./observability-design.md) — INCIDENT SUMMARY sink; observability enters here
- [`core-agent/examples/gke-troubleshoot-agent`](https://github.com/go-steer/core-agent/tree/main/examples/gke-troubleshoot-agent) — the reference recipe
- [`core-agent/cmd/k8s-event-watcher`](https://github.com/go-steer/core-agent/tree/main/cmd/k8s-event-watcher) — the sidecar reused unchanged
