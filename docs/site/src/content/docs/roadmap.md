---
title: Roadmap
description: What v0.4 ships, and what lands after it — honestly.
---

mast is at **v0.4.0** — an operator approves the exact call that will fire,
the loop runs on a schedule without an orchestrator, and every verdict becomes
a labelled eval row. On the v0.3.0 write gate and the v0.2.0
durable-execution spine. See [Shipped in v0.4.0](#shipped-in-v040) below.

**All eleven v0.1 exit criteria from the fork design are green.** The
`--task` profile criterion cleared with the P1.3a/P1.3b adapter ports and
was verified against live endpoints on 2026-07-29 (gemini one-shot with
grounded search on the tier defaults, Claude on Vertex completing the
Task-mode tool loop, sessions durable across runs and providers). The
final criterion — attach-mode reachability from mast-web — cleared the
same day with the P1.3c port: the real mast-web SPA, served in proxy mode
against a live `mast serve --attach-listen` daemon, connected, listed
sessions, and round-tripped a prompt through a real turn over SSE.

## Stability, precisely

Semver stability from v0.1 is reserved for the **five packages the four
pillars stand on**: the root `mast` package, `agent`, `transcript` (the
session operator surface — named `session` pre-v0.1.0, renamed to avoid
colliding with ADK's own `session` package), and the `provider` and
`tool` interfaces. Everything else is explicitly
**experimental** — API may change without a deprecation cycle until the
version named in the
[library API design's import-surface table](https://github.com/go-steer/mast/blob/main/docs/library-api-design.md).

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
  `core-agent@25d8531c`. `mast serve --attach-listen` binds the surface
  (requires `--session-db`; bearer auth via `MAST_ATTACH_TOKEN`;
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
  per-key state deltas, and client-declared tools are v0.3.
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
  that scored nothing. Scenarios the tool surface cannot satisfy are listed
  as structural ceilings rather than folded into a low score.

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
  `claude-haiku-4-5` and billed $0.00256 for 854 tokens, where the
  `claude-opus-5` root's rate would have charged $0.01281. On Gemini:
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

## Next: v0.5 — unattended monitoring, end to end

The parity claim, and the release where the other two projects land their
halves: cross-run finding state and resource-name normalization in
[k8s-lookout](https://github.com/go-steer/k8s-lookout), chat egress and
in-chat Approve/Reject with an approver allowlist in switchboard. On mast's
side: zero-token collection, wiring the finding diff, notifying only on
change (with a failed post that does not resurrect the diff next cycle), and
ack windows.

## Further out

- **AG-UI remaining slices** — the `agui://` federation client, per-key
  `StateDelta` emission, activity/reasoning events, webhook push, and
  client-declared tool acceptance.
- **Pre-call budget gating** — today a ceiling is crossed by the call that
  reports it, and a crossed specialist ceiling stops the session rather
  than handing the coordinator a refusal it can route around. Both need a
  seam in front of the model call rather than behind it.
- **Planner shapes** — the `run_shape_*` vocabulary tools wired to the
  reference-graph library (they return `not_implemented` in the v0.2
  scaffold), plus more starters: supervisor+workers, sequential pipeline,
  map-reduce, adversarial verifier, autonomous loop.
- **Multi-session substrate** — `mode: multi_session` bundles honored.
- Shared memory + audit-derived memory, multi-tenant isolation scopes, MCP
  credential resolution, full mast-native federation, bundle learning.

## The design corpus

Every claim above traces to a design doc in the repo —
[`docs/README.md`](https://github.com/go-steer/mast/blob/main/docs/README.md)
is the index and carries the resolved-decisions table. Start with
[`positioning.md`](https://github.com/go-steer/mast/blob/main/docs/positioning.md)
(the thesis) and
[`fork-design.md`](https://github.com/go-steer/mast/blob/main/docs/fork-design.md)
(the plan this roadmap is cut from).
