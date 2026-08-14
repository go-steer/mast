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

## Attach — the operator's live view

`--attach-listen` serves the mast-native attach protocol (HTTP + SSE) that
[mast-web](https://github.com/go-steer/mast-web) speaks: live-tail a
running turn, read a session's transcript, inject an operator message,
answer a parked approval. It needs `--session-db`, since the live tail
pumps from the event log.

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

## Picking one

| The caller is… | Surface |
|---|---|
| Alertmanager, a cron job, CI, a runbook `curl` | **inject** |
| A human operator watching or steering an incident | **attach** (mast-web) |
| Another agent, in or out of your org | **A2A** |
| A chat UI or CopilotKit app with end users in it | **AG-UI** |
| mast calling *out* to another agent | `invoke_remote_agent` |

Wire shapes, flags, and auth details for all of them: [CLI
reference](/reference/cli/). Opt-in fields and per-skill policy:
[workload bundle](/reference/workload-bundle/).
