# Changelog

## Unreleased

- **Three of the four consequential misses are the same tool, and the board
  now says so.** `k8s_resource_top` is the only tool in the catalog that
  answers either saturation question, and two unrelated model families
  skipped it independently. Each miss is charged to a tool — but only when
  exactly one tool would have answered it. With 14 of the table's 19 intents
  served two or three ways, charging a miss to every candidate would triple
  the numbers and name no cause, so a miss with several possible answers is
  counted as shared rather than pinned on anyone.

  The ranking leads with how many corpus scenarios a tool is the sole answer
  for, then with how often it was actually missed. That order is deliberate:
  the first is a property of the fixture and the second is one model on one
  night, and the first is what says whether fixing the tool is worth an
  afternoon. Skipping a sole-source tool does not risk the miss, it
  guarantees it, which is a different fact from a model that chose a
  different route to the same answer.

  This exists to make an experiment readable. `judge.toolDescription` lives
  in this repo rather than in k8s-lookout, so a tool that keeps being skipped
  can have its description rewritten and the board re-run — and the nightly
  delta prints the result as `k8s_resource_top: 3 miss(es) → 0`. The scores
  would not have shown it: a fix worth keeping moves `intent_coverage` by a
  fraction of one row's mean. The competing hypothesis is that the models
  reason past the tool rather than fail to find it, and the same experiment
  tells the two apart.

- **The judged board names the questions a different tool choice would have
  answered — and there are four of them, not twenty-nine.** The obvious
  thing to measure is whether a run called the tools that had data for its
  scenario. That was computed against both v0.4.0 boards before being
  rejected, and the data is the whole argument: it flags 11 of 31 scenarios
  on Claude and 18 of 31 on Gemini, and the flagged rows score *better* on
  `response_quality`, not worse. Most skipped tools were redundant with one
  that had already answered the question — 14 of the intent table's 19
  intents are satisfiable by two or three different tools, which is mast's
  consolidated read path showing up in the measurement rather than a defect
  in it.

  What the board reports instead is a **consequential miss**: a question the
  corpus expected answered, that a tool in the catalog would have answered,
  that the run never asked. Across both boards that is four rows, all node
  or pod saturation, so it is printed as a list — the scenario, the intent,
  and the tools that would have served it — with a tally by intent, because
  the same intent recurring across rows is one tool's description or
  discoverability rather than several unrelated incidents. The nightly delta
  names each one that arrives and each one that clears.

  An unsatisfied question no read-only tool can answer is deliberately not
  in that list. LC-13 expects a rollback, lookout excludes write tools by
  design, and it is already reported as a structural ceiling; counting it
  here would publish a deliberate scope decision as a permanent
  tool-selection failure. An expected tool name the intent table has never
  seen is a third thing again — a gap in the fixture, which deflates
  `intent_coverage` on purpose but is nobody's miss. The score and the list
  are now derived from one shared reading of the corpus, so a board cannot
  say one thing in its number and another in its enumeration.

- **The judged board says how a tool was called, not just that it was.** It
  recorded which tools a run reached for and nothing about how, which left
  two different failures looking identical: the model never called the tool,
  and the model called the right tool against a namespace that held nothing,
  read the empty result as "no problem here", and reported anyway. Both
  landed as an unsatisfied intent; only the first is a tool-*selection*
  problem, and the fix for the second is not the same one.

  The trace now retains each call's result alongside its arguments — the
  arguments say what the model asked for, and only the result says whether
  it learned anything. ADK's `error` key rides in the same payload, which is
  what keeps a failed call distinguishable from an empty one. Both are
  persisted onto the board with the arguments, so a run can be read after
  the fact rather than re-run.

  Every recorded call is then checked against the schema the model was
  actually shown — read off the built tools rather than written beside them,
  so it cannot go stale — and the violations are **listed, not averaged**:
  an unknown tool name, a missing required argument, a wrong type, a value
  outside a declared enum, an invented argument, an errored call, a call
  with no completion, and a well-formed call that found nothing. A rate over
  rows that call different numbers of tools is not comparable to itself
  between runs, and the actionable part is never the mean — it is which
  tool, which argument, which scope. Malformed calls are printed one by one
  and empty reads are counted, because an empty read is ordinary triage
  right up until *every* completed read in a row is empty; those rows are
  now named, since a score on one of them measures what the model already
  knew about Kubernetes rather than what it read. The nightly delta reports
  the same counts, which move when no mean does.

  The fixture's clean reading is unchanged, deliberately. The signal that a
  read found nothing is returned beside the prose rather than encoded in it:
  a flag in the tool's payload would change what the model reads and move
  every score on the board in the same run that introduced the column.
  Arguments and results are recorded verbatim, which is the answer v0.4's
  W8 already settled for decision exports — a violation whose scope has been
  redacted is one nobody can act on — and the board states its own exposure,
  which for a corpus of synthetic fixtures is none.

- **The tool-schema invariant is now stated over every construction path, not
  the one that was noticed.** #154 was a total tool-calling outage: every
  mast-authored tool and every MCP tool reached Claude as a bare name with an
  empty input schema, because the converter handled only the typed
  `genai.Schema` spelling while ADK v2 populates `ParametersJsonSchema`. The
  regression test that shipped with the fix covers `functiontool.New` — the
  shape of that bug, not the shape of the bug class.

  New `internal/toolcatalog` assembles the tool catalog a real turn puts in
  front of a model, and it is **captured rather than enumerated**: a
  coordinator+MCP rig and a planner rig are driven through an ADK runner with
  a recording model, and what the model was handed is what gets asserted.
  Nothing lists the construction paths, so none can be forgotten — a tool
  wired into either shape is covered the next time the test runs. Today that
  is ten tools spanning all three paths: typed `Parameters` (`finish_task`,
  the delegation tool), `ParametersJsonSchema` via `functiontool.New` (the
  planner vocabulary, `pause_session`, the federation invoke tool), and
  `ParametersJsonSchema` via `pkg/mcp`. Expectations are derived from each
  declaration rather than written down, so they cannot go stale.

  `toolcatalog.Verify` states the invariant once, shared by every provider's
  test: every tool arrives, a tool with arguments arrives with a non-empty
  properties map, every argument name survives and none is invented, every
  required argument is still required, and each argument's own schema is
  still a non-empty object. An adapter held to a weaker check than its
  siblings is how a defect hides — the Gemini path was fine throughout #154,
  so nothing measured against it would have caught the Anthropic converter.

  Both adapters now have one. Anthropic's asserts the real Messages request
  body and fails on eight of ten tools with the #154 fix reverted. Gemini's
  is a dependency probe — mast converts nothing there, and
  `ParametersJsonSchema` is a pass-through `any` that nothing would complain
  about losing — plus a separate check that built-in tool injection sits
  alongside function declarations instead of replacing them. Both run
  offline, credential-free, in the ordinary `go test ./...` presubmit.

- **Every durable approval named the channel instead of the human.** The
  audit answer to "who approved this?" is `PauseRecord.ConsumedBy`, and it is
  most of the reason a durable gate is worth more than an in-memory prompt.
  The daemon wrote the literal `"operator resume --token"` into it on every
  HTTP resume, and `{"resumed_by": "operator"}` into the response the resumed
  turn sees — so an incident review could establish that a session was
  resumed over HTTP and nothing else. Both now name the caller the request
  authenticated as.

  The identity is read from the credential, and never from the request body.
  `ResumeRequest` deliberately has no approver field: an attribution a caller
  writes about itself is worth nothing after an incident, the same reasoning
  that makes attach's `GuardrailResetRequest.Caller` `json:"-"`. It is not in
  the token either — a token is a bearer capability, so a claim minted at
  pause time is a claim about whoever *later* holds it, and a token handed to
  a colleague has to attribute the colleague. Both halves of the plumbing
  already existed (`pkg/inject` resolves the caller onto the request context;
  `cmd/mast` renders it); the daemon just never read them.

  New `auth.Attribution(ctx, fallback)` renders the three shapes — an
  identity, `alice@example.com (asserted by sa:switchboard)` on the
  `X-Asserted-Caller` proxy path a chat relay needs, and the fallback. The
  fallback is a **mechanism name**, not `"unknown"`: a context with no caller
  is an in-process path (the timed-pause scheduler, boot auto-resume, an
  embedder calling the library), and the code always knows which. `"timer"`
  and the direct-DB CLI's `"operator resume --token --session-db"` keep their
  literals for that reason — the first is pinned normatively in
  `docs/durable-execution-design.md`, and the second is honest about a path
  with no credential to read. `mast.ResumeByToken` picks up an embedder's
  `auth.WithCaller` identity and otherwise says `library ResumeByToken`.

  What this does **not** do: nothing wires an `auth.Authenticator` into the
  daemon's inject listener, so a CLI-launched daemon records
  `shared-bearer-token` — honest about what a shared credential proves, which
  is that *someone* holding it resumed. Per-person attribution is reachable
  by embedders today and needs a config surface to reach the daemon.
  `newResumeByToken` was lifted out of `serve()` to a top-level constructor
  so the identity it records is asserted without standing up a daemon; an
  audit field nobody tests is a field that drifts back.

- **The intermittent `/metrics` failure in the v0.2 UAT was never about
  metrics: `pipefail` was reporting a match as a miss.** `assert_metric` ran
  `curl -s "$BASE/metrics" | grep -Fxq "$want"`. `grep -q` exits the instant it
  matches, which closes the pipe under curl; curl fails its next write and
  exits 23; `pipefail` reports the highest-numbered failure in the pipeline, so
  it promotes curl's 23 over grep's own 0 and the assertion fails on a metric
  line that is right there in the scrape. Whether it happened came down to
  whether the body had been fully written when grep quit — which is why it
  needed a loaded box to show up, and why it moved between assertions from run
  to run instead of sitting on one. Measured against the real 5,566-byte scrape
  with the match at line 12 of 89: **30 wrong in 600** for the piped form, **0
  in 600** for a here-string. End to end under `nproc` busy loops, the pre-fix
  script failed 3 of 4 runs; the fixed script, same load, 0 of 4.

  Nine sites across five scripts had the shape, and the interesting ones are
  not the UAT's. In `assert_hasnt` (v0.3, v0.4) and `assert_no_session_label`
  (v0.2) a *match* is the violation, so the promoted status turns a detected
  violation into a pass — these were failing **open**. So was the preflight in
  `scripts/live-kind-v0.4.sh`: `kind get clusters | grep -qx "$CLUSTER" && die
  ...` is a refusal to adopt a cluster the script did not create, and the `&&`
  could not fire, so the script would have gone on to adopt it. Every site now
  captures first and matches with a here-string, which is not a pipeline and so
  carries grep's status and nothing else's.

  `dev/tools/shell-lint` (wired into `dev/ci/presubmits/all.sh` and the
  `hygiene` CI job, with a `--self-test` that fails if the rule is ever
  defanged) keeps the shape out. It is deliberately one rule rather than a
  shellcheck adoption: this one has a measured failure rate and a fail-open
  direction behind it, and a second rule should have to clear the same bar.
  `scripts/uat-v0.3.sh` already carried a comment documenting this exact hazard
  for `awk`, written when one instance of it was fixed in `show_field` — a note
  next to one occurrence is not a guard against the other eight.

- **A pricing regen said 32 rows changed and could not say which one mattered,
  and the answer turned out to be two scheduled price doublings nobody had
  written down.** Every regen re-stamps `UpdatedAt` on every row, so the diff a
  reviewer opens is dominated by rows that did not move. On 2026-08-19 that was
  31 of 32; the one that did — `gemini-3.6-flash`, halved — read exactly like
  the other 31 from the PR body. `dev/regen-builtin-pricing --report` now writes
  a "What moved" section that `pricing-regen.yml` puts at the top of the
  auto-PR: the rows whose *values* changed, counted and itemized, computed from
  the same timestamp-normalized comparison `--check` already used. Silence in
  that section is now a claim rather than an absence.

  Chasing that one row is what turned up the rest. `gemini-3.6-flash` had not
  been permanently cut — Google had moved it onto `gemini-3.7-flash`'s
  introductory rate, and **both revert from $0.75/$3.75 to $1.50/$7.50 on
  2027-01-01**, cache reads doubling alongside. `gemini-3.7-flash` is the
  gemini/vertex frontier default, so that is a `max_cost_usd` ceiling buying
  half the tokens it is budgeted for, silently, from the day the rate changes.
  Both rows are now in `introductoryRates` with the date. `pkg/taskclass`'s
  frontier comment justified the promotion partly on 3.7-flash being "half the
  price" of 3.6-flash, which stopped being true the day after it was written —
  rewritten to rest on the UAT, which is what actually chose the model.

  In the other direction, `claude-sonnet-5`'s increase to $3/$15 was
  **cancelled**: the introductory $2/$10 is now Anthropic's standard price. The
  dated assertion shipped in v0.4.x would have failed CI on 2026-09-01 over a
  rate that is correct — a guard firing in the direction that gets guards
  deleted. `introductoryRate.resolved` records that outcome as data, and
  inverts the assertion to pin the rate at the former promotional number
  instead. A table whose only exits are "fires" and "gets deleted" cannot say
  that a price was provisional and then became standard.

- **The guard against a constant metric was reporting a constant metric as
  healthy.** `internal/evals/measurability.go` exists to catch a column that
  is green because it never ran, and its own doc comment names upstream's two
  constant functions as the cautionary case. mast's `tool_coverage` has been
  0.000 on all 31 rows of every board since the corpus was ported — the
  dataset expects upstream's `kubectl_*` names, mast emits `k8s_*`, and the
  intersection is empty — and the reach table read `31/31 scenarios scorable`
  for it throughout.

  The cause was a definition, not a line: reach asked whether a scenario had
  *declared* an expectation, when the question is whether the expectation can
  be *met*. `intent_coverage` sitting beside it never had the bug, because it
  builds its denominator after mapping names through the intent table — two
  definitions of reach in one file, and only the declaration-shaped one was
  wrong. `ToolCoverage` now marks a scenario vacuous when no expected name is
  one the runtime can emit, which the intent table's `lookout_tools` answers
  by construction. The score does not move: an unsatisfiable expectation still
  reports 0/N and still names the tools, because the consolidation penalty is
  why the metric is on the board. What changes is that the harness stops
  averaging that 0 and says the column is a constant.

  A dead **diagnostic** is now reported on a line of its own rather than
  appended to the run's problems. The previous release described that line as
  non-gating and implemented it as gating; nothing caught it because no
  diagnostic was dead yet. With `tool_coverage` permanently dead — a fact
  about two tool surfaces, not a defect — that would have turned the whole
  deterministic tier red.

- **A hub restart un-federated the deployment, silently.** `attach.PeerRegistry`
  was memory-only: every registration went with the process, and each peer
  stayed invisible until its own heartbeat failed and it re-registered. That is
  a 20-60 second window in which "who is in the fleet?" answers wrong rather
  than answering slowly — recoverable where somebody is watching, and mast's
  premise is that nobody is. `attach.NewPeerRegistryWithState` snapshots the
  registry to a JSONL file on every mutation and reloads it at startup, so the
  hub comes back already knowing its fleet. Ported from core-agent
  `9f81626`; registrations are still in-memory by default, and
  `NewPeerRegistry` is unchanged.

  Two things the state file is careful about. It is **not** the wire shape:
  `Peer.Owner` is `json:"-"` because discovery responses must not leak it, so
  persisting `Peer` directly would reload every registration ownerless and
  quietly undo the enumerate-then-delete hardening — a separate on-disk record
  carries the owner, and the restart is tested through the HTTP handlers to
  prove the redaction survives it. And **a lease is re-clamped to the running
  configuration's ceiling**, not replayed at the width it was granted: this is
  what #166 settled for budget grants, arrived at again. Lower the max TTL and
  restart, and an unclamped reload would honour the withdrawn ceiling twice —
  once for the outstanding lease, then indefinitely, because a heartbeat renews
  at whatever width it reloaded. That divergence from upstream is mast's; the
  clamp runs before the expired-lease drop and is written back, so a peer only
  live under the old ceiling is not live at all.

  The daemon is unaffected: `cmd/mast` has no peer-hub flag, so
  `attach.Options.PeerRegistry` remains an embedder-only surface. Upstream's
  `--attach-peer-state-file` half of the port has nothing to hang on here and
  did not land.

- **`severity_accuracy` was partly measuring markdown, and the rest of it
  cannot be acted on.** Two findings, one metric.

  The extractor read `## SEVERITY: CRITICAL` as no severity at all. It
  stripped the `Severity:` label before the markdown decoration, and the
  label pattern is anchored — so a line carrying both matched neither
  pass, and the run was recorded as having declared nothing. On the
  2026-08-19 boards that hit **7 of 31 rows on Claude and 0 of 31 on
  Gemini**: the published Claude figure of 0.419 is 0.548 once the
  decorated verdicts are read, and part of the cross-family gap the
  nightly has been reporting was one family's heading style. A missing
  verdict and a wrong verdict both score 0, which is why the column read
  as a model that could not classify rather than a harness that could not
  parse. `rig.go` already named this hazard — a coordinator rewrite would
  "move severity_accuracy for a formatting reason rather than a capability
  one" — for a different formatting change.

  The remaining misses cannot be attributed to anyone. #179 asked for them
  to be partitioned against the corpus's *own* severity definitions:
  a row where the label contradicts the definition it ships with is a
  corpus defect, one where the agent escalated past a definition the label
  honours is mast's. That partition is not derivable. The definitions are
  twelve enumerated conditions, taken verbatim from upstream — CRITICAL is
  "service down, crash loops, OOM kills, 0 ready endpoints", WARNING is
  "no PDB, missing probes, :latest images, wildcard RBAC", INFO is
  "right-sizing, orphaned PVs, suspended CronJobs". Measured over the
  corpus, **the CRITICAL conditions occur in 5 of the 31 scenarios and the
  WARNING and INFO conditions in none**, while 20 of the 31 rows are
  labelled WARNING or INFO. On almost every row that misses there is no
  stated definition to partition against, so "the corpus is wrong" and
  "the rubric escalates" are indistinguishable in principle, not merely by
  this metric. Where the rubric does decide a row it contradicts the label
  twice in five — LC-03 is an OOMKill labelled WARNING, LC-10 has 0
  endpoints and is labelled WARNING — and both model families miss both by
  answering what they were told to answer.

  So `severity_accuracy` is now **diagnostic**, the disposition
  `tool_coverage` has: still on every board, still per-row legible, no
  longer a parity claim. Tuning the rubric until it moves was rejected in
  v0.3 as teaching to the test and there is nothing here to tune against
  anyway. `judge.TestSeverityRubricDoesNotSpanCorpus` pins the measurement
  that justifies the demotion and fails if either the corpus or the rubric
  moves toward covering the other — which is the condition for promoting
  it back.

  The demotion opened one gap, now closed: `evals.DeadMetrics` names
  gating metrics only, so a diagnostic that can score nothing anywhere
  would have become a silent column instead of a harness failure. The
  corpus summary reports that case separately — it does not gate, and it
  still says it is reporting a constant.

- **A budget ceiling a restart resets is a ceiling on what a workload spends
  per process.** `newMeterPool` minted every session's meter at zero, so a
  daemon restart handed each session its full cap back: a workload stopped by
  `max_cost_usd: 5.00` after $5.02 resumed with $5.00 available. mast's
  restarts are automatic and unattended, so a crash loop could spend the cap
  once per restart, indefinitely — the exact situation the ceiling was bought
  for. #166 made the watchdog halt durable and deliberately stopped here,
  because a trip is one latched bit and spend is an accumulator that has to
  come back to the cent.

  It comes back as a ledger: one row per priced model call in
  `agent_budget_spend`, written through a new `budget.Config.OnSpend` hook and
  folded into a fresh meter by `budget.Meter.Restore` on a session's first
  touch after a restart — session totals and each specialist's own, so scoped
  ceilings survive too.

  **Why not replay the session's own events**, which would have needed no new
  state: ADK's database session service persists `UsageMetadata` and `Author`
  but has no column for `ModelVersion` (`session/database/storage_session.go`,
  adk/v2 v2.2.0). Every replayed call would therefore miss `pkg/pricing`'s
  catalog and fall back to the flat per-1K rate that `pkg/budget` measures at
  5.9x error on a cache-warm session, and a restored ceiling that wrong is
  worse than none because it looks right. Replay would also price history at
  today's weekly-regenerated rates, rewriting money that already left the
  account, and its cost grows with the transcript — on the first turn after
  every restart, for exactly the long sessions that survive restarts. Against
  checkpointing the accumulator: that has to be reconciled against a
  transcript that may have advanced, where a ledger written at the same
  granularity the accumulator moves has nothing to reconcile. Its only loss
  window is a call whose row had not landed, which is always an undercount,
  never a phantom charge.

  **Operator grants are replayed now too**, which #166 explicitly declined to
  do — correctly at the time, since raising a ceiling over an accumulator that
  had forgotten what it spent is arithmetic on a number that no longer means
  anything. With the spend durable that inverts: a session an operator rescued
  at $5.02/$5.00 would otherwise come back with the spend and without the
  rescue. This needed `GuardrailRecord.GrantScope`, because replaying a
  specialist's grant onto the session would raise a cap by an amount nobody
  granted it. A pre-#175 row carries no scope, cannot be attributed either
  way, and is skipped — it still counts toward the audit total, which is why
  that total and the replayed figure can differ on an old database.

  Durable spend also makes a wedged session permanently wedged, so a turn on a
  session already past its ceiling is now refused **before** the model call
  rather than after it. Enforcement is derived from usage-against-ceiling on
  every priced event, so without the preflight a scheduler retrying every
  minute would buy one model call a minute forever. The refusal is a `409`
  naming `POST /sessions/{id}/guardrails/reset`, the same shape the watchdog's
  is.

  Restore fails open the way the watchdog's does — an unreadable ledger logs
  and the turn runs — and the read is retried next turn rather than latched as
  "restored to nothing". All of it requires `--attach-listen`, which already
  requires `--session-db`: with no durable connection the pool behaves exactly
  as it did before, and a daemon with budget ceilings and no attach surface now
  warns at startup, as one with `--watchdog=enforce` already did.

- **A rate can be current in its source and still be wrong, and nothing in
  the table could say so.** `claude-sonnet-5` carries Sonnet 5's
  *introductory* price of $2 in / $10 out per MTok, which lapses
  2026-08-31 in favour of $3 / $15. `Rates.UpdatedAt` cannot catch that:
  it records when the row was last read from LiteLLM, and the weekly regen
  re-reads LiteLLM, gets the same promotional number back, and stamps a
  fresh date on it — so the freshness signal reports maximum confidence in
  a value that has become false. The 2026-08-19 regen did exactly this,
  re-dating the row to the day with no change to the price.

  It cannot be fixed at the source either: LiteLLM's catalog carries no
  expiry metadata on any row.

  `pkg/pricing/introductory_rates_test.go` now holds the expiry as a dated
  assertion — past the end date, a row still reading the promotional rate
  fails the build. It runs inside `pricing-regen.yml` as well as ordinary
  CI, so an expiry LiteLLM has not caught up with stops the regen rather
  than being rubber-stamped by it. This matters beyond `/pricing`:
  `claude-sonnet-5` is the Anthropic mid-tier default, so an understated
  rate lets a workload spend past its `max_cost_usd` ceiling before the
  guardrail trips.

  Crude on purpose, and scoped to the one row we know about. #188 tracks
  the general version — a `provisional_until` field, `/pricing` marking
  provisional rows, and a regen that reports which rates actually moved
  instead of reading identically whether any did.

- **Gemini could only reach Vertex AI through an environment variable.**
  `--provider` had no value that named the backend: `BuildModel` handed
  genai an empty client config and let it decide from
  `GOOGLE_GENAI_USE_VERTEXAI`. Claude got an explicit
  `anthropic-vertex` alias at port time; Gemini did not, and the tier
  table has carried a `vertex` family since the port that nothing could
  ask for (`pkg/taskclass.Providers()`).

  The failure mode is the reason this matters more than the ergonomics.
  A deployment whose credential is the service account's ADC — Cloud
  Run, GKE with Workload Identity — that forgot the variable did not
  get "you meant Vertex, say so". It got an API-key error from the
  *other* backend, naming a credential it deliberately does not have,
  which points away from the fix.

  `--provider=vertex` now serves `gemini-*` against Vertex outright:
  `Backend: genai.BackendVertexAI` with the project from
  `GOOGLE_CLOUD_PROJECT` and the location from `GOOGLE_CLOUD_LOCATION` /
  `GOOGLE_CLOUD_REGION` (falling back to genai's own `global`). A
  missing project fails at construction naming the variable, not inside
  the SDK as a struct dump. This is core-agent's `vertex` provider
  (`pkg/models/gemini.NewVertex`) ported, down to the name, so the two
  repos describe the same thing the same way.

  The env-var path is untouched: with no alias, genai's env-driven
  selection still decides, and `geminiClientConfig` hands it the same
  empty config it always did. What changes is that there is now a way to
  be explicit. Empty-chunk tolerance follows the backend actually in use
  rather than the environment, so a Vertex run by alias no longer reads
  Vertex's candidate-less heartbeat chunks as malformed.

  One adjacent fix, since the same switch decides it: a Gemini-family
  alias no longer refuses a `claude-*` specialist override. The alias
  picks a *Gemini* backend and says nothing about Anthropic's, so the
  override now detects its own backend the way an unaliased run does —
  which is what `NewModelResolver` has documented as allowed since
  cross-provider overrides were resolved (2026-08-12).

- **An MCP server could turn a call around and ask mast for input, and mast
  would answer.** The MCP protocol lets a server request elicitation,
  sampling, or the client's roots in the middle of a call the client already
  made. mast's approval gate sits on tool *dispatch*: an operator approves a
  specific call and that call runs. A server-initiated request happens
  inside a call the gate has already cleared, so nothing the operator
  approved covers it — and the SDK answers it and then **retries the
  original call**. Against a stateful HTTP fixture, one approved dispatch
  ran the server's tool 20 times.

  This was filed as latent, on the reasoning that mast registers no
  elicitation or sampling handler so such a request would error. That holds
  for those two. It does not hold for `roots/list`, which go-sdk answers
  itself out of the client's own root set — no handler, no capability
  opt-in, no error — so the round trip completed against mast today.

  The documented off switch, `MultiRoundTripOptions.Disabled`, is also only
  half of it: the client-side middleware it governs runs on protocol
  ≥ 2026-07-28, and `StreamableServerTransport.SupportsProtocolVersion`
  serves that version only when the server is stateless. An ordinary
  stateful HTTP server negotiates 2025-11-25 and its *server*-side
  middleware sends the request directly, which `Disabled` never sees.

  `pkg/mcp` now builds its own MCP client for both transports, with the
  middleware disabled, no client capabilities advertised, and a receiving
  middleware that refuses `elicitation/create`, `sampling/createMessage`,
  and `roots/list` by name. Refusing by method rather than by omitting a
  handler is the point: a future elicitation handler, added for a good
  reason, has to delete a line someone reviews instead of silently
  acquiring a gate bypass. Both protocol regimes are pinned in
  `pkg/mcp/mrtr_test.go`, alongside two tripwires that fail if the SDK's
  defaults move.

  An MCP server that needs elicitation to complete a tool now fails against
  mast rather than proceeding unapproved. On an unattended deployment there
  is no operator to answer the question anyway; supporting elicitation with
  the gate wired through it is separate work.

## v0.4.0 (2026-08-17)

*An operator approves the exact call that will fire, the loop runs on a
schedule without an orchestrator, and every verdict becomes a labelled eval
row.* v0.3 put an operator in front of every mutating call; v0.4 makes what
they approve a typed object rather than a paragraph, and closes the honest
gap v0.3 shipped with — the change executor was unreachable from a diagnosis
in both shipped dispatch shapes.

Parity scoreboard: **11 of 19** rows green, up from 7 at the v0.3.0 tag
(`docs/v0.3-plan.md` §1). The four this release is accountable for — the
per-specialist model tier, the scheduled trigger, the bounded analysis path,
and the decision→eval feedback loop — are all flipped. The remaining eight
are v0.5's, and seven of those are halves of surfaces k8s-lookout and
switchboard have not landed yet: the parity claim is v0.5's, not this one's.

Landing alongside that scope rather than inside the claim above: the
behavioral watchdog port — two more detectors, the `feedback` and `enforce`
postures, a `safety.watchdog` field on the bundle, and halt state that
survives the process. **It changes a default.** A workload that never typed
`--watchdog` ran at `warn`, which on an unattended deployment is off with
extra steps: the alert goes to a pod log nobody is tailing. The default is
now `feedback`, so a detected runaway is told to the party that can stop it.
`--watchdog=warn` restores the previous behavior; the entries below give the
reasoning for the divergence from upstream's `enforce`.

- **Claude could not see the arguments of any tool mast defines.** Every
  mast-authored tool — the planner's dispatch tools, `pause_session`,
  and every MCP tool — reached the Anthropic wire as
  `{"type":"object","properties":{}}`: a name, a description, and no
  parameters. The model had to infer argument names from prose.

  ADK v2's `functiontool.New` derives a declaration into
  `ParametersJsonSchema` and leaves the typed `Parameters` field nil;
  `toolsParam` handled only `Parameters` and sent the canonical
  "no arguments" shape for everything else. The declarations ADK builds
  internally — `finish_task` among them — *do* use `Parameters`, which
  is why this survived a green nightly and a live GKE run: the failure
  is silent degradation on mast's own tools, not an error anybody sees.
  Gemini is unaffected; it takes the genai declaration natively.

  Found by triaging the first drift report (below) against
  `core-agent@ee3d6ec`; upstream fixed the same gap in their #754. The
  regression test builds its declaration with `functiontool.New` rather
  than by hand — a hand-built `genai.Schema` fixture exercises the
  branch that always worked, which is how this survived the existing
  tool tests.

- **A turn could die on the eventlog's write lock instead of waiting for
  it.** SQLite takes one database-wide write lock, and mast opens two
  connection pools against the same file — ADK's session service and the
  overlay that assigns event seq numbers. Under the default deferred
  `BEGIN`, an `AppendEvent` reads the session row first, taking a read
  snapshot, and the first write then attempts a snapshot→write upgrade.
  SQLite refuses that upgrade with an *immediate* `SQLITE_BUSY` when
  another connection holds the lock — `busy_timeout` deliberately never
  retries an upgrade, because two upgraders waiting on each other is a
  deadlock. So the 5s busy timeout mast already sets did nothing for the
  one case that bites.

  `eventlog.Open` now injects `_txlock=immediate` on the SQLite DSN, so
  every read-write transaction begins with `BEGIN IMMEDIATE` and waits
  on `busy_timeout` like an ordinary contended writer. Reads (`Get`,
  `List`) use no explicit transaction and stay lock-free under WAL.

  `pkg/eventlog/service.go`'s write mutex reads like it already covers
  this and doesn't: it serializes writes made *through the service
  wrapper*, not writes another connection makes on the same file. The
  concurrent writer here is the daemon's own machinery — the scheduler
  firing a cadence, auto-resume replaying a marked session, an A2A or
  AG-UI submission, an attach inject. Unattended is the shape that hits
  this most often and has nobody watching when it does.

  Ported from core-agent's #576, found by the triage below; upstream hit
  it through auto-continue, which mast does not have. The regression
  test holds the write lock on an independent connection and fails on
  pre-fix code with `database is locked (5) (SQLITE_BUSY)`.

- **A Vertex context cache that fails to be created is retried, instead
  of disabling caching for the daemon's lifetime.** `Caches.Create`
  failures split into two classes — context cancellation retried,
  everything else permanently sticky — and IAM errors landed in the
  permanent class. That is exactly backwards for the way a fresh
  deployment fails: a pod that starts before its Workload Identity
  binding has propagated gets `403 PERMISSION_DENIED` on its first
  turn, becomes correctly configured a couple of minutes later, and
  pays full input price for the rest of its life regardless. mast is
  the deployment shape this happens to — an unattended daemon starts
  when its controller schedules it, not when its bindings land, and
  nobody is watching the first turn.

  A non-context failure now gets 15s, 30s, 1m, 2m, 4m — six attempts
  over roughly eight minutes — before the manager gives up for good,
  which preserves what the sticky failure was protecting: a genuinely
  misconfigured project stops being hammered, loudly. Retries are
  demand-driven off `Init`, so the schedule is a floor on spacing
  rather than a timer and an idle daemon issues no RPCs at all. Each
  attempt logs its number and next eligible delay, so a rollout can be
  read as "waiting on IAM" versus "fix your config".

  Ported from core-agent's #723. The two new tests fail on the old
  sticky behavior.

- **The watchdog gained two detectors, and learned to read tool results
  as well as tool calls.** Its one signal — five identical calls in a
  row — is structurally blind to the loop that actually happens: an
  agent alternating between two tools, `list_agents → check_agent`
  forever, where no call is ever followed by itself.
  `alternating-tool-cycle` scans for a repeating short cycle instead.
  Argument comparison is also path-aware now, so `main.go`, `./main.go`,
  and `/workspace/main.go` are one file rather than three; the match is
  on a path-component boundary, so `a/doc.go` and `b/doc.go` stay
  distinct.

  `tool-failure-streak` is the one to read in a report. It fires when
  three tool calls in a row all return errors with none succeeding
  between, which is the state where a workload writes a confident
  summary of a system that nothing it ran could reach. The calls look
  ordinary; the results are the story. No prose is inspected — a
  detector that tried to recognize an over-confident claim would be a
  heuristic about English wearing the costume of a runtime guarantee.

  Outcome observation is an optional extension (`ToolResultObserver`)
  rather than a widening of the `Watchdog` interface, which is a
  documented plug-in point: a third-party watchdog should not break to
  gain a signal it may not want. Responses dedup against the same
  per-turn set as calls under a separate key space, since the streaming
  aggregator re-emits both and a double-counted failure would trip the
  streak at half its threshold.

  Ported from core-agent's #679 and #690 — the first two of seven; see
  `docs/sibling-sync.md` for the rest of the cluster and the
  sync-classification call that unblocked it.

- **`--watchdog=enforce`: the loop detector can now stop the loop, in
  the turn where it is happening.** The default is unchanged
  (`--watchdog=warn`, log and keep going), because a false positive
  that stops an unattended incident responder mid-triage is worse than
  one that annotates it. What is new is that the other posture exists,
  and that choosing it does something during the incident rather than
  after.

  Two things had to change for it to be worth having. First, alerts now
  drain **in-turn** — as soon as an observation lands — not only at the
  turn boundary. The loop the watchdog catches lives *inside* one turn:
  the model emits a tool call, the flow runs it and calls the model
  again, all within a single `Run`. A turn-boundary drain never fires
  while that is happening, and never at all if the turn does not end.
  That was a real gap in warn mode too — a looping session logged
  nothing until it stopped looping. Second, `repeated-tool-call` and
  `alternating-tool-cycle` are **Critical**; `tool-failure-streak`
  stays **Warn**, deliberately, because stopping a daemon three denials
  into a legitimate RBAC probe would make the backstop the outage.
  Severity is a property of the pattern; the posture decides the
  reaction.

  Under enforce, the first Critical alert cancels the turn through the
  same context handle a budget trip and an operator abort use, and the
  session refuses **every** subsequent turn — auto-resume, a scheduled
  fire, an attach inject — at the `runTurnPre` chokepoint, until
  `POST /sessions/{id}/guardrails/reset`. Structural, not advisory: a
  refusal only at the entry point an operator happens to use is a
  refusal that auto-resume walks straight through, re-driving the loop
  that tripped it. Turns that end this way are counted as
  `watchdog_halt` on `mast_turns_total`, and
  `GET /sessions/{id}/guardrails` reports `advisory: false` under
  enforce whether or not it has fired yet — "will this thing stop my
  agent?" is the question the field answers.

  Upstream keeps this state on its `Agent` struct; mast has no `Agent`,
  so it lives in a `watchdog.Enforcer` the daemon holds per session and
  the halt itself is the caller's `cancel()`. The package decides
  *whether* a session is halted and why; it never decides how a turn
  dies. Ported from core-agent's #628 and #719.

- **`--watchdog=feedback`: the loop detector tells the party that can
  stop the loop.** Every posture before this one routed the observation
  to an operator. On an unattended workload — the deployment mast exists
  for — that operator is a pod log nobody is tailing, and even a
  watching one can only interrupt a turn already in flight. The model
  choosing the next tool call is the only party that can decide not to
  make it, and it was the one party never told.

  Each alert now carries a model-facing `Guidance` sentence alongside
  its operator-facing `Reason`, and from `feedback` up they are
  prepended to the session's next prompt as a `[watchdog]` block framed
  as *an automated observation about your own previous turn — this is
  not a message from the user, and the user cannot see it*. The split
  matters because the readers can do different things: mast's operator
  text names `POST /sessions/{id}/interrupt` and the bundle's budget
  ceiling, and telling a looping model about an endpoint it cannot call
  is at best noise. A test asserts every shipped signal sets `Guidance`
  and that none of them leaks an operator control, so a fourth signal
  cannot ship with only half the prose.

  The three postures are now a ladder — `warn` < `feedback` <
  `enforce`, each including the one before it. **`enforce` implies
  `feedback` deliberately:** an enforce halt is cleared by an operator
  reset, and a reset resumes a model whose context still ends in the
  loop it was halted for, so without the injected observation the very
  next turn re-issues the same call and the reset is a treadmill. For
  the same reason a reset clears the halt but **keeps** the queued
  observation. The queue is bounded at four (oldest dropped), nothing is
  queued below `feedback` so flipping a long-running deployment up a
  rung cannot deliver a stale backlog, and it drains on read so an
  observation lands exactly once even if that turn fails.

  `feedback` is a correction, not a backstop — nothing stops a model
  that reads the block and loops anyway, which is why a workload with a
  bounded tool loop still wants `enforce`, which now gets both. It is also the one rung
  that does not apply to one-shot mode, whose whole mechanism is a next
  turn a one-shot does not have; it logs a line saying so rather than
  quietly running `warn`. The block is steering, not a trust boundary.
  Ported from core-agent's #678, and the commit that settled which side
  of the fork this feature lives on — shared infrastructure, not
  lean-fork-specific. See `docs/fork-design.md` § "Sync discipline under
  (E)".

- **A workload ships its own watchdog posture, and the default is no
  longer "off".** The three postures above were reachable only through
  `--watchdog`, and unset meant `warn` — log the alert and keep going.
  On mast that is off with extra steps: every mast run is unattended, so
  the operator the log is addressed to is a pod log nobody is tailing.

  Bundles now declare `safety.watchdog: warn | feedback | enforce`, and
  the posture resolves from three sources in order — the `--watchdog`
  flag, then the bundle, then mast's default. The bundle is where it
  belongs because the bundle is mast's deployment unit; the flag stays
  above it so an operator debugging a halted workload can drop the
  posture for one run without editing, and later forgetting to revert,
  the deployed manifest. The daemon logs which source won at startup
  (`watchdog posture resolved mode=… source=…`) — `enforce` otherwise
  announces itself only by refusing a turn. An unrecognized posture is a
  load error naming the field, not a silent fall back to the default:
  the failure mode being avoided is a workload whose author believes the
  halt is armed.

  **The default is now `feedback`.** Upstream defaults an unattended run
  to `enforce` and the premise is right — a warn-only backstop on a
  deployment nobody watches is not a backstop. The conclusion is not
  mast's: `alternating-tool-cycle` has a workload-shaped false positive
  (a scheduler-driven daemon watching a rollout settle calls the same
  tool with the same arguments on purpose), and on an unattended
  deployment a false halt is an outage that waits for the morning, where
  a false `feedback` costs one paragraph the model may disregard.
  Recoverable beats unrecoverable when nobody is watching. Set
  `safety.watchdog: enforce` on a workload whose tool loop is bounded by
  construction. Two tests pin the default, because this changes runtime
  behavior for every deployment that never typed the flag.

  This also closes a gap that was mast's alone: the library-embed
  surface (`mast.RunWorkload`) ran its turn with **no watchdog tap at
  all** — the one mast surface with no runaway backstop of any kind. It
  now reads the same `safety.watchdog` field and taps the same signals,
  with the rungs bounded by what a library call holds: `enforce`
  abandons the runaway turn, but there is no cross-call session state
  for the "refuse every later turn" half and no next turn for `feedback`
  to inject into.

  Ported from core-agent's #665 — the fourth of seven. The commit's
  other half, a `$10` per-session cost ceiling armed by default, is
  deliberately **not** ported: upstream keys it on a run being
  unattended, which in mast is a constant, and a fixed dollar cap on the
  lifetime of a `single_session` daemon is a scheduled outage rather
  than a runaway guard. `docs/sibling-sync.md` § "Deliberately not
  ported" carries the full reasoning.

- **A halt that a restart cleared was not a halt.** `--watchdog=enforce`
  latched its trip in a map on the daemon's `watchdogPool`, so a crash,
  an OOM kill, or a pod roll started a fresh process with the backstop
  disarmed. That is the one deployment shape enforce mode exists for:
  mast's restarts are automatic and nobody is watching, so the runaway
  loop → halt → crash → restart cycle could repeat indefinitely, each
  restart handing the loop a clean slate.

  Trips and resets are now appended to `agent_guardrail_log`, a table
  mast owns on the connection `pkg/eventlog` already holds, and folded
  forward on a halted session's next turn — before any model call,
  whichever surface drives it. The reset is durable in the same place,
  so clearing a halt clears it for good, and the row records the
  authenticated caller and any runway they granted. That upgrades the
  reset audit from a log line to a queryable row, closing a gap
  `cmd/mast/guardrails.go` had flagged in a comment.

  Three constraints worth knowing. Persistence is wired **only under
  `--attach-listen`**, because `POST /guardrails/reset` is attach-only
  and a persisted halt with no reachable reset is a brick rather than a
  backstop; enforce mode without an attach listener now says so at
  startup. **Configuration still wins over history** — a deployment
  dialed back to `feedback` does not inherit a halt only `enforce` could
  have produced. And restore **fails open**: an unreadable guardrail
  table logs a warning and lets the turn run, because a storage fault
  must not halt every session in the deployment with no trip behind it.

  Ported from core-agent's #671 — the last of seven, closing the
  watchdog cluster — but by a different route. Upstream writes both rows
  into the ADK session's own event stream from inside its agent's write
  lease; mast has no `Agent` to hang that lease on, and an out-of-band
  session append while a turn holds the handle trips ADK's
  optimistic-concurrency check. A table mast owns has no such
  contention. **Budget spend still does not survive a restart** — the
  meter starts every process at zero — which is a larger, separate piece
  of work, recorded in `docs/sibling-sync.md` § "Found while porting,
  not fixed here" rather than quietly folded into this.

- **An MCP server that rejects a call now says why.** Google's GKE MCP
  surface answers an IAM denial with HTTP 403 and a body naming the exact
  permission to grant. The operator saw `Forbidden`. HTTP MCP transports
  are now wrapped so a 4xx/5xx carries the server's own text — the
  permission name, the quota metric — through to the log, the model, and
  the attach surface.

  Narrower than the upstream fix it comes from (core-agent's #305,
  `b1101f9`), because the MCP SDK closed most of that gap in the interval:
  go-sdk v1.7.0 surfaces a standard `{"error":{"message":..}}` object
  itself. mast covers only what v1.7.0 still drops — the MCP tool-result
  error shape (`{"result":{"isError":true,...}}`, which decodes as a
  *result* and falls through to the status line, and is precisely what
  the GKE surface sends) and transient statuses, where the SDK returns
  early without reading the body at all. Standard error objects on
  non-transient statuses are left to the SDK, so the typed
  `*jsonrpc.Error` stays in the chain.

  Three things it deliberately does not touch: anything but POST, so the
  SSE reconnect keeps its retry budget; 404, whose `ErrSessionMissing`
  sentinel is worth more than the body; and any response that is
  non-JSON, over 32 KiB, or has no extractable text — all replayed
  intact. An extracted error does not tear down the MCP session.
  `TestSDKStillDropsTheseBodies` drives each case through the bare SDK
  and through mast's wrap in the same run, so the day the SDK closes one
  of the remaining holes it fails with instructions to delete the branch.

- **How far behind core-agent each ported package has fallen is now a
  number, reported weekly.** 182 files in this repo carry an
  `// Originally derived from go-steer/core-agent@<sha>` trailer, frozen
  at four different upstream snapshots, and nothing reconciled them —
  `pkg/pricing` went five weeks behind while three model families
  shipped, and it surfaced because a human noticed.

  - `dev/upstream-drift` answers `git log <ported-sha>..origin/main --
    <upstream-path>` for every trailer it finds, and aggregates by
    package. It is deliberately not a diff: every ported file already
    differs from upstream by construction (the trailer itself, `~/.mast`
    paths, mast-flavored comments), so a diff is 100% noise. Trailers
    may carry an explicit `:<upstream-path>` suffix for files renamed on
    the way in — three of them turned out to need one
    (`pkg/watchdog/bridge.go` ← `pkg/agent/watchdog.go` and two more),
    which is the bug class the tool has a distinct `missing-path` status
    for: an upstream rename makes the query return zero commits, and
    zero commits reads as "in sync". Once watchdog's real upstream path
    was known its drift went from 4 commits to 9.
  - `.github/workflows/upstream-drift.yml` runs it every Monday into one
    long-lived tracking issue, edited in place so a quiet week doesn't
    re-notify. It never fails a build — drift is the expected state
    here, much of it in code P1.3 will re-port wholesale, and a red
    build nobody can turn green trains people to ignore red builds.
    `GITHUB_TOKEN` suffices, unlike `pricing-regen`: the org restriction
    is on creating *pull requests* and the loop guard is on *pushes*.
  - The point is the deferral in `docs/fork-design.md`, which says to
    revisit extracting an `agent-substrate` shared library once "the
    shared surface has stabilized" without saying how to tell. Now
    there's a weekly sample. Baseline at first run: **48 upstream
    commits across 53 of 182 ported files in 17 packages**, against
    core-agent `main` at `ee3d6ec`.
  - `dev/ci/presubmits/fmt.sh` now covers `dev/`, which has held real Go
    programs since `regen-builtin-pricing` landed. vet, lint and test
    already reached them via `./...`; gofmt takes paths, so it had to be
    told.

- **`fetch_url` is no longer exempt from plan-first gating.** Under
  `require_plan_artifact`, read-only tools are exempt so research can
  happen before the plan is recorded. `fetch_url` was in that list: it
  reads, but it reads *across the network*, and egress before a plan
  exists is an exfiltration channel rather than research. Inert in mast
  today — no `fetch_url` tool is registered — which is exactly how it
  survived: a dead entry in a live table. `planExemptTools` is real
  policy, consulted by `CheckMutatingToolCall` on every mutating call,
  so the next tool named `fetch_url` would have inherited the exemption
  silently.

  This is the last sixth of core-agent's #465 security roundup. mast
  already had the other five for free, because `pkg/attach` was ported
  two days *after* that roundup landed; this one was missing because
  `pkg/permissions` was ported two days *before* it. Port timing is not
  a security control, which is the finding, not the fix.

  `docs/sibling-sync.md` is new and is where findings like that now
  live: every one of the 48 commits in the first drift report has a
  verdict, and the doc records what the instrument cannot see. The
  split is deliberate — the machine keeps the inventory, the doc keeps
  the verdicts — because the per-commit ledger `fork-design.md`
  originally specified went unwritten for three weeks. Also corrected
  along the way: `pkg/permissions/denylist.go` claimed the package was
  unwired and that "nothing in mast is protected by these checks",
  false since the v0.3 write gate; ten trailers left at `83ec071` by a
  re-port, which would have re-reported eight absorbed commits every
  Monday forever; and an 8 MiB response cap in `pkg/pricing/refresh.go`
  justified by a payload size ("tens of KB") that measures 1.67 MiB.

- **The model tables are generated from a rule, refreshed weekly, and
  can no longer drift apart quietly.** Mast forked its pricing code
  early and then went five weeks without a bump while three model
  families shipped. Nothing broke, which is the problem: a model with no
  rate is metered at a flat fallback and counted as unpriced, so the
  symptom of a stale table is a `max_cost_usd` that quietly means a
  different number of dollars.

  - `dev/regen-builtin-pricing` decides membership by **rule** rather
    than by a hand-curated list — every chat-mode, tool-calling,
    priced, non-deprecated Gemini/Anthropic model in LiteLLM's catalog
    ships built-in. That took `pkg/pricing`'s table from 12 models to
    31, and it now emits context windows beside the rates. Rejected
    near-misses are printed with their reason, so a model dropping out
    is visible rather than silent.
  - `.github/workflows/pricing-regen.yml` runs it every Monday and
    opens an auto-PR when the catalog has actually moved. Drift is
    detected with `--check`, which normalizes away the regen date — the
    old shape would have opened a no-op date-churn PR every week. No new
    repository settings: the PR is opened by the `go-steer-bot` App,
    whose credentials are already org secrets — `GITHUB_TOKEN` cannot do
    it, both because the org forbids it creating PRs and because pushes
    it authors never trigger the checks `main` requires.
  - Two new invariant tests fail the build when the tables separate:
    every priced model must classify in `pkg/modeltier` and carry a
    context window, and every tier default must be the **latest** model
    in its line. The second one is why the Anthropic defaults moved —
    frontier and mid were two generations behind (`claude-opus-4-7` /
    `claude-sonnet-4-6` with Opus 5 and Sonnet 5 shipped and priced).
  - `gemini-3.5-pro` is gone from `pkg/modeltier` (upstream
    core-agent#786). It is a model id that never shipped — the 3.5
    generation went flash-first — and it had been sitting in the
    classifier since the fork as a needle matching nothing. Harmless,
    but a phantom entry in a hand-maintained table is a claim that
    somebody checked.
  - Prompt-cache *writes* are now priced. Anthropic bills three
    disjoint input buckets — uncached input, cache reads at 0.1x, cache
    writes at 1.25x — and charging writes at the read rate under-reports
    a cache-heavy turn. `Rates.CostUSDWithCacheWrites` takes all three.

  **Tier defaults that moved:** Gemini `small`
  `gemini-2.5-flash` → `gemini-3.5-flash-lite`, Gemini `frontier`
  `gemini-3.6-flash` → `gemini-3.7-flash`, Anthropic `mid`
  `claude-sonnet-4-6` → `claude-sonnet-5`, Anthropic `frontier`
  `claude-opus-4-7` → `claude-opus-5`. The Gemini frontier bump is half
  the per-token price of what it replaces ($0.75/$3.75 per MTok against
  $1.50/$7.50) and was promoted off a live Vertex UAT — all 31 judged
  corpus scenarios ran through it and scored within noise of the
  `gemini-3.6-flash`-era board. That UAT is the bar on purpose: a
  frontier bump taken on the strength of a spec sheet is how you ship a
  parent agent that stops mid-plan.

- **The judged nightly now runs on two providers, on two boards.**
  `.github/workflows/evals-nightly-gemini.yml` runs the same 31 parity
  scenarios against `gemini-3.7-flash` at 07:30 UTC, half an hour behind
  the Claude board. Deliberately a second workflow rather than a matrix
  leg: each provider keeps its own artifact and its own history, so a
  delta is a delta against the same model — one shared run would make
  each provider's night-to-night comparison depend on whether the other
  provider had a good night, and one provider's outage would erase the
  other's baseline. The logic is not forked; `dev/ci/evals-nightly.sh`
  and `dev/ci/evals-nightly-baseline.sh` now take the provider, the
  workflow to read history from, and the artifact name as environment,
  and the two workflows differ only in their env blocks.

  No new repository settings: `roles/aiplatform.user` on the
  impersonated service account already covers both publishers, so the
  existing WIF provider serves both. `vars.MAST_EVALS_VERTEX_REGION`
  defaults to `global` on the Gemini side, which is where the flash line
  is served.

  This is a second *board*, not a second tier ladder — `tier: frontier`
  on Gemini resolves through `pkg/taskclass.ModelForTier`, and
  `J-cost-tier` prices the tiers the product ships rather than the model
  under test. (The two now name the same model; see the model-table
  entry below. They remain separate knobs.)

  **Where both boards stand at this tag.** Fired by hand against the
  shipped tier table so the release quotes a measurement rather than
  last night's, over 31 scenarios each. Claude is run
  [`32029654748`](https://github.com/go-steer/mast/actions/runs/32029654748),
  taken with the Anthropic tool-schema fix above in place; Gemini is
  [`32026851892`](https://github.com/go-steer/mast/actions/runs/32026851892),
  taken before it, which that provider's path does not go through.
  What landed between either board and the tag is docs, derivation-header
  bumps, an eventlog locking fix, the Vertex cache retry, and the watchdog
  port — none of it on the path either board measures. The judged rig's
  link closure does not reach `pkg/watchdog`, `pkg/mcp`, `cmd/mast`, the
  library entry point, or `pkg/providers/vertexcache` at all; of the three
  packages it does share with those commits, `pkg/eventlog` gained a file
  and modified none, `pkg/workload` gained an optional field that is
  absent from the rig's bundles, and the `pkg/providers/gemini` change is
  a comment. The first Claude board of the day is not quoted anywhere: it
  ran before the tool-schema fix and is superseded by this one.

  | metric | Claude | Gemini |
  |---|---|---|
  | `intent_coverage` | 0.973 | 0.951 |
  | `response_quality` | 0.855 | 0.976 |
  | `severity_accuracy` | 0.419 | 0.484 |
  | `effect_ordering` | 1.000 | 1.000 |
  | `exactly_once` | 1.000 | 1.000 |
  | `tool_coverage` | 0.000 | 0.000 |

  Claude is `claude-opus-5` graded by `claude-haiku-4-5`; Gemini is
  `gemini-3.7-flash` graded by *itself*, which flatters it — the board
  prints that warning on every run and its `response_quality` should be
  read with it. **These are report-only**; only `J-cost-tier` gates. The
  two columns are not comparable to each other, and neither is
  comparable to the previous board on the same provider: the models
  under test changed, and the harness refuses to call that a delta.

  Three things the pair says, two of which are not about either model:

  - `severity_accuracy` sits near half on both, and the misses run
    almost entirely one way: 13 of Claude's 18 and 15 of Gemini's 16
    are the run pitching a scenario *higher* than the corpus expects,
    mostly `expected WARNING, got CRITICAL`. Exactly one miss on each
    board goes the other way. Two unrelated model families failing in
    the same direction is a rubric that over-escalates, not a model
    quirk.
  - `tool_coverage` is **0.000 on all 31 scenarios on both boards**,
    every row reading `0/2 expected tool names called verbatim`. It is
    diagnostic-only so it gates nothing, but it scores against the
    upstream corpus's tool *names* and mast's tools are not named that:
    it measures nothing as written.
  - Claude's `response_quality` reads 0.855 against 0.984 on the last
    `claude-opus-4-7` board, and that pair of numbers should not be
    subtracted. Two things changed underneath it: the frontier default,
    and the Anthropic tool-schema conversion the entry above fixes — the
    corpus builds its cluster tools with `functiontool.New`, which is
    exactly the path that was sending Claude a name and no arguments. Of
    the seven scenarios below full marks at this tag, five turn on
    `correct_diagnosis`: the run declines to commit to the root cause
    the corpus expects. The other two are scored *not specific* or *not
    actionable* with the diagnosis accepted. Whether that residue is the
    corpus rewarding confident assertion or the run genuinely
    underperforming is not something one board can say, and this
    release does not claim it either way.

  The first two are findings against the corpus rather than the
  release, and neither is fixed here.

- **New: a workload can wake itself up, and the cadence survives the
  daemon** (#132, W4.1). Until now every run started with somebody
  calling in — an inbound POST, an operator, an external cron holding
  the schedule on mast's behalf. A bundle can now hold its own:

  ```yaml
  edge_trigger:
    scheduled:
      interval: 15m
      jitter: 45s        # optional; defaults to a tenth of the interval, capped at 30s
      prompt: Sweep every namespace for pods in CrashLoopBackOff and report what you find.
  ```

  The cadence is **anchored, not restarted**: fires land on
  `anchor + k×interval`, and the anchor is written to the session store
  the first time the trigger comes up. A daemon that is redeployed twice
  a day therefore resumes the phase it had, instead of quietly moving a
  02:00 sweep to the middle of the afternoon — the failure mode where
  the schedule keeps working and stops meaning what it said. Jitter
  applies to each fire and is re-drawn every time, so it cannot
  accumulate into drift; it exists because N replicas started by one
  rollout waking on the same second is a self-inflicted thundering herd
  against one API server.

  **A tick the daemon was down for is skipped, not caught up.** Coming
  back after an outage that spanned three ticks fires none of them and
  logs one line naming how many were dropped and over what window. A
  periodic run samples the *current* state of the world, so a sample
  nobody took is owed to nobody; the alternative is a crash-looping
  daemon that buys a fresh backlog of model runs at every restart, about
  the crash. (mast's timed-pause scheduler still catches up, deliberately:
  a pause is a promise about one specific parked session that nobody else
  will keep.)

  Each fire opens its own session, named for its tick and listed by
  `mast sessions list` like any other, and runs as
  **`mast:scheduler`** — a namespaced identity no human login can take,
  so an audit row can never read a scheduled run as somebody's. It goes
  through the same chokepoint every other turn kind uses, which is the
  point: a mutating call inside a scheduled run still parks for a real
  approver, still burns the workload's budget, still refuses to start
  while the daemon is draining. Unattended is not unsupervised.

  Fires are counted by `mast_scheduled_fires_total{workload,outcome}`
  (`ran`, `skipped`, `error`, `missed`). A malformed `interval:` or
  `jitter:` is a load error naming the bundle file, not a trigger that
  silently never fires. One daemon per session store, as everywhere
  else in mast — there is no leader election, and two replicas of a
  scheduled workload each keep their own cadence.

- **New: the judgement an operator spends on one call survives as data**
  (#132, W8). Approving, rejecting or correcting a mutating call used to
  leave a log line and a transcript entry — legible to whoever was
  reading the terminal at the time, and to nobody afterwards. The
  highest-signal data a gated fleet produces is exactly this: a human
  looked at `scale_deployment(deployment=api, replicas=10)` and said two,
  or said no. That is a labelled example, and mast was throwing it away.

  Every adjudication the write gate makes is now a durable record on the
  session's event log, and a new subcommand harvests them:

  ```
  mast sessions export-decisions --session-db=/var/lib/mast/sessions.db \
      --workload=gke-triage --since=2026-08-01T00:00:00Z --out=decisions.jsonl
  ```

  The output is JSON Lines: a `_meta` provenance object, then one object
  per decision. Each row carries what the model proposed, what actually
  executed, which of `approve` / `reject` / `edit` the operator chose,
  and whether the call was authorized, refused by the operator, or
  refused by mast — that last distinction because a gate that rejected an
  unattributed edit is not a human saying no, and a dataset that
  conflates them teaches the wrong lesson. A change-set grant being spent
  writes its own row too, so a file cannot show one approval where four
  calls ran.

  **Approver identities are digested by default.** The row carries
  `sha256:` plus sixteen hex characters, which is stable — you can group
  by approver, count how many distinct people approved a class of change,
  or find the identity that approves everything — without naming anyone.
  `--include-approver` opts into raw identities, and `_meta.redaction`
  records which mode produced the file, so a redacted export can never be
  mistaken for a raw one. Machine identities (`mast:internal`) pass
  through in the clear: they name a mechanism, not a person, and hiding
  them would hide the one thing worth knowing about an unattended run.

  **Argument values are exported verbatim, and that is deliberate.** The
  proposed→executed pair is the entire label; strip it and the file
  records only that somebody edited something. So an export is exactly as
  sensitive as the arguments your tools take — namespaces, hostnames,
  anything a remediation call carries. `--out` writes `0600`, the warning
  travels inside the file, and you should treat the result like the
  cluster it describes.

  v0.4 captures and exports. Nothing scores these rows, nothing retrains
  on them, and mast never reads the file back — what you do with it is
  yours.

- **New: `dispatch: bounded` — one cheap model call, a report forced to a
  schema, and a step count you can read off the meter** (#132, W4.3). Some
  workloads exist to answer one question on a schedule, and their whole
  value is that they cannot get expensive. Running them under
  `coordinator` gave you a plausible-looking cheap run and no way to prove
  it: the shape allowed delegation, retries and follow-up turns, so "this
  costs one call" was a hope about a prompt rather than a property of the
  build.

  A bounded workload is a fourth dispatch shape — a roster of exactly one
  `SingleTurn` specialist, built as a single node with no orchestrator
  above it:

  ```yaml
  # workload.yaml
  dispatch: bounded
  specialists:
    - incident-report
  ```

  ```yaml
  ---
  # specialists/incident-report.tmpl
  description: Classifies one incident and returns the finding report.
  mode: SingleTurn
  tier: small
  output_schema: ../schemas/finding.json
  ---
  ```

  The specialist gets no tools, one turn, and its reply is validated
  against `output_schema:` before the turn ends — a model that answers in
  prose fails the run rather than returning an unparseable paragraph that
  looks like success. `Result.Usage.ModelCalls` is `1`, the daemon logs
  `"session_model_calls":1`, and `mast_model_calls_total` advances by one:
  the count comes off the meter, not off a stopwatch or a token estimate.

  **The refusals are the shape.** A roster with two specialists, a spec
  left in the default Task mode, a spec with no `output_schema:`, or a
  bundle that also enables the planner is a startup error naming what it
  found — the count and the names, or `Task (the default for a spec with
  no mode:)` for the author who wrote nothing. None of it is inferred:
  `dispatch: auto` never picks `bounded`, because a one-specialist roster
  is an ordinary coordinator and a cost ceiling nobody asked for is not a
  favor.

  `examples/workloads/bounded-triage` is the worked example, on the same
  `finding.json` report contract the GKE triage bundle uses.

- **The nightly now checks that a cheap tier is actually billed cheaply**
  (#132, W0.6). "A tiered specialist's tokens are priced at the model it
  ran on" has been true since v0.3 and unit-tested since, but it could
  not be demonstrated end to end anywhere: the offline fakes that make
  the free test tiers credential-free are exactly what collapses every
  tier back onto one model, leaving no two rates to compare. `J-cost-tier`
  runs in the judged nightly — a `tier: small` analyst under a
  `tier: frontier` synthesis, one turn, against real models — and reads
  the meter's per-scope snapshot back:

  ```
  J-cost-tier — every tiered specialist was billed at its own rate (root claude-opus-5 at $0.01500/1K)
    specialist  tier      resolved          calls  tokens  billed    at root   rate/1K
    analyst     small     claude-haiku-4-5  1      848     $0.00254  $0.01272  $0.00300
    _synthesis  frontier  claude-opus-5     1      1094    $0.01641  $0.01641  $0.01500
  ```

  The counterfactual column is the point: "not billed at the parent's
  rate" means nothing without the number it would have been. The check
  also refuses to pass vacuously — a tier that resolved to one model but
  *ran* as another is a finding, so is a specialist that made no call,
  and a roster whose tiers all collapsed onto the root is reported as
  proving nothing rather than as green. Unlike the scores on that board
  it does gate the nightly job, because its verdict is arithmetic over
  the meter's own numbers and does not depend on what the model said.

  That block is transcribed from run
  [`32029654748`](https://github.com/go-steer/mast/actions/runs/32029654748),
  not composed for the changelog. The Gemini board
  ([`32026851892`](https://github.com/go-steer/mast/actions/runs/32026851892))
  measured the same shape against its own ladder — `analyst` at
  `tier: small` resolved to `gemini-3.5-flash-lite` and was billed
  $0.00038 for 274 tokens at $0.00140/1K, where the root
  `gemini-3.7-flash`'s $0.00225/1K would have charged $0.00062.

  On both boards the `frontier` row is a **control, not a measurement**:
  that tier resolves to the model the nightly already runs as root, so
  its rate cannot disagree with the parent's, and the board says so in
  its own notes rather than counting it. One real scope per provider is
  what this claim rests on. Worth recording that the Gemini side proved
  nothing at all until the tier table moved: on the previous table
  `small` resolved to `gemini-2.5-flash` at exactly the root's
  $0.00060/1K, so the counterfactual equalled the billed figure on
  *both* rows and the check was vacuous on that provider — passing, and
  empty. It is a measurement there for the first time in this release.

- **New: a specialist can declare how much model it is worth, not which
  vendor's** (#132, W1.1a). `model: claude-haiku-4-5` in a specialist's
  frontmatter says two things at once — "this step is cheap" and "this
  bundle runs on Anthropic" — and only the first one is usually meant. A
  spec can now say the first alone:

  ```yaml
  ---
  description: Inspects pod state and returns a 3-bullet digest.
  tier: small          # small | mid | frontier
  ---
  ```

  Mast resolves the tier against the provider it is actually running —
  the `--provider` alias when you passed one, otherwise the root model
  id's own prefix, which is the same dispatch `--model` already makes —
  through `pkg/taskclass.ModelForTier`. `tier: small` is
  `gemini-2.5-flash` under a Gemini root and `claude-haiku-4-5` under an
  Anthropic one, so the roster reads the same and costs the right thing
  on either backend. A root model whose provider can be told from
  neither fails startup asking for `--provider` rather than guessing a
  vendor and billing you for the guess.

  Every property `model:` overrides already had carries over: an
  unresolvable tier fails startup instead of quietly inheriting the
  parent's model, resolution is memoized per resolved id so twelve
  `tier: small` analysts open one client, and the offline fakes
  (`echo`, `scripted`, `toolactor`) collapse every tier back to the fake
  so a tiered bundle still runs with no credentials. Startup logs each
  tier next to the id it resolved to.

  **Metering follows the tier.** A tiered specialist's tokens are priced
  at the model it actually ran on, not the parent's rate — build and
  pricing now go through one `SpecModelName`, because the alternative is
  a bundle that runs cheap and bills expensive, which is the same
  fiction in the other direction.

  `model:` and `tier:` on one spec is a load error naming the file, not
  a precedence rule. The shipped `ns-audit` bundle is the worked
  example: four `tier: small` namespace analysts under one `tier: mid`
  synthesis step.

- **New: one answer can approve a whole change set — bounded by a clock the
  bundle sets and a re-read the bundle declares** (#132, W7). A remediation
  is usually several calls: scale two Deployments, or patch a ConfigMap and
  restart what reads it. Until now each one parked on its own, so a
  three-call fix was three questions, and an operator who had already read
  the plan answered the same question three times.

  Answering a parked call with `{"verdict":"approve","scope":"change_set"}`
  now mints a grant for **each remaining call in that finding's proposed
  change set**. The per-call gate spends them silently. A grant authorizes
  one exact `(tool, arguments)` pair — not the tool, not the verb — and is
  single-use, durable across a restart, and re-checked before it fires. A
  deny policy still adjudicates first, and a spent grant is still recorded
  as an allow-once decision in the audit log: the grant removes the
  *question*, never the accounting. If any call in the set cannot be granted
  the whole verdict is refused with a reason, rather than quietly covering
  the part that fits — a partial "approve all of this" is the one outcome
  the operator did not choose.

  **What makes an approval go stale is the cluster, not the clock.** A set
  approved twenty seconds ago against a Deployment somebody has since scaled
  by hand is stale; one approved an hour ago against an untouched object is
  not. So a tool in `tool_catalog` can declare its own freshness re-read:

  ```yaml
  - name: scale_deployment
    mutating: true
    precondition:
      read: get_deployment            # must be declared mutating: false
      args_from: {name: deployment}   # the read's "name" <- this call's "deployment"
      fields: [output.replicas]
  ```

  The read runs at approval time and again just before the granted call
  fires. If a watched field moved, the grant is voided and the call goes
  back to the operator with a question that names the field and both values
  — `output.replicas was 1 at approval and is 5 now`. `hitl.change_set_ttl` (default `10m`)
  is the backstop for everything a precondition cannot see. A tool that
  declares no precondition gets the TTL and nothing else, and its parked
  question says so rather than implying a check mast is not making.

  Two things worth knowing before writing your first `precondition:`. Each
  call is checked against **its own** object — that is what `args_from` is
  for; a precondition watching the field the set itself rewrites would have
  call 1 invalidate call 2 by succeeding. And `fields:` paths start
  `output.` for MCP tools, because that is how the structured result of an
  MCP tool arrives.

  Proven offline in `scripts/uat-v0.4.sh` (`U-changeset`, five legs: the
  set, a `scope: once` control, cluster drift, the expiring window, and a
  SIGKILL between the question and the answer), and against a real API
  server by the new opt-in live tier below.

- **New: an opt-in live acceptance tier over a throwaway kind cluster**
  (#132, W7). `MAST_LIVE_KIND=1 ./scripts/live-kind-v0.4.sh` runs the
  change-set legs against a real Kubernetes API server: one approval scales
  two Deployments, and a second leg has a person run `kubectl scale` out of
  band while an approved call is held open, which voids the next grant and
  re-parks it. Two claims need this — that a declared `fields:` path is
  right about a real tool's real JSON, and that "the cluster moved" means
  somebody else moved it — and neither can be staged by a harness writing
  its own fixture file.

  It creates its own cluster, refuses to adopt or delete one it did not
  create, writes a single-context kubeconfig under `/tmp`, and never
  resolves the ambient `current-context`; the MCP fixture behind it repeats
  those checks itself before it will start. It needs a container runtime and
  takes minutes, so it is **not** part of `dev/ci/presubmits/e2e.sh` and does
  not gate the build.

- **New: a finding carries the call it recommends, and that is the call the
  operator approves** (#132, W7.0). Until now a diagnosis recommended a
  remediation in prose — "scale the api Deployment back to 2 replicas" — an
  operator agreed with the prose, and the change executor composed a tool
  call from it on a later turn. What reached the cluster was a call nobody
  had looked at. Prose cannot be approved, only agreed with.

  A workload's report schema can now declare `proposed_change`: a
  possibly-empty list of `{tool, arguments}` entries. Every entry has to
  name a tool the workload actually declares and carry arguments that fit
  *that tool's own* input schema, and both are checked at the moment the
  report is returned — a report that names an unknown tool, or arguments the
  tool would reject, comes back to the specialist as an error it can correct
  rather than failing the run. The check is the same code that validates an
  operator's edit at the gate, so the two cannot drift apart on what
  "schema-valid arguments" means. `examples/workloads/gke-triage` declares
  the field on all twelve diagnosers; `recommended_actions` stays, because
  "what should happen" and "what call makes it happen" are different
  questions and an empty proposal still needs prose.

  **Empty is a first-class answer.** "Raise the memory limit, but I don't
  know to what" is an honest diagnosis, and a specialist that cannot name an
  exact call says so in `escalate` instead of inventing a plausible one to
  fill a field. Nothing changes for a roster that does not declare the
  field.

  Under graph dispatch the diagnoser→executor edge becomes structural: a
  finding with a non-empty change set that the operator approved routes to
  the change executor, which receives those calls verbatim. The record is
  durable per specialist, because a confirmation resume re-enters the graph
  at START and re-runs the nodes above it. A roster with two change
  executors logs an error and leaves the edge unwired rather than guessing.

  End to end, offline, in `scripts/uat-v0.4.sh`: the parked question quotes
  `replicas=2` — not the `replicas=10` the same model proposes when it picks
  a call for itself — and approving it runs `apply_change` exactly once,
  with `replicas=2` in the tool's own ledger.

- **New: `GET /sessions/{id}/guardrails` + `POST /guardrails/reset` — and a
  way out of a budget trip that didn't exist before** (#135). mast's cost
  ceilings are real: `budget:` in the bundle bounds the session, `budget:`
  on a specialist bounds that specialist, and the meter cancels the run
  context the moment either is crossed. What was missing was any recovery.
  Enforcement is re-derived from accumulator-vs-ceiling on every priced
  event, with no flag in between — so a session past its cap crossed it
  again on the first event of the next turn, forever. The only cure was
  restarting the daemon, which drops every *other* session's in-flight turn
  with it. Unattended is exactly where nobody is watching for that.

  The read answers "what is armed, what tripped, why" in all three of mast's
  dimensions — cost, tokens, and model calls — plus a per-specialist
  breakdown, because a session halted by `max_turns: 40` has spent six
  tenths of a cent and a dollars-only view sends the operator looking for
  the wrong cause. Wire names and status codes are core-agent's so one
  client speaks both daemons.

  "Reset" means raising the ceiling, never zeroing the accumulator: /usage,
  the eventlog-derived cost, and the ceiling check keep counting the same
  dollars, so a post-incident review of a session that spent $40 doesn't
  find it reporting $10. A reset that provably wouldn't survive the next
  turn is refused with 409 and the numbers rather than performed — and the
  check runs before anything is mutated, so a refusal costs the operator
  nothing. A grant never *imposes* a ceiling: "+5 turns" on a session with
  no turn cap would otherwise cap it at 5 and wedge it four calls later.

  Turn errors now classify as `cost_ceiling` with the reset endpoint in the
  hint; the watchdog is reported `advisory: true`, because mast's only logs.

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
