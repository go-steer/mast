# mast — architecture (v0.8)

**Status:** current as of v0.8.0 (2026-09-13). This is the map of what
actually ships — the working architecture for contributors and
embedders. The *why* behind each subsystem lives in the design corpus
under [`docs/`](./docs/README.md) (start with
[`docs/positioning.md`](./docs/positioning.md) for the thesis and
[`docs/fork-design.md`](./docs/fork-design.md) for the fork mechanics);
the resolved-decisions table in [`docs/README.md`](./docs/README.md)
is the index of settled questions.

## Shape of the system

mast is a substrate for agent workloads that run **unattended**: the
same engine is consumable as a Go library and as a standalone binary,
and every subsystem assumes nobody is watching in real time —
durability, budgets, audit, and the operator surface are load-bearing,
not add-ons.

```
                    ┌────────────────────────────────────────────────┐
 envelopes ────────▶│  inject HTTP (dispatch / resume / abort,       │
 (webhooks, queues) │  pause / stop / ack-effects)                   │
                    │                                                │
 schedules ────────▶│  scheduled fires (durable anchor; a missed     │
 (bundle scheduled) │  tick is skipped, one session per fire;        │
                    │  monitor.collect gathers first, as mast)       │
                    │                                                │
 operators ────────▶│   workload bundle ──▶ root agent               │
 (mast-web, curl,   │   (.agents/ or        (coordinator / graph /   │
  attach clients)   │    programmatic)       fanout / bounded;       │
                    │                        specialists as tools)   │
                    │                                                │
                    │   write gate ──▶ operator verdict              │
                    │   (a mutating call parks; approve / reject /   │
                    │    edit, durable across a restart)             │
                    │                                                │
                    │   ADK v2 runner ──▶ session store (SQLite/     │
                    │   (span tree,        Postgres via ADK          │
                    │    HITL pause)       session/database)         │
                    │                        └─ eventlog overlay     │
                    │                           (seq + Watch/Since)  │
                    │                                                │
                    │   attach HTTP/SSE · A2A · AG-UI (list, tail,   │
                    │   inject, wake, interrupt, guardrail reset)    │
                    └────────────────────────────────────────────────┘
```

The substrate is **ADK v2** (`google.golang.org/adk/v2`): mast uses its
agent modes (Task / SingleTurn / Chat), workflow graph engine, runner,
unified span tree, and `session/database` persistence directly rather
than wrapping them. [`docs/adk-v2-usage.md`](./docs/adk-v2-usage.md)
records the verified substrate behavior mast relies on;
[`docs/spike-findings.md`](./docs/spike-findings.md) records the
resume contract and allowlist semantics that are verified behavior,
not suggestions.

## Consumer shapes

Two first-class shapes. They share the **governance layer** — the agent
loop, dispatch shapes, the write gate, the effect outbox, the budget meter
and its call gate, the behavioral watchdog, and the session event log — and
they differ in what surrounds a turn: everything that *starts* one without a
caller (schedules, the monitoring cycle, notify, auto-resume, timed pauses,
drain) and every operator listener is `package main` under `cmd/mast`, not
`pkg/`. "Same subsystems" is the claim for the governance half and was
overstated for the rest until 2026-09-11
([#288](https://github.com/go-steer/mast/issues/288)); an embedder's host
already owns the trigger. See
[`docs/library-api-design.md`](./docs/library-api-design.md):

- **Library.** The root package: `mast.Run` (instruction + input),
  `mast.RunWorkload` (programmatic bundle + specialists),
  `mast.ListSessions` / `mast.ResumeSession` / `mast.ResumeByToken`
  (operator surface), `mast.Pause` and `mast.AckEffects` (durability
  controls — the programmatic pause and the ambiguous-effect ack). A
  CI-enforced **slim-embed guarantee** (reference consumer
  `examples/deploy/slim` + denylist script) keeps the minimal import
  path free of heavyweight deps — pay for what you import.

  Two seams inside the shared half are narrower here than in the daemon,
  and both fail **closed**: `compose.WriteGateConfig`'s `ToolSchemas` and
  `ToolRead` are daemon-supplied, so a library embed's producer contract
  refuses a proposed change rather than validating it against a wired
  tool's input schema, and a change set that declares a freshness
  precondition mints no grant — its calls park one at a time instead. A
  bundle is also what turns the gate on at all: `mast.Run` has no bundle,
  no workload policy and no resume surface, so it registers no gate
  (parking a call in a process with no way to un-park it is a hang, not a
  safety property).
- **Binary.** `cmd/mast`: serve mode (workload daemon with inject +
  attach + A2A + AG-UI + metrics listeners), one-shot mode, and the
  `mast sessions` / `mast stop` operator CLIs. Serve and one-shot are
  the *same* invocation distinguished by a positional prompt — `mast
  --workload=<name> …` serves, `mast --task=<class> "<prompt>"` runs
  one turn and exits. There is no `serve` subcommand; only `sessions`
  and `stop` are subcommands (`cmd/mast/main.go`).

Nothing here is under a semver promise yet — mast is pre-1.0, and
dropping API stability promises is what restarting at v0.1.0 bought.
Six import paths plus the CLI acquire one at v1.0; see
[The v1.0 stability promise](#the-v10-stability-promise) below for the
list, for what is deliberately outside it, and for what the number does
not claim.

## Package map

**Dispatch + orchestration** —
[`docs/workflow-scaffolding-design.md`](./docs/workflow-scaffolding-design.md),
[`docs/orchestration-design.md`](./docs/orchestration-design.md)

| Package | Role |
|---|---|
| `pkg/agent` | Agent-mode constructors over ADK (coordinator, Task, SingleTurn) + per-mode default instructions; echo/scripted fake models for offline smoke. |
| `pkg/graph` | Workflow-graph dispatch (LLM-as-router over ADK's workflow engine) and the `fanout` shape — concurrent read-only branches on `parallelagent` (never `ParallelWorker`, whose branch events the log suppresses) with one `_synthesis` merge. |
| `pkg/router` | LLM-as-router classifier (SingleTurn) used by graph dispatch. |
| `pkg/specialists` | Subagent-as-tool: `.specialist.md` files (YAML frontmatter plus a prose body — not Go templates; renamed from `.tmpl` in v0.8 (#292), which stopped loading in v0.9 (#349)) with budgets, model overrides, tool allowlists ([`docs/specialists-design.md`](./docs/specialists-design.md)). |
| `pkg/workload` | Workload bundles: declarative YAML naming specialists, tool catalog, budgets, HITL policy. |
| `pkg/monitor` | The run-to-run classification a monitoring cycle carries: parses the record stream a bundle's `monitor.transitions_from` key names (logfmt or flat JSON, one record per line, mandatory `scanned=/findings=` summary) into a `monitor.Set` the scheduled envelope ships whole. Domain-neutral by construction — no enum of transition classes, no severity comparison, no fingerprinting; the classifier's verdict is consumed verbatim. Also the two argument names an operator's acknowledgement is forwarded under (`subject_key`, `ack_by`) — constants rather than strings in `cmd/`, because the loader refuses a bundle that pins either and both ends must mean the same thing by them. |
| `pkg/notify` | The chat egress a monitoring cycle speaks through: a dependency-free client for switchboard's `POST /v1/messages` ingress and its edit/append verbs, with the two non-error answers to an append (409 "send the full text", 200 with a continuation ref) modelled as sentinels a caller acts on rather than as faults. Knows nothing about monitoring — the timeline policy lives in `cmd/mast/notify.go`. |
| `pkg/planner` | Supervisor-body planner scaffold (`plan`/`finish_plan`; `invoke_remote_agent` composes here). |
| `pkg/envelope` | Inject payloads — the unattended entry-point contract. |
| `pkg/config` | `.agents/` discovery (workloads, specialists, MCP refs, A2A registrations) ([`docs/config-layout-design.md`](./docs/config-layout-design.md)). Since [#289](https://github.com/go-steer/mast/issues/289) it also computes the **config identity** — a digest over exactly the files the loaders read — which the daemon logs at startup and re-hashes once a minute, warning once per edit when the mount stops matching what is running. It never reloads. |

**Durability + governance** —
[`docs/durable-execution-design.md`](./docs/durable-execution-design.md)

| Package | Role |
|---|---|
| `pkg/transcript` | Operator surface over the ADK session store: list/show summaries, pending-interrupt scan, durable abort/pause markers, resume-token records, and the durable decision records (`approve`/`reject`/`edit`) that `mast sessions export-decisions` writes out as JSONL. (Named `session` pre-v0.1.0; renamed to end the alias collision with ADK's `session`.) |
| `pkg/eventlog` | Seq-overlay + `Since`/`Watch` stream + audit metadata sidecar layered **on** ADK `session/database` (ADK owns the tables), plus two mast-owned append-only logs folded forward across restarts: `GuardrailStore` (trips and resets, so an `enforce` halt outlives the process that observed it) and `SpendStore` (one row per priced model call, so a cost ceiling does too). Ported from core-agent. |
| `pkg/budget` | Turn/cost metering folded from event usage; trips cancel the run context. Metering stays in memory and database-free; durability is a two-part seam — `Config.OnSpend` writes each priced call out, `Meter.Restore` folds a previous process's spend back in — which `cmd/mast` wires to `eventlog.SpendStore`. Token buckets come from the ADK usage record, overridden per bucket by the provider sidecar when the adapter attached one (`budget.Detailer`, #352); an over-reported count is fitted to the room the prompt leaves rather than credited. |
| `pkg/effects` | The recorded-effect outbox: the session event log **is** the outbox (durable `FunctionCall` = intent, paired `FunctionResponse` = completion), read once per turn in an ADK runner plugin's `BeforeRun`. A dangling mutating intent puts the turn in fail-closed ambiguous-effect mode until an operator acks. |
| `pkg/approval` | The write gate: the runner-plugin seam where a mutating call parks for an operator, the three-valued verdict (`approve`/`reject`/`edit`), the typed change-set producer, and exact-`(tool, arguments)`-signature grants with their freshness re-read. Policy stays in `pkg/permissions`; the durable pause is ADK's tool-confirmation flow. Since [#296](https://github.com/go-steer/mast/issues/296) it also records what a change overwrote: `capture.go` takes a declared prior-state read before the call runs, on all four paths to execution, and writes the old values plus a proposed revert onto the event log. mast never fires the revert. |
| `pkg/permissions` | Permission gate + prompt contract (ported). Runtime-wired since v0.3 through `pkg/approval`'s plugin — it decides policy (proceed / ask / refuse) and stays ADK-independent. |
| `pkg/auth` | Caller identity, session ACL types, bearer/mTLS config (ported). Approvals and edits are recorded against the authenticated approver it resolves. |
| `pkg/watchdog` | Loop signals (repeated call, alternating cycle, tool-failure streak) + session-event bridge + the `warn`/`feedback`/`enforce` posture ladder; alerts are logged, projected onto the guardrail surface, and — from `feedback` up — routed into the model's own next prompt. The posture resolves `--watchdog` > the bundle's `safety.watchdog` > `watchdog.DefaultMode` (`feedback`), and every turn-driving surface taps it, the library embed included. Under `--attach-listen` a halt is persisted through `eventlog.GuardrailStore` and adopted on the next turn after a restart — configuration still wins, so a posture dialed back below `enforce` inherits nothing. |

**Providers** — reshaped at port time (per-provider Options structs,
no registry; dispatch is an explicit switch in `internal/compose`)

| Package | Role |
|---|---|
| `pkg/providers/gemini` | Built-in-tool wrapper (search grounding, URL context), Vertex context-cache stamping, per-request built-in gating for models that reject mixed tools. |
| `pkg/providers/anthropic` | First-party + Vertex backends; thinking-block round-trip, prompt-cache usage fold, draft-2020-12 schema normalization. |
| `pkg/providers/vertexcache` | Vertex context-cache manager (public so compose and embedders can wire hooks). |
| `pkg/providers/mock` | Scripted JSONL replay for tests and offline demos. |
| `pkg/providers/usage` | The normalized usage record adapters attach beside ADK's, under `budget.DetailKey`. Exists because mast reads usage through `genai.GenerateContentResponseUsageMetadata`, which has a cache-*read* bucket and no cache-*write* one — so Anthropic's `cache_creation_input_tokens` was folded into fresh input and billed at 1x instead of 1.25x for eight releases ([#352](https://github.com/go-steer/mast/issues/352)). Every count is a pointer: nil is "the provider did not say", which is not zero. |
| `pkg/taskclass` / `pkg/modeltier` / `pkg/pricing` | Task-class profiles → model-tier defaults → catalog pricing for the budget meter. |
| `pkg/instruction` | Instruction assembly. |
| `pkg/digest` | Tool-result digesting — the structural / agentic / passthrough router, its retrieval store, and per-method telemetry (eventlog-store variant descoped at port). Driven by `pkg/mcp`'s digest wrap since [#221](https://github.com/go-steer/mast/issues/221) (on by default, `--mcp-digest=false` to disable), which is what populates `attach.UsageInfo.DigestMethods` and the `latency_ms` / `savings` tool-result sidecars (both ride a digested response; a response the wrap hands back undigested is byte-identical to the tool's own, because `pkg/approval` compares two reads of the same tool for equality). mast's caller is **structural-only**: it passes no `LLMFallback`, so the agentic path runs only for an embedder that supplies one and `Savings.Subagent*` stay zero on the daemon. |

**Operator + interop surfaces**

| Package | Role |
|---|---|
| `pkg/attach` | The mast-native operator transport (HTTP/SSE): session registry + resume gating, seq'd replay + live tail, inject/wake/interrupt, capabilities frames, agent card, prompt broker, peer registry (optionally durable across hub restarts), rate limiting, and the guardrail surface (`GET`/`POST /sessions/{id}/guardrails[/reset]`) an `enforce` halt is cleared through. Ported from core-agent; wire-compatible with it (mast-web serves both). |
| `pkg/attachadapter` | Bridges the runner-driven daemon into attach's `Registrant` contract: one injected message = one serialized turn; typed operator events in wire order; interrupt cancels the turn context. |
| `pkg/inject` | The unattended entry point: `POST /inject`, `/resume`, `/abort`, `/pause`, `/extend-token`, `/stop`, `/ack-effects`, plus `/metrics`. |
| `pkg/observability` | Fixed Prometheus counter registry + env-gated OTel trace export ([`docs/observability-design.md`](./docs/observability-design.md)). |
| `pkg/a2a` / `pkg/federation` | A2A v0.3 both ways: the synchronous client, and the server (agent card, `message/send`·`tasks/get`·`tasks/cancel`·`message/stream` over SSE) exposing workloads that opt in via the bundle's `a2a.expose`. Plus the `federation.Adapter`/`Handle` interface + `invoke_remote_agent` (called "frozen" here since v0.1 in the sense that its shape is settled — **not** a v1.0 commitment; `pkg/federation` is on the uncovered list below, and [`docs/compatibility-policy.md`](./docs/compatibility-policy.md) does not bind it) ([`docs/a2a-design.md`](./docs/a2a-design.md), [`docs/federation-design.md`](./docs/federation-design.md)). |
| `pkg/agui` | The AG-UI server surface (agent↔user) for CopilotKit apps and chat-platform bots: hand-rolled zero-dep wire types, an HTTP+SSE run endpoint, `/agui/agents.json` discovery, and the HITL interrupt/resume lifecycle. Runtime-free, like `pkg/a2a` ([`docs/ag-ui-design.md`](./docs/ag-ui-design.md)). |
| `pkg/serverauth` | The request-admission seams both network servers share: pluggable bearer auth (`TokenValidator` → `Principal`, per-surface scope checks) and rate limiting. Stdlib + `golang.org/x/time` only, so it stays slim-embed-safe. |
| `pkg/mcp` | MCP toolset wiring + per-specialist tool allowlists. HTTP servers get their transport wrapped so a 4xx/5xx carries the server's own error text (an IAM permission name, a quota metric) rather than a bare status line. Every toolset is also wrapped for response digesting (`WithDigest`, `retrieve_raw`) unless the daemon or the server opted out; the wrap exposes `Unwrap()` so mast's own non-model caller — the write gate's precondition read — reaches the tool rather than a digest of it. |

**Internal:** `internal/compose` (model/backend dispatch, shared
one-shot construction, the `bounded` single-node build),
`internal/evals` (the deterministic eval suite and the judged
nightly's scoring, including the tiered-cost check; the judged tier
retries a provider's `429`/`503` at the `model.LLM` seam so a quota
blip costs a wait rather than a corpus row, and counts what it
retried onto the board — plus `internal/evals/outcome`, the **O tier**:
corpus loader, fixture provisioner, verifiers, board and runner for a
real model driven against a real `kind` cluster, the only tier that
gates ([`docs/outcome-evals-design.md`](./docs/outcome-evals-design.md))),
`internal/version` (ldflags-injected build identity, reported by
`--version` and the attach capabilities frame), `internal/toolcatalog`
(the tool declarations a real turn puts in front of a model, captured
by driving two agent rigs through an ADK runner, plus the shared
invariant every provider adapter's wire test is held to — see
`docs/model-support-design.md` R2/R8). The scheduled-trigger
loop is daemon-side in `cmd/mast/schedtrigger.go`, reading the
bundle's `scheduled:` section; a cycle's collection leg
(`cmd/mast/monitor.go`, `cmd/mast/monitorctx.go`) runs ahead of it,
off the bundle's `monitor.collect` block, and parses the one result
named by `monitor.transitions_from` through `pkg/monitor` before the
envelope is built. The cycle's tail — whether to wake the model at
all, and what to tell the chat — is `cmd/mast/notify.go` over
`pkg/notify`, configured by the bundle's `monitor.notify` block and
the daemon's `--notify-url` / `MAST_NOTIFY_TOKEN`. The one leg that
runs the other way is `cmd/mast/monitorack.go`, off the bundle's
`monitor.ack` block: an operator's acknowledgement arrives on the
daemon's `POST /monitor-ack`, is attributed from the credential that
carried it, recorded durably by `pkg/transcript`, and forwarded to the
producer's own ack tool. It is not on the cadence — an ack arrives
when somebody reads their chat.

## Key contracts worth knowing before changing anything

- **Sessions are event logs.** State derives from the append-only
  event history (ADK reconstructs run state from it every turn), which
  is why restart-survival is free once the store is durable, and why
  the eventlog overlay (seq + watch) is the audit/tail surface rather
  than a second store.
- **One session service instance per store.** ADK's `AppendEvent`
  type-asserts its own session type — every writer must go through the
  same service instance (the daemon owns it; the abort path routes
  through the daemon for exactly this reason).
- **Budgets act by cancellation.** The meter folds usage from the
  event stream and trips by canceling the run context — subsystems
  must tolerate mid-turn cancellation.
- **Two runner plugins bracket every tool call, in this order.**
  `pkg/effects` (the outbox) registers first, `pkg/approval` (the
  write gate) second, so a call replayed after a crash is answered
  from the outbox instead of asking an operator to re-approve a
  mutation that already fired. Reordering them is a correctness bug,
  not a preference.
- **Those plugins are runner-scoped, and a planner dispatch builds its
  own runner.** `invoke_specialist` constructs a runner in the tool
  body (`pkg/planner/dispatch.go`) with no `PluginConfig`, so neither
  the outbox nor the write gate reaches a mutating call made inside a
  dispatch. Accounting does reach it — the sub-run's spend and
  watchdog signal travel out-of-band through `SubRunSink`, which is the
  standing proof that a host-owned concern *can* cross this boundary
  without being a plugin. So
  `compose.CheckPlannerWriteSurface` refuses any planner roster holding
  a `change_executor` while `hitl.on_mutation` asks for the write to be
  gated; `apply` is exempt, because there was no gate to bypass.
- **That refusal is the design for the gate; the outbox half is
  closed.** The two halves came apart under measurement (2026-08-31,
  [#235](https://github.com/go-steer/mast/issues/235)). The **gate**
  cannot cross and no wiring makes it: a park writes its question into
  the session event log and returns normally — the turn is not
  suspended — and a resume matches that log and re-enters at the *root*.
  A dispatch sub-session is in-memory, dies with the tool call, and
  nothing re-enters a dispatch mid-flight. Coordinator and graph
  dispatch get the gate at full per-call fidelity *because* they share
  the log (`pkg/approval/dispatchseam_test.go`). Planner dispatch exists
  to keep a private session, and a private session is what puts a resume
  round trip out of reach; gating `invoke_specialist` itself instead
  would have the operator approve a specialist name and a prose string,
  which is the behaviour lead row **L7** was written to beat. The
  **outbox** half has no such obstacle, because recording is
  one-directional: `SubRunSink` sees a mutating `FunctionCall` before
  the tool body runs (`pkg/planner/outboxseam_test.go`). What it cannot
  be is a durable sub-session — `Store.ScanInterrupted` lists one
  `AppName` and the sub-runner uses `"planner_dispatch"`
  (`pkg/transcript/dispatchscope_test.go`) — so the record belongs in
  the outer session, and as of 2026-09-01 it is there
  (`pkg/effects/subrun.go`). So the refusal is not waiting on anything:
  what it names is what an operator does. Its message offers
  `coordinator`, `graph`, and `on_mutation: apply`, and each of the
  three is composed in `internal/compose/plannerwrite_test.go` — a way
  out that nobody built is worse than none, because the operator spends
  their next hour on it.
- **mast calls a tool nobody asked for in exactly three places, and
  each has its own fence.** `cmd/mast/toolschemas.go`'s `runOwnBehalf`
  is the whole surface: the write gate's precondition read, a
  monitoring cycle's `monitor.collect` leg, and the `monitor.ack`
  forward. The read is fenced by *classification* — compose refuses to
  start if the declared read is mutating, so that exception can only
  widen towards safer calls. Prior-state capture has two consumers on
  that one caller rather than a fourth caller of its own, because it
  wants the same thing under the same fence: a non-mutating read of the
  target, run by mast, at the gate, before the call. The collection leg inverts that (it
  permits a mutating call precisely because it is mast's own and would
  otherwise park every fire), so it is fenced by *reachability*:
  `compose.CheckMonitorCollectSurface` refuses to start if a tool mast
  runs on its own behalf is reachable from any roster. The ack forward
  sits behind that same reachability fence — one rule over one list,
  because which direction a self-run call goes is why the exception
  exists, not what bounds it — and behind a second fence of its own:
  *arrival*. Nothing but the authenticated `/monitor-ack` route can
  reach it, and it is the one self-run call whose arguments mast
  overwrites rather than passes through. A fourth caller needs a
  fourth fence, not a fourth call site.
- **What changed since last run is the classifier's answer, not
  mast's.** A cycle may check that a transition record is *well
  formed* and may not check that it is *right*: a record with no
  `subject_key` is malformed (nothing downstream can ack or
  de-duplicate a subject it cannot name), an unrecognized transition
  class is simply a class mast has not seen. The one integrity lever
  is the stream's trailing `scanned=/findings=` summary — without it,
  or with a `findings=` count that disagrees with the records parsed,
  the result is void rather than quiet, because a truncated answer and
  "nothing changed" must not read the same. Adding a severity ladder,
  a fingerprint, or a local heuristic here re-implements
  [`k8s-lookout`](https://github.com/go-steer/k8s-lookout) inside mast
  and is the bug this contract exists to prevent.
- **A cycle with nothing to report does not wake the model, and a
  failed report is never replayed.** The skip is decided before the
  turn runs (`cmd/mast/notify.go`'s `decide`) and only where both
  `monitor.transitions_from` and `monitor.notify` are declared — so
  "nothing changed" is always the classifier's answer, never mast's
  guess. The no-replay half is the ordering constraint the whole M4b
  chain was built around: state advances during collection, so a
  message that failed to send describes a world that has already moved
  on. There is no queue and no spool; the failure is an errored fire
  and `mast_monitor_notifications_total{outcome="error"}`. Anything
  added here that holds an assessment for the next cycle re-opens the
  problem. Silence is bounded by wall clock (`digest_after`), never by
  a count of quiet cycles, and the daemon's own broken/recovered
  notices are edge-triggered so an operator does not learn to mute the
  channel.
- **An ack is not an approval.** They share an operator, a chat window
  and a verb, and nothing else: an approval mints a grant that
  licenses a write and is consumed on use, while an ack asserts no
  diagnosis and authorizes no change. `cmd/mast/monitorack.go` touches
  neither `pkg/permissions` nor `pkg/approval` and writes no decision
  record — if it did, the v0.3 answer to "who approved this change"
  would start including people who muted an alert. The split that
  falls out: **the producer is the store of record for the
  suppression** (how long it lasts, whether a repeat was redundant —
  mast holds no window and forwards a repeat regardless), and **mast
  is the store of record for who asked**, which is the half nobody can
  reconstruct afterwards, because a producer's ack surface takes an
  `ack_by` string from whoever calls it and cannot check it. So
  attribution comes from the credential and never from the body: a
  request carrying `ack_by` is refused by name rather than ignored,
  since silently dropping it produces an audit line naming the wrong
  person with nothing anywhere saying so.
- **Unknown tools count as mutating.** The mutation predicate is
  default-deny: a tool nothing has classified is gated, so a bundle
  cannot get write access by omission. Un-gating is an audited
  per-tool `tool_catalog.tools[].mutating` override.
- **A provider's server-side built-ins are gated at construction,
  because they are the one capability that never becomes a tool
  call.** Gemini's `google_search` / `url_context` / `code_execution`
  and Anthropic's `web_search` run inside the vendor's infrastructure
  and arrive folded into the response, so the permissions gate has no
  name to allow, the write gate no call to park, and the outbox
  nothing to record — a `read_only` specialist could read the public
  internet with nothing downstream saying so
  ([#324](https://github.com/go-steer/mast/issues/324)). The only gate
  is the bundle's `builtin_tools:` block, read once when the model is
  built, and **mast's baseline is off on every provider** rather than
  each vendor's own: Gemini ships search on and Anthropic ships it
  off, so inheriting them would make an unattended agent's reach a
  function of `--provider`. The corollary is the point — the paths
  with no bundle (`mast run`, `mast.Run`, the eval rigs) are safe by
  construction, not by remembering a key. `pkg/providers/gemini`'s own
  `DefaultBuiltinTools()` is still on and is a recommendation to a
  library caller wrapping a Gemini model directly; `internal/compose`
  does not pass it. The gate is per bundle: a specialist's `model:`
  override resolves through it, and there is no per-specialist axis to
  hand a tool back with.
- **Nothing mutating runs in a parallel branch.** Fan-out branches all
  run *before* the single post-synthesis approval gate, and a branch's
  `Output` payload is its only durable record — so mutating tools (and
  `request_operator_input`) are refused in a branch's allowlist at
  construction.
- **Attach is wire-compatible with core-agent.** The protocol shape is
  the contract; mast-web and any attach client work against both.
  Divergence in *shape* is a bug on whichever side left the documented
  form. The version *numbers* are a different matter and have already
  diverged — mast is on v1.6.0, core-agent on v1.8.0, and the same
  number does not name the same feature set on both. Feature-detect
  against the capabilities frame's `event_types` / `features`, never
  against the version alone.
- **Ports carry provenance.** Adapter packages derive from
  core-agent at per-stage pinned SHAs (`83ec0713` / `b8dd225e` /
  `25d8531c`), one derivation header per file. Shared-infrastructure
  fixes land wherever found first, then port within a week
  ([`docs/fork-design.md`](./docs/fork-design.md) sync discipline).

## The v1.0 stability promise

Versioning restarted at v0.1.0 to signal "new project, not a
continuation," and the thing it dropped was API stability promises
([`docs/fork-design.md`](./docs/fork-design.md)). Read backwards, that
is the definition: **v1.0 is the release where mast makes them again.**
Written here before it is arrived at by accident
([#300](https://github.com/go-steer/mast/issues/300)).

**What the promise covers.** Six import paths follow semver from v1.0:

| Path | Why it is in |
|---|---|
| `github.com/go-steer/mast` | The library pillar's front door: `Run`, `RunWorkload`, `ListSessions`, `ResumeSession`, `ResumeByToken`, `Pause`, `AckEffects`. |
| `github.com/go-steer/mast/pkg/agent` | Agent-mode constructors and `Config` — what an embedding host builds a loop out of. |
| `github.com/go-steer/mast/pkg/transcript` | The operator projection over sessions; the durable pillar's read surface. |
| `github.com/go-steer/mast/pkg/workload` | Bundle types. The operator contract has a Go form and a YAML form; both are promised. |
| `github.com/go-steer/mast/pkg/specialists` | Spec + registry + loader — the authoring model the slim embed imports. |
| `github.com/go-steer/mast/pkg/budget` | Limits and the meter. An unattended workload's ceilings are part of its contract, not an implementation detail. |

The set is the pillar-serving one from
[`docs/library-api-design.md`](./docs/library-api-design.md), corrected
against the tree: that table's five included `provider` and `tool`,
and **neither package exists** — there has never been a `pkg/tool`, and
`pkg/providers` is a directory of four backends with no interface above
them. The provider extension point is real but it is a *field*, not a
package (below). `agent`, `specialists`, `workload` and `budget` are
what `examples/deploy/slim` actually imports, which is the only
evidence available that a surface has been exercised by a consumer.

**What it does not cover.** The other 32 importable packages under
`pkg/`, named rather than left to omission: `a2a`, `agui`, `approval`,
`attach`, `attachadapter`, `auth`, `config`, `digest`, `effects`,
`envelope`, `eventlog`, `federation`, `graph`, `inject`, `instruction`,
`mcp`, `modeltier`, `monitor`, `notify`, `observability`,
`permissions`, `planner`, `pricing`, `providers/anthropic`,
`providers/gemini`, `providers/mock`, `providers/usage`,
`providers/vertexcache`, `router`, `serverauth`, `taskclass`,
`watchdog`. They are importable, they are not supported, and a minor
release may break them. Shrinking that list — by demotion to
`internal/` where nothing outside the module needs the symbol — is
[#301](https://github.com/go-steer/mast/issues/301); the promise does
not wait on it, because "unsupported" is a statement mast can make
today and "unreachable" is work.

*(Corrected 2026-09-14 with the rest of
[#338](https://github.com/go-steer/mast/issues/338). This list said
"27" over 28 names, and one of the 28 was `providers` — which is a
directory, not a package: it holds no `.go` files, so it cannot be
imported, while the five real packages beneath it were named by
nothing. Exactly the failure #300 found in the promise it replaced,
committed while writing the replacement. `go list ./pkg/...` returns
37; five are covered above.)*

*(The 2026-07-25 rule said the unpromised packages would each carry an
`// Experimental:` marker. Seven releases later there are **zero** in
the tree. A marker nobody writes is not a boundary — this list is, and
it lives in one file that a reviewer can diff.)*

**The two lists were not a partition, and reconciling them was
[#338](https://github.com/go-steer/mast/issues/338)** — task 1 in v0.9
as [#339](https://github.com/go-steer/mast/pull/339), task 2 below. A covered package
whose exported signature names a type from the unsupported 27 has
committed that type as well, whatever this section says. Two do:
`pkg/budget` named `pricing.Catalog`, and `pkg/transcript` returns three
`approval` records. They get opposite remedies, because they are
opposite kinds of leak.

`Limits.Catalog` was an *input* — a consumer setting it had to build one,
which committed `NewCatalog`, `Options`' three config-discovery fields,
`ModelRates` and `Rates`: roughly fourteen declarations, none of them
about budgets, and the one thing among them the meter actually used is
the one `docs/model-support-design.md` M2 already owes a change to
(`LookupFor` re-keyed from the backend name onto a provider profile).
Freezing that would make the third backend a major. So the meter owns a
one-method `budget.Pricer` instead, taking a `budget.Call` struct so the
usage buckets §4.3 wants next arrive as fields rather than as a new
signature; `internal/compose` holds the only adapter, and `pkg/budget`
now imports nothing else from this module.

That bet paid out one release later. The usage sidecar (#352) needed the
meter to read cache-write counts that only a provider adapter can
produce, and `CacheWriteTokens` went into `Call` as a field, with the
read side a second budget-owned interface — `Detailer`, returning a
budget-owned `Buckets` — so `pkg/providers/usage` names `pkg/budget` and
never the reverse. A test in the package now parses its own imports and
fails on any that names this module, because the property is the point
and a compiler will not notice it going away.

The `approval` records are *outputs*. There is no constructor to
drag in, and their field set is already committed as
`approval.DecisionSchema = "mast.decision/v1"` — so a `pkg/transcript`
copy would be a second Go spelling of one JSON schema, which is the
drift the v0.5 wire-literal pin exists to prevent. The rule
generalizes: **freeze by reference when the type is an output whose
shape is already committed on the wire; own a narrow interface when it
is an input the consumer must construct, or a dependency already
scheduled to change.**

**So these eight `approval` declarations are covered by reference
through `pkg/transcript`, and the rest of `pkg/approval` is not:**

| Reached from | Covered by reference |
|---|---|
| `Detail.AppliedEdits` | `approval.AppliedEdit` |
| `Detail.Captures` | `approval.CaptureRecord` |
| `(*Store).Decisions` | `approval.Decision` |
| `Decision`'s own fields | `approval.Outcome`, `approval.Scope`, `approval.Authority`, `approval.Disposition` — with their constants, since an enum whose values may change is not frozen |
| `CaptureRecord.Revert` | `approval.ProposedChange` |

`ProposedChange` is the correction #338's own table needed: it lists
three records, and following the fields finds a fourth type behind
`CaptureRecord.Revert`. The answer is still by-reference — a revert an
operator can be handed has to be the same shape as a change they can
approve ([#296](https://github.com/go-steer/mast/issues/296)), and
`revert` is already a field of the `mast.decision/v1` capture on the
wire — but it was reached by resolving the closure rather than by
reading the issue, which is the habit #300 was written to install.

**The closure is a test, not only a paragraph.**
`pkg/transcript/freeze_test.go` parses this package's exported
declarations, collects every in-module qualified type reachable through
one, closes over those types' exported fields to a fixed point, and
fails against a checked-in list. Adding a field of an unsupported type
to an exported struct here otherwise compiles, passes every behavioural
test, and silently enlarges what v1.0 promises. The test was verified
to detect in both directions — dropping an entry, and adding a leak —
rather than only observed to pass.

Nothing about `pkg/transcript`'s code changed here, and nothing needed
to. Task 2 of #338 is a statement of what the freeze already commits.

**The two ADK types in the promised surface.** `mast.Config` exposes
`Model model.LLM` and `Sessions adksession.Service` — deliberate
injection points, and the only ADK types in the root package's public
API. Under Go's semantic import versioning `adk/v3/model.LLM` is a
different type from `adk/v2/model.LLM`, so **an ADK major bump breaks
mast's public API and therefore ships as a mast major.** That is
mechanical, not a policy choice; the policy part is what mast does
about it: such a release carries *no other* breaking changes, so the
migration is an import path and nothing else. Wrapping these behind
mast-owned interfaces was considered and refused — `LLM.GenerateContent`
takes `*model.LLMRequest` and returns `*model.LLMResponse`, so a
mast-owned interface would only move the leak into its own method
signature unless mast also owned a request/response model and
translated both ways forever, on the hottest path in the system.

**Wire contracts freeze, on their own clock.** attach, A2A, AG-UI and
inject are consumed by repos whose compiler cannot see this one; v0.5
pinned their literals as tests for that reason, and v1.0 makes them a
promise. They version by their own protocol fields — the attach
capabilities frame, the agent card — **not** by mast's major, so a
protocol addition does not force a Go major and a Go major does not
invalidate a client that speaks the old frame. Feature-detect, as the
contracts section above already says.

**The bundle schema versions independently.** `workload.yaml` is edited
by operators who never import Go. Coupling it to the Go major would
mean a new YAML key forces a mast v2. So it carries its own
`schema_version:`, currently `1`, on its own clock
([#302](https://github.com/go-steer/mast/issues/302)). Absent means 1 —
every bundle written before the key existed keeps loading unchanged —
and a bundle declaring a version this binary does not speak is refused
by name rather than partially read. **A version bump is for a key that
changes shape or meaning, not for a key being added:** adding
`builtin_tools:` did not bump it, because an older mast reading a newer
bundle is the case the version exists to catch, and additive keys are
already caught. Unrecognised keys are a load error, not a warning —
this is the one file that declares which tools may run without an
operator, so a block that silently does nothing is a policy that
silently does not apply. Specialist frontmatter takes **no** version of
its own; it is reached only through a bundle that names it, so the
bundle's version governs the roster.

**The CLI is part of the promise.** The binary is a first-class
consumer shape, and its callers — shell scripts, systemd units,
Kubernetes manifests — are exactly the ones a Go compiler cannot warn.
Promised: flag names and their meanings, the `sessions` and `stop`
subcommands and their verbs, and the exit codes (`0` ok, `1` the work
failed, `2` the invocation was rejected, `3` serve mode's drain expired
with sessions still interrupted). Not promised: log lines, stdout
prose, `--help` wording, and metric names, which have their own gate.
The surface is pinned in `cmd/mast/testdata/cli-surface.txt` and
enforced by `TestCLISurface`; before that file existed, a flag rename
passed every test in the tree.

**What v1.0 is not.** It is not a claim of production readiness. It
says the API stops moving and nothing else. The evidence mast does have
is the outcome tier gating every release, the per-version UAT suites, a
measured RBAC matrix on live GKE, and — since 2026-09-14 — a written
threat model ([`docs/threat-model.md`](./docs/threat-model.md),
[#305](https://github.com/go-steer/mast/issues/305)). What that document
is *not* is an external security review; mast has had none, and the
threat model says so in its own second paragraph.

**How a covered thing is allowed to change** is the other half of the
promise, and it is
[`docs/compatibility-policy.md`](./docs/compatibility-policy.md)
([#304](https://github.com/go-steer/mast/issues/304), settled
2026-09-14 — a gate on the tag, not a follow-up to it). The load-bearing
parts: a covered symbol is deprecated in a minor and removed in a
major, after **two released minors and 90 days** — both floors, because
mast's fastest minor-to-minor gap is three days and a count nobody is
awake for is not a cycle. A mast major triggered by an **ADK** major
removes nothing, because "your migration is an import path and nothing
else" is what makes that release cheap. The support window is the
current release; mast does not backport, which is a description of
eight releases of practice before it is a policy. There is **no
experimental tier** and there will not be one — the covered list above
is the mechanism, since a reader can diff a list and cannot grep for a
marker nobody wrote. And the `Deprecated:` marker must name its removal
version, enforced by `deprecation_test.go` at the module root rather
than asked for in prose; its first run deleted mast's only marker, an
inherited one with no end date in an unsupported package.

## Deliberately not in v0.7

Deferrals are decisions ([`AGENTS.md`](./AGENTS.md) house rule #7);
the owning doc names the version that lifts each one, and the
[roadmap](https://go-steer.github.io/mast/roadmap/) is the
user-facing view.

Unattended monitoring's four legs shipped in v0.5 as `monitor.collect`,
`monitor.transitions_from`, `monitor.notify` and `monitor.ack`, which
*consume* [`k8s-lookout`](https://github.com/go-steer/k8s-lookout)'s
classification and switchboard's ingress rather than growing either
here. Two things around them stay out on purpose. An **ack window** is
deferred permanently rather than to a version, because the expiry
belongs to the producer and a clock kept here would be a second
suppression state to disagree with. The **approver allowlist** was
switchboard's to write and it wrote it (parity row 17, green
2026-09-03): a press is refused against a per-channel list of asserted
callers before the prompt is even located.

The **in-chat Approve/Reject surface** was recorded here as
switchboard's too, and that was wrong — corrected 2026-09-14 while
closing [#242](https://github.com/go-steer/mast/issues/242).
switchboard shipped its half; it answers `POST
/sessions/<app>/<sid>/perms/respond`, and on a mast daemon that route
is a **501**. Every `/perms` route in `pkg/attach` gates on a
capability no type in this module implements outside a test, because
`internal/compose` builds the write gate with no `Prompter` — there is
no human on stdin in an unattended daemon, so the synchronous prompt
path is never reached. That is the right posture and it is not a
defect; what is a defect is the corpus counting the resulting red row
against a sibling repo. mast's approval model is the durable
write-gate park, and making *that* answerable from a thread is
switchboard's #84 over mast's
[#313](https://github.com/go-steer/mast/issues/313). What mast does
with the dead `/perms` fork it inherited is
[#364](https://github.com/go-steer/mast/issues/364). mast's existing
side is a wire-contract test that the resume shape and
`X-Asserted-Caller` do not move underneath either of them.

Settled rather than deferred, as of 2026-08-31: **the write gate does
not reach inside a planner dispatch, and will not** — the combination
stays refused at composition, for the structural reason in the
contracts above ([#235](https://github.com/go-steer/mast/issues/235)).
This is the sharpest entry in
[`docs/threat-model.md`](./docs/threat-model.md) § 5.1, which is where
the security reading of it lives: the write gate is mast's answer to
prompt injection, so the gate's coverage boundary is a security
boundary, and that is the argument for refusing the composition rather
than documenting the hole.
The other half of that issue — the **outbox record** — shipped
2026-09-01: a per-dispatch recorder on the same observer seam that
meters a dispatch writes each mutating intent and completion to the
session's companion ops row (`pkg/effects/subrun.go`,
`pkg/transcript/subrun.go`), and the outbox and the auto-resume scan
fold them in. An interrupted dispatch now leaves a visible dangling
intent, so `apply` gives up the stop and not the record. Recording
could cross the boundary the gate cannot because it is
one-directional; the record never enters the session log the planner's
model reads, and it is not an approval.

**Pre-call budget gating** shipped 2026-09-02. `budget.Meter.Allow`
asks, before each call, whether a ceiling can still be respected;
`agent.RefuseOnGate` is the `BeforeModelCallback` that asks it and
synthesizes the agent's answer when it cannot, installed by the three
`pkg/agent` constructors and armed per turn with `agent.WithCallGate`.
`Observe` is unchanged and still the durable ledger — the pre-call
check refuses only what it can prove (`max_turns` is now exact; tokens
and cost stop *at* the cap rather than one call past it) and never
estimates the size of the next call.

The routing half shipped the same day. **A specialist that reaches its
own ceiling closes one path, not the session**: `budget.Scope` reports
whose ceiling an enforcement error was, the turn drivers stop only for
the workload's own, and the refused specialist's `finish_task` report
goes back to the coordinator as something to route around. The trip is
still counted (`mast_budget_trips_total`), logged, listed per specialist
on `GET /guardrails`, and returned to a library caller as
`mast.Result.Exhausted` — a workload that quietly loses half its roster
would otherwise return the same `nil` as one that did not.

**A change carries a route back, and mast does not take it.** Shipped
2026-09-05 ([#296](https://github.com/go-steer/mast/issues/296)):
`pkg/approval/capture.go` runs a tool's declared prior-state read before
the mutating call fires, on all four paths to execution, and writes the
old values plus a proposed revert onto the event log. **Restore stays out
by decision, not by sequencing** — mast can render the call and cannot
decide that firing it an hour into an incident is still right, so the
revert goes back through the same gate with a person answering. mast also
derives neither the read nor the inverse: both are domain knowledge, and
a workload that declares no `revert:` gets a record whose undo is marked
undeclared rather than a guess.

**Measurement gates, and it gates the release.** The **O tier** shipped
2026-09-05 ([#294](https://github.com/go-steer/mast/issues/294),
[#297](https://github.com/go-steer/mast/issues/297)): a real model against
a real workload on a real `kind` cluster, on every pull request, with a
20-minute ceiling budgeted before the roster and a check that measured
nothing reding rather than passing. The write gate's park became readable
to it the same week ([#295](https://github.com/go-steer/mast/issues/295))
— a gated call carries its question and its answer, `approval.Parked` gains
`CallID`, and the `approval_requested` check reads the **question, never
the verdict**, so a call the operator refused still passes. Settled
2026-09-06: `dev/release/require-outcome.sh` refuses to release a commit
the tier has not passed, *including* one it never ran on, and that is
**instead of** making `outcome` a required check on `main` rather than a
step toward it — requiring it would make a fork's pull request unmergeable
forever. The accepted cost is that a red can land on `main` and the
refusal arrives at the tag.

Still deferred here: the remaining AG-UI slices (`agui://`
federation client, per-key `StateDelta`, webhook push, client-declared
tools, [`docs/ag-ui-design.md`](./docs/ag-ui-design.md)); the
`run_shape_*` planner vocabulary wired to the reference-graph library
(it returns `not_implemented` in the shipped scaffold); multi-session
attach (ACL store, per-caller auth, operator session creation) and
`mode: multi_session` bundles; skills consumption
([`docs/skills-design.md`](./docs/skills-design.md)); audit-derived
memory ([`docs/memory-design.md`](./docs/memory-design.md), gated on
core-agent's shared-memory stack); OTel *metrics* export (Prometheus
scrape + OTel traces only). Providers beyond Gemini and Claude are a
proposal, not a plan —
[`docs/model-support-design.md`](./docs/model-support-design.md)
targets a later release and nothing in it is settled.
