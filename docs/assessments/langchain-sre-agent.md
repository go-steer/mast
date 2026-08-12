# Assessment: LangChain's autonomous SRE agent vs. mast

**Date:** 2026-08-12 · **Status:** assessment, not a design decision · **Subject:** [`langchain-ai/sre-agent`](https://github.com/langchain-ai/sre-agent) (blog: *How we build an autonomous SRE agent for Kubernetes deployments*)

An evidence-grounded comparison of LangChain's published SRE agent against mast v0.2.0, plus a plan to a functionally equivalent MVP spanning `mast`, [`switchboard`](https://github.com/go-steer/switchboard), and [`k8s-lookout`](https://github.com/go-steer/k8s-lookout).

This is an assessment doc (like [`../uat-v0.2-plan.md`](../uat-v0.2-plan.md)) — it records findings and a proposed plan. Decisions it produces belong in the relevant design doc and its resolved-decisions section. No docs-site mirror.

Read alongside [`../orchestration-design.md`](../orchestration-design.md) (bundles, mutation predicate, evaluation harness), [`../specialists-design.md`](../specialists-design.md), [`../triage-demo-plan.md`](../triage-demo-plan.md), and [`../durable-execution-design.md`](../durable-execution-design.md).

---

## TL;DR

**mast is heading in the right direction.** The subagent↔specialist mapping the reading suggested is real and close to 1:1, and on four axes mast is already ahead of what LangChain shipped: durable execution (the recorded-effect outbox has no counterpart on their side), multi-provider, USD cost metering, and read-path depth (`k8s-lookout` is a materially stronger deterministic collector than their 44 hand-rolled tools).

**But the specific things that make their agent work in production are the parts of mast that are designed-and-unwired.** Three of them are load-bearing:

1. A **structural** read/write boundary with per-mutating-tool approval. Ours is prompt text plus a result-level gate.
2. **Parallel fan-out.** Ours routes to exactly one specialist, so "audit the cluster" is not expressible.
3. **Stateful finding tracking** across scheduled runs. Without it, proactive monitoring is unusable noise — a conclusion they reached the hard way.

Plus the operator half: `switchboard` cannot post unprompted, and cannot render an Approve/Reject button.

Six slices (P0–P5) close the gap. P1 is the long pole.

---

## 1. What they actually built

Read from source (~10.7k lines), not the blog. **The repo has moved past the post** — Postgres persistence, stateful monitoring with finding-diffing, and notification policy all exist now and are listed as future work in the post. Assess against the code.

### Shape

| Piece | Implementation |
|---|---|
| Orchestrator | `create_deep_agent()` on LangGraph. Sonnet (`claude-sonnet-4-6`), 44 read tools, `write_todos` planning, `task()` subagent dispatch |
| Subagents | 8 read-only analysts on Haiku (`pod-inspector`, `scaling-analyzer`, `performance-analyzer`, `log-analyzer`, `security-auditor`, `reliability-auditor`, `job-analyzer`, `config-auditor`) + `change-executor` on Sonnet |
| Write boundary | Write tools exist **only** on `change-executor` (`subagents/change_executor.py`); all 13 enumerated in `CHANGE_EXECUTOR_INTERRUPT_ON = {name: True}`. Mirrored in RBAC: cluster-wide read ClusterRole + namespace-scoped write Role |
| Fast path | `_HEALTH_CHECK_RE` intent router → `_collect_cluster_data()` (zero LLM tokens) → **one** forced-tool Haiku call (`tool_choice={"type":"tool","name":"report_health"}`) → validated Pydantic `HealthReport` |
| Monitoring | `MonitoringScheduler` on an interval, 30s stagger. Fingerprints findings, diffs against prior run, posts **only on change**, digest every 12 checks |
| Slack | Bolt Socket Mode, Block Kit, `sre_approve`/`sre_reject`/`sre_ack` action_ids → `Command(resume=...)` on the LangGraph thread. Approver allowlist (`SLACK_APPROVER_IDS`) |
| Durability | LangGraph checkpointer (MemorySaver dev / Postgres prod) + `InMemoryStore`. Their own comment: the checkpointer "is what makes HITL work at all: without it, a subagent interrupt has nowhere to persist" |
| Guards | `recursion_limit` 60, `ModelCallLimitMiddleware` (40/run, 120/thread), `ToolCallLimitMiddleware` (80/run), per-FS-tool cap 25/run, Anthropic prompt caching via `@wrap_model_call` |
| Evals | LangSmith. 32-example scenario dataset + 3 evaluators; online evals on production traces |

### The three ideas worth stealing

1. **Don't invoke the agent when code suffices.** Their scheduled check spends zero tokens on collection and exactly one model call on analysis, with a forced tool schema. Fixed cost, fixed latency, no loop risk. This is the same thesis as `k8s-lookout` and we should apply it at the mast layer too.

2. **Fingerprint identity is a schema concern.** `Finding` carries `kind` / `resource_name` / `reason` *expressly* so `monitor_state.fingerprint()` gets an identity the model cannot reword between runs. `normalize_resource_name()` strips Kubernetes' vowel-free random suffix alphabet (`[bcdfghjklmnpqrstvwxz2456789]`) so a crashlooping pod keeps one identity across restarts. Without this the diff is worthless and every run re-alerts.

3. **The write boundary is structural, not textual.** No amount of system-prompt discipline gates a tool the agent holds. They gate by *not granting it*.

### Two warts their own `docs/architecture.md` flags — do not copy

- **Two divergent output contracts.** The fast path returns a typed `HealthReport`; the agent path returns free text that Slack parses with a `[CRITICAL]` regex. Downstream code has to handle both.
- **7 exported write tools no agent can reach.** `WRITE_TOOLS` has 18 entries; `change-executor` wires 13. The rest are dead surface that looks live.

A third, found in review: `evals/evaluators.py:146-152` annotates the judge result as `QualityGrade` then subscripts it (`grade["score"]`). With `with_structured_output` returning a Pydantic model that raises. **Read their dataset for content; don't copy their evaluator code for form.**

---

## 2. Is mast heading in the right direction?

**Yes.** The architecture is right, and the mapping is closer than "similar spirit."

### Where the mapping is clean

**Subagents ↔ specialists.** Their subagent dict is `{name, model, description, system_prompt, tools, interrupt_on}`. Our `.tmpl` frontmatter is `{description, budget, mode, model, tools}` + prompt body. Ours is strictly richer — budgets, allowlist algebra with per-field presence semantics, per-tool MCP filtering via stock `tool.FilterToolset` (`pkg/specialists/register.go:49-70`). We even already ship the same roster *shape*: `examples/workloads/gke-triage/specialists/` holds 12 symptom-keyed specialists, reason-named exactly the way their analysts are.

**Deterministic-first.** Their #1 stated lesson is `k8s-lookout`'s entire thesis, and lookout is the stronger version: `lookout health` is a ten-category scorecard; `triage logs` compresses ~150k tokens to ~350; plus blast radius, drift, edges, storm correlation, and a `mcp` mode exposing read commands 1:1 as MCP tools. Their `_collect_cluster_data()` is ~200 lines of hand-rolled collection.

**HITL.** Their interrupt/resume maps onto ours directly — `interrupt_on` + `Command(resume=...)` is our `workflow.NewRequestInputEvent` + `ctx.ResumedInput` (`pkg/graph/graph.go:168-217`). Same primitive.

### Where mast already leads

| Axis | mast | Theirs |
|---|---|---|
| Durable execution | Recorded-effect outbox (`pkg/effects`), boot-time auto-resume, kill-9-survivable interrupts, dangling-mutation refusal | Checkpointer only. A crash between `kubectl_scale_deployment` firing and the response landing resumes **blind** — no equivalent of our `skipped_ambiguous` |
| Cost | USD metering (`pkg/budget`), workload ceilings, watchdog, pricing refresh | Call *counts* only |
| Providers | Multi-provider by pillar | Anthropic-shaped (judge on OpenAI) |
| Deployment | Single Go binary, distroless, library-embeddable | Python service + Postgres |
| Observability | OTel + Prometheus, fixed registry | LangSmith (good product loop, vendor-shaped) |
| Read path | `k8s-lookout` as a released, RBAC-read-only binary | 44 hand-rolled tools inside the agent process |

The differentiators are real and none of them are at risk from this comparison.

### The honest caveat

Their agent's *production* properties come from a small set of mechanisms mast has specified and not built. The direction is right; the distance is real. Section 3 quantifies it.

---

## 3. Gap register

Ranked by how much each blocks the MVP. Evidence is `file:line` in the mast worktree unless noted.

### G1 — No per-mutating-tool approval gate — **blocking, safety**

Their structural property: the orchestrator holds zero write tools, and every write is a `change-executor` tool listed in `interrupt_on`.

mast today:
- HITL gates the **specialist's result**, not the tool call. `pkg/graph/graph.go:181-217` runs the specialist to completion via `workflow.RunNode`, *then* parks on a `RequestInput`. By the time an operator sees the prompt, any mutation the specialist chose to make has already fired.
- `hitl_policy.on_mutation` is specified with a mutation predicate (`docs/orchestration-design.md`, resolved-decision row 129) but unimplemented. `pkg/workload/bundle.go:92-97` carries only `RequireApproval bool`.
- `pkg/permissions` is ported but **no `permissions.Gate` is constructed anywhere in non-test code**. Only `pkg/attach/prompter.go` imports it — the `PromptBroker` transport that would carry a gate's prompts to an operator exists; the gate itself is never interposed on tool execution. (The seam is proven: `pkg/effects` registers as a runner plugin at `cmd/mast/main.go:443` and `cmd/mast/oneshot.go:127`.)
- **Specialists mix read and write in one allowlist.** `examples/workloads/gke-triage/specialists/OOMKilled.tmpl:12-20` grants `patch_resource` and `rollout_undo` to a *diagnosis* specialist, held back only by prompt text at lines 34-38: *"Do NOT mutate anything on your own initiative."* That is precisely the boundary LangChain rejected.

### G2 — No parallel fan-out — **blocking, capability**

Their on-demand path fans out to 8 analysts concurrently and synthesizes. `pkg/graph/graph.go` is classify → route → **one** specialist (`StringRoute` per reason, `Default` → `_fallback`). `run_shape_fan_out_fan_in` returns `not_implemented` in the v0.2 scaffold (`docs/site/src/content/docs/roadmap.md`). Today mast cannot answer "audit the whole cluster."

Note the existing constraint: HITL cannot originate inside parallel branches (`ErrParallelHITLUnsupported`, [`../workflow-scaffolding-design.md`](../workflow-scaffolding-design.md)). That is compatible with the target design — analysts are read-only, so no branch needs to interrupt; remediation happens in a sequential post-synthesis node.

### G3 — No scheduled trigger, no finding-state tracker — **blocking, the proactive half**

- `EdgeTrigger` is HTTP-only. `pkg/workload/bundle.go:121-126`: *"other transports (message queue, scheduled) will join here."*
- More important: **nothing equivalent to `monitor_state.py`.** No fingerprints, no run-to-run diff (new / escalated / ongoing / resolved / suppressed), no ack windows, no digest cadence, no notify-only-on-change.

This is the difference between a scheduler that works and one that gets muted. lookout's dedup and storm correlation cover part of it at the *signal* layer, and `triage status` keeps records, but there is no cross-run *finding* state at the mast layer.

### G4 — No chat egress, no in-Slack approvals — **blocking, the operator half**

`switchboard` is mention-driven inbound only. `pkg/chat/slack/slack.go:152-174` dispatches `EventTypeConnecting` / `Connected` / `ConnectionError` / `EventsAPI` / `SlashCommand` — **no `EventTypeInteractive`**, so no `block_actions`, so no Approve/Reject/Ack buttons. There is also no "post this into a channel" API, which unattended monitoring requires by definition (there is no thread to reply into).

Foundation is present: `pkg/chat/slack/blockkit.go` and `mrkdwn.go` already render, and the `X-Asserted-Caller` path already carries per-caller attribution.

### G5 — No typed report contract

They emit a validated `HealthReport{overall_severity, summary, findings[], recommended_actions[]}` via forced tool use, with `Finding` fields chosen for fingerprint stability (`schemas.py:22-59`). mast specialists return free text through `finish_task`. We have `ResponseSchema` plumbing on HITL interrupts but no *output* schema on specialist results.

G5 is a prerequisite for G3: you cannot fingerprint free text.

### G6 — Per-specialist model tier parsed but ignored

`pkg/specialists/loader.go:84` reads `Model` into the `Spec`. `pkg/specialists/register.go:74-97` builds **every** specialist with `opts.Model` — `spec.Model` is dropped on the floor. "Sonnet for synthesis, Haiku for scale" is unreachable today.

Same story for budget: `pkg/graph/graph.go:100-106` maps only `MaxWallclockSeconds` → `NodeConfig.Timeout`. Per-specialist `max_turns` falls back to the workload meter and per-specialist `max_cost_usd` is not enforced at all — the package doc says so, and `OOMKilled.tmpl:8-11` declares all three.

### G7 — No eval harness

Designed in [`../orchestration-design.md`](../orchestration-design.md) (golden traces, `mast sessions capture-golden`, A/B, cost regression), phased v0.3+. Nothing shipped.

**We already have the hard part.** `pkg/providers/mock/scripted.go` (JSONL replay) and `pkg/agent/toolactor.go` (request-driven fake model, restart-safe, from the v0.2 UAT work) give deterministic, zero-cost trajectory testing. Their harness needs live model calls for every eval run; ours would not. See §5.

### G8 — No decision-feedback loop

Their approve/edit/reject attaches to the run as feedback ("proposed 10 replicas, human edited to 4") and becomes eval data. mast has the event log and could capture this trivially — but the verdict schema is only `{approved: boolean, note: string}` (`pkg/graph/graph.go:206-213`), so the richest signal (the *edit*) is not representable.

### G9 — Loop/step guards are workload-level, not per-run

Minor. Their `ModelCallLimitMiddleware` distinguishes run vs. thread; ours is a session-scoped meter. Worth noting for parity but not blocking — arguably ours is the better substrate since it meters money, not calls.

### Non-gaps, for the record

- **Loop and cost guards** — mast is ahead.
- **Tracing** — OTel export to any backend beats a LangSmith dependency. The *product loop* around traces (§5) is theirs; the substrate is ours.
- **RBAC split** — `deploy/base` already separates daemon and watcher service accounts; G1's namespaced-write Role is an increment, not a new pattern.
- **Event-driven ingress** — `deploy/base/51-deployment-watcher.yaml` already runs a k8s-event-watcher in `--mode=per-incident --dedup-window=5m` against `/inject`. They have no equivalent; their reactive path is human-initiated only.

---

## 4. Plan to MVP

**Target:** proactive monitoring + on-demand investigation + gated remediation, driven from Slack, with an eval gate in CI.

Six slices. P1 is the long pole. P3 and P4 parallelize behind P0.

### P0 — Cheap unblocks (mast, small)

1. **Honor `spec.Model`** in `specialists.Build` — resolve per-specialist model against the provider config, fall back to `opts.Model`. Touches `pkg/specialists/register.go:74-97` and the compose path. Unlocks the whole cost story; interacts with specialists-design open Q#4 (cross-provider override).
2. **Enforce per-specialist `MaxTurns` / `MaxCostUSD`** — compose with the workload meter, tightest-cap-wins (already the documented semantic in `pkg/workload/bundle.go:76-86`).
3. **Add `output_schema` to specialist frontmatter** → `OutputSchema` on the Task agent (`pkg/agent/modes.go:43` already carries the field).

   **Mast never gains a `Finding` or `HealthReport` Go type** (per Q1). The mechanism stays generic — a `*genai.Schema` mast does not interpret — and the concrete k8s-shaped schema, with `kind` / `resource_name` / `reason` for fingerprint stability, is a workload asset shipped with the `gke-triage` bundle and published by lookout. This is *less* mast code than first drafted, and it removes the one place P0 was quietly going to make the substrate domain-aware.

**One report contract, not two.** Both the bounded path and the agent path emit the same schema. This is the wart we refuse to inherit.

*Exit:* a bundle can declare a Haiku-tier analyst and a Sonnet-tier synthesizer; both return a validated `HealthReport`.

### P1 — The write gate (mast) — **the important one**

Granularity is settled (Q3): **both `per_call` and `per_change_set`, one gate, `per_call` first and UAT'd before `per_change_set` lands.** Sub-steps 1–5 are the `per_call` half; step 6 is `per_change_set`.

1. **Wire `pkg/permissions` into the tool-execution seam `pkg/effects` already occupies.** Ordering is settled: outbox check *then* gate (resolved-decision row 144 — a replayed result needs no fresh approval).
2. **Implement `hitl_policy.on_mutation`** (`require_approval` / `apply` / `dry_run`) against the documented mutation predicate: built-in annotation + MCP `readOnlyHint`, default-deny-unknown, per-tool audited override via the existing `tool_catalog.tools[].mutating` (`pkg/workload/bundle.go:64-74`). Note the shipped finding that ADK's mcptoolset drops MCP annotations — the override is the real un-gate in practice.
3. **Restrict grant scopes for mutating tools** — normative, and it must land *with* the wiring rather than after. `AllowOnce` and change-set-minted exact-signature grants only; `AllowSessionVerb` / `AllowSessionTool` / `AllowAlways` refused. The ported scopes are developer-tool ergonomics: `AllowSessionTool` on `patch_resource` means one approval hands over the namespace for the session.
4. **Split the roster by capability.** Analyst specialists get read-only allowlists; one `change-executor`-shaped specialist holds writes. Strip `patch_resource` / `rollout_undo` from `OOMKilled.tmpl` and its siblings. Make the boundary structural.
5. **Extend the verdict to `{approve | reject | edit}`** with edited arguments validated against the tool's input schema. Copy their legibility rule: narrow, named tools only — nothing with `helm upgrade`-shaped blast radius, because an operator cannot approve what they cannot read.
6. **Then `per_change_set`** — a grant-minting path in front of the gate built in 1–5, not a second mechanism. Durable minted-not-chosen grants on the resume-token pattern, consumption paired against the outbox record, plus the freshness bound and precondition re-check the window demands.
7. **Mirror in RBAC** in `deploy/base`: cluster-wide read, namespace-scoped write.

*Exit (per_call):* a diagnosis specialist structurally cannot mutate; a remediation call parks the turn *before* it fires, survives `kill -9`, and resumes on operator approval with the operator's edits applied. UAT green before step 6 starts.

*Exit (per_change_set):* an operator approves N mutations once; a crash after call 3 of 5 resumes knowing which fired and re-fires none of them; a stale change set is refused back to the operator rather than silently applied.

### P2 — Fan-out (mast)

Implement `run_shape_fan_out_fan_in` — or, narrower and faster to land, a `dispatch: fanout` graph shape: N cheap-tier analysts run concurrently, one frontier-tier synthesis node merges into a single `HealthReport`. Concurrency capped at the `NewParallelWorker` `maxConcurrency` argument (the one that actually binds from a workflowagent root — resolved-decision row 133). Each branch budget-bounded. Remediation stays sequential and post-synthesis, respecting `ErrParallelHITLUnsupported`.

*Exit:* "audit namespace X" produces one merged report from concurrent analysts.

### P3 — Proactive monitoring (k8s-lookout + mast)

1. **`scheduled:` on `EdgeTrigger`** (interval + jitter), reusing the timed-pause scheduler machinery in `cmd/mast/pausesched.go`.
2. **Collector = `k8s-lookout`.** `lookout health --format=json` plus `triage delta` / `triage top` over lookout's MCP mode. Zero model tokens, and better than their hand-rolled collection.
3. **Bounded analysis.** One cheap-tier call, forced structured `HealthReport`, no orchestrator, fixed step count. Their strongest lesson; we get it nearly free once P0.3 lands.
4. **Finding state lands in lookout, not mast** (Q1). New surface: `lookout findings diff --report -` takes a `HealthReport` on stdin and returns the delta (new / escalated / ongoing / resolved / suppressed) plus a notify decision. Fingerprint + ack windows + digest-every-N live behind it. **Port their `normalize_resource_name` outright** — the vowel-free-alphabet trick is genuinely clever and we would otherwise rediscover it the hard way.

   Build it by **generalizing lookout's existing occurrence store** (§9.1 `--store`) rather than adding a tracker beside it. The duplication risk does not disappear by relocating the code: lookout's dedup is window-scoped and signal-level, findings are unbounded-horizon and report-level, and two things called dedup that mean different things is the failure mode to avoid.

   **Scope to single-cluster for the MVP.** Dedup state persists in the single-cluster model but is in-memory in the multi-cluster model, where finding state would not survive a restart — acks evaporate and everything re-alerts as new. Durable multi-cluster finding state (a shared store, or per-cluster sentinels reporting into a central one) is a named follow-on.
5. Advance finding state **before** notifying (their ordering — a Slack failure must not replay the whole diff next cycle).

*Exit:* a single-cluster bundle runs every N minutes at bounded cost and notifies only on change, with a periodic digest. Mast orchestrates, meters, gates, and notifies; it never learns what a pod is.

### P4 — Slack (switchboard)

1. **`POST /notify`** — agent-initiated post to a channel, rendering `HealthReport` through the existing Block Kit layer.
2. **`EventTypeInteractive`** handling; map `block_actions` → mast's resume endpoint (`pkg/inject`'s `ResumeRequest` takes session + interrupt or a token) and → the ack endpoint. Per Q1's open sub-decision, acks traverse mast for authn and audit and are forwarded to lookout, which owns the state — so switchboard keeps a single backend and the approver-attribution story does not split across two systems.
3. **Approver allowlist**, asserted through the existing `X-Asserted-Caller` path so mast attributes the decision to a real human in the audit log.

Verify the rest runs unchanged: mast's attach server already serves the daemon contract switchboard speaks (`pkg/attach/handlers.go:232-270` — `POST /sessions`, `/sessions/{sid}/inject`, `/wake`, `/interrupt`, SSE `/events`).

*Exit:* a finding lands in Slack with working Approve / Reject / Ack buttons, and the decision is attributed.

### P5 — Evals (mast)

Detailed in §5.

*Exit:* `dev/ci/presubmits/evals.sh` gates PRs at zero model cost; a nightly judge tier reports quality drift.

### Sequencing

```
P0 ──┬── P1 ── P2 ──┐
     ├── P3 ────────┼── P5 (gates everything after)
     └── P4 ────────┘
```

---

## 5. Evals, in depth

Their eval design is the most directly reusable thing in the repo, and the part most worth improving on.

### What they have

`evals/sre-agent-k8s-eval.jsonl` — 31 examples on disk (`create_dataset.py` builds 32). Each is:

```json
{"inputs": {"scenario": "..."},
 "outputs": {"expected_tools": [...], "expected_actions": [...], "expected_response": "..."}}
```

Scenarios are **text descriptions, not live clusters** — which is why the suite runs anywhere. Coverage is genuinely good and reads like an on-call runbook index: CrashLoopBackOff, OOM/exit 137, ImagePullBackOff (401), Pending/insufficient CPU, unbound PVC, stalled readiness, HPA at max, node NotReady + DiskPressure, zero endpoints / selector mismatch, evictions, exit-0 restart loop, bad rollout → rollback, DaemonSet toleration, missing secret, init-container failure, ResourceQuota rejection, liveness probe killing a healthy pod, HPA scale-down stabilization, healthy-cluster OK case, DNS FQDN resolution, CoreDNS forwarding loop, NetworkPolicy blocking, Ingress pathType, LoadBalancer pending on IAM, TLS expiry, port mismatch, 503 during rollout / missing preStop, Istio STRICT mTLS, NodePort firewall, CNI IP exhaustion.

Three evaluators (`evals/evaluators.py`):

| Evaluator | Kind | Method |
|---|---|---|
| `severity_accuracy` | code | exact match on classified severity |
| `tool_coverage` | code | set overlap of called vs. expected tools, partial credit |
| `response_quality` | LLM judge | `gpt-4o-mini`, `QualityGrade{reasoning, score 1-5, specific, actionable, correct_diagnosis}`, normalized `(score-1)/4` |

Plus online evals over production traces, and a detect → fix → prevent loop where a production failure becomes a dataset example.

### What we should build

**Port the dataset.** Put it under `testdata/evals/*.jsonl`. Their 32 scenarios are good and were clearly written by someone who has carried a pager. Then **add the ones their harness structurally cannot express**:

- interrupt / resume mid-remediation (does the approved mutation fire exactly once?)
- ambiguous-effect refusal (dangling mutating call → `skipped_ambiguous`, not a blind retry)
- budget exhaustion mid-investigation
- approval **rejected** — does the agent stop, or rationalize a workaround?
- approval **edited** — are the operator's arguments the ones that execute?

These are mast's differentiators. If they are not in the eval suite they are not defended.

**Split the evaluator tiers.**

| Tier | Runs on | Evaluators | Cost | Where |
|---|---|---|---|---|
| Deterministic | `pkg/providers/mock/scripted.go` + `pkg/agent/toolactor.go` | `tool_coverage`, `severity_accuracy`, effect-ordering, exactly-once — all pure code over the event log | zero | every PR |
| Judge | real provider | `response_quality` | metered | nightly |

The deterministic tier is the structural advantage. Their `tool_coverage` requires a live model call per example to observe a trajectory; ours reads the trajectory out of the durable event log, which we already query for the UAT suite. That means we can afford to gate on it.

**Harness:** `dev/ci/presubmits/evals.sh`, sibling to the existing `e2e.sh`, following the `scripts/uat-v0.2.sh` pattern.

**Close the loop (G8).** Log every approve / reject / edit verdict as a labeled example. "Human edited 10 → 4" is the highest-signal training data the whole system produces, and it arrives free with P1.4.

**Do not copy `evaluators.py` mechanically** — see the `grade["score"]` bug in §1.

---

## 6. What we deliberately do differently

| Theirs | Ours | Why |
|---|---|---|
| Two output contracts (typed + regex-parsed free text) | One `HealthReport` everywhere | Their own architecture doc calls this a wart |
| 7 exported-but-unreachable write tools | Roster is the allowlist; unreferenced servers are a fatal load error | Dead surface that looks live is a security smell |
| Checkpointer-only durability | Recorded-effect outbox + auto-resume + ambiguous-effect refusal | Blind resume can double-fire a scale or a rollback |
| Call-count budgets | USD metering | Operators budget in dollars |
| LangSmith-coupled evals | OTel + event-log-derived, deterministic tier in CI | No vendor dependency; gate-able at zero cost |
| Prompt-level "do not mutate" | Structural read/write split | P1.3 — the boundary must not be a sentence |
| Judge-only quality signal | Judge tier **plus** durability/safety assertions | The properties we sell need tests |

---

## 7. Open questions

**Q1 and Q3 resolved 2026-08-12** — see [`../README.md`](../README.md)'s resolved-decisions table for the canonical rows.

1. ~~**Where does `pkg/findings` live?**~~ **Resolved: `k8s-lookout`, not mast.** mast is domain-neutral substrate and Kubernetes finding identity is domain logic. Two premises in the original framing were also wrong: findings outlive sessions, so mast's session store was the wrong home on its own terms; and lookout's read-only RBAC governs *cluster* access, not its own persistence. lookout already carries the storage layer — `--store` (SQLite occurrence store, §9.1, TTL + size-bounded prune), `--dedup-persist`, and the `--distill-interval` recurrence→durable-fact pass (§9.2). **Durability is model-dependent:** single-cluster persists; multi-cluster holds dedup state in memory, so finding state would not survive restart there — acks evaporate, everything re-alerts as new. **P3's MVP targets single-cluster**; durable multi-cluster finding state is a named follow-on, not an assumption. A third repo was rejected on YAGNI. Mast's side is a wire contract (`lookout findings diff --report -`), not a Go import. *Sub-decision still open:* ack routing — bias is that acks traverse mast for authn and audit and are forwarded to lookout for state, so switchboard keeps one backend.
2. **Does the fan-out synthesis node re-read, or synthesize only from branch reports?** Deliberately deferred: this is a cost/accuracy tuning knob, and deciding it by argument means guessing. Build P2 with the cheap option (branch reports only) and let P5's evals say whether accuracy justifies the re-read.
3. ~~**Approval granularity?**~~ **Resolved: both, via `hitl_policy.approval_granularity: per_call | per_change_set`, default `per_call`.** Crucially it is **one gate, not two** — per-change-set compiles down to per-call by minting grants bound to exact normalized `(tool, arguments)` signatures, which the per-call gate consumes silently. The grant model is already ported and dormant in `pkg/permissions`. The sharp finding: the ported grant scopes are developer-tool ergonomics and are dangerous here — `AllowSessionTool` on `patch_resource` hands over the namespace for a session — so mutating tools admit only `AllowOnce` plus change-set-minted exact-signature grants. `per_call` is the default because `per_change_set` carries two hazards it doesn't: staleness between approval and execution, and the rubber-stamp risk if a change set isn't legible. Phasing: `per_call` ships and earns UAT first; `per_change_set` follows in the same slice. Full treatment in [`../orchestration-design.md`](../orchestration-design.md).
4. **Does the edit verdict need a schema-validated diff surface** in the AG-UI / attach protocols, or is a free-form arguments object enough for MVP? Deferred as downstream of Q3 — now partly answered by it: `per_call` needs only arguments validated against the tool's input schema, while `per_change_set` wants a richer surface. Revisit when `per_change_set` starts.
5. **`ErrParallelHITLUnsupported` under P2** — confirmed compatible for read-only analysts, but does a `dry_run` mutation inside a branch count as an interrupt origin? Still open, and cheap but not free: the likely answer is no (nothing executes, so nothing needs approval), but the documented gap that `NewParallelWorker` never appends worker events means a proposed mutation recorded *inside* a branch may not reliably reach the event log. If that holds, the real constraint is that branches must return proposed mutations in their report payload rather than relying on the log — a constraint on P2's synthesis contract, not a yes/no. Wants a verification pass against `pkg/effects`.

---

## 8. Provenance

Assessed against:

- `langchain-ai/sre-agent` at `/home/user/projects/langchain-samples/sre-agent` — `docs/architecture.md`, `agent.py`, `config.py`, `subagents/*`, `tools/__init__.py`, `schemas.py`, `monitor_state.py`, `scheduler.py`, `persistence.py`, `slack_notifier.py`, `evals/*`
- `mast` @ `34e932a` (v0.2.0)
- `switchboard` — `README.md`, `pkg/chat/slack/*`
- `k8s-lookout` — `README.md`, command surface

Blog post: *How we build an autonomous SRE agent for Kubernetes deployments*, langchain.com. **Where the post and the code disagree, the code is newer** — persistence, stateful monitoring, and notification policy are all listed as future work in the post and are shipped in the repo.
