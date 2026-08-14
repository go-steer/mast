---
title: Roadmap
description: What v0.2 ships, and what lands in v0.3 — honestly.
---

mast is at **v0.2.0** — the durable-execution spine plus the ecosystem
interop surfaces, on the v0.1.2 hardened-shutdown base. See
[Shipped in v0.2.0](#shipped-in-v020) below.

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

## Landed toward v0.3 (unreleased)

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
  previous night. It reports; it never gates — no score can fail a build,
  and the only things it flags are a scenario that did not run and a metric
  that scored nothing. Scenarios the tool surface cannot satisfy are listed
  as structural ceilings rather than folded into a low score.
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
  quietly narrowed.
- **Read-only diagnosers** — a specialist now declares whether it is allowed
  to change anything, and read-only is what it gets by saying nothing. Mast
  refuses to start a workload in which a read-only specialist can reach a
  tool that changes something — whether it names one, helps itself to a whole
  tool server, or simply inherits the workload's catalog without saying which
  tools it needs. So a diagnosis specialist is not kept in its lane by the
  wording of its prompt; it structurally has nowhere else to go. The shipped
  GKE triage bundle is now seven read-only diagnosers that name the
  remediation, plus one change executor that carries it out under the write
  gate — and which specialists can change your cluster is a startup log line,
  not something you work out by reading three files.
- **ADK v2.2.0** — the agent substrate is upgraded from v2.1.0. The fix that
  motivated taking it now: a workflow-graph run whose invocation context was
  cancelled from outside — an evicted attach session, a dispatch deadline, an
  operator abort — could finish reporting success. It now reports the
  cancellation. Human-in-the-loop resume also became deterministic and can no
  longer run a tool twice within one resume. One wire-visible consequence: the
  attach `agent` frame embeds ADK's event struct verbatim, and its JSON field
  names moved from `PascalCase` to `camelCase`; stored sessions are unaffected
  and the operator UI already reads both.

## Further out (v0.3+)

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
