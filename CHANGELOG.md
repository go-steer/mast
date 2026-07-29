# Changelog

## Unreleased

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
