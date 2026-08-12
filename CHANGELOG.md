# Changelog

## Unreleased

- **Deterministic evaluators** (docs/v0.3-plan.md W0.2). `internal/evals` scores
  a recorded run with no provider and no cluster: `intent_coverage` (the primary
  trajectory metric), `tool_coverage` (name-level, emitted as a diagnostic only
  so the consolidation penalty stays visible instead of scored),
  `severity_accuracy`, and the two mast-only invariants `effect_ordering` and
  `exactly_once`. `TraceFromEvents` is the single adapter onto ADK's event log;
  everything downstream is a pure function over a plain struct, so a test can
  construct a double-fired mutation or an orphaned completion directly.

  Its pairing rules mirror `pkg/effects` — same event indexing, same empty-ID
  skip, same treatment of a confirmation placeholder as *not* a completion, so a
  declined approval never scores as an executed effect. One deliberate
  divergence: long-running calls are kept rather than deferred, because a
  finished run's completed blocking tool is an effect like any other and
  dropping it would blind `exactly_once` to a re-fired mutation.

  Two guards exist specifically because upstream's equivalents are constant
  functions on this dataset: every result carries a `Vacuous` flag when it
  scores 1.0 for want of anything to measure, and `exactly_once` keys effect
  identity on tool name plus canonicalized arguments rather than call ID, which
  would make it structurally incapable of failing.

- **Parity scenario corpus and intent table** (docs/v0.3-plan.md W0.1, W0.1a).
  The 31 LangChain SRE-agent evaluation scenarios are ported to
  `testdata/evals/scenarios/langchain-sre.jsonl`, and `testdata/evals/intents.yaml`
  maps their 23 distinct tool names onto 19 diagnostic intents plus the lookout
  tools that satisfy each. `internal/evals` loads and validates both. Nothing
  is scored yet — the evaluators are W0.2 — but the corpus and the mapping the
  scoreboard will be computed from now exist and are tested.

  The mapping is at *intent* level, not tool-name level, because lookout
  consolidates: 22 of the 31 scenarios are fully answered by a single lookout
  call, and name-level set overlap would score a better-factored read path as a
  regression. Three properties of the upstream data are recorded rather than
  smoothed over: the corpus is ported from the `.jsonl` (what upstream actually
  uploads and scores) even though its `.json` sidecar is an unwired *repair* of
  it; 7 of the 23 tool names do not exist in upstream's own registry, so 16 of
  71 tool references are unsatisfiable and are annotated `unreachable_upstream`;
  and both upstream custom-code evaluators are constant functions on this
  dataset, so neither provides an adoptable baseline. See the plan's W0.1/W0.1a
  findings for the detail.

- **Per-specialist model overrides are honored** (docs/specialists-design.md
  open Q#4; docs/v0.3-plan.md W1.1). A specialist `.tmpl` has always been able
  to declare `model:`, and `pkg/specialists` has always parsed it — but `Build`
  constructed every specialist with the parent's model, so the field did
  nothing and "cheap analysts, frontier synthesis" was unreachable. `Build` now
  resolves the override through a `specialists.ModelResolver` that
  `internal/compose` supplies, dispatching on the model id exactly as `--model`
  does. **Cross-provider overrides are allowed** (a `gemini-*` specialist under
  a `claude-*` parent): the dispatch is already id-based, and refusing would
  mean maintaining a provider-family classifier as a second source of truth
  beside it. Resolution is memoized per model id, so a roster of eight analysts
  on one tier opens one client.

  Two behaviors are deliberate. A **declared override that cannot be resolved
  fails the build** rather than falling back to the parent's model — silent
  fallback is the bug being fixed, and it would let a bundle read as tiered
  while everything ran on one tier; the corollary is that credentials for every
  provider in the roster must resolve at construction. And an **offline-fake
  parent** (`--model=echo` / `scripted` / `toolactor`) collapses every override
  back to itself, so tiering a bundle cannot break the credential-free smoke
  and acceptance runs.

  Behavior change for library consumers: `specialists.Build` /`BuildAll` now
  return an error when a `Spec.Model` is set and `BuildOptions.Resolve` is nil.
  Callers going through `mast.RunWorkload` / `mast.ResumeSession` / `cmd/mast`
  get a resolver wired automatically. `internal/compose.BuildRoot` takes a
  `context.Context` first argument.

  No shipped bundle declares an override yet: naming a concrete model id binds
  a bundle to one provider, so tiering `gke-triage` waits on a
  provider-portable `tier:` field (proposed as W1.1a).

## v0.2.0 (2026-08-10)

- **AG-UI server — Stage 2: HITL interrupt/resume lifecycle**
  (docs/ag-ui-design.md "Implementation status"; #84). Turns the Stage-1
  honest-placeholder `RunError{interrupt}` into the real human-in-the-loop
  loop. A turn that parks on a HITL primitive (a `request_operator_input`-class
  long-running tool, or a programmatic / external-signal pause) now closes the
  SSE stream with a terminal `RunFinished` whose `outcome` is
  `{type: "interrupt", interrupts: [{id, message, responseSchema?, expiresAt?}]}`,
  projected from the durable session's pending-interrupt state rather than
  fabricated. The client resumes by starting a **new run** whose
  `RunAgentInput.resume` carries one entry per interrupt (`status:
  "resolved" | "cancelled"`, optional payload); the daemon reconciles each
  entry against the session's open interrupt ids, builds the resume
  function-response, and drives the resume turn through the same `runTurnPre`
  chokepoint every other turn kind uses. A resume that references no open
  interrupt (or an unknown id) is refused with `409` (`ErrNotResumable`)
  instead of silently forking a fresh turn, and a resume run may carry empty
  input — the `resume` array alone drives it. Since a resume is a new run,
  reaching the parked session under `session_model: per_run` (keyed on
  `runId`) requires the resume to carry `parentRunId`; the default
  `per_thread` reaches it via the shared `threadId`. The terminal interrupt frame
  records a new `interrupted` outcome on `mast_agui_runs_total{workload,outcome}`.
  Still hand-rolled in `pkg/agui` with zero new dependencies. Per-key state
  deltas, client-declared tools, and the `agui://` federation client remain
  follow-on stages.

- **Effect-outbox durability hardening** (docs/durable-execution-design.md
  "Recorded-effect outbox"; #71). Two follow-ups from the outbox gate review.
  **Sub-agent/tool name collision is now refused at construction (N2):** the
  dangling scan excludes every FunctionCall named after a sub-agent (task
  delegations are engine control flow, not effects), so a genuine mutating
  tool that shared a specialist's name was invisible to the outbox — a
  fail-open hole. mast now refuses to start when a composed sub-agent name
  also names a mutating- or spawning-class tool (`effects.CheckNameCollisions`,
  wired in all three construction paths — daemon, one-shot, and the library
  entrypoints), turning a silent durability gap into a clear startup error the
  operator fixes by renaming one side. A read-only tool of the same name is
  harmless and still allowed. Coverage is bounded by what is known by name at
  construction — mast's builtins and the tool names declared in
  `tool_catalog.tools`; a mutating tool never declared there (the common case
  for MCP verbs, since `tool_catalog.tools` is an override list) is not
  enumerable and remains the authoring rule's responsibility: do not name a
  specialist after a mutating tool.
  **Direct `--session-db` ack now warns (N4):** `mast sessions ack-effects
  --session-db=…` cannot serialize its watermark write against a running
  daemon (mast has no on-disk liveness signal to probe), so it prints a clear
  warning that the path is safe only when no daemon serves the DB.

- **Local (stdio) MCP server hardening** (docs/mcp-catalog-design.md
  "Implementation status"; #89). Three measures bound the blast radius of a
  `mcp.json` catalog that launches local commands. **Environment scoping:** a
  stdio server may set `env_mode: "clean"` to start its child from an empty
  environment, passing through only the daemon variables named in
  `env_passthrough` (plus the configured `env`) — so a `clean` server never
  sees the daemon's provider keys or cloud credentials unless named.
  **Command allowlist:** a new catalog-level `command_allowlist`, when
  non-empty, makes any stdio server whose resolved `command` is not listed a
  fatal load error (both sides `${VAR}`-expanded before comparison).
  **Control-plane coverage beyond `.agents/`:** the permission gate now
  accepts an explicit set of registered control-plane paths
  (`Options.ControlPlanePaths`) so a catalog loaded from a path-mode workload
  directory or a non-`.agents` config root can be write-protected once the
  gate is runtime-wired, closing the parent-directory heuristic's gap. Default
  behavior is unchanged (`env_mode` defaults to `inherit`; an empty allowlist
  imposes no restriction).

- **`/abort` re-abort now returns HTTP 409, not 500** (#88). Aborting a
  session that is already terminal is a state conflict, not a server fault:
  the inject `/abort` door now maps the `ErrAlreadyAborted` sentinel to
  `409 Conflict`, mirroring `/pause`. The durable abort marker was already
  idempotent (the `mast_aborts_total` counter stays at 1); this fixes only
  the status code. (A2A `tasks/cancel` keeps its idempotent-success
  semantics — the operator door reports the conflict instead.)

- **Local (stdio) MCP servers + generic catalog wiring**
  (docs/mcp-catalog-design.md "Implementation status"; #87). mast now wires
  every MCP server referenced by a workload generically from the `mcp.json`
  catalog, dispatched by transport kind — the previous build special-cased a
  single hard-coded HTTP `gke` toolset. Two transports are supported:
  streamable **HTTP** (with optional Google OAuth / ADC bearer auth, the GKE
  path) and local **stdio**, where mast launches a `command` (with
  `args`/`env`, `${VAR}`-expanded against the daemon environment) as a child
  process and speaks MCP over its stdin/stdout. Because a stdio server needs
  no cloud credentials, real tool calls can now be driven fully offline under
  `--model scripted`. A workload that references a server missing from
  `mcp.json` is a fatal load error; `mcp.json` is treated as a
  privilege-bearing control-plane file (a stdio entry is code execution) and
  each launch is logged for audit. New `pkg/mcp` catalog loader + transport
  dispatch (`Catalog`/`NewToolset`), a new `docs/site` reference page, and the
  unblocking prerequisite for the deferred blocking-tool UAT legs.

- **End-to-end UAT harness for the v0.2 durable-execution spine**
  (docs/uat-v0.2-plan.md "Implementation status"; #12). `scripts/uat-v0.2.sh`
  drives a real `mast` daemon process — boot, inject, pause, abort, timed
  resume, drain, restart, scrape — against the offline echo model and a real
  SQLite session DB, asserting on session state, exact `/metrics` lines, HTTP
  status, and process exit codes. It is deterministic, credential-free, and
  network-free (a fixed test bearer, no live provider), and runs in under a
  minute as a new `e2e` presubmit (`dev/ci/presubmits/e2e.sh`, wired into
  `all.sh` and a new `e2e` job in CI). It ships the no-blocking-tool subset of
  the scenario catalogue — metric priming + cardinality, auth, operator gate
  pause + token lifecycle (consumed-replay no-op vs expired-token rejection),
  timed-pause fire-and-resume, terminal abort marker + idempotency, and the
  clean-drain / usage exit codes. The scenarios that need a controllable
  registered blocking tool (crash-mid-effect ambiguity, drain-expired exit 3,
  mid-turn cancel, loop-breaker) are deferred until local/stdio MCP support
  lands; the plan doc records why. A minimal fixture bundle lives under
  `testdata/uat/`. Building the harness surfaced one latent wart — `/abort`
  returns HTTP 500 on an already-aborted session instead of mirroring
  `/pause`'s 409 (the durable marker is idempotent regardless) — tracked for a
  follow-up fix.

- **AG-UI server — Stage 1: server core** (docs/ag-ui-design.md
  "Implementation status"; #84). mast gains its fourth ecosystem interop
  surface — AG-UI, the agent↔user protocol CopilotKit apps and
  chat-platform bots speak — alongside MCP, A2A, and attach. With
  `--agui-listen`, a workload that opts in via the bundle's new `agui:`
  section is served at a per-workload HTTP endpoint: a client POSTs an
  AG-UI `RunAgentInput` and receives the turn back as a Server-Sent
  Events (`text/event-stream`) run stream — `RunStarted`, a `StateSnapshot`
  echoing the input state, the model's answer as a `TextMessage` triad
  with `ToolCall*` frames for tool activity, then one terminal
  `RunFinished` / `RunError`. A `GET /agui/agents.json` descriptor lists
  the exposed endpoints. Like the A2A server, it is **hand-rolled with
  zero new dependencies** (`pkg/agui`, over the shared `pkg/serverauth`)
  rather than wrapping the community AG-UI Go SDK — so every run drives
  the same `runTurnPre` chokepoint every other turn kind funnels through
  (turn-lock, abort / gate-pause refusal, budget meter, watchdog, effects
  outbox), and the deployment slim-graph gate stays green. The session id
  is always daemon-derived from the AG-UI `threadId`/`runId` and
  namespaced under `agui-` (`session_model: per_thread` default, or
  `per_run`), fenced off the reserved `…:mast-ops` namespace — a client
  never supplies a raw session id. Auth is a shared bearer validator
  (`MAST_AGUI_TOKEN`, per-workload scopes; non-loopback binds refused
  without it) with per-caller rate limiting (`MAST_AGUI_RATE` /
  `MAST_AGUI_BURST`, HTTP `429` + `Retry-After`). New metrics
  `mast_agui_runs_total{workload,outcome}` and
  `mast_agui_run_duration_seconds{workload}`. An interrupted turn maps to
  an honest `RunError{interrupt}` — the full HITL interrupt/resume
  lifecycle, per-key state deltas, client-declared tools, and the
  `agui://` federation client are follow-on stages.

- **A2A server — Stage C: `message/stream` over SSE** (docs/a2a-design.md
  "Mast as A2A server"; #15, #78). The A2A endpoint now streams turns:
  `POST /a2a` `message/stream` runs a turn exactly like `message/send` but
  emits its progress as Server-Sent Events (`text/event-stream`), one
  JSON-RPC response per `data:` frame — an initial `Task` snapshot, a
  `status-update` per model response (its text carried as progress), then a
  closing `artifact-update` (the agent's answer) and a final `status-update`
  (`final: true`) with the terminal state. `message/send` and
  `message/stream` share one `runTask` body, differing only in whether an
  `emit` callback is threaded through the turn's event loop. Updates are
  **message-granular** (one per model response, not token deltas — token
  streaming needs `StreamingModeSSE` across all turn kinds and is a
  follow-on). The SSE response is upgraded lazily on the first emitted
  frame, so auth, scope, and rate-limit refusals — all decided before the
  turn starts — ride a normal JSON-RPC error rather than a truncated
  stream, and `message/stream` shares the `message/send` rate-limit bucket.
  The agent card now advertises `capabilities.streaming: true`. This closes
  the A2A server umbrella (#78); `message/stream` no longer answers the
  `-32004` unsupported-operation error.

- **A2A server — Stage B2: pluggable rate-limiter seam** (docs/a2a-design.md
  "Rate limiting"; #78). `message/send` — the only budget-consuming verb —
  can now be rate limited through a pluggable `a2a.RateLimiter` seam on the
  server config (the seam AG-UI is designed to reuse, #11); control-plane verbs
  (`tasks/get`, `tasks/cancel`) are deliberately never gated, so an operator
  can always read or cancel a task. The built-in `TokenBucketLimiter` keeps
  an independent bucket per **(caller, workload)** — caller being the token's
  tenant claim if set, else its subject — and the daemon builds it from
  `MAST_A2A_RATE` (requests/second) + `MAST_A2A_BURST` (bucket depth;
  defaults to `ceil(rate)`, min 1). `MAST_A2A_RATE` unset means no limiting;
  a malformed value fails startup. A refused send returns the retryable
  `-32000` with an advisory `Retry-After` header and records a `rejected`
  task-outcome metric. The tenant-claim → session-isolation half of B2
  stays **deferred**: ADK v2.1.0's `IsolationScope` is an event/task-level
  field (the workflow `finish_task` machinery), not a session-create or
  tenant seam, so multi-tenant session isolation waits on an upstream
  session-scope seam or a mast-side user-namespacing design. `Principal.Tenant`
  ships now as the rate limiter's caller identity. SSE streaming
  (`message/stream`) remains Stage C.

- **A2A server — Stage B1: `message/send` turn execution + trace
  propagation** (docs/a2a-design.md "Mast as A2A server"; #78). The A2A
  endpoint now runs turns: `POST /a2a` `message/send` drives a synchronous
  turn through the same `runTurnPre` chokepoint every other turn kind
  funnels through (budget, pause, abort, turn-lock, effects outbox by
  construction). A task id **is** a mast session id; a message with a
  `taskId` continues that task, and one without routes to the single
  exposed skill and mints a fresh task. The agent's final answer is
  captured off the turn's event stream and surfaces as a `result` text
  artifact on the terminal task. An **in-process task registry** is the
  authority for `completed`/`failed` (which a transcript read cannot
  prove): `tasks/get` consults it first and falls back to the session's
  log-proven state for tasks this process did not run (e.g. after a
  restart) or that are non-terminal (the transcript is authoritative for
  `working`/`input-required`, so only terminal outcomes are pinned). The
  registry write is **cancel-wins**: a task canceled as its turn finishes
  still reports `canceled` and never leaks the model's answer as a result
  artifact, regardless of which write lands last. The A2A surface
  addresses **only tasks it minted** (the `a2a-` id prefix) — `tasks/get`,
  `tasks/cancel`, and `message/send` continuation all report `-32001` for
  any other session id, so a caller cannot read, cancel, or drive a turn
  into another surface's session (operator incidents, attach, autoresume)
  by presenting its id. The server assigns a `contextId` when the caller
  omits one (A2A v0.3), returned on the task so follow-ups can be grouped.
  Stage B1 is text-only — a message with no text parts is rejected
  (`-32602`), and an endpoint exposing more than one skill refuses a
  selector-less fresh send (`-32004`); a HITL pause returns
  `input-required`; a session that is aborted or gate-paused refuses the
  call at the chokepoint and reports its durable state; and new tasks are
  refused with the retryable `-32000` once shutdown drain begins.
  **Distributed tracing** is wired both directions: the A2A client injects
  the caller's W3C trace context (`traceparent`/`baggage`) on every
  outbound JSON-RPC call, and the server extracts an inbound one so the
  turn's spans parent under the caller's span (a no-op when tracing is
  disabled). The mast A2A client sends structured inputs as a `data` part,
  which this text-only server does not yet ingest — there is no mast↔mast
  `message/send` round trip until a later stage. Rate limiting and tenant
  → `WithIsolationScope` are deferred to Stage B2; SSE streaming
  (`message/stream`) remains Stage C.

- **A2A server — Stage A: agent card, read/control surface, auth**
  (docs/a2a-design.md "Mast as A2A server"; #78). mast can now expose its
  workloads to the [A2A](https://a2a-protocol.org) ecosystem as a server,
  on its own listener (`--a2a-listen`, e.g. `127.0.0.1:7780`), separate
  from the inject and attach surfaces. A workload opts in via the bundle's
  `a2a.expose` section (`skill_name`, `skill_description`, `auth.scopes`).
  This stage ships the discovery + control surface and its auth so they
  can be exercised end-to-end before turn execution lands: an **aggregated
  agent card** at `/.well-known/agent-card.json` (all exposed workloads as
  skills) plus **per-workload cards** at
  `/.well-known/agent-card/<name>.json`, and a single JSON-RPC 2.0
  endpoint `POST /a2a` serving `tasks/get` and `tasks/cancel`.
  `tasks/cancel` routes to the same terminal-abort path the `/abort` door
  uses (marker-first, then sweep the in-flight turn), idempotently.
  `tasks/get` projects the session's log-proven state onto the A2A task
  lifecycle and **never reports `completed`** from a transcript-only read
  (the event log cannot prove a turn finished vs. is in flight).
  (`message/send` turn execution landed in Stage B1, above;
  `message/stream` remains recognized-but-unsupported, `-32004`, until
  Stage C.) **Auth** is
  pluggable via the `a2a.TokenValidator` interface (built-in
  `StaticBearerValidator`, keyed off `MAST_A2A_TOKEN`); card endpoints are
  public, `/a2a` requires a valid bearer when a validator is configured
  (401 otherwise), and each skill's `auth.scopes` are enforced per call on
  reads *and* the destructive `tasks/cancel` (403 on a missing scope).
  Because `tasks/cancel` is destructive, a non-loopback `--a2a-listen`
  bind is refused at startup without a token (mirroring the attach
  surface's #376 policy); bind loopback or set `MAST_A2A_TOKEN`. A new
  observability family
  `mast_a2a_server_tasks_total{workload,outcome}` counts task-lifecycle
  transitions. Build-vs-buy: hand-rolled over the wire types this repo
  already owns so every A2A task runs through the same `runTurnPre`
  chokepoint every other turn kind funnels through (budget, pause, abort,
  turn-lock, effects outbox by construction), rather than adopting ADK's
  `adka2a.Executor`, which drives the runner directly and bypasses it.

- **Observability fixed-registry v0.2 pass + teardown watchdog**
  (docs/observability-design.md "Metric families",
  docs/durable-execution-design.md "Shutdown contract" item 6; closes
  #50). The v0.2 durable-execution surface built over the sprint — the
  interruption/abort markers, pause planes, timed-pause scheduler, and
  boot-time auto-resume — was previously observable only through logs.
  This pass canonicalizes it into **five fixed counter families** — the
  `mast_autoresume_total` family shipped earlier with #41, and this pass
  adds the four below it — all low-cardinality, all primed to zero per
  workload, and each incremented at the write site so a counter advances
  only when the durable operation it names actually happened (with one
  deliberate inversion, `mast_marker_write_failures_total`, which
  advances only when a marker write *failed*):
  `mast_autoresume_total{workload,outcome}` (boot-pass dispositions;
  #41), `mast_marker_write_failures_total{workload,operation}` (a marker
  write that failed, previously silent; `operation` ∈ `mark`/`clear`
  interruption marker, `pause` planned-stop gate-pause write),
  `mast_aborts_total{workload}`,
  `mast_gate_pauses_total{workload,source}` (`source` ∈
  `operator`/`planned_stop`), and
  `mast_timed_pause_fires_total{workload,outcome}` (`outcome` ∈
  `resumed`/`skipped`/`error`). The registry stays fixed — callers
  increment through typed methods and cannot mint names or labels — and
  the shipped names supersede the pre-implementation
  `mast_pauses_total`/`mast_resumes_total` sketch (they split by the
  mechanism that emits them, not a single `reason` label). Also adds a
  **teardown watchdog** on the shutdown unwind: after the (already
  bounded) drain completes, `serve()` arms a 15s watchdog over the
  deferred teardown (OTel flush, eventlog/attach `Close`, context
  cancels); on overrun it dumps every goroutine's stack to stderr and
  force-exits with a dedicated code `4` (distinct from the
  drain-expiry `3`), so a wedged `Close` or leaked goroutine surfaces a
  diagnostic instead of hanging silently until the supervisor's
  SIGKILL. A healthy teardown exits first and the watchdog never fires.

- **Boot-time auto-resume — the v0.2 durable-execution closer**
  (docs/durable-execution-design.md "Boot-time auto-resume"; closes
  #41). On boot the daemon scans for `interrupted` sessions (turns cut
  short by a prior shutdown) and drives a continuation turn for each
  eligible one through the same chokepoint every other turn kind uses,
  so unattended work restarts on its own — on by default
  (`--auto-resume`, `--auto-resume=false` disables), serve mode with a
  durable `--session-db` only. **The guarantee is the operational form
  of exactly-once: auto-resume never double-fires a mutation.** A
  session carrying **any** dangling mutating tool call (an ambiguous
  prior effect the recorded-effect outbox surfaces) is skipped
  (`skipped_ambiguous`) and left for an operator `ack-effects`, never
  resumed — an ack watermark does not pair the call, so re-running it
  would either replay the raw `tool_use` to the provider or falsely
  synthesize a did-not-happen response. A dangling **read-only** call
  on the single last function-call event is repaired with a synthetic
  `interrupted before completion` response (`.ID`/`.Name` set for
  ID-pairing and Gemini) and the turn re-runs; a transcript already
  ending on a completed model turn just has its stale marker cleared
  (`cleared`, no spurious "Continue" turn); a genuine trailing user /
  paired-tool turn re-invokes the model over history with a nil
  message. Rails: `--auto-resume-window` (default `1h`) skips work
  already stale at crash (`skipped_stale`); a per-session restart-loop
  breaker (3 attempts / 10m, durably pre-incremented so a process that
  SIGSEGVs mid-turn still counts) plus a per-boot turn cap bound a
  poison session (`skipped_loopbreak`); a `preTurn` recheck under the
  session turn lock skips a session a concurrent inject/resume advanced
  between scan and turn (`skipped_superseded`). Slice-1 drives
  `coordinator` dispatch only (`skipped_unsupported` otherwise, and for
  foreign user IDs and deferred sub-run delegations). Every decision is
  counted in `mast_autoresume_total{workload,outcome}`. `mast stop
  --pause-sessions` opts a session out — a gate pause outranks
  `interrupted`, handing those sessions back to the operator instead of
  continuing them. New store seams: `ScanInterrupted`,
  `Summary.InterruptedAt`, and `RecordAutoResumeAttempt` /
  `ClearAutoResumeAttempts`; new effects seam: `ScanDangling` (mutating
  vs repairable vs deferred, sharing `scanHistory`'s pairing core, which
  keeps its exact shipped output).

- **Programmatic pause/abort — the v0.2 durable-execution surface**
  (docs/durable-execution-design.md "The v0.2 pause/abort mechanics",
  designed in #72; closes #42). Two pause planes: an **interrupt
  pause** (`pause_session`, a long-running builtin in the planner
  vocabulary when a durable store exists — the body writes a token
  record keyed by its own function-call ID to the companion ops row,
  then parks; a record-write failure returns an error result, never a
  tokenless park) and a **gate pause** (`mast sessions pause` /
  `POST /pause` / `mast.Pause`) enforced at the daemon's turn
  chokepoint: every turn kind — inject, attach, resume, timer —
  refuses gate-paused sessions with `session_paused` (HTTP 409), and
  `--interrupt` additionally cancels the in-flight turn. **Resume
  tokens** (`mrt_` + 128-bit random) are minted, never caller-chosen;
  scope-checked before execution; 7-day default TTL that `PauseSpec`
  may only shorten; consumed on the durable append of the resume
  FunctionResponse (a resume turn that fails before the append leaves
  the token live for retry); expired tokens refuse with the pause
  intact — `mast sessions extend-token` / `POST /extend-token` is the
  audited recovery. `mast sessions resume --token=...` resolves the
  session itself (`--session-db` direct mode clears gate pauses only,
  on DBs no daemon serves); `mast.ResumeByToken` is the library twin.
  A **timed-pause scheduler** (min-heap, boot ops-scan seeded) fires
  `resume_at` timers through the normal budget-wrapped resume paths;
  refused fires requeue with backoff; abort purges a session's timers
  and tokens. **Terminal abort**: aborted sessions now refuse ALL turn
  kinds at the chokepoint (v0.1 only refused resume — inject/attach
  turns ran on aborted sessions) and abort cancels the in-flight turn
  (marker first, then sweep). **Planned stop** (`mast stop` /
  `POST /stop`): the SIGTERM drain path with interruption markers
  classified `operator stop`; `--pause-sessions` gate-pauses the
  marked set so boot-time auto-resume (#41) hands them back to the
  operator; new exit code **3** = drain window expired with
  interrupted survivors (0 = clean drain — exit codes encode work cut
  short, not initiator). The `paused` state derivation gains two
  sources: unanswered long-running parks — **fixing a shipped v0.1 gap
  where `request_operator_input` parks projected `idle` or
  `interrupted`** (and were boot-repair candidates) — and the gate
  marker; `show` prints pause records, tokens, and ready-to-paste
  token resume commands.

- **Recorded-effect outbox (`pkg/effects`) — the v0.2 durable-execution
  guard for mutating tools under at-least-once re-execution**
  (docs/durable-execution-design.md, resolves open question #8; closes
  #69; unblocks #41). The three runner construction paths mast owns
  (serve, one-shot, library) attach an ADK runner plugin that: refuses
  mutating and sub-run-spawning tool calls with a structured
  `ambiguous_prior_effect` error while the session carries a dangling
  mutating tool call from an interrupted turn (read-only work
  proceeds); replays a call's recorded completion instead of
  re-executing when the log already holds one for the exact
  function-call ID (a nil recorded payload replays an explicit marker
  result rather than re-executing); and treats unknown tools — MCP
  tools included — as mutating (ADK drops MCP `readOnlyHint`
  annotations before mast can read them). All history reads happen
  once per turn at turn start; task delegations (sub-agent-named
  calls the coordinator deliberately leaves unresolved across HITL
  turns), engine control calls, long-running calls, and empty-ID
  calls never read as dangling effects — the pre-merge adversarial
  gate caught both an unreachable replay branch and a false-positive
  wedge on the default coordinator composition in the first
  implementation, and the suite now pins both on the real wire
  shapes. New surfaces: `tool_catalog.tools[].mutating` per-tool
  overrides in the workload bundle (audit-logged at startup);
  `mast sessions ack-effects <id>` (daemon `/ack-effects`, serialized
  against in-flight turns; `--session-db` direct mode for DBs no
  daemon serves — the interrupted turn usually leaves NO pending
  interrupt, so resume alone cannot reach it);
  `mast sessions resume --ack-effects` + `ack_effects` on `/resume`
  for the paused case; and `mast.AckEffects` for library embeds. The
  watermark covers only intents persisted at or before it and is not
  a transcript state. The suite also pins the substrate property the
  design rests on: a tool's own FunctionCall event is durable before
  the tool runs.

## v0.1.2 (2026-07-31)

Patch release: the v0.1.1 shutdown contract hardened through two
further adversarial review rounds, the second gated pre-merge. All
twelve v0.1.2-milestone issues (#53-#58, #60-#65) are closed; the
fixes are backed by reproducers verified to fail on the pre-fix code.

- **Round-three hardening from the pre-merge adversarial gate (#60,
  #61, #62, #63, #64, #65).** The third review round refuted two of
  the round-two fixes and the fixes here were validated by re-running
  the discovering reproducers against them (now the standing rule):
  (1) the #55 fix had reintroduced false-`interrupted` through the
  opposite door — `end()` read the marked flag outside the write
  mutex — so the clear decision moved fully inside it; the
  40-sessions-finishing-during-drain reproducer is in the suite and
  fails 3/3 on the pre-fix code (#60). (2) `/inject` could mint a
  reserved `:mast-ops` session from the untrusted payload UID; now
  refused like every other surface, with reserved-ID rejections
  answering HTTP 400 on all three write endpoints (#61). (3) Two concurrent turns on
  the SAME session lost one to ADK's stale-session check — the daemon
  now runs one turn per session, queueing same-session injects and
  resumes behind the in-flight turn (#62). (4) The pre-mark pass is
  bounded by the drain window again, and the drain-expiry log
  separates survivors with durable markers from those whose mark
  write failed (#63). (5) The "every SQLite construction" hardening
  claim is now true — the one-shot and sessions-CLI paths route
  through the same hardened opener, ops-row writers serialize through
  a store-level mutex, and a failed clear keeps its bookkeeping so
  the marker stays visible (#64). (6) Drain refusal answers 503 +
  Retry-After instead of a 500, the InterruptSelfAuditor capability
  is compile-time pinned, and the docs site caught up with the v0.1.2
  behavior changes (#65).

- **The default `--session-db` path gets the attach path's SQLite
  write hardening; fixes silent loss of markers and transcript events
  under concurrent sessions (#53, #54, #55, #56, #57, #58).**
  Adversarial re-review of v0.1.1 found all SQLite write safety
  (serialization + WAL + busy_timeout) lived in `pkg/eventlog` and
  engaged only with `--attach-listen`; the plain path — what
  `deploy/base` runs — opened raw SQLite and, under concurrent
  incidents, lost transcript events (killing their turns) and
  drain-time interruption markers (silently, warn-only), falsifying
  v0.1.1's SIGKILL-survivability claim on the shipped deploy shape.
  Both paths now share `eventlog.OpenSessionService`; a concurrency
  test pins the behavior. Also from the same review: the write-lease
  regression tests were neutralized by future-pinned timestamps and
  passed on the pre-fix code — rewritten with natural timestamps and
  verified to fail against it (#54); mark/clear ordering is now
  serialized with its store writes so a turn finishing mid-pre-mark
  can no longer be left falsely `interrupted` (#55); the reserved
  `:mast-ops` suffix is enforced on every surface — Get/show, resume,
  abort, and the attach resumer all refuse ops-row IDs instead of
  presenting phantom sessions or driving turns into marker rows
  (#56); the attach interrupt-audit event moved from the protocol
  layer (where it could stale the interrupted turn's session handle —
  the last write-lease violation, present upstream too) into the
  adapter's serialized turn loop (#57); and the drain now closes the
  inject listener before pre-marking, gates the inject/resume
  handlers, and reports only genuinely-marked survivors (#58).
  Marker-write failures are now error-level logs. Known limitations,
  tracked in #50: no marker-failure metric yet (v0.2 fixed-registry
  work), no teardown watchdog, and boot-time auto-resume remains
  deliberately deferred behind the recorded-effect outbox (#41).

## v0.1.1 (2026-07-31)

Patch release: the SIGTERM shutdown contract, shipped and then
hardened by adversarial review — plus the durable-by-default GKE base.
All seven v0.1.1-milestone issues (#38-#40, #45-#48) are closed.

- **Session markers move to a companion ops row; fixes shutdown
  markers and abort killing live turns on database stores (#45, #46,
  #47, #48).** Adversarial review of the shutdown contract found that
  ADK's database session service treats a session handle as a write
  lease (optimistic concurrency on `last_update_time`): appending the
  interruption marker — or an operator abort — to the session's own
  row invalidated the live runner handle, killed the in-flight turn
  with a `stale session error`, and (for shutdown markers) the dying
  turn's cleanup then erased the marker and the drain logged clean.
  All marker writes (abort + interruption) now go to a companion ops
  row (`<sid>:mast-ops`, reserved suffix, hidden from `sessions
  list`), mirroring core-agent's derived-session-ID fix for the same
  ADK behavior; projections fold ops-row state into the primary, and
  legacy v0.1.0 abort markers in primary-row state are still honored.
  Consequences: abort truly is marker-not-preemption now (its
  documented contract), markers work even before the runner has
  created the session, and marker events no longer appear in the
  model-visible transcript. Also from the same review: `/resume`
  turns now get the workload wallclock budget like inject and attach
  turns; new attach-injected turns are refused once a shutdown drain
  begins (live tail unaffected); cancelled drain survivors get a
  short bounded grace to unwind before teardown; and the tracker's
  marker writes are timeout-bounded. Regression tests now run the
  marker paths against a real SQLite database service with a held
  live handle — the in-memory-only test blind spot that hid the bug.

- **The `deploy/` kustomize base is durable by default (#40).** The
  daemon converts from a bare Deployment to a **StatefulSet** with a
  1Gi `volumeClaimTemplate` mounted at `/var/lib/mast` and
  `--session-db=/var/lib/mast/sessions.db` — bringing the shipped base
  in line with the v0.1 GKE row in `docs/deployment-design.md`, which
  already pinned SQLite-on-GKE to StatefulSet+PVC (a rescheduled
  bare-Deployment pod loses the session DB, and with it durable
  pauses, abort markers, and the new shutdown interruption markers).
  `fsGroup: 65532` makes the volume writable for the non-root user;
  the single-replica RollingUpdate recreate preserves the old
  `Recreate` semantics the RWO claim needs. In-memory sessions remain
  a deliberate opt-out (omit `--session-db`), not a deploy default.

- **Shutdown contract: SIGTERM now actually drains (#38, #39, #40).**
  The daemon's graceful-shutdown path returned as soon as the drain
  *began* (`http.Server.ListenAndServe` unblocks the moment `Shutdown`
  is called), so in-flight turns died at process exit with no
  bookkeeping. `mast serve` now: drains in-flight turns — inject and
  attach alike — for up to the workload's
  `budget.max_wallclock_seconds` (30s without a budget); durably marks
  every in-flight session **before** waiting, so a SIGKILL mid-drain
  still leaves the markers on disk; clears the marker for turns that
  finish inside the window; and cancels survivors' contexts instead of
  abandoning them. `pkg/transcript` derives the new `interrupted`
  state (precedence `aborted > paused > interrupted > idle`), and
  `mast sessions list --state=interrupted` filters on it. Deploy
  surfaces sized to match: the K8s base sets
  `terminationGracePeriodSeconds: 330` against the demo workload's
  300s turn ceiling, and the standalone unit gains
  `TimeoutStopSec=330` plus `Restart=always` (the daemon exits 0 on
  any signal-initiated stop; `on-failure` left it down after a stray
  SIGTERM). Boot-time auto-resume of interrupted sessions is
  deliberately deferred to v0.2 behind the recorded-effect outbox
  (#41); planned-stop classification folds into the v0.2 pause/abort
  work (#42). See docs/durable-execution-design.md, "Shutdown
  contract".

## v0.1.0 (2026-07-30)

Phase 1 complete: **all eleven v0.1 exit criteria** from
[`docs/fork-design.md`](./docs/fork-design.md) are green. This release
finishes the staged adapter ports from
[`go-steer/core-agent`](https://github.com/go-steer/core-agent) — the
ported packages are originally derived from core-agent at the
per-stage pins `83ec0713` (P1.3a, 2026-07-27), `b8dd225e` (P1.3b,
2026-07-29), and `25d8531c` (P1.3c, 2026-07-30); every ported file
carries a per-file derivation header with its stage's SHA. Everything
else is mast-native on ADK v2.1.0.

- **`pkg/session` renamed `pkg/transcript`.** The operator-projection
  package collided with ADK v2's own `session` package, forcing an
  alias (`mastsession` / `adksession`) in every file touching both —
  including library embedders' code. Renamed before the v0.1 freeze
  (this package is one of the five stable-from-v0.1 surfaces), while
  the change costs nothing; mirrors core-agent's own rename (#513).
  The `mast sessions` CLI and the root-package API
  (`mast.ListSessions` / `mast.ResumeSession`) are unchanged.

- **P1.3c: the operator attach surface, ported and wired
  (`--attach-listen`).** `pkg/attach` (HTTP/SSE protocol v1.4.0:
  session listing, seq'd replay + live tail, inject/wake/interrupt,
  capabilities frames, agent card, prompt broker, peer registry,
  per-caller rate limiting), `pkg/auth`, and `pkg/eventlog` port from
  `core-agent@25d8531c` — pinned at the first HEAD after attach went
  quiet, deliberately including #519's transport-neutral
  OperatorEventTarget seam so mast never carries the deprecated
  emitter shape. The eventlog lands in the re-scoped shape the fork
  design called for: ADK v2's `session/database` owns the session
  tables; the package adds the seq-overlay + Since/Watch stream on
  top. New `pkg/attachadapter` bridges mast's runner-driven daemon
  into the Registrant contract (one injected message = one serialized
  turn; typed operator events in spec order; interrupt cancels the
  in-flight turn; callers ride the turn context into eventlog
  metadata). `mast serve --attach-listen` binds the surface (TCP or
  `unix:` socket; requires `--session-db`; bearer auth via
  `MAST_ATTACH_TOKEN`; loopback-only without auth). **Exit criterion
  4 verified with the real client:** mast-web (headless chromium,
  proxy mode) connected to a live daemon, listed its sessions, and
  round-tripped a prompt through a real turn over SSE.

- **Build identity moved to `internal/version`.** `mast --version`
  output is unchanged; the version string is now importable so the
  attach capabilities frame and agent card can report it
  (`mast/<version>` server banner). GoReleaser ldflags path updated.

- **One-shot turns get a `--timeout` deadline (default 5m; `0`
  disables).** A one-shot against an unresponsive backend hung forever
  — genai's silent retry-with-backoff on quota errors looks exactly
  like a hang from the outside, observed live. The deadline covers the
  whole turn (model construction included) and trips with an error
  naming the flag. Serve mode is unaffected; workload budgets own its
  wallclock ceilings.

- **gemini mid-tier default is now `gemini-3.5-flash`.** Mid-tier
  classes (research, chat) previously defaulted to `gemini-2.5-pro`,
  which predates mixed built-in + function tools — so `--task=research
  --provider=gemini` could never ground: observed live, the model
  hallucinated a `search` tool (ADK's tool-not-found recovery answered
  it) and then apologized. The 3.5-flash line supports the mix, is
  already classified mid by `pkg/modeltier`, and is cheaper per the
  pricing catalog.

- **One-shot mode refuses flags placed after the prompt.** Go's flag
  package stops parsing at the first positional argument, so
  `mast --task=x "prompt" --session-db=y` silently ran with in-memory
  sessions and sent `--session-db=y` to the model as prompt text (hit
  live twice). A trailing token that names a defined flag is now a
  hard error with an explanation; prompts that legitimately mention
  flag-like words are unaffected when quoted.

- **Fix: the Anthropic adapter respects `stream=false`.** The ported
  adapter ignored model.LLM's stream flag and always yielded
  partial-text chunks; under ADK v2's `StreamingModeNone` every
  fragment became a runner event — ~30 noise log lines per one-shot
  turn on the first live anthropic-vertex run (the runner persists
  only non-partial events, so session stores were unaffected). With
  `stream=false` the caller now sees exactly one TurnComplete
  response; the transport still streams SSE underneath (pause_turn
  continuation and the #487 close discipline depend on that shape).
  core-agent's adapter has the same signature quirk; flagged upstream.

- **Fix: `--session-db` creates missing parent directories (SQLite).**
  SQLite won't create intermediate directories and reports a missing
  parent as "unable to open database file: out of memory (14)"
  (SQLITE_CANTOPEN) — hit on the first smoke run with
  `--session-db=/tmp/mast/smoke.db` before `/tmp/mast` existed. The
  sqlite dialector path now MkdirAlls the parent (0750) so an
  unattended daemon's first boot works against an empty state
  directory; `file:` URIs are unwrapped, in-memory forms untouched.

- **Live-smoke fallout: three provider fixes (2026-07-29).** The first
  credentialed runs surfaced three port seams, all fixed:
  - *gemini frontier-tier default is now `gemini-3.6-flash`.* The
    ported tier table (and core-agent's, still) said `gemini-3.5-pro` —
    a model id that never shipped. Both directions updated together per
    the table's own maintenance note (`taskclass.ModelForTier` and
    `modeltier.Classify`, which now recognizes `gemini-3.6-flash` as
    frontier). Known gap: the builtin pricing catalog (generated
    2026-07-20) has no `gemini-3.6-*` entry yet, so budget metering uses
    the flat non-zero fallback rate until the next catalog regen.
  - *Gemini built-ins now skip pre-3.0 models when function tools are
    present.* Gemini 2.x rejects server-side search built-ins alongside
    client-side function declarations ("Multiple tools are supported
    only when they are all search tools"), and mast's Task/SingleTurn
    agents always carry `finish_task` — so blanket injection 400'd
    every turn on `gemini-2.5-pro`. The wrapper now degrades per
    request (model keeps working unGrounded, one operator log line);
    requests with no function tools keep built-ins on every generation.
  - *Anthropic tool schemas normalized to JSON Schema draft 2020-12.*
    genai marshals `Schema.Type` as uppercase proto enums
    ("OBJECT"/"STRING"), which Anthropic's strict `input_schema`
    validation rejects — hit by ADK v2's `finish_task` declaration on
    the first anthropic-vertex run. `schemaToInput` now recursively
    lowercases type enums (and drops `TYPE_UNSPECIFIED`), leaving
    schema data untouched. core-agent shares this latent seam; flagged
    upstream.

- **P1.3b: provider adapters + watchdog (2026-07-29).** The staged adapter
  ports resume — core-agent closed all four cleanup milestones on
  2026-07-28, so P1.3b's gate (the correctness bugs #357/#367/#370/#363/#372)
  is cleared; attribution pinned at
  `go-steer/core-agent@b8dd225e9ae7fdeb3ff23772cc5be25eed34b818`. New
  packages: `pkg/providers/anthropic` (first-party + Vertex backends;
  preserves the thinking-block round-trip #357, per-request tool_use ID
  synthesis #367, streaming-close #487, pause_turn continuation, and the
  three-bucket prompt-cache usage fold); `pkg/providers/gemini` (the
  builtin-tool wrapper — GoogleSearch/URLContext defaults, server-side
  invocation gating, Vertex context-cache stamp/strip + eviction retry,
  empty-response retry #220 — plus the GoogleSearch grounding audit
  projection); `pkg/providers/vertexcache` (context-cache manager with the
  transient-Init retry #370; relocated from core-agent's `internal/` so
  compose and library consumers can wire it); `pkg/providers/mock`
  (scripted JSONL replay with a self-contained `RecordedTurn` format);
  `pkg/watchdog` (repeated-tool-call signal plus the session-event bridge
  carrying the #363 aggregator-dedup fix). Deliberate reshape, mirroring
  core-agent #492 item 7: no `*config.Config` constructors and no `init()`
  registry — per-provider Options structs are the construction API, and
  `internal/compose.BuildModel` dispatches `echo`/`scripted`/`gemini-*`/
  `claude-*` (`--provider` picks the Anthropic backend; without it,
  `ANTHROPIC_API_KEY` then a Vertex project decides). CLI: `--provider`
  grows `anthropic`, `anthropic-vertex`, and `scripted`; claude-* budget
  rates derive from the builtin pricing catalog; every runner stream now
  runs through a per-session watchdog tap (alerts are logged — the
  model-context routing of core-agent #159 remains bucket-3 work).

- **P1.3a: the ADK-independent adapter packages (2026-07-27).**
  *(Recorded at roll time — this stage landed in PRs #21/#22 without a
  CHANGELOG entry.)* Pinned at `go-steer/core-agent@83ec0713`:
  `pkg/taskclass` (task-class profiles + tier defaults, and with it
  one-shot mode — `mast --task=<class> "<prompt>"`), `pkg/permissions`
  (ported, deliberately not runtime-wired: the package doc records
  core-agent #385's gate findings as wiring-time inputs), `pkg/pricing`
  (builtin catalog wired into budget-meter rates), `pkg/instruction`,
  `pkg/digest` (minus the ADK-v1-entangled `store_eventlog.go` — an
  honest descope), and `pkg/modeltier`.

- **CI split into parallel jobs (core-agent's ci.yml shape).** The single
  `build-test` job running `all.sh` sequentially becomes four parallel
  jobs — `test` (build/vet/fmt/-race tests), `lint`, `hygiene`
  (mod-tidy/govulncheck/slim-deps), `docs-lint` — each step still
  invoking the identical presubmit scripts, so the scripts remain the
  single source of truth and local `all.sh` remains the sequential
  equivalent. Buys: per-check status on PRs (a red run names the
  failing category instead of one opaque `build-test`), shorter wall
  clock (slowest leg paces instead of the sum), per-job re-runs,
  `concurrency: cancel-in-progress` on superseded pushes, and cached
  golangci-lint/govulncheck binaries. Branch protection needs its
  required check renamed from `build-test` to the four new job names.

- **CI parity with core-agent: lint, mod-tidy, vuln, docs-lint presubmits.**
  Four checks ported from core-agent's presubmit set: `lint`
  (golangci-lint v2.12.1 pinned, same linter set and settings as
  core-agent's `.golangci.yml` so ported code lints identically on both
  sides), `mod-tidy` (`go mod tidy` must be a no-op; content-compared,
  not git-compared, so uncommitted local edits don't false-positive),
  `vuln` (govulncheck, symbol-level), and `docs-lint` (prose-drift rules
  over README + site content: tool/specialist counts, variant counts,
  pinned self-install snippets — with a self-test so a defanged regex
  fails loudly). First run caught real issues: two reachable
  vulnerabilities (grpc → v1.82.1 for GO-2026-6061; pgx/v5 → v5.9.2 for
  GO-2026-5004, a SQL-injection path reachable through the Postgres
  session store), three doc-drift instances, and a sweep of lint
  findings — including `serve()` restructured to return errors instead
  of `os.Exit` so the OTel flush and signal cleanup defers actually run
  on fatal startup errors. Ported-file attribution lines moved from
  inside the license comment block to their own comment group below it
  (goheader can't express an optional template suffix; the convention
  is otherwise unchanged). core-agent's `verify-version-fallback` is
  deliberately not ported: mast's ldflags fallback is the constant
  string `dev`, which cannot go stale.

- **Presubmit tests run under the race detector.** `dev/ci/presubmits/
  test.sh` (and therefore CI) now runs `go test -race -timeout 5m ./...`,
  matching core-agent's bar — the P1.3b ports introduced mast's first
  real concurrency, and the ported regression tests were written against
  `-race` upstream. The vertexcache tests' 1s poll deadlines widen to 10s
  so mast doesn't inherit core-agent's #499 flake under loaded CI (a
  passing poll still returns in milliseconds).

## v0.1.0-pre (2026-07-26)

Phase-1 pre-release: nine of fork-design's eleven v0.1 exit criteria are
green; `--task` profiles and attach-mode reachability remain gated on the
P1.3 adapter ports per the revised trigger. Highlights below; details in
the per-item entries that follow.

- Workflow-graph and SubAgents dispatch on ADK v2.1.0; durable HITL
  surviving process death; budget metering with cost + turn caps; the
  full 13-specialist GKE triage roster; `.agents/` config discovery;
  sessions operator surface (CLI + HTTP); observability v0.1; A2A v0.3
  client + federation adapter + `invoke_remote_agent`; planner scaffold;
  forkable workflow starters; the slim-embed guarantee with CI
  enforcement; presubmits-as-CI; deploy starters incl. Cloud Run with
  Postgres session store; top-level `mast` library API.

- **P1.4 interop slice: A2A client + federation surface (2026-07-26).** The
  v0.1 slice of the 2026-07-25 re-cut ([`docs/fork-design.md`](./docs/fork-design.md)
  P1.4): `pkg/federation` with the frozen `Adapter`/`Handle` interface
  (chosen so v0.2 streaming/HITL propagation is additive, not breaking —
  rationale in the package doc), `a2a://<name>/<skill>` reference parsing, a
  scheme-keyed adapter registry, and the planner tool
  `invoke_remote_agent(reference, inputs)`; `pkg/a2a` with the synchronous
  A2A v0.3 client (agent-card fetch + cache, JSON-RPC 2.0 `message/send`
  with `A2A-Version` header and bearer auth from env-var references,
  direct-message and task-opened replies, bounded `tasks/get` polling,
  `tasks/cancel` on cancellation/timeout) and static `.agents/a2a/*.yaml`
  registrations wired into `pkg/config` root scanning. Fork-design exit
  criterion 9 is covered by an httptest round-trip through
  `invoke_remote_agent`. Server, streaming, registry discovery: v0.2 per
  [`docs/a2a-design.md`](./docs/a2a-design.md) phasing.

- **P1.1 bootstrap (2026-07-26).** The repo grows code: the spike-validated
  prototype graduates from the standalone `mast-prototype` repo
  (tags `spike1`/`spike2`; verified findings in
  [`docs/spike-findings.md`](./docs/spike-findings.md)) — workload-bundle +
  specialists loaders, workflow-graph and SubAgents dispatch shapes, durable
  HITL on ADK's SQLite session service, budget metering, per-specialist MCP
  tool allowlists, GKE MCP wiring, inject/resume HTTP endpoints, and the GKE
  triage example workload. Pinned to `google.golang.org/adk/v2 v2.1.0`.
  Provenance per [`docs/fork-design.md`](./docs/fork-design.md): attribution
  is by reference, not git history — prototype history remains in
  `mast-prototype`.
