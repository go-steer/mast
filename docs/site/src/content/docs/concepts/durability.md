---
title: Durability
description: What survives process death, why resume re-executes rather than replays, and how the effect log turns the duplicate-mutation risk into a visible refusal instead of a silent retry.
sidebar:
  order: 2
---

An unattended agent runs where processes end without warning: a pod gets
rescheduled, a node drains, a rolling upgrade lands mid-incident, someone
sends `kill -9`. Durability is the pillar that makes the other three
survivable — **unattended without durable is unwatched but fragile.**

## Everything is an event, and the event is on disk

A session is an append-only event log in SQLite (default) or Postgres,
selected with `--session-db` and `--session-db-driver`. Every model turn,
tool call, tool result, and approval pause is appended and fsynced as it
happens. Nothing important lives only in process memory, so a process that
dies loses at most the turn in flight — never the session.

That is also why the operator surface is honest about state: `mast sessions
list` derives `paused` / `interrupted` / `aborted` / `idle` from what the
log *proves*, not from a status field someone remembered to update.

SQLite is the single-instance and library-embedded default. Multi-instance
deployments — anything on Cloud Run, anything with more than one replica —
go to Postgres. Shared-filesystem SQLite is deliberately unsupported:
SQLite's own documentation warns that most network-filesystem lock
implementations can corrupt the database, and "documented caveat" is not
acceptable cover in a product whose pillar is durability.

## A pause outlives the process that asked

When a specialist hits an approval gate, the pause is a durable event, not
a blocked goroutine. The daemon can exit — crash, redeploy, `kill -9` —
and a fresh process reading the same database finds the same paused session
with the same pending interrupt id and the same question. The operator
answers it later, possibly to a different pod.

The [offline quickstart](/quickstart/unattended-triage/) walks exactly this:
inject, park, kill the daemon, restart it, resume, done — no credentials
required.

## Resume re-executes; it does not replay

This is the semantic worth internalizing before you point mast at anything
that matters.

Resume works by **reconstructing state from the log and re-executing** —
the model is re-invoked over history rather than fast-forwarded through a
recording. The consequence is direct: **mutating tool calls are
at-least-once.** A `scale_deployment` whose completion event was not
durably written before the crash may run again on recovery.

mast does not build an exactly-once illusion over that, because a truthful
at-least-once contract is safer than an exactly-once one that is subtly
false. What it does instead is narrow the window and make what is left
*visible*.

## The effect log makes the residual risk blocking

The session event log **is** the effect outbox — no second table, no second
retention policy, no chance of the record drifting from the transcript. A
durable `FunctionCall` event is the intent record; its paired
`FunctionResponse` is the completion record. Both are keyed by
`(session, invocation, function-call id)`.

The check is installed once, at the single seam every tool call converges
on inside the runner, so no future construction path can quietly miss it.
Read-only tools never pay for it — scope is exactly the [mutation
predicate](/concepts/tools-and-mcp/).

Two behaviours fall out:

- **Turn-level, fail-closed.** If a turn starts on a session whose log
  holds a *dangling mutating intent* — a call recorded with no completion —
  the whole turn runs in ambiguous-effect mode: every mutating call is
  refused with a structured `ambiguous_prior_effect` error the model sees
  and surfaces. Read-only work continues, so the agent can still
  investigate. The dangling call may or may not have executed; the window
  between an effect committing externally and its completion fsyncing
  cannot be closed, only detected — and mast refuses to guess.
- **Call-level replay.** A call whose exact key already has a durable
  completion returns the recorded result rather than re-executing.

Clearing ambiguous-effect mode is an operator action, never an inference:

```bash
mast sessions ack-effects <session-id>          # via the running daemon
mast sessions resume <id> --interrupt=... --ack-effects
```

The acknowledgement is a watermark: it covers intents persisted at or
before it, and nothing after. If the acked turn then fails, the watermark
stays — it acknowledged those prior intents, not the new turn.

**Two limits stated plainly.** A re-invoked model that emits a *fresh*
call id for a semantically identical mutation is a new call by
construction, and the exact-key check cannot dedupe it — that class is
covered by the turn-level mode and by the write gate re-asking. Content-hash
dedup was considered and rejected: scaling up twice is legitimately two
scale-ups, and a silent false-positive dedup in an SRE tool is worse than
the duplicate it prevents.

## Shutdown is accounted for, not just survivable

Persisted-as-you-go makes a stop survivable; the shutdown contract makes it
*accounted for*.

On SIGTERM the daemon stops accepting work immediately — the inject
listener closes, and inject, resume, and attach-injected turns are refused
with a clear error — then **marks every in-flight session as interrupted
before it starts waiting**, so a `SIGKILL` landing mid-drain still finds
the markers on disk. The drain bound is the turn's own
`budget.max_wallclock_seconds`, so a finishing turn is never cut shorter
than its budget allows; with no budget set it is a hard 30-second cut.
Worst-case drain is therefore known statically — size
`terminationGracePeriodSeconds` above it, as the shipped Kubernetes base
does.

Turns that finish inside the window clear their own marker. Turns that
outrun it keep it, and `mast sessions list --state=interrupted` finds them.
The attach surface stays up through the drain so an operator tailing a
finishing turn sees its last events.

Exit codes carry the outcome: `0` for a clean signal-initiated stop, `3`
when the drain expired with interrupted survivors, `4` when the *teardown*
after the drain overran its watchdog — which also dumps every goroutine's
stack, because a silently wedged `Close` is a latent bug someone has to
chase.

## Boot-time auto-resume

Interruption markers make stalled work findable; auto-resume acts on it. On
boot the daemon scans for interrupted sessions and drives a continuation
turn for each eligible one, through the same chokepoint every other turn
uses — same turn lock, same budget meter, same effect checks. On by
default (`--auto-resume=false` to disable), daemon-only, one session at a
time.

**The guarantee: auto-resume never double-fires a mutation.** It gets there
by declining to be clever. A session carrying *any* dangling mutating
intent is skipped and left for an operator's acknowledgement. Work older
than `--auto-resume-window` (default 1h) is skipped as stale — a long
outage should not replay an incident nobody cares about anymore. A
per-session restart-loop breaker (incremented durably *before* the turn, so
a turn that kills the process still counts) and a per-boot cap bound the
blast radius of a poison session. A transcript that already ends on a clean
model turn had actually completed, so its marker is cleared with no turn at
all. Every outcome is a labelled metric:
`mast_autoresume_total{workload,outcome}`.

This slice covers `coordinator` dispatch; graph and fan-out auto-resume
need their own verification and are on the [roadmap](/roadmap/).

## Where to look next

- [`mast sessions`](/reference/cli/) — list, show, resume, abort,
  ack-effects.
- [Approvals](/concepts/approvals/) — the pause that most often outlives a
  process.
- [Metrics](/reference/metrics/) — what to alert on.
