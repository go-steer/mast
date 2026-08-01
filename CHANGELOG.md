# Changelog

## Unreleased

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
