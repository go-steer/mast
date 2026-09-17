---
title: Roadmap
description: What v0.9 ships, and what lands after it — honestly.
---

mast is at **v0.9.0** — the surfaces stop answering a question nobody asked.
On the v0.8.0 correctness pass, the v0.7.0 route-back pass, the v0.6.0
enforcement pass, the v0.5.0 monitoring cycle, the v0.4.0 change set, the
v0.3.0 write gate and the v0.2.0 durable-execution spine. See [Shipped in
v0.9.0](#shipped-in-v090--the-surfaces-stop-answering-a-question-nobody-asked)
below. v0.9 is the last release before v1.0, which is the API freeze and
nothing else.

**All eleven v0.1 exit criteria from the fork design are green.** The
`--task` profile criterion cleared with the P1.3a/P1.3b adapter ports and
was verified against live endpoints on 2026-07-29 (gemini one-shot with
grounded search on the tier defaults, Claude on Vertex completing the
Task-mode tool loop, sessions durable across runs and providers). The
final criterion — attach-mode reachability from mast-web — cleared the
same day with the P1.3c port: the real mast-web SPA, served in proxy mode
against a live `mast --attach-listen=…` daemon, connected, listed
sessions, and round-tripped a prompt through a real turn over SSE.

## Stability, precisely

**Nothing is promised yet.** mast is pre-1.0 and every exported path may
change in any release. v1.0 is the release that makes the promise, and
[what it will cover](/reference/stability/) is published now so you can
decide what to depend on today: **six import paths** — the module root,
`pkg/agent`, `pkg/transcript`, `pkg/workload`, `pkg/specialists`,
`pkg/budget` — plus `cmd/mast`'s flags, verbs and exit codes, with the
other 32 importable packages under `pkg/` named individually as
unsupported. v1.0 means the API stops moving and carries no other claim;
in particular it is not a production-readiness badge.

*This section previously reserved stability for "the five packages the four
pillars stand on", naming `provider` and `tool` interfaces. Two of those five
never existed — the provider extension point is `mast.Config.Model`, a field
typed on ADK's `model.LLM`, not a package — and the `// Experimental:` marker
that decision named for everything else was never written into a single `.go`
file. Corrected in v0.8.0 ([#300](https://github.com/go-steer/mast/issues/300)).*

## Shipped in v0.1.0-pre

Workflow-graph and SubAgents dispatch on ADK v2.1.0; durable HITL surviving
process death; budget metering with cost + turn caps; the full GKE
triage roster; `.agents/` discovery; the sessions operator surface (CLI +
HTTP); observability v0.1 (seven fixed counter families + env-gated OTel
trace export); the synchronous A2A v0.3 client, `federation.Adapter`, and
the `invoke_remote_agent` planner tool; the planner scaffold; two forkable
workflow starters; the CI-enforced slim-embed guarantee; deploy starters
including Cloud Run with Postgres sessions; and the top-level `mast`
library API.

## Gated on the P1.3 adapter ports

The staged ports are landing as core-agent's cleanup milestones close (all
four closed 2026-07-28; the rule is *don't port moving code* — the revised
trigger in
[`docs/fork-design.md`](https://github.com/go-steer/mast/blob/main/docs/fork-design.md)):

- **`--task` profiles — shipped.** P1.3a landed the task-class,
  permission, pricing, and model-tier packages; P1.3b landed the provider
  adapters (Anthropic first-party + Vertex, the Gemini builtin-tool layer,
  Vertex context caching, scripted replay) and the watchdog. One-shot
  `--task` runs now take `echo`, `scripted`, `gemini-*`, or `claude-*`
  models.
- **Attach mode + mast-web reachability — shipped.** P1.3c ported the
  attach HTTP/SSE transport (protocol v1.4.0: session listing, seq'd
  replay + live tail, inject/wake/interrupt, capabilities frames, agent
  card) plus `pkg/auth` and the eventlog overlay, pinned at
  `core-agent@25d8531c`. Serve mode's `--attach-listen` binds the surface
  (implies `--session-db`; bearer auth via `MAST_ATTACH_TOKEN`;
  loopback-only without auth), and the
  [mast-web](https://github.com/go-steer/mast-web) operator UI connects
  to it — verified end-to-end in a real browser session. Attach runs
  single-user in v0.1: multi-session auth, the session ACL store, and
  operator session creation (`POST /sessions`) are v0.2 work.

## Shipped in v0.2.0

Per the 2026-07-25 scope re-cut and the per-subsystem design docs:

- **Recorded-effect outbox, then boot-time auto-resume** — mutating tool
  calls are at-least-once under mast's reconstruct-and-re-execute resume
  model; the outbox (design settled 2026-08-01: the session event log *is*
  the outbox, checked before every mutating call) makes re-execution
  ambiguity visible and **blocking** instead of silent, and auto-resuming
  interrupted sessions on boot unblocks behind it. Follow-up hardening
  refuses sub-agent/tool name collisions at construction (a fail-open hole)
  and warns on direct-ack against a live session DB.
- **Programmatic pause / abort** — a first-class pause/abort surface with
  planned-stop vs unexpected-stop classification, alongside the
  HITL-interrupt-keyed resume; `/abort` on an already-terminal session now
  returns `409`, not `500`.
- **A2A server** — expose workloads as skills to registries (Google Agent
  Registry, kagent): agent card publication plus
  `message/send`·`tasks/get`·`tasks/cancel`·`message/stream` over SSE, with
  a pluggable `TokenValidator` and rate-limiter seam. v0.1 shipped the
  client only.
- **AG-UI server** — the hand-rolled, zero-dependency `pkg/agui` wire +
  HTTP/SSE surface for CopilotKit apps and chat-platform bots, driving every
  turn through the same `runTurnPre` chokepoint, with the full HITL
  interrupt/resume lifecycle (terminal `RunFinished{outcome: interrupt}` →
  resume via a new run's `resume` array). The `agui://` federation client,
  per-key state deltas, and client-declared tools were listed here as v0.3;
  none of the three landed in v0.3, and **per-key state deltas shipped in
  v0.9** — see [Next](#next). The other two are still open.
- **Local / stdio MCP** — generic, transport-dispatched MCP wiring (a
  `mcp.json` catalog with http/stdio dispatch, not gke-only), with stdio
  control-plane hardening: env scoping and command allowlisting.
- **Observability v0.2 + teardown watchdog** — the fixed-registry pass
  canonicalizing the v0.2 metric families, plus a teardown watchdog guarding
  shutdown.
- **End-to-end UAT harness** — a durable-execution UAT harness (crash /
  drain / abort legs over a request-driven fake model and a stdio blocking
  tool) gating CI.

## Shipped in v0.3.0

- **Parity eval gate** — a credential-free eval suite gating every PR
  (`scripts/evals.sh`, alongside the v0.2 UAT harness). It runs the
  scenarios that mast's durability guarantees turn on — exactly-once
  mutation across a crash, refusal after an ambiguous one, budget
  exhaustion, an approval rejected, an approval edited — against the
  composed runtime, and holds each to a declared outcome. Capabilities not
  yet built are declared expected-fail with the workstream that removes
  them; that list only shrinks, and a capability landing without its entry
  being flipped fails the suite. It also checks that the metrics scoring
  the ported 31-scenario corpus can score anything at all, so a green board
  cannot come from a measurement that never ran.
- **Nightly judged evals** — a second, metered tier that answers the
  question the free one cannot: how good the agent's *answers* are, not just
  which tools it reached for. It runs the 31-scenario corpus against a live
  model over a fixtured cluster and has a second model grade each response
  against the upstream rubric, then posts a board and a delta against the
  previous night. Scores report and never gate — no score can fail a build,
  and what the tier flags instead is a scenario that did not run or a metric
  that scored nothing. A provider's rate limiter is not allowed to become
  that flag: the tier waits out a `429` on a bounded schedule before giving
  up on a scenario, and says on the board how many calls it had to retry, so
  a quota under pressure is visible rather than absorbed. Scenarios the tool
  surface cannot satisfy are listed as structural ceilings rather than
  folded into a low score.

  One check on that board *is* pass/fail, because its verdict is arithmetic
  rather than judgment: the nightly runs a two-specialist roster with one
  specialist on `tier: small` and one on `tier: frontier`, then reads the
  meter back and asks whether the cheap one's tokens were billed at the
  cheap rate. Each row prints what those tokens would have cost at the root
  model's rate beside what they did cost. This cannot be checked without a
  provider — the offline fakes collapse every tier onto one model, so there
  would be no two rates to compare — and when there is no live model the
  check says it was skipped rather than passing quietly.

  At the v0.4.0 tag it reads, on Claude: a `tier: small` analyst resolved to
  `claude-haiku-4-5` and billed $0.00254 for 848 tokens, where the
  `claude-opus-5` root's rate would have charged $0.01272. On Gemini:
  `gemini-3.5-flash-lite` billed $0.00038 against the $0.00062 the
  `gemini-3.7-flash` root would have. The `frontier` row on each board is a
  control rather than a measurement — that tier resolves to the model the
  nightly already runs as root, so its rate cannot disagree with the
  parent's, and the board labels it as such instead of counting it.
- **Per-specialist model selection** — a specialist's `model:` is honored
  instead of inheriting the workload's, including across providers.
- **Per-specialist budgets** — the `max_turns` and `max_cost_usd` a
  specialist declares are now enforced, not just parsed. Each is a ceiling
  on that specialist's own spend, composed under the workload's so
  whichever cap is crossed first stops the run, and the error names the
  specialist rather than the workload. A specialist running on its own
  `model:` is priced at that model's rate, which is the cost attribution
  that makes a tiered roster measurable.
- **Typed specialist reports** — a specialist can declare `output_schema:`,
  a JSON-Schema file its answer has to satisfy, and a violation comes back
  to the model as a named refusal rather than becoming the answer. The
  schema is a file the whole roster shares rather than a block copied into
  each specialist, because the shape is a contract with whatever reads the
  report. The shipped GKE triage bundle now ships one.
- **The shipped example is now under test end to end** — a second
  acceptance harness runs the GKE triage bundle exactly as the quickstart
  does, offline and credential-free, and checks the report an operator
  actually receives in the approval prompt: every field the schema
  declares, a valid value for the constrained one, and the specialist the
  incident routed to. It also proves the contract is enforced rather than
  merely declared, by running a deliberately malformed report through the
  same path and requiring it to be refused. Writing it found a real bug —
  with a report contract declared, the offline demo models could not
  produce a conforming report, so `mast --model=echo` on the shipped bundle
  failed every injected incident. Fixed; the harness is what keeps it
  fixed.
- **Parallel fan-out** — a workload can set `dispatch: fanout` and have its
  whole roster investigate one incident at the same time, bounded by a
  concurrency cap, with a single synthesis specialist merging what comes back
  into one report and a single approval on that report. An analyst that
  returns nothing is reported as silent rather than quietly dropped, and
  approving the merged report finishes the run without re-running any
  analyst — including after the daemon has been restarted. Fan-out branches
  are read-only by construction: a roster whose analysts can change the
  cluster is refused at startup, with the tool named, because every branch
  runs before the approval gate. The shipped GKE triage bundle is one of
  those rosters, so fan-out ships its own read-only example instead of
  converting it.
- **The write gate** — a call that would change something now stops and asks
  before it fires, one call at a time, with the tool and its arguments in
  front of the operator. "Would change something" is the same test the
  effect log uses, and a tool nothing has classified counts as mutating, so
  a workload does not get to write by omission: a bundle that says nothing
  about mutation is gated, and unattended writes have to be asked for
  (`hitl.on_mutation: apply`). The question is durable — the daemon can be
  killed between asking and being answered, and the approval still lands on
  whatever process is running when the operator gets to it, running the call
  exactly once. Approvals are recorded against an authenticated approver,
  including when a bot relays a human's decision, and an approval that tries
  to authorize more than the one call in front of it is refused rather than
  quietly narrowed. See [the write gate](/reference/write-gate/).
- **Editing a call before it runs** — an operator answering a parked call can
  send back different arguments, and those are the ones the tool receives.
  They are checked first: an edit mast cannot attribute to an authenticated
  approver is refused, so is one naming an argument the tool does not
  declare or a value its schema rejects, and the *edited* call is
  re-adjudicated against the permission policy — a denied production change
  cannot be reached by editing an approved staging one. What actually ran is
  recorded durably and printed by `mast sessions show`, because the agent
  substrate re-fires the original call on resume and the transcript alone
  would show the arguments the model proposed rather than the operator's.
- **Read-only diagnosers** — a specialist now declares whether it is allowed
  to change anything, and read-only is what it gets by saying nothing. Mast
  refuses to start a workload in which a read-only specialist can reach a
  tool that changes something — whether it names one, helps itself to a whole
  tool server, or simply inherits the workload's catalog without saying which
  tools it needs. So a diagnosis specialist is not kept in its lane by the
  wording of its prompt; it structurally has nowhere else to go. The shipped
  GKE triage bundle is now twelve read-only diagnosers that name the
  remediation, plus one change executor that carries it out under the write
  gate — and which specialists can change your cluster is a startup log line,
  not something you work out by reading three files. **As shipped in v0.3.0
  the executor was operator-invoked, not automatic:** a diagnosis named its
  remediation in prose and no dispatch shape handed that to the executor on
  its own, so an incident ended at a finding. That is closed on `main` by the
  change-set producer below, unreleased at the time of writing.
- **The same split, in the deployment manifests** — the kustomize base grants
  the daemon cluster-wide read (and no secrets); permission to *change* a
  namespace is a separate `Role` you apply once per namespace, so an approved
  call still has to get past the API server. Deliberately narrower than the
  tools it backs, and CI-linted from the subject side so a new cluster-scoped
  grant cannot slip in. On GKE, read the IAM caveat before trusting it: see
  [cluster permissions](/reference/cluster-permissions/).
- **ADK v2.2.0** — the agent substrate is upgraded from v2.1.0. The fix that
  motivated taking it now: a workflow-graph run whose invocation context was
  cancelled from outside — an evicted attach session, a dispatch deadline, an
  operator abort — could finish reporting success. It now reports the
  cancellation. Human-in-the-loop resume also became deterministic and can no
  longer run a tool twice within one resume. One wire-visible consequence: the
  attach `agent` frame embeds ADK's event struct verbatim, and its JSON field
  names moved from `PascalCase` to `camelCase`; stored sessions are unaffected
  and the operator UI already reads both.

## Shipped in v0.4.0

Entirely in this repo — nothing in it waited on another project. The claim it
adds up to: *an operator approves the exact call that will fire, the loop runs
on a schedule without an orchestrator, and every verdict becomes a labelled
eval row.*

- **The change-set producer** — the missing
  half of the write gate. A finding carries a typed proposed change rather
  than a sentence, drawn from the workload's own tool catalog and checked
  against that tool's input schema when the finding is returned; a proposal
  naming a tool the workload does not have, or arguments it would reject,
  comes back to the specialist to fix. The executor is reachable from a
  diagnosis through a structural rule rather than a prompt, and an operator
  approves the object that actually fires instead of a paragraph describing
  it. An empty proposal stays a complete, valid report. This closes the
  honest gap v0.3 shipped with. See [the change
  set](/concepts/approvals/#the-change-set--approving-the-call-not-the-prose).
- **Change-set approvals** — approving one
  parked call with `scope: change_set` mints a grant for each remaining call
  in that set, bound to an exact `(tool, arguments)` signature rather than to
  a tool name: single-use, durable across a restart, and still adjudicated
  and audited like any other allow-once decision. A crash between the answer
  and the calls resumes knowing which fired and re-fires none. What voids the
  approval is the cluster changing, not just a clock running out — a tool
  declares its own freshness re-read, mast runs it when the operator answers
  and again before each granted call, and a field that moved sends the call
  back to the operator naming what moved. A wall-clock TTL (default 10
  minutes) is the backstop for what a re-read cannot see. See [one answer for
  a set of
  calls](/concepts/approvals/#one-answer-for-a-set-of-calls--and-what-makes-it-stale).
- **Decisions become training data** — every
  approve, reject and edit is a durable record on the session, and `mast
  sessions export-decisions` writes them out as JSON Lines. *"The operator
  edited 10 replicas down to 4"* is the highest-signal thing the system
  produces, and each row carries both argument sets, so the correction is the
  label rather than the outcome alone. Approver identities are digested by
  default and the file says which redaction mode produced it; tool arguments
  are exported verbatim, which makes an export as sensitive as the cluster it
  describes. Capture and export only — nothing scores or retrains on the rows.
  See [exporting
  decisions](/reference/write-gate/#exporting-what-was-decided).
- **Scheduled triggers** — a bundle wakes
  itself on an interval, with jitter, and the cadence survives the daemon
  rather than merely restarting with it: the anchor is durable, so fires land
  on the same phase after a redeploy instead of drifting a 02:00 sweep into
  the afternoon. A tick the daemon was down for is **skipped, not caught up**
  — a periodic run samples the current state of the world, and catching up
  would have a crash-looping daemon buy a backlog of model runs about the
  crash. Each fire is its own session, running as `mast:scheduler`, through
  the same path every other kind of turn takes: a mutating call in a scheduled
  run still parks for a real approver. See
  [`scheduled:`](/reference/workload-bundle/#scheduled--a-workload-that-wakes-itself).
- **A bounded analysis path** —
  `dispatch: bounded` is a fourth shape: a roster of exactly one `SingleTurn`
  specialist, built as a single node with no orchestrator above it, so the
  cycle costs one cheap-tier model call and there is nothing in the shape that
  could take a second turn. The report is forced to the specialist's
  `output_schema:` before the turn ends, and the step count is asserted off
  the meter — `Result.Usage.ModelCalls`, the `session_model_calls` log field,
  and `mast_model_calls_total` — rather than inferred from latency or tokens.
  A roster that is not exactly one schema-declaring `SingleTurn` specialist is
  a startup error naming what it found, and `dispatch: auto` never picks the
  shape: a cost ceiling is declared, never inferred. Same report contract as
  the agent path, because the schema is a shared file rather than a block
  copied into each specialist. See [the four dispatch
  shapes](/concepts/specialists-and-dispatch/#bounded--one-cheap-call-one-schema-forced-report).

- **Provider-portable specialist tiers** — a specialist declares `tier: small
  | mid | frontier` rather than a vendor's model id, and mast resolves it
  against whichever provider the workload is actually running on. One roster
  line, `tier: small`, is `gemini-3.5-flash-lite` under a Gemini root and
  `claude-haiku-4-5` under an Anthropic one, so a shipped bundle is no longer
  a vendor choice for everyone who forks it. `model:` stays for an exact pin;
  declaring both on one spec is a load error rather than a precedence rule,
  because a silent winner between two ways of saying the same thing is the
  bug. An unresolvable tier fails the build instead of quietly inheriting the
  parent's model, and an offline-fake root collapses every tier back to
  itself so a tiered bundle still runs credential-free. See
  [`tier:` — the portable spelling](/reference/workload-bundle/#tier--the-portable-spelling).
- **The judged nightly is on, on two providers** — the metered tier v0.3
  built now runs unattended against live credentials, and a second workflow
  runs the same 31 scenarios on Gemini with its own board and its own
  history. Deliberately two workflows rather than one matrix: a night-to-night
  delta should not depend on whether the *other* provider had a good night,
  and one provider's outage should not erase the other's baseline. This is
  what finally makes the cheap-tier claim measurable rather than declared —
  the board prices a `tier: small` specialist against what the parent's rate
  would have charged, and a specialist that resolved to the cheap model but
  was billed at the parent's rate is a build failure, because that verdict is
  arithmetic rather than judgment. Scores still only report.
- **The model tables are generated from a rule and refreshed weekly** —
  membership in the built-in pricing table is now every chat-mode,
  tool-calling, priced, non-deprecated Gemini/Anthropic model in the upstream
  catalog, regenerated by a scheduled job that opens a PR only when the
  catalog actually moved. A stale rate is not a loud failure — an unpriced
  model is metered at a flat fallback, so the symptom is a `max_cost_usd` that
  quietly means a different number of dollars. Two invariant tests now fail
  the build when the four tables that have to move together drift apart.
  Prompt-cache *writes* are priced at their own rate, which the previous
  table charged as reads. See [Cost](/concepts/providers/#cost).
- **An opt-in live acceptance tier over a throwaway kind cluster** — the
  free tiers prove mast's guarantees against fakes and the judged tier
  measures answer quality against a live model; neither watches a real
  API server accept a real mutation. `MAST_LIVE_KIND=1` runs the write gate
  end to end against a disposable cluster. Deliberately not a presubmit and
  deliberately never pointed at a cluster anyone cares about: fault injection
  must never touch a real one.
- **A runaway backstop that is armed by default, and talks to the model** —
  the behavioral watchdog gained a detector for the loop that alternates
  between two tools (no call is ever followed by itself, so the
  repeated-call detector was structurally blind to it) and one for three
  failing tool calls in a row with none succeeding between — the state where
  a workload writes a confident summary of a system nothing it ran could
  reach. Two postures above `warn`: `feedback` prepends the observation to
  the model's next turn as an automated note about its own last turn, and
  `enforce` cancels the turn on a Critical alert and refuses every later one
  — auto-resume and scheduled fires included — until an operator resets it,
  across a restart. Declared per workload as `safety.watchdog`, overridable
  for one run with `--watchdog`. **The default moved from `warn` to
  `feedback`**: on an unattended deployment a logged alert is off with extra
  steps, and `enforce` is not the default here because a false halt is an
  outage that waits for morning while a false paragraph costs one paragraph.
  See [`--watchdog`](/reference/cli/) and
  [`safety.watchdog`](/reference/workload-bundle/).

## Shipped in v0.5.0 — unattended monitoring, end to end

The parity claim, and the release names the rows it did not make: the
scoreboard reads **17 of 19** at this tag, up from 11 at v0.4.0, and the two
still open are switchboard's — in-chat Approve/Reject and the approver
allowlist. Every row that was mast's to flip is flipped. mast's accountability
for the two that are not is that the resume shape and the caller-attribution
header do not move underneath switchboard while it writes them, which is a
test that asserts the JSON names, the verdict and scope vocabularies and the
status codes as strings rather than as constants — a Go rename compiles and
passes every behavioural test in the package, and breaks a client this
compiler cannot see.

**Most of the other two projects' halves have now landed**:
[k8s-lookout](https://github.com/go-steer/k8s-lookout) ships cross-run finding
state and resource-name normalization — a scan diffed against the previous one
into new / ongoing / escalated / resolved / suppressed, keyed by a normalized
per-object subject, plus an ack that suppresses a subject for a window — and
switchboard ships agent-initiated chat egress, so a monitoring loop with an
escalation to raise at 3am no longer needs a thread someone else started.
Still theirs to do: in-chat Approve/Reject with an approver allowlist.

On mast's side, four things: zero-token collection, wiring the finding diff,
notifying only on change (with a failed post that does not resurrect the diff
next cycle), and ack plumbing. One decision shapes all four. A finding diff
genuinely *writes* —
it advances the stored state as a side effect of answering — so if the model
were the one to call it, every cycle would stop and ask an operator for
permission to find out whether anything had changed, which is not unattended
monitoring. So mast makes the collection and state-advance calls itself,
outside the model's tool surface, and hands the model the transitions already
classified. That is also why the collection leg costs nothing: a step the model
is not part of cannot spend a token.

**All four have landed.** A workload can now declare a
[`monitor.collect`](/reference/workload-bundle/#monitor--the-facts-a-cycle-gathers-for-itself)
block: a list of catalog tools mast runs itself when a scheduled cycle fires,
before the model is woken, with the results handed to it in the tick envelope
under names the bundle chose. The board's zero-token-collection row is green,
and the number is read off the meter rather than inferred — a cycle that
collects two facts and writes one report still reports exactly one model call.
Because the whole point is to run a call the model would have needed approval
for, the exception is fenced on reach instead: mast refuses to start a workload
whose specialists can get at a collect tool by any route — named in an
allowlist, covered by a whole-server grant, or inherited by a roster that
declares no allowlist at all — so a tool cannot be both mast's to call and the
model's. A collection failure aborts the cycle before any model call and is
counted as an errored fire, because a monitor that reports calm when its
collection broke is worse than one that reports nothing.

**The second is the finding diff itself.** A bundle points
[`monitor.transitions_from`](/reference/workload-bundle/#transitions_from--the-classification-comes-from-the-tool)
at one of its own collect keys, and that result rides the tick envelope as
classified transitions rather than as text the model has to re-read. What is
worth noticing is what mast did *not* gain: no list of transition classes,
no severity comparison, no fingerprinting, no second opinion on whether a
finding is really new. The classifier's verdict is carried through verbatim,
because lookout already keeps the per-run state and two implementations of
"what changed" is one more than can be right. The single check mast does
make is that the stream is *whole* — a record stream ends in a
`scanned=/findings=` summary, and one that is missing or that disagrees with
the records above it voids the cycle. A truncated answer is a prefix of a
healthy one, and the notify half is about to stay silent when nothing
changed; those two must not look the same. A cycle that classified and found
nothing says so explicitly, which is a different fact from a workload that
never classified at all.

**The third is the part an operator actually sees.** With a
[`monitor.notify`](/reference/workload-bundle/#notify--speaking-only-when-something-changed)
block naming a conversation, a cycle whose classifier reported nothing
changed **does not wake the model at all** — not "wakes it and declines to
post". A fifteen-minute cadence that spends a model call on every quiet cycle
costs more per month in nothing-happened than the incidents it exists to
catch. Consecutive speaking cycles extend one message, so an incident that
takes six cycles to resolve reads as one growing story rather than six
notifications to reassemble at 3am; the first quiet cycle closes it, and the
next incident gets a message of its own. Switchboard's two non-error answers
to an append — "I no longer remember that message" and "that message is
full, here is its continuation" — are both handled rather than surfaced.

Three deliberate absences. There is **no retry and no spool**: a send that
failed is an errored fire, because the classifier advanced its own state when
it answered, so a replay next cycle would describe a world that has already
moved on. Silence is bounded by a **wall-clock deadman** (`digest_after`)
rather than a count of quiet cycles — a monitor that has been quiet for a
week is otherwise indistinguishable from one that died a week ago. And a
cycle that *breaks* says so in the same channel, once on the way down and
once on the way back, because a monitor whose failure is visible only in a
log file is one everybody believes is working.

**The fourth runs the other way.** Everything above pushes outward on a
cadence; an ack comes back in when somebody reads their chat. A bundle with an
[`ack:`](/reference/workload-bundle/#ack--taking-an-acknowledgement-back) block
opens `POST /monitor-ack` on the daemon's inject listener, and an
acknowledgement that arrives there is attributed from the credential that
carried it, recorded durably, and forwarded to the producer's own ack tool.
Two things it is not. It is **not an approval**: no grant, no verdict, no
freshness window, and nothing in the decision export — a suppression and an
adjudication are different acts, and only one of them is a person taking
responsibility for a change. And `ack_by` is **not a field a caller may set**;
it is read off the authenticated identity at the moment of the request, and a
body that supplies it is refused by name rather than quietly overridden,
because an attribution a caller writes about itself is worth nothing after an
incident. The suppression itself stays where the state is: mast forwards, and
the next cycle reports the subject as `suppressed` because the classifier says
so — not because mast filtered it out.

Landed already on the way there: **durable budget spend**. A `max_cost_usd`
that a restart reset was a ceiling on what a workload spent per process,
which under automatic unattended restarts is no ceiling at all. Spend now
comes back from a ledger of priced calls, per session and per specialist,
along with the runway an operator granted — and a session already past its
cap is refused before the model call rather than after it. See
[budgets](/concepts/budgets/#spend-survives-a-restart).

Also on the way there, and a correction to what the judged board has been
saying: `severity_accuracy` is now reported as a **diagnostic** rather than
as one of the numbers the parity claim rests on. Two things were wrong with
it. The extractor read a verdict written as `## SEVERITY: CRITICAL` as no
verdict at all — which scores the same as a wrong answer, so a parsing
failure on one model family's heading style was published as that family
classifying worse. And the ported corpus does not state a severity
definition for the buckets most of its rows are labelled with: the
definitions it ships enumerate twelve conditions, five of the 31 scenarios
present one, and all five fall under CRITICAL, while 20 rows are labelled
WARNING or INFO. A score nobody can act on is not a comparison, so it is
reported as what it is. The v0.3 promise that a green board cannot come
from a measurement that never ran still holds — this is the other half of
it, that a measurement can reach every row and still not support a claim.

Also on the way there, the judged board now records **how** each tool was
called, not just which ones were reached for. It could not previously tell
two failures apart: a run that never called the tool, and a run that called
the right tool against a namespace holding nothing, read the empty result as
"no problem here", and reported anyway. Both landed on the board as an
unsatisfied intent, and only the first is a tool-*selection* problem. Every
call's arguments and a digest of its result are now kept and checked against
the schema the model was actually shown — an unknown tool name, a missing or
invented argument, a value outside a declared enum, a call that errored —
and the violations are listed rather than averaged into a score. A rate over
rows that call different numbers of tools is not comparable to itself
between runs, and the part a reader can act on is never the average: it is
which tool, which argument, which scope. Rows where every completed read
came back empty are named as such, because a score on one of those measures
what the model already knew about Kubernetes rather than what it read.

The companion question — which unanswered questions a different tool choice
would have answered — turned out to have a much smaller answer than the
obvious metric suggests, and the obvious metric points the wrong way. Counting
scenarios that skipped a tool holding data flags a third of one board and more
than half of the other, and **the flagged rows score higher**, because most of
those tools were redundant with one that had already answered: 14 of the 19
intents in the table are satisfiable by two or three different tools, which is
the consolidated read path showing up in the measurement. What the board
reports instead is a **consequential miss** — a question the corpus expected
answered, that a tool in the catalog would have answered, that the run never
asked. Across both v0.4.0 boards that is four rows, all of them node or pod
saturation. Four is a list, so it is printed as one, with the row, the intent
and the tools that would have served it. An unsatisfied question no read-only
tool can answer stays out: LC-13 expects a rollback, lookout excludes write
tools by design, and that is already reported as a structural ceiling.

Four misses is few enough to ask which tool each one is, and the answer is
that three of them are the same tool. `k8s_resource_top` is the only thing in
the catalog that answers either saturation question, and two unrelated model
families skipped it independently. So the board now charges a miss to a tool —
but only when exactly one tool would have answered it. That restraint is the
whole point: with 14 of 19 intents served two or three ways, charging a miss
to each candidate would triple the numbers and name no cause, so a miss with
several possible answers is counted as shared rather than blamed on anyone.
The ranking leads with how many corpus scenarios a tool is the sole answer
for, not with how often it was missed — the first is a property of the fixture
and the second is one model on one night. What this buys is an experiment:
`judge.toolDescription` lives in this repo rather than in k8s-lookout, so the
description can be rewritten and the board re-run, and the nightly delta prints
the answer as `k8s_resource_top: 3 miss(es) → 0`. The scores would not have
shown it either way — a fix worth having moves `intent_coverage` by a fraction
of one row's mean. The competing hypothesis is that the models reason past the
tool rather than fail to find it, and the same experiment separates the two.

## Shipped in v0.9.0 — the surfaces stop answering a question nobody asked

v0.8 closed a run of claims that turned out not to hold. v0.9 is the
consequence, and it has one shape. Almost everything below is a surface that
was reachable, authenticated and green, and that could not tell a caller the
one thing the caller needed: `GET /perms` returned 200 and an empty body on
every mast daemon; a session parked on a human approval reported `idle`, the
same string an idle session reports; nothing outside the CLI could show the
change an operator was being asked to approve. None of those were outages, and
that is what took so long — **a surface that answers confidently with nothing
in it is worse than one that refuses**, because nothing upstream can tell the
difference. Where a route cannot honour its contract it now refuses.

Underneath that, three defects that had never once misbehaved. Every outward
text path concatenated the model's thinking into its answer and leaked nothing
only because the default model returns a signed thinking block with an empty
body. The only thinking config mast could send was rejected by that same model
— and the unit test that let it ship asserted what mast *sends* rather than
what the model *accepts*, so it passed for exactly as long as every live call
failed. An AG-UI thread had no owner, so any authenticated principal could read
and continue someone else's conversation. Each was one vendor's implementation
detail away from being real.

**v0.9 is also the run-up to the freeze**, and two of its issues were gates on
v1.0 rather than features. Both are now written. The [compatibility and deprecation
policy](/reference/compatibility/) is a stability promise's other half — a
promise with no process for breaking things is not a promise — and it lands
with the one rule in it that a test can hold: a `// Deprecated:` marker must
name the release that removes it.

The [threat model](/reference/threat-model/) is the document a security review
asks for, and the corpus had gone 29 docs without one for a product whose whole
thesis is that an agent acts while nobody is watching. It is mostly collection —
the boundaries, the controls and the accepted risks were all reasoned out
already, just scattered across a YAML comment, five startup checks and half a
dozen issues. What it adds is the sentence none of those said out loud: **mast
does not defend against prompt injection, and the write gate is the defence.**
The boundary is placed after the model rather than before it, which makes the
gate's coverage a security property and every accepted gap in it worth naming.

Alongside them: `.tmpl` specialist files [stop
loading](https://github.com/go-steer/mast/issues/349), and the second of the
two exported-API leaks that writing down the v1.0 promise turned up
[closes](https://github.com/go-steer/mast/issues/338).

**An AG-UI client can now watch a run's state change — if you say which keys.**
Until v0.9 the only state frame a client ever saw was the opening snapshot of
its own input echoed back; `StateDelta` was an exported wire type mast never
emitted. A workload now declares
[`agui.state_projection`](/reference/workload-bundle/) and a write to a named
key arrives as an RFC 6902 patch. **The list is empty by default and that is
the point.** Session state is not a view someone designed for display — it is
whatever the run put there, graph node results and judge verdicts and approval
grants and captured change sets alike — and the client on the other end is a
browser. So publication is an allowlist, and a key you did not name emits
nothing at all rather than a redacted placeholder: a client cannot even learn
that it changed. Adding a key is a decision about a browser-reachable surface;
the daemon logs the enabled list at startup so you can check what a running
workload publishes without trusting the bundle you think is mounted.

**And a workload can now publish the model's reasoning — if you say so.**
[`agui.emit_reasoning`](/reference/workload-bundle/) streams the thinking as
AG-UI's `REASONING_*` frames, and it copies the projection's shape on purpose:
per bundle, off by default, logged at startup. Three things are its own. Off
emits **nothing at all**, not an empty phase bracket, because the existence of
a thought is itself a disclosure. The opt-in is a *deliberate read* of the
thought parts rather than the removal of a filter, so the answer stream is
identical either way and reasoning cannot reach a client by omission — which
is the standing floor v0.9 built one release earlier. And the provider's
thought **signature** has no frame under any setting: it is a replay
credential rather than a thought, so AG-UI's `ReasoningEncryptedValue` is
deliberately not implemented and a signature-only block publishes nothing.
Expect quiet at first — the request mast sends `claude-opus-5` today comes
back with the thinking block's body empty.

Also in v0.9: **the parity scoreboard was re-measured rather than re-asserted**,
it had one row attributed to the wrong repo, and that row is now green. The
board reads **19 of 19**. The **approver allowlist** is green — switchboard
shipped it on 2026-09-03, and it is stricter than the env var it was matched
against: the list is per channel, the "anyone here may answer" posture is a
value somebody wrote rather than an empty setting, and a list that would match
nobody is refused at startup instead of discovered from a thread.

**You can now approve a parked mutation from a chat button.** This was the last
red row, and it was mast's, not switchboard's: switchboard had written its half
and answers a mast daemon at `POST /sessions/<app>/<id>/perms/respond`, where
**mast returned 501** for five weeks. Both repos built a coherent half against
a different endpoint and each half passed its own tests.

The reason the route was dead is worth knowing, because the fix is not the
obvious one. The `/perms` family is a synchronous ask-the-human-at-the-keyboard
surface mast inherited with a ported package, and mast builds its write gate
with no prompter on purpose — the premise of the product is that nobody is
watching. Reviving the inherited wiring would have connected the route to a
code path that, in this daemon, nothing reaches. But mast is not un-interactive;
its interactive surface is the operator listener, where a parked mutation is
answered by an authenticated `POST /resume`. So the route was not missing a
mechanism, it was pointed at the wrong one. The two prompt endpoints now take
a source, and mast supplies one backed by its **durable approval park** — the
same questions `mast sessions` shows you, projected onto the stream, answered
through the same resume path. A client cannot tell which mechanism is
underneath, which is the point.

Three things to know if you are writing that client. The capability report is
now **exact**: `perms_stream` is true when the routes will actually answer, and
false when they will not. Only **deny** and **allow-once** are accepted — the
four broader decisions the protocol defines are refused with a `400` naming the
reason rather than quietly narrowed, because a durable mutation approval has
never been grantable for a whole session and a button that means more than it
says is worse than an absent one. And the ack tells you **who** approved when
mast recorded an identity, rather than only that somebody did.

What has not changed: `mast sessions` plus an authenticated `POST /resume`
still works exactly as before, and is still the path for an operator without a
chat client. Shipped as
[#364](https://github.com/go-steer/mast/issues/364), on top of
[#313](https://github.com/go-steer/mast/issues/313) — see below.

Which brings the other half of v0.9's attach work: **a session waiting on a
human now says so.** Until this release a parked session reported
`turn_state: "idle"` — the same string a session that finished its work
reports — so nothing watching the stream could tell the difference between
"done" and "blocked on you". It now reports `awaiting_permission` when the
write gate has parked a mutating call, and `awaiting_elicit` when the session
asked you a question. Both values were declared in the protocol from the
beginning and neither had ever been sent.

The reason it took this long is worth knowing if you write a client: **a
parked session really is finished**, from the daemon's point of view. The
gate does not block inside the call waiting for you; it writes the question
down and lets the turn return. So the answer is read from the transcript, not
from whatever the process happens to be doing, and two useful properties
follow. It survives a restart — a session parked yesterday still reports
`awaiting_permission` today. And it is in the snapshot every client gets on
connect, so attaching an hour after the park still shows you the park.

Two pauses deliberately stay `idle`. A `pause_session` hold is something an
operator placed, not a question anyone owes an answer to. And a session
resuming from a park reports `streaming` for the whole resume turn, even
though the interrupt it is answering stays open on the transcript until
mid-turn — otherwise the session would look frozen on your screen while it
works.

One ordering fix ships with it: **`turn-complete` no longer arrives before the
answer it terminates**. The terminal frame went straight out the moment a turn
returned while the turn's final text travelled through the event log, so a
client that finalized its render on `turn-complete` could drop the last
message. The frame now waits for the log to catch up — and if the log cannot
be read, or takes longer than two seconds, the frame goes out anyway and the
daemon logs why. A late frame is a correctness bug; a missing one is worse.

**And an out-of-process caller can finally read the change it is asking
someone to approve.** `POST /resume` has been able to *answer* a parked
approval since v0.2; until v0.9 nothing could find out what it was answering,
because the projection that renders a park lived in `pkg/transcript` and was
CLI-only. So a chat bot or a web UI wiring up an Approve button could offer
"approve the thing, whatever it is" — uninformed consent with an audit trail,
which is worse than the CLI it replaces because it looks like the opposite.
Two read-only routes close it: `GET /parks` for every parked session and
`GET /parks/{session}` for one, the latter answering 200 with an empty list
when the session is not parked and 404 only when there is no such session,
because "nothing to approve" and "I cannot see parks" are different answers
and only one of them means it is safe to stop looking. What the projection
carries is written out field by field, so a field added to the transcript
cannot reach that wire without someone deciding it should: no model output, no
tool results, and — for a capture — the read's name and digest but never its
values, because those are cluster state and can be anything.

**A client can now tell in advance what a run will publish**, and it matters
precisely because the two publication keys above are per bundle. A stream with
no reasoning in it looks identical whether the workload publishes none or the
model simply did not think, so inferring the setting from a finished run is
hopeless. `/agui/agents.json` now states it up front as a `capabilities`
object per workload, read from the same value the run's emitter is built from
rather than recomputed from the bundle — a capability claim that restates a
config instead of reading what it describes is how `/tools` and `/perms` each
spent releases advertising something untrue.

A third frame family ships with **no** key and no capability bit, and the
contrast is the useful part. `STEP_STARTED`/`STEP_FINISHED` now bracket each
stretch of a run by the agent that authored it, so a client can see where a
coordinator handed off to a specialist. State and reasoning are gated because
they disclose something new; a handoff already arrives on the wire as a
`transfer_to_agent` tool call naming the same agent, so gating steps would be
a switch with nothing behind it.

**An AG-UI thread now belongs to the caller who opened it.** A `threadId` is
the client's own correlation string, not a secret, and under the default
`per_thread` model the thread *is* the durable session — so carrying the
endpoint's scopes let any authorized principal continue and read back another
one's conversation. The two questions look like one and are not: *may this
caller run this workload* was answered; *is this conversation yours* was
answered by nobody. The fix folds a hashed tenant and subject into the derived
session id rather than recording an owner and checking against it, which makes
the separation structural — a foreign caller is not refused, they address a
session of their own and never reach the first. No owner record to store or
clear, and no 403-versus-404 disclosure question, because nothing is refused.
**Upgrading:** authenticated threads opened before this release are not
reachable after it. An endpoint with no validator has no subject to own
anything and is unchanged.

**And a client hanging up no longer destroys the run it was watching** — see
[Further out](#further-out) for why that was filed as half of "reconnect" and
shipped as a correctness fix instead.

**The model's thinking is not its answer, and now nothing treats it as one.**
Frontier models return reasoning in the same field as the answer, marked
rather than separated, and eight outward paths read model text with a
predicate a thinking block satisfies. Nothing leaked, for one reason: the
default model returns a signed thinking block with an empty body. Part order
decided which sites would have leaked first. The predicate now lives in one
place, and a ninth site closed a release later.

The companion defect is the more instructive one. **The only thinking config
mast could send was rejected by its own default model.** Anthropic has two
mutually exclusive request shapes and they split *inside* one provider, by
model — so a provider-grained capability flag would have been the wrong grain
— and a zero budget, which read like "off", disabled nothing. The test that
let it ship asserted the request shape mast *builds*; it passed for exactly as
long as every live call 400'd.

**Anthropic cache writes are billed at their own rate.** A long-running
session was undercounted because the count had nowhere to live — the fix is a
usage-detail sidecar, landed before `pkg/budget` freezes rather than after.

## Shipped in v0.8.0 — what was quietly not true is now refused

v0.7 added capability. This release adds very little, and instead closes a run
of things the project had been asserting that turned out not to hold. None of
them were caught by a test, for the same reason in every case: each was a
**claim** rather than a code path. Tests exercise what the code does.

**Three breaking changes, and the first is silent.**

- **A provider's own server-side tools are off unless a bundle asks.** Every
  Gemini model mast constructed was wrapped with `GoogleSearch: true,
  URLContext: true`, and no config key reached it. These tools are invisible
  to every control mast has — a built-in runs inside the vendor's
  infrastructure and its result arrives folded into the response, so it never
  becomes a tool call: nothing for the permissions gate to allow, nothing for
  the write gate to park, nothing for the effect outbox to record. A
  specialist declared `read_only` could read the public internet, and four
  releases of hardening were looking the other way. A bundle now opts in with
  `builtin_tools:` — provider-neutral keys, mast's baseline off on *every*
  provider rather than each vendor's, so the same bundle does not change an
  unattended agent's reach when you change `--provider`.

  **Upgrading:** a workload that wants grounded search must now say so. It
  will not error; it will answer worse.

- **An unrecognised key in `workload.yaml` is a load error.** This is the only
  file in a deployment that can declare a tool safe to run without an
  operator, and mast's predicate is default-deny-unknown — so the failure
  being designed against is a misspelled block that leaves a section of policy
  unapplied while the daemon logs a clean start and the workload runs all
  night. A `WARN` at boot is not a control. Strictness reaches nested keys,
  where the sharper version lives: `mutatin: true` on a catalog entry leaves
  the tool catalogued, so nothing looks missing.

- **`budget.Limits.Catalog` is now `budget.Limits.Pricer`**, a one-method
  interface the meter owns, so freezing `pkg/budget` at v1.0 stops freezing
  `pkg/pricing` alongside it. The daemon and `mast.RunWorkload` are
  unaffected.

**v1.0 now has a definition, and it is the API freeze and nothing else.** Six
import paths plus `cmd/mast`'s flags, verbs and exit codes; the other 27
packages under `pkg/` named individually as unsupported. Writing it down meant
discovering that the promise the corpus had carried since 2026-07-25 named two
packages that **never existed**, and enforced its own exceptions with a marker
never once written into a `.go` file. v1.0 is explicitly **not** a
production-readiness claim — see [stability](/reference/stability/).

**Specialist files are `<name>.specialist.md`.** They were `.tmpl` and were
never Go templates: `text/template` is imported by one package in the module
and reads none of them. The name was not merely inaccurate — a `{{ ... }}` in
a body is ADK's placeholder syntax, not the author's, and mast had to add a
load-time refusal for a defect the extension invited. `.tmpl` still loads with
a warning through this release and stops loading in v0.9.

**Also:** `gemini-3.8-flash` is priced and classified, and the frontier
default deliberately stays at `gemini-3.7-flash` — 3.8 costs exactly what 3.7
costs on the same window, so promotion would move no ceiling and the only
argument left is that the id is newer. Four further corrections changed no
code: mast no longer claims the library gets the daemon's subsystems,
"multi-provider" is stated as two vendors and four deployment paths rather
than counting a fake and a cache as peers, the deployment ambition is settled
as *someone else installs this* (see below), and `docs-lint` now reads version
claims — the rule whose absence let this page's install instructions serve
v0.4.0 download URLs, under a green check, for three releases.

## Shipped in v0.7.0 — a route back from a change, and a gate a real model can red

v0.6 closed the distance between what a bundle promised and what the runtime
enforced. This release is about the two things still taken on trust after that:
**a change mast makes carries a route back, the write gate's question is a
measurement, and a real model's behaviour can red the build.** The scoreboard
is unchanged at 17 of 19 — both rows still red are switchboard's to write.

- **A change carries a record of what it overwrote.** A tool in the workload's
  catalog can declare a `capture:` block — a read-only tool that records the
  target's prior state, the fields to keep, and optionally the call that puts
  them back. The write gate runs that read *before* the call fires and writes
  what it found, plus the proposed revert, into the session's durable log;
  `mast sessions show` prints the old values and the exact call and arguments
  that restore them. It covers all four paths a mutating call can take, and it
  is fail-closed: everything happens before the forward call, so a read that
  fails refuses the call while nothing has happened yet.

  Three things it deliberately does not do. mast does not **derive the read** —
  which tool reads the object a write is about is domain knowledge, and the
  neutrality that keeps a Kubernetes schema out of mast keeps one out of this.
  It does not **derive the inverse**: a scale inverts by re-scaling, a delete
  does not invert at all, and a patch inverts only over the fields it touched.
  And it does not **fire the inverse** — the recorded revert is a proposal that
  goes back through the same gate with a person answering, because an automatic
  rollback is a mutating call nobody approved.

- **The write gate's record is readable as a measurement.** Everything needed
  was already durable, and nothing could ask for it: the eval trace treated the
  confirmation call as engine control flow, so a run where the gate asked and a
  run where it never did projected identically. A gated call now carries its
  question and its answer, and the outcome tier gains an `approval_requested`
  check that reds a workload which mutated without parking. It reads the
  **question**, never the verdict, including on a call the operator refused —
  the claim is that the change was put to a person, not that they allowed it,
  and in a test the answer comes from the harness, so a check reading it would
  be asserting that the test rig ran. It compares arguments rather than counting
  parks, because a gate that asks about one call and runs another is not a gate.

- **A real model's behaviour reds the build.** The new **O — outcome** tier runs
  a real model against a real workload on a real `kind` cluster, on every pull
  request. Three crash-loop cases, five repetitions each, graded on the run's
  tool calls, its final report and the cluster's own state before and after —
  five because the difference between diagnosing an OOM three times in five and
  five in five is the whole product. Four rungs: a catastrophic safeguard
  violated in any single repetition, which demotion never reaches; a required
  check that turned out **vacuous**; every repetition of a case failing; and a
  case that ran fewer times than it should have. A check that measured nothing
  is a red, not a pass, in both directions — the passing direction is the one
  nobody investigates.

  The ceiling is 20 minutes for the whole pass, a deadline the runner checks
  between cases rather than a job timeout, so a pass that runs out of budget
  produces a short board that reds instead of a cancelled job with no board. A
  pass measures 2m47s locally and 2m48s on a hosted runner, so admitting a
  fourth case has to argue for raising the ceiling in a reviewable diff. An
  unconfigured run fails rather than skips — for a gate, *green because it could
  not run* is exactly the rung that cannot fire — and the cost of that is that a
  fork's pull request cannot run the tier.

- **A release refuses a commit the tier has not passed.** The release workflow
  reads the `outcome` check run for the SHA the tag points at and refuses on
  anything but success, *including on its absence*. `outcome` is deliberately
  **not** a required check on `main`: requiring it would make a fork's pull
  request unmergeable forever, and would buy stopping a red from landing rather
  than from shipping. The accepted cost is that a red can land on `main` and the
  refusal arrives at the tag.

- **A metered cost is the price of the call that was made.** The exact-pricing
  catalog had never been assigned at any of its three construction sites, so
  every session metered at the flat blended rate the catalog exists to replace;
  and thinking tokens, which Gemini reports in their own counter and Google
  bills at the output rate, were not counted at all. Measured on a live GKE
  triage workload: $1.13 flat against $0.19 exact, and 6,449 thinking tokens
  against 1,180 candidate tokens. The two move the meter in opposite directions
  and do not cancel — one removes a 5.9× overcharge, the other adds back the
  ~36% it was masking — and both had to land before v0.6's pre-call ceiling was
  computing from a number that meant anything.

- **A change executor under `dispatch: graph` can make the write an operator
  approved.** One turn used to raise two parks — the write gate held the
  mutating call and the executor's own node raised a result-approval interrupt
  over a result that did not exist yet — and answering either stranded the
  other. A node now parks behind its child's question rather than adding one of
  its own, and finding verdicts are durable in session state, so an answer given
  one turn is not asked again the next.

- **The cluster read/write split bounds the path mast actually uses.**
  `WRITE_SCOPE=namespaced` shipped opt-in for four releases and is now the
  default, because running it against live GKE turned up why it had been out of
  reach: GKE does not resolve the Workload Identity Federation principal to the
  KSA's RBAC ServiceAccount subject, so the shipped bindings granted the MCP
  path — the one the agent's tools take — nothing at all. Both bindings now name
  both subjects, and the matrix refuses to report green without a project id,
  since measuring only the in-cluster path goes green on a cluster where mast
  cannot write. Measured 41/41 against the rendered shipped manifests.

- **The daemon says which configuration it is running**, and says when the files
  on disk stop matching it. mast reads its bundle once and reloads nothing,
  while Kubernetes rewrites a mounted ConfigMap under the running pod — so an
  edit could land on disk and change nothing, with no line an operator could
  grep. Startup logs a digest over exactly the files the loaders read, and the
  daemon warns once per edit when they diverge. It still never reloads: what a
  mid-flight turn or a parked approval should do when the bundle changes
  underneath them is a design question, and this is a diagnosis.

## Shipped in v0.6.0 — a ceiling that stops a call, and a refusal that is the design

The first release the parity scoreboard did not choose. It reads 17 of 19 and
the two rows still red are switchboard's to write, so this release states its
own claim: **no mutating call goes unrecorded, no call is paid for before it
is checked, and where enforcement cannot reach, mast refuses rather than
pretends.** Nothing here adds a tool, a bundle block, an endpoint or a flag —
if it shipped correctly, an existing bundle behaves the way its author already
believed it did.

- **A cost ceiling refuses the call that would cross it.** `max_turns`,
  `max_tokens` and `max_cost_usd` are checked before the model is called
  rather than after it answers. Spend used to be folded out of the event
  stream only after a call returned, so the first thing that happened when a
  workload ran out of money was that it spent more; on turns it was not
  merely late but structurally unable to be right, because `max_turns` was
  checked with `>` and **a workload capped at 3 turns had always made 4**.

  The pre-call check asks a different question — not *has a ceiling been
  crossed* but *can this ceiling still be respected*. It never estimates the
  next call's size: a projection refuses affordable work on a bad guess and
  permits unaffordable work on a worse one, and you cannot tell those apart
  from outside. It refuses only where the arithmetic is a proof. The
  post-hoc fold is untouched and is still the durable ledger.

  A refusal is a synthesized response rather than an error — a cap that
  fires must not arrive looking like a crashed tool — and it leaves no
  phantom spend. From outside, `BUDGET_REFUSED` and `BUDGET_EXCEEDED` are
  one wall reported with two codes: both are `cost_ceiling`, both carry the
  reset hint, and they differ only in which one spent money.

- **A spent specialist closes one path, not the session.** A crossed
  specialist ceiling is handed to its coordinator as a report it can route
  around, the same as a specialist that declines, and the workload finishes
  through whatever paths still have budget. Only the workload's own ceiling
  ends the turn — there is nothing left to route to. Through v0.5 the
  opposite held, and it was never a decision: cancelling the run was the
  only lever the after-the-fact fold had, so *one path is spent* and *the
  workload is over* came out the same way.

  Because a run that quietly loses half its roster otherwise looks exactly
  like one that did not, the loss is reported in four places: the trip
  counter and a WARN line naming the specialist, `cost_ceiling.scopes[]` on
  `GET /guardrails`, and `Result.Exhausted` for a library caller. A
  session-scoped guardrail reset says honestly when it cleared nothing, and
  names who is still out and the `scope=` to raise them with.

- **A dispatched specialist's mutations reach the effect ledger.** Under
  `hitl.on_mutation: apply`, an interrupted planner dispatch now leaves a
  dangling intent that the next boot's auto-resume scan can see. Recording
  is one-directional, so unlike the write gate it crosses the dispatch
  boundary freely: a recorder on the sub-run observer seam writes each
  dispatch's mutating intents and completions to the outer session's
  companion operations row. A failed intent write stops the dispatch —
  under `apply`, that record is the only control the call has.

- **A rate belongs to the (backend, model) pair.** Prices are keyed on
  `<backend>/<model>` over `anthropic`, `anthropic-vertex`, `gemini` and
  `vertex`, and looked up for the pair the call will actually be billed
  against. Through v0.5 a price was something mast *reported*, and a
  reported number can be approximately right; a pre-call ceiling makes a
  price the input to a refusal, where it is either the price of the call
  about to be made or it is not. It was not — the builtin table's bare
  model ids are a mixture of two backends, so Claude-on-Vertex was priced
  off the first-party row and Developer-API Gemini off the Vertex row. This
  moves no number today, because every shipped model currently costs the
  same on both of its backends; that agreement is upstream's to keep and
  not mast's to depend on.

- **The dominant-tool-call density detector stays opt-in**, recorded as a
  decision rather than a pending one. The default watchdog posture is
  `feedback`, so an alert is not a log line an operator triages — it is a
  paragraph prepended to the next turn's prompt on a workload with nobody
  watching, which makes a false positive an instruction. This detector's
  false positive is a polling workload, which is exactly the shape v0.5's
  scheduled monitoring ships. It remains available to any caller that
  builds its own signal set and knows its workload does not poll.

## What v0.9.0 will not let you do

**Restore.** A change now carries a recorded route back — the prior state and
the exact call that undoes it — and mast will not fire that call. Rendering the
undo is a question about the past, which the capture answers; deciding that
firing it an hour into an incident is still the right thing is a question about
the present, which nothing in a recording can answer. So the revert goes back
through the same gate with a person answering it, and a workload that declares
no `revert:` gets a record whose undo is marked undeclared rather than a guess.
That is scope, not sequencing.

A monitoring workload that also **remediates** — a planner roster holding a
`change_executor` — is refused at startup whenever `hitl.on_mutation` asks for
the write to be gated. The write gate and the effect outbox are runner
plugins, and `invoke_specialist` builds its runner without them, so a mutating
call made inside a planner dispatch would neither park nor dry-run. Refusing
is the honest answer to that: the alternative is executing an unapproved write
on a bundle that asked for approval. The escape is cheap — the same roster
runs under `coordinator` or `graph`, where the runner carries the gate — and
the startup error names the specialist and says so.

**This is the design, not containment**, and
[#235](https://github.com/go-steer/mast/issues/235) closed on that answer
rather than on a fix. An approval comes back through the session event log — a
park writes its question there and a resume re-enters at the root — and a
dispatch runs on a private in-memory session that dies with the tool call, so
there is nowhere for an answer to return to. Giving the sub-run the host's
session service would buy the gate by spending the context isolation the shape
exists for; gating `invoke_specialist` itself would have you approve a
specialist name and a sentence of prose. So what the refusal names —
`coordinator`, `graph`, `on_mutation: apply` — is what you do, and each of
those three is covered by a test that builds it.

Under `on_mutation: apply` the refusal does not fire, because there was no
gate for a dispatch to bypass. The **record** was missing there too through
v0.5, and that half was separable and shipped in v0.6.0 — see above.

**Stop an AG-UI run from the browser.** New in v0.9, and the direct cost of
the disconnect fix: closing the tab no longer cancels anything, because a
transport dropping is not a decision to abandon work. A run with no reader
left is bounded by its wallclock budget, the watchdog, and an explicit
`mast sessions pause <id> --cancel-turn`. A client that needs to *mean* stop
needs a verb for it, and AG-UI has none on mast yet — `pkg/attach` already
holds the shape (`POST /interrupt`), so this is a wiring decision waiting on a
consumer rather than an open question. **Rejoin a stream you dropped** is the
other half and is also unbuilt: reconnecting with the same `threadId` starts a
new run rather than resuming the view of the old one, so a blip costs the tail
of the event stream.

**Let an AG-UI client bring its own tools.** AG-UI's `RunAgentInput.tools`
declares tools the *browser* will execute — highlight a range, fill a form,
confirm a step. mast parses the field and ignores it, and no workload can call
one. The gate for it was designed on 2026-09-17 and the implementation is
v1.1: a bundle opts in, the opt-in bounds the class rather than naming tools
(an operator cannot know what a frontend ships next week), client tools count
as **mutating** by default so an approval-gated workload parks before the
browser is asked to act, and only the root agent may call them — not
specialists, which is the limit most likely to send this back for another
pass. Designing it turned up
[#389](https://github.com/go-steer/mast/issues/389): the daemon's AG-UI and
A2A endpoints each hold a single shared bearer token, so per-workload scopes
are checked and cannot refuse, and any gate hung on *who is calling* would
have been decoration.

**Stack runs on a busy thread without an answer.** Through v0.9.0, one turn
ran per thread and a second run waited for the first, unbounded, with the
workload's wallclock budget the only ceiling — so a caller could not tell
waiting from wedged, and the eventual failure was a timeout rather than a
refusal it could act on. **Fixed after v0.9.0** and listed here because it is
the last release you can still hit it on: the thread now has a bounded queue
([`agui.run_queue.depth`](/reference/workload-bundle/#run_queue--bounding-concurrent-runs-on-one-thread),
default 3 waiting plus the one executing), and the run past it is refused with
a `409` and a `Retry-After` before the stream opens. It had to land before the
v1.0 freeze rather than after: bounding a queue that was unbounded changes
what an existing bundle does, which after v1.0 costs a deprecation cycle
rather than a release note — [#384](https://github.com/go-steer/mast/issues/384).

## What installing it costs you today

mast is built to be a thing **you** install, not a service someone runs for
you. That was settled on 2026-09-11 and written down in
[`positioning.md`](https://github.com/go-steer/mast/blob/main/docs/positioning.md#who-installs-mast-answered-2026-09-11-closing-291);
it is why the operator surface, the workload bundle and the durability
guarantees are shaped the way they are. It is also a bill the product has not
finished paying, and the four unpaid items are worth knowing before you
deploy rather than after.

- **The install is a kustomize base, not a package.** `deploy/` plus
  `scripts/setup-wif.sh`, applied by you. There is no chart, no Terraform
  module, no Homebrew tap, and release tarballs are not signed —
  [#342](https://github.com/go-steer/mast/issues/342).
- **Run one replica.** The scheduler is single-instance by design: two
  replicas of a scheduled workload each keep their own cadence and **both
  fire**, with nothing warning you. `ScheduledTrigger.Jitter` staggers those
  duplicate fires, it does not deduplicate them, and the heartbeat lease in
  the event log guards a *session* rather than a fleet. The first fix here is
  making the trap loud, which needs no coordination mechanism at all —
  [#345](https://github.com/go-steer/mast/issues/345).
- **One mast, one tenant.** A bundle carries no `isolation.scope`. The design
  for `per_request` / `per_tenant` / `global` exists in
  `deployment-design.md` and none of it is built, so the second team to
  install mast alongside the first shares a session store with them —
  [#344](https://github.com/go-steer/mast/issues/344).
- **Config drift is diagnosed, not reconciled.** The ConfigMap name is
  stable and there is no hot reload, so an edit can land on disk and change
  nothing until the pod restarts. The daemon logs a digest of what it loaded
  and warns when the mounted files stop matching it — that is the answer,
  chosen deliberately over reconciliation
  ([#343](https://github.com/go-steer/mast/issues/343)). The absent CRD is
  part of the same answer and is **not** on this list: mast is a workload you
  schedule, not a controller you extend.

The first three are gaps. The fourth is a decision that looks like a gap,
which is why it is spelled out rather than left to inference.

## Next

**v1.0 is the API freeze and carries no other claim.** What it covers is
[published now](/reference/stability/) so you can decide what to depend on
today: six import paths plus `cmd/mast`'s flags, verbs and exit codes, with
every other importable package named individually as unsupported. It is not a
production-readiness badge, and saying so is the point — the version number
stops being a proxy for a judgement nobody made.

Two things have to land before it, because both get harder the day the freeze
starts. The exported surface wants **shrinking**: runtime glue belongs under
`internal/`, and moving a package after v1.0 is itself a breaking change
([#301](https://github.com/go-steer/mast/issues/301)). And the AG-UI bundle
keys still owed a shape — the concurrent-run policy
([#384](https://github.com/go-steer/mast/issues/384)) and the client-tool
gate — need their *key* decided even where the implementation slips, since a
key lands on the frozen `Bundle`.

## Further out

- **AG-UI remaining slices** — the `agui://` federation client, the
  `ACTIVITY_*` frames, webhook push, client-declared tool acceptance
  (`RunAgentInput.tools` is parsed today and then dropped), and
  reconnect proper. Per-key `StateDelta` emission, the reasoning events, the
  `STEP_STARTED`/`STEP_FINISHED` bracket and the discovery `capabilities`
  object all left this list in v0.9.

  Reconnect was filed as one thing and was two, and the correctness half
  shipped in v0.9. **A client disconnect used to cancel the run** rather than leave it to finish — the turn rode the request's context
  — and if the drop landed while a mutating tool was executing, the session
  kept no record of the call, so a retry re-applied the change. Fixed in
  v0.9: the turn now outlives its reader, and a frame that cannot be written
  retires the stream instead of the run. Reconnect proper — a replay cursor,
  so a client can rejoin a stream it dropped — is the actual feature, and it
  was easy to defer once a blip costs only the tail of a stream.

  One consequence is worth stating rather than discovering: **hanging up no
  longer stops anything.** A run whose client vanished is bounded by the
  workload's wallclock budget, the watchdog, and an explicit
  `mast sessions pause <id> --cancel-turn`, exactly as a run the daemon started
  on a schedule with no client at all. A browser that means "stop" needs a verb
  for it; AG-UI has no such endpoint on mast yet.

  Still ahead of the rest: **there is no concurrent-run policy**. A second
  run on a thread waits on the session's turn lock with no depth limit and
  no refusal, so a caller queued behind a long turn learns nothing until its
  own wallclock budget expires.

  **Client-declared tools need a gate that does not exist yet.** The plan was
  to intersect what the browser declares against the bundle's tool catalog,
  but that catalog is a policy table rather than a permission list, and the
  allowlist that *is* one covers tools mast itself wired — which a
  browser-executed tool never is. So the honest question is what allowing one
  even means: everything mast does to a tool call reads a tool it can
  describe, and a client tool is a name and a schema supplied per run.
  Parsed-and-dropped stays the behaviour until that has an answer.

  What is left of the activity family is the **planner** half, and it is
  blocked rather than unscheduled. A planner dispatch runs each specialist
  under a private runner, so none of its events reach the stream the AG-UI
  emitter reads — coordinator and graph dispatch never had that gap, which is
  why their handoffs already bracket as steps. Opening it means deciding
  whether a planner-dispatched specialist's interior may reach a browser at
  all, which is a publication question of the same class as reasoning and
  wants its own answer. A client is not blind to the dispatch in the
  meantime: the `invoke_specialist` call is an ordinary tool call on the
  stream. It just cannot see inside it.
- **Planner shapes** — the `run_shape_*` vocabulary tools wired to the
  reference-graph library (they return `not_implemented` in the v0.2
  scaffold), plus more starters: supervisor+workers, sequential pipeline,
  map-reduce, adversarial verifier, autonomous loop.
- **Multi-session substrate** — `mode: multi_session` bundles honored.
- Shared memory + audit-derived memory, multi-tenant isolation scopes
  ([#344](https://github.com/go-steer/mast/issues/344)), MCP credential
  resolution, full mast-native federation, bundle learning.

## The design corpus

Every claim above traces to a design doc in the repo —
[`docs/README.md`](https://github.com/go-steer/mast/blob/main/docs/README.md)
is the index and carries the resolved-decisions table. Start with
[`positioning.md`](https://github.com/go-steer/mast/blob/main/docs/positioning.md)
(the thesis) and
[`fork-design.md`](https://github.com/go-steer/mast/blob/main/docs/fork-design.md)
(the plan this roadmap is cut from).
