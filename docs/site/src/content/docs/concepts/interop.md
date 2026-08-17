---
title: Interop surfaces
description: Five ways into a running mast daemon — inject, attach, A2A, AG-UI, and federation — aimed at five different callers, each its own trust boundary.
sidebar:
  order: 7
---

An unattended agent is only useful if something can reach it. mast exposes
several surfaces rather than one, because "an alertmanager webhook", "an
operator watching a live incident", "another team's agent", and "a chat UI"
are genuinely different callers with different auth models — and collapsing
them into one endpoint means the weakest one sets the bar.

Every surface is **off by default except inject**, and each carries its own
token. That is deliberate: they are separate trust boundaries, not four
views of one.

## Inject — the machine trigger

The daemon's own HTTP endpoint (`--listen`, default `:7777`): `/inject`,
`/resume`, `/abort`, `/metrics`. This is how an incident gets in — an
Alertmanager webhook, a Cloud Scheduler job, a CI step, a `curl` in a
runbook. Bearer auth via `MAST_INJECT_TOKEN`; unset means unauthenticated,
which is a dev-only posture and is warned about at startup.

If you are wiring monitoring to mast, this is the surface. The others are
additions to it.

A Cloud Scheduler job or a Kubernetes CronJob posting here still works, and
it remains the right answer when you already run one — your schedules live
in one place and mast is just another target. What changed in v0.4 is that
you no longer need one to run something periodically: see [no caller at
all](#no-caller-at-all--the-workloads-own-clock) below.

## Attach — the operator's live view

`--attach-listen` serves the mast-native attach protocol (HTTP + SSE) that
[mast-web](https://github.com/go-steer/mast-web) speaks: live-tail a
running turn, read a session's transcript, inject an operator message,
answer a parked approval. It needs `--session-db`, since the live tail
pumps from the event log.

`GET /sessions/{id}/tools` lists the tools the daemon actually holds, each
with the MCP server it came from and a `gate_state` — what the
[write gate](/concepts/approvals/) would do to a call of it: `allowed`,
`prompted` (it parks for approval), or `denied` (`on_mutation: dry_run`, so
it will never run). The list is read from the live MCP servers, not from
the bundle's declaration, and refreshed at most every 30 seconds; a server
that fails to answer is left out and logged rather than blanking the rest.
Tools registered outside MCP — the planner's own dispatch calls — are not
listed yet.

`GET /sessions/{id}/subagents` lists the roster the daemon **loaded** —
what this thing can do. (`/agents` lists what has been *spawned*, which for
mast is always empty: every dispatch shape resolves its specialists inside
the turn.) Each entry carries the specialist's description, its `model:`
override if it declared one, its declared `capability` (`read_only` or
`change_executor`), its `agent_mode` (`Task` or `SingleTurn`), and an
`invocation` — how the composed root actually reaches it:

| `invocation` | means |
|---|---|
| `parent_tool` | the planner calls it via `invoke_specialist` |
| `transfer` | coordinator dispatch hands the turn to it |
| `graph_node` | a node in a graph, or fan-out's synthesis merger |
| `fanout_branch` | one of fan-out's concurrent analysts |
| *(empty)* | **nothing in this shape reaches it** |

That last row is the useful one. A roster can carry a member the composed
shape never routes to — a `_fallback` under fan-out dispatch, a second
`SingleTurn` spec under graph dispatch — and an empty `invocation` is how
you see it without reading the composition code.

`GET /sessions/{id}/guardrails` answers the question an operator actually
has when a session stops responding: *what stopped it, and what do I do?*
It reports the budget ceilings in force and the usage against them across
all three dimensions plus each specialist's own, and the watchdog's posture
(`advisory: true` under the default `--watchdog=warn` and under
`--watchdog=feedback`, which corrects but never stops; `false` under
`--watchdog=enforce`, whether or not it has fired yet).

The watchdog raises three signals, and each names a different way an
unattended run goes wrong:

| signal | severity | fires when |
|---|---|---|
| `repeated-tool-call` | Critical | the same call, five times running — path spellings (`./main.go`, `/workspace/main.go`) count as the same file |
| `alternating-tool-cycle` | Critical | a short loop repeating, e.g. `list_agents → check_agent` three times over — the shape a consecutive-repeat check is structurally blind to |
| `tool-failure-streak` | Warn | three calls in a row all returned errors, so nothing the agent ran has verified the state of anything |

The last one is the one to read carefully in a report. It does not mean the
agent was stuck; it means the agent's conclusions rest on tools that all
failed. It stays Warn deliberately: three denials into a legitimate RBAC
probe is not a runaway, and halting there would make the backstop the
outage.

Severity is a property of the pattern, not of the posture. What changes
between postures is the reaction, and the three of them are a ladder —
each rung includes the one below it:

- **`--watchdog=warn`** (default) logs every alert and lets the turn run.
- **`--watchdog=feedback`** also tells the model. Each alert carries a
  model-facing sentence alongside the operator-facing one, and on the
  session's next turn those are prepended to the prompt as a `[watchdog]`
  block: *an automated observation about your own previous turn — this is
  not a message from the user*. Every posture below this one routes the
  observation to a reader who may not be there; the model about to repeat
  the call is the party that can decide not to. It is a correction, not a
  backstop — nothing stops a model that reads the block and loops anyway,
  which is why unattended runs still want `enforce`.
- **`--watchdog=enforce`** also cancels the turn in flight on a
  **Critical** alert and refuses the session's every subsequent turn —
  auto-resume, a scheduled fire, and an attach inject all included — until
  an operator resets. The cancel happens *during* the turn, not at its
  boundary: the loop the watchdog catches usually lives inside a single
  turn, and a reaction that waits for the turn to end waits for the thing
  it is supposed to stop.

`enforce` including `feedback` is deliberate. An enforce halt is cleared
by an operator reset, and a reset resumes a model whose context still ends
in the loop it was halted for; without the injected observation the very
next turn re-issues the same call, and the reset is a treadmill. For the
same reason, a reset clears the halt but **keeps** the queued observation.

The `[watchdog]` block is steering, not a trust boundary — nothing
downstream grants authority based on it, and a user prompt may contain the
literal string.

`POST /sessions/{id}/guardrails/reset` is the way out: a budget trip is
otherwise permanent, since enforcement is re-derived from usage against the
ceiling on every priced event. See
[getting unstuck after a trip](/concepts/budgets/#getting-unstuck-after-a-trip)
for what a reset does and the three things it deliberately refuses to do.

Attach can read transcripts and drive turns, so it gets its own token
(`MAST_ATTACH_TOKEN`) and a hard rule: a non-loopback bind without auth is
**refused**, not warned about. It also stays up through a shutdown drain,
so an operator watching a finishing turn sees its last events rather than a
dropped connection.

## A2A — other agents

`--a2a-listen` publishes an [A2A](https://a2a-protocol.org) agent card and
a JSON-RPC endpoint (`message/send`, `tasks/get`, `tasks/cancel`,
`message/stream`) for workloads that opt in with `a2a.expose: true`.

The opt-in is per workload and never automatic, because publishing an agent
card is an *external contract*: you are telling other systems this skill
exists and is callable. `MAST_A2A_TOKEN` enables per-skill scope
enforcement; a non-loopback bind without a token is refused, since
`tasks/cancel` is destructive.

Use it when another team's agent — or another mast deployment — should be
able to hand you work.

## AG-UI — user-facing clients

`--agui-listen` serves an [AG-UI](https://docs.ag-ui.com/introduction)
run endpoint per opted-in workload, plus a discovery descriptor, for
CopilotKit apps and chat-platform bots. Also opt-in
(`agui.expose: true`), also its own token (`MAST_AGUI_TOKEN`), with rate
limiting, since a run drives a budgeted turn.

The one concept worth deciding up front is `agui.session_model`:
`per_thread` (default) maps one continuing mast session to each AG-UI
thread, which is what chat UX expects; `per_run` gives every run a fresh
session, which is right for stateless one-shots. Either way the daemon
derives and namespaces the session id — a client never supplies a raw one.

## Federation — calling out

The surfaces above are inbound. `invoke_remote_agent` is the outbound
counterpart: a specialist calling another agent over A2A.

It classifies as **mutating**, unconditionally. Effects on the far side of
a federation call are invisible from here, and a call whose consequences
you cannot see is not one to fire unattended — so it goes through [the
write gate](/concepts/approvals/) like any other change.

## No caller at all — the workload's own clock

Every surface above answers "what reached the daemon". A workload that
declares
[`edge_trigger.scheduled`](/reference/workload-bundle/#scheduled--a-workload-that-wakes-itself)
answers nothing: it wakes on its own interval, and the run has no caller to
take an identity from. That is why it runs as `mast:scheduler` — a
namespaced identity no human login can produce — and why a mutating call
inside a scheduled run still parks for a real approver. The trigger is a
bundle field rather than a surface: nothing is listening, so there is no
token and no trust boundary to configure.

Which to use is a question about where your schedules should live, not
about capability. An external scheduler posting to `/inject` gives you one
place to see every job your org runs and one place to change them; the
bundle's own cadence gives you a workload that is still periodic after it
is copied to a cluster where that scheduler does not exist.

## Picking one

| The caller is… | Surface |
|---|---|
| Alertmanager, CI, a runbook `curl` | **inject** |
| A scheduler you already run — Cloud Scheduler, a CronJob | **inject** (it is just another caller) |
| Nothing — the workload should run periodically on its own | `edge_trigger.scheduled`, no surface |
| A human operator watching or steering an incident | **attach** (mast-web) |
| Another agent, in or out of your org | **A2A** |
| A chat UI or CopilotKit app with end users in it | **AG-UI** |
| mast calling *out* to another agent | `invoke_remote_agent` |

Wire shapes, flags, and auth details for all of them: [CLI
reference](/reference/cli/). Opt-in fields and per-skill policy:
[workload bundle](/reference/workload-bundle/).
