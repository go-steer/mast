# Changelog

## Unreleased

- **New: `GET /sessions/{id}/subagents` — the roster the daemon loaded**
  (#134). The route block in `pkg/attach/handlers.go` has advertised this
  endpoint in a comment since the attach surface landed and never registered
  it, so mast-web's specialists view 404'd against every mast daemon.
  `/agents` was the only roster read available, and it lists *spawned*
  instances — always empty on mast, since every dispatch shape resolves its
  specialists inside the turn. "What is running" is the wrong answer to
  "what can this thing do".

  Entries are wire-compatible with core-agent's `SubagentCatalogInfo`
  (`name`, `description`, `model`, `root`, `modes`) plus three mast-native
  fields. `modes` carries only what is true in core-agent's vocabulary —
  `["sync"]` under planner dispatch, where `invoke_specialist` really is a
  parent tool, and empty otherwise; mast has nothing spawnable by reference,
  so `"async"` is never emitted. Claiming a mode to make the field look
  populated is the upstream defect the issue warned about (core-agent#741).

  What `modes` cannot say, `invocation` does: `parent_tool`, `transfer`,
  `graph_node`, or `fanout_branch` — how the composed root actually reaches
  this specialist. **Empty means nothing in this shape reaches it**, which
  is how a roster orphan becomes visible without reading `internal/compose`
  (the `change-executor` orphan the first live GKE run found was exactly
  this). `capability` and `agent_mode` carry the declared read/write half
  and the ADK mode, spelled as the frontmatter spells them.

- **Fixed: `GET /sessions/{id}/tools` answered "200, no tools" on every mast
  daemon** (#133). `attachadapter.Config` has carried a `ToolsFn` since the
  attach surface landed; `cmd/mast` never set it, so the endpoint took the
  nil-means-empty path and returned a well-formed empty list — a read that
  looks like an answer. Any operator client asking a mast daemon what tools
  it holds got told: none.

  The daemon now projects the MCP toolsets it wired at composition time,
  which is the only place the per-server attribution survives — ADK exposes
  no accessor for a built agent's tools, so by the time the adapter holds an
  agent, "which server did this come from" is already gone. Each entry
  carries `source: "mcp"`, the server name, and a `gate_state` derived from
  the mutation predicate and `hitl.on_mutation`: `allowed` for a read (or a
  write under `apply`), `prompted` under `require_approval`, `denied` under
  `dry_run`. Listings are cached for 30s and bounded at 5s per refresh; one
  unreachable server is omitted and logged rather than blanking the catalog,
  and an outage that takes down every server serves the last good answer
  instead of caching the emptiness.

  Not yet listed: mast's non-MCP tools (the planner's dispatch calls), which
  are registered inside `internal/compose` with no handle surviving the call.
  A second hand-maintained list would drift, and a wrong catalog is worse
  than a partial one.

  The regression guard is a reflection test over the rendered
  `attachadapter.Config`: a new func field on it fails `cmd/mast`'s tests
  until serve either wires it or records why it can't. The bug was not a
  broken projection — it was an assignment nobody made, and nothing asserted
  on the served artifact.

- **Fixed: every GitHub Release mast has ever cut published with an empty
  body.** The notes were composed from this file by `dev/release/notes.sh`,
  printed in full to the workflow log, and passed to GoReleaser as
  `--release-notes` — and then dropped, because `.goreleaser.yaml` set
  `changelog: disable: true` and GoReleaser loads the release-notes file
  *inside* the changelog pipe it was thereby skipping. The config comment
  said the two went together; they are mutually exclusive. Leaving the pipe
  enabled is what honours the flag, since the file short-circuits it before
  any git-log changelog is generated. v0.3.0's body was backfilled by hand;
  v0.1.0-pre through v0.2.0 are still empty.

  The release workflow now reads the *published* release back and fails if
  its body is under 200 characters. Every check that existed passed on every
  one of those releases, because each one checked the step before the one
  that broke: the notes script was unit-tested, the compose step echoed
  correct output, and the dry run never publishes and so never had a body to
  look at.

## v0.3.0 (2026-08-14)

- **New: the write gate — a tool call that changes anything stops and asks,
  once per call, and the pause survives `kill -9`** (docs/v0.3-plan.md
  W2.1–W2.3; parity rows 9–11). Approval used to be per *turn*: a specialist
  finished, its whole result parked, and approving it authorized whatever it
  did next. The gate is now in front of the call. `hitl.on_mutation` takes
  `require_approval` (the default), `apply` (run it, audit it) or `dry_run`
  (never run it, report what would have run); the older `require_approval:
  true` spelling still parses, and declaring both forms is refused rather than
  silently resolved.

  What counts as mutating is a declaration, not a guess. mast reads the
  built-in annotation where a tool has one, and otherwise the workload's
  `tool_catalog.tools[].mutating` — **a tool named nowhere counts as
  mutating**. That default is load-bearing rather than cautious: ADK's
  mcptoolset drops MCP's own `readOnlyHint` annotations on the way through, so
  for an MCP tool the catalog is the *only* place the answer can come from,
  and the failure mode of guessing "read-only" is a silent unreviewed write.
  The cost is that adding a read tool without classifying it makes it stop and
  ask.

  Every gate decision is an audit record — `awaiting_approval`, `apply`,
  `dry_run`, `denied_by_policy`, `denied_by_operator` — carrying the tool, the
  arguments, the specialist, and the approver. The approver is taken from the
  authenticated caller on `/resume`, never from the payload: a body claiming
  `"approver":"sre-oncall"` over an unauthenticated connection has that field
  overwritten, because an approval whose author is self-asserted is not an
  approval. And an approval is `AllowOnce`. A verdict that tries to widen
  itself to the session is refused and audited as
  `approval_scope_refused` — approving one `delete_k8s_resource` cannot
  become standing permission to delete.

- **New: a specialist declares whether it may change anything —
  `capability: read_only | change_executor`** (docs/v0.3-plan.md W2.4; parity
  row 12). Defaults to `read_only`. A roster where a `read_only` specialist
  can reach a mutating tool is refused at construction
  (`internal/compose.CheckCapabilitySplit`), on every path that builds a root
  and under every dispatch shape, so the check cannot be dodged by entering
  through a different door. The shipped `gke-triage` roster was rebuilt around
  it: the twelve diagnosers gave up their write tools and name the remediation
  in their finding, and one new `change-executor` specialist holds the write
  surface behind its own report contract (`schemas/change-report.json`).

  This replaces a boundary that used to live in prompt text — "diagnose only,
  do not remediate" in a specialist's `.tmpl`. A prompt is a request; a
  construction-time refusal is a property. Rosters that relied on the prose
  version keep working only if they now say so in the field.

- **New: an operator can correct a call instead of approving it —
  `{"verdict":"edit"}`** (docs/v0.3-plan.md W2.5; parity row 13). The resume
  payload is three-valued: `approve`, `reject`, or `edit` with replacement
  `args`. An edit is checked before it runs — attributable to an
  authenticated approver, restricted to arguments the tool declares,
  schema-valid against that declaration, and re-adjudicated against policy, so
  an edit cannot turn a permitted call into a forbidden one. A tool that
  declares no input schema cannot be edited at all. What was applied is
  written to the session as a durable `AppliedEdit` record and printed by
  `mast sessions show`, because ADK re-issues the *original* call on resume:
  without that record the log would say the operator approved arguments that
  never ran.

- **New: the deploy base mirrors the capability split in Kubernetes RBAC**
  (docs/v0.3-plan.md W2.6; parity row 14). `deploy/base` grants the daemon
  cluster-wide read and nothing else, with secrets excluded; the permission to
  change a namespace is a separate, per-namespace apply under
  `deploy/remediation-target/`. `scripts/rbac-matrix.sh` proves both halves
  against a live or dry-run cluster.

  **Read the caveat before trusting the split on GKE.** GKE authorizes a call
  if *either* IAM or RBAC allows it, and mast reaches the cluster through the
  GKE MCP server as its Workload Identity principal, which
  `scripts/setup-wif.sh` binds to `roles/container.admin` — so today IAM, not
  RBAC, is what bounds the daemon, and the RBAC split documents intent rather
  than enforcing it. The narrowing (`WRITE_SCOPE=namespaced`, dropping the
  binding to `roles/container.viewer` and carrying writes on the namespaced
  Role) ships as an opt-in rather than the default, because the
  WIF-principal-to-RBAC-subject mapping has not been verified against a live
  cluster.

- **Fixed: the shipped `gke-triage` catalog named three tools the GKE MCP
  server does not have.** `rollout_undo`, `patch_resource` and `scale_resource`
  are not tools `https://container.googleapis.com/mcp` offers; its 23 real
  tools are now enumerated, each classified to match its own `readOnlyHint`,
  verified against a live `tools/list`. This mattered more than a typo
  normally would, in both directions. ADK's allowlist predicate is a map
  lookup on the tool name, so an allowlisted name the server does not offer is
  dropped **silently**: `change-executor` would have started, declared its
  write capability, passed the capability-split check vacuously, and held zero
  write tools. In the other direction, sixteen genuinely read-only tools were
  absent from the catalog, so default-deny-unknown would have parked ordinary
  reads at the write gate. Found by pointing the workload at a real cluster —
  no offline test could have caught it, since the fakes answer whatever is
  declared.

- **Known limitation: the change executor is operator-invoked, not
  automatic** (docs/v0.3-plan.md W7.0). The gate above is real, and the
  executor is the only specialist that could reach it — but in both shipped
  dispatch shapes an incident ends at a finding. A diagnoser names its
  remediation in prose, graph nodes are terminal, and the coordinator is
  instructed to return analysis only, so nothing hands the diagnosis to the
  executor. Closing that needs the finding to carry a *typed* proposed change
  rather than a sentence — scoped as W7.0 and deliberately not patched with
  coordinator prompt text, which is the same prompt-held boundary W2.4 just
  finished deleting. What v0.3 claims is the narrower statement: a remediation
  call cannot fire without an operator, and that survives `kill -9`.

- **New: `dispatch: fanout` — the whole roster runs concurrently over one
  incident, one synthesis specialist merges the findings, one approval gate
  on the merged report** (docs/v0.3-plan.md W3; parity row 7). A bundle sets
  `dispatch: fanout` and names a `_synthesis` specialist; every other
  specialist becomes a branch, run under a `fanout.max_concurrency` cap
  (default 4, `-1` for unbounded). Synthesis receives each analyst's reported
  payload plus an explicit list of who returned nothing — silence is
  reported, never filled in. `examples/workloads/ns-audit` is the shipped
  example: four read-only namespace analysts plus synthesis.

  Fan-out branches are structurally read-only, checked at construction. A
  roster whose analyst can reach a mutating tool is refused before the daemon
  serves, and so is one that grants an MCP server without enumerating its
  tools — under mast's default-deny-unknown predicate an un-enumerated grant
  *is* a grant of mutating tools. `request_operator_input` is refused too:
  every branch runs before the one approval gate, and a branch has no pause
  the outer graph could record. The shipped `gke-triage` roster is
  deliberately one of the refused ones — it holds `patch_k8s_resource`, which is
  why fan-out ships a second example rather than converting the anchor.

- **Fixed: an agent inside a parallel branch could not see its own tool
  results.** Not a regression — nothing shipped on that path before — but
  worth stating, because it decided the shape above. `workflow.ParallelWorker`
  suppresses every branch event that is not an output event, and an LLM
  agent's working memory *is* the session event list, so a branch agent's
  second model call received a prompt identical to its first: it re-issued
  the same tool call and looped until something cancelled it. The fan-out is
  built on ADK's `parallelagent` instead, which delivers branch events to the
  runner. Three things follow: a tool-using analyst works, per-specialist
  `max_turns` / `max_cost_usd` are enforced inside a branch (the meter
  buckets by event author, and a suppressed event is an unmetered one), and a
  branch's work is in the durable log where crash recovery can see it.
  Approving a merged report resumes without re-running any analyst, including
  after the daemon has been restarted.

- **Fixed: the shipped `gke-triage` example could not complete a run offline**
  (docs/v0.3-plan.md W1.4). Since `output_schema:` landed, the twelve
  diagnosers declare a report contract that becomes their `finish_task`
  parameters; the offline fake models (`--model=echo`, `--model=toolactor`)
  answered with a hard-coded `{"result": "<text>"}`, which violates it. The
  call was refused, retried unchanged, and the run died on the specialist's
  newly-enforced `max_turns` — `POST /inject` returned **500** on the bundle
  the README and `scripts/demo-spike2.sh` point at. Neither the schema work
  nor the budget work is individually at fault; the composition is, and
  nothing caught it because no test drove the shipped bundle end to end.

  The fakes now read the `finish_task` declaration off the request and
  synthesize a value for every property in it (`pkg/agent/schemafill.go`), so
  a roster can change its report shape without anyone touching a fake. Where
  no `output_schema:` is declared, ADK's default single-`result` wrapper is
  recognized and the previous text goes out unchanged. Two smaller fixes fell
  out: the echo model's unschema'd digest read `diagnosed from envelope:`
  with nothing after the colon when a specialist was reached as a
  workflow-graph node (the incident is now recovered from the whole history,
  not just the last user message), and `--model=toolactor` answered a
  classifier turn with prose, which routed every incident to `_fallback`
  instead of to its specialist.

- **New end-to-end acceptance harness: `scripts/uat-v0.3.sh`**
  (docs/v0.3-plan.md W1.4, tier U). It drives the **shipped**
  `examples/workloads/gke-triage` bundle — not a fixture — under
  `--dispatch=graph --model=echo`, credential-free and network-free, and
  asserts on what an operator actually receives: the report quoted in the
  approval prompt that `mast sessions show` prints. Three legs, 29
  assertions: a declared `output_schema:` is answered in full (every
  property, a valid enum member, one `finish_task` call, no budget error, and
  the routed specialist named rather than `_fallback`); the same roster with
  the schema removed falls back to the default wrapper; and a deliberately
  violating report is *refused* and reaches the gate with no result. The last
  two are what make the first a statement about mast rather than about the
  fake. `dev/ci/presubmits/e2e.sh` now runs this alongside the v0.2 spine —
  both, in order — so the durable-execution harness is added to, not replaced.

- **Per-specialist budgets are enforced, not just parsed**
  (docs/v0.3-plan.md W1.2). A specialist's declared `max_turns` and
  `max_cost_usd` are now ceilings on that specialist's own spend. The roster's
  declarations become per-agent scopes on the session meter
  (`internal/compose.MeterScopes` → `budget.Config.Scopes`), and
  `budget.Meter.Observe` buckets each usage event by its author, checking it
  against that specialist's ceiling and against the workload's. Composition is
  tightest-cap-wins by construction rather than by arithmetic — whichever
  ceiling a call crosses first stops the run — and when one call crosses both,
  the error names the specialist, because that is the more specific fact and
  the one an operator acts on. A specialist running on its own `model:` is
  priced at that model's rate, so a cheap analyst's tokens are no longer
  billed at the synthesizer's.

  Attribution is by `session.Event.Author`, which carries the agent's name on
  every dispatch shape mast builds — a coordinator's sub-agent tool, a
  workflow-graph node, the planner's `invoke_specialist`. The design docs had
  named `Event.Branch` as the eventual seam; it is empty in the
  coordinator/sub-agent-tool shape, so a branch-keyed meter would have metered
  nothing there while passing under graph dispatch. Corrected in
  `pkg/specialists`, `pkg/graph` and `docs/specialists-design.md`.

  Two limits, recorded in the `pkg/budget` package doc rather than left
  implicit: metering reads the event stream, so a ceiling is crossed *by* the
  call that reports it (the cap bounds spend within one call's overshoot, it
  does not pre-authorize a call); and a crossed specialist ceiling stops the
  session rather than handing the coordinator a refusal it could route
  around. Both need a seam in front of the model call.

- **Specialists can declare a report contract: `output_schema:`**
  (docs/v0.3-plan.md W1.3). The field names a JSON-Schema document
  (`.json`/`.yaml`/`.yml`) **relative to the `.tmpl` file's own directory** —
  a shared file rather than an inline block, because a report shape is a
  contract with its consumers and inlining it makes that contract private to
  one specialist. `pkg/specialists/schema.go` reads, type-normalizes and
  checks the document at *load* time, so a malformed contract fails the roster
  on startup instead of on the first turn that dispatches to it. Refused at
  load: unknown keys (`propertys:` constrains nothing), untyped nodes, arrays
  without `items`, objects with no `properties`, `required` names that are not
  properties, and a non-object top level.

  Enforcement is ADK's, and it is the same on both agent modes: in `Task` mode
  the schema becomes the `finish_task` declaration and a non-conforming call
  comes back as a validation error naming the missing key; in `SingleTurn`
  mode the reply is validated on the way out and the delegation is refused.
  Either way the model that asked sees the refusal and no result, so
  non-conforming output cannot become a specialist's answer. Mast gains no
  `Finding` Go type — the shape is a workload asset.

  The `gke-triage` bundle ships that asset:
  `examples/workloads/gke-triage/schemas/finding.json`, referenced by all
  twelve diagnosers (the `triage-classifier` router emits a token, not a
  report, and is deliberately unschema'd). The test asserts the twelve resolve
  to *one* file — twelve individually-valid schemas would satisfy "every
  diagnoser is typed" and still be the drift a shared asset exists to prevent.

  Fixed while wiring the deploy mirror: `50-statefulset-daemon.yaml` projected
  3 of the 13 specialists the ConfigMap carries. A ConfigMap `items:` list
  projects only what it names, so nine failure modes routed to `_fallback` in
  a real deployment with nothing in the logs to say a dedicated specialist
  existed.

- **Judge tier: the 31 parity scenarios are scored against a live provider**
  (docs/v0.3-plan.md W0.5). `internal/evals/judge` stands up a fixture cluster
  per scenario over lookout's read-only tool surface, runs one SRE agent
  against it, and grades the answer with upstream's `_QUALITY_PROMPT`
  verbatim — `{reasoning, score 1-5, specific, actionable, correct_diagnosis}`
  normalized to `(score-1)/4`. `evals --tier=judge`, with `--model`,
  `--grader`, `--provider`, `--out` and `--baseline`.
  `.github/workflows/evals-nightly.yml` runs it at 07:00 UTC against
  Anthropic-on-Vertex over keyless WIF and posts the board plus a delta
  against the previous night's artifact. It **reports and never gates**: no
  score, however low, can produce a non-zero exit — only "a scenario did not
  run" or "a metric scored nothing anywhere" can.

  This flips the last W0 parity row. First numbers over 31 scenarios against
  `claude-opus-4-7`: `intent_coverage` 0.917, `response_quality` 0.968,
  `severity_accuracy` 0.484, `tool_coverage` 0.000 (diagnostic — mast's read
  surface shares no tool *name* with the corpus, which is the whole reason
  `intent_coverage` is the primary metric), `effect_ordering` and
  `exactly_once` vacuous on every row because a read-only surface cannot
  produce a mutation to order.

  Structural ceilings are reported, never folded into the score: LC-13 expects
  a write tool, so its `intent_coverage` is capped at 0.50 and it scores 0.50.

  Pointing `--model`/`--grader` at compose's `echo` fake runs all 31 rows in
  ~2.5s with no credentials, so the tier's plumbing is exercised in CI without
  spending a metered run. That dry run is what caught the grade parser
  matching `(?s)\{.*\}` — greedy from the reply's first brace to its last, so
  a restated JSON contract or a quoted `{key='node-role', …}` made every row
  report "unparseable", which on a board is indistinguishable from a real
  failure. Now a brace-depth scanner taking the last complete object.

  Two defects the first live board found, both fixed before shipping: a tool
  that answered a scenario but whose half of the fixture was empty returned a
  bare header, which the model read as broken tooling and refused to diagnose
  around — an empty half now says the same thing a non-answering tool says.
  And the fixture-override rule was drawn at "only where the quoting rule is
  silent", which admitted rows whose sole derived observation was a bare
  identifier; it is now drawn at displacement — an override may enrich a thin
  fixture, but every span the quoting rule vouched for must still reach the
  agent.

- **ADK upgraded to v2.2.0** (from v2.1.0), ahead of the v0.3 workstreams that
  build on the code it changes.

  **Fixed by the upgrade: a cancelled workflow-graph run reported success.**
  Under v2.1.0 the scheduler reacted to invocation-context cancellation only in
  its `doneChan` select arm, and a ready `doneChan` does not have to win against
  a queued node completion — `cancelAll` never touches the parent context, so a
  run could drain and return cleanly after being cancelled. mast cancels
  in-flight turns from outside the graph (attach session eviction, the dispatch
  deadline, daemon-level abort), so this was reachable in shipped builds.
  `TestGraphRunSurfacesExternalCancellation` pins it, and was verified to fail
  on v2.1.0.

  Also improved: confirmation resume is now deterministic (re-dispatch follows
  request order instead of Go's randomized map iteration, which made the
  resumed calls, the assembled response, and the last-writer-wins `StateDelta`
  merge vary run to run) and no longer re-dispatches a call whose response is
  already in the event snapshot — i.e. no running a tool twice inside one
  resume. `memory.InMemoryService.Search` no longer races concurrent writes.

  **Wire-visible:** ADK put JSON tags on `session.Event`, `EventActions`, and
  `LLMResponse` — PascalCase to camelCase with `omitempty`. The attach `agent`
  frame embeds that struct verbatim, so the change is visible to SSE consumers;
  mast-web already reads both casings, and stored sessions decode either way
  (`encoding/json` matches field names case-insensitively). Attach protocol
  stays at 1.4.0: mast never specified that payload's shape.

  The bump drags dependencies ADK's own go.mod requires: MCP go-sdk 1.4.1 →
  1.7.0, genai 1.63 → 1.66, glebarez/sqlite 1.8.0 → 1.11.0, jsonschema-go
  0.4.2 → 0.4.3, gorm 1.31.0 → 1.31.2, grpc 1.82.1 → 1.83.0, plus otel/api/
  genproto. Note for the v0.3 approval work: MCP 1.7.0 enables SEP-2322
  multi-round-trip client middleware by default, which auto-fulfills
  server-initiated input requests. mast registers no elicitation or sampling
  handler, so such a request errors today rather than self-fulfilling — but a
  handler added later would satisfy server input requests without traversing
  mast's approval gate. `MultiRoundTripOptions.Disabled` is the off switch.

  Not fixed upstream: adk-go#1229 (the concurrent-append affordance tracked in
  #51). `session/database` is untouched in v2.2.0, so the ops-row convention
  stays.

- **The parity eval suite is a CI gate** (docs/v0.3-plan.md W0.4).
  `scripts/evals.sh` → `dev/ci/presubmits/evals.sh` → an `evals` job in
  `.github/workflows/ci.yml`, wired into `all.sh` so local == CI. Runs
  credential-free in a couple of seconds and prints one board. Exit 0 green, 1
  a scenario missed its declared outcome or a metric scores nothing, 2 the
  harness could not run. `--format=json` emits the same report in the shape
  W0.5's nightly will diff run-to-run.

  The deterministic tier does **not** score the 31 ported scenarios — a
  scripted provider does not choose, so replaying a fixture would assert only
  that the script equals itself. What it gates instead is that the measurement
  works: `CorpusReach` reports how many scenarios each metric can score at all,
  and a gating metric that scores nothing anywhere fails the run. Both of the
  upstream harness's custom-code evaluators are constant functions today, and
  the test suite reproduces both defects to prove the guard catches them.

  The expected-fail allowlist is the report's headline (`3 of 5` today), each
  entry printed with the workstream that removes it. There is deliberately no
  second allowlist file: `differentiators.Scenario.Expect` is the one record,
  and it is checked in both directions.

- **Fixed: the eval rig inherited ADK's default GORM logger**, emitting ~48KB
  of SQL chatter per suite run. `pkg/eventlog.Open` has silenced this since
  v0.1, but `internal/evals/differentiators` builds ADK's session service
  directly and never picked it up.

- **The five differentiator scenarios** (docs/v0.3-plan.md W0.3).
  `internal/evals/differentiators` drives the composed runtime — the
  `internal/compose` root, the `pkg/effects` outbox, a `pkg/budget` meter, a
  real SQLite session store — with a scripted model and checks one invariant
  per scenario: `E-exactly-once`, `E-ambiguous-refusal`, `E-budget-exhaustion`,
  `E-approval-rejected`, `E-approval-edited`. These are the situations the
  upstream harness structurally cannot express, because it scores a single
  uninterrupted trajectory against a list of expected tool names.

  Two of the five pass today; three are declared expected-fail against W1.2 and
  W2. The declaration is checked *in both directions*, so landing a capability
  without flipping its entry fails the suite by naming the entry to remove. A
  scenario has three outcomes, not two — Pass, Fail, and **Broken**, the zero
  value, for a fixture that could not produce a run. Broken is never
  allowlistable, and every scenario must return both a stated reason and a
  non-empty trace, so "the capability is missing" is always an observation
  about a run that happened rather than the absence of one.

- **Fixed: `TraceFromEvents` counted task delegations as effects.** The walk
  excluded engine control-flow and sub-agent-named calls, but only on the call
  side; a delegation's completion then had no recorded call to pair with and
  fell through the orphan-completion fallback, classified mutating by
  default-deny. Two delegations to the same specialist scored as one effect
  applied twice, and every trace's tool set was inflated with `finish_task` and
  the specialist's own name. The exclusion now applies to both sides.

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
