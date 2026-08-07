---
title: CLI
description: The mast daemon flags and the mast sessions operator surface.
sidebar:
  order: 3
---

## `mast` (the daemon)

`mast --workload=...` runs the daemon: the HTTP inject endpoint, the
runner, and the session store.

| Flag | Default | Meaning |
|---|---|---|
| `--workload` | — | Workload to run: a name resolved via [.agents/ discovery](/reference/agents-discovery/), or a path to a workload directory. Empty = trivial single-agent coordinator (inject-endpoint smoke only). |
| `--dispatch` | `coordinator` | Dispatch shape: `coordinator` (SubAgents pattern) or `graph` (workflow-graph LLM-as-router). |
| `--model` | `echo` | `echo` (offline fake, no credentials), `scripted` (JSONL recorded-turn replay; path via `MAST_SCRIPT`, strict matching via `MAST_SCRIPT_STRICT=1`), a Gemini model id like `gemini-2.5-flash`, or a Claude model id like `claude-sonnet-4-6`. |
| `--provider` | — | Provider alias: `echo`, `scripted`, `gemini`, `anthropic`, or `anthropic-vertex`. Validates `--model` when both are set; picks the provider's default model from the `--task` profile's tier when `--model` is unset. For `claude-*` models the alias also picks the backend — without it, `ANTHROPIC_API_KEY` selects the first-party API, then a Vertex project (`ANTHROPIC_VERTEX_PROJECT_ID` / `GOOGLE_CLOUD_PROJECT`) selects Vertex. |
| `--listen` | `:7777` | HTTP bind address for `/inject`, `/resume`, `/abort`, `/metrics`. |
| `--attach-listen` | (empty) | Operator attach surface (HTTP/SSE for [mast-web](https://github.com/go-steer/mast-web) and other attach clients): a TCP address like `127.0.0.1:8484`, or a Unix socket as `unix:/path/mast.sock`. Empty = disabled. Requires `--session-db` (live-tail pumps from the eventlog overlay). Non-loopback TCP binds are refused unless auth is configured — set `MAST_ATTACH_TOKEN`. Serve mode only. |
| `--a2a-listen` | (empty) | [A2A](https://a2a-protocol.org) server surface: a TCP address like `127.0.0.1:7780`. Empty = disabled. Publishes an agent card and a JSON-RPC 2.0 endpoint (`POST /a2a`) for workloads that opt in via the bundle's `a2a.expose` section. Card endpoints (`/.well-known/agent-card.json`, `/.well-known/agent-card/<name>.json`) are public; `/a2a` is authenticated when `MAST_A2A_TOKEN` is set, with per-skill scope enforcement. Non-loopback binds are refused without a token (`tasks/cancel` is destructive). Serve mode only. See [A2A server](#a2a-server). |
| `--session-db` | (empty) | SQLite file path (default driver) or Postgres DSN/URL with `--session-db-driver=postgres`. Empty = in-memory sessions, **no durability**. |
| `--session-db-driver` | `sqlite` | `sqlite` or `postgres`. `postgres` with an empty `--session-db` is a startup error, never a silent in-memory downgrade. |
| `--timeout` | `5m` | One-shot turn deadline (`2m`, `90s`, …); `0` disables. One-shot only — serve-mode wallclock ceilings come from workload budgets. An unresponsive backend (or a provider SDK silently retrying on quota errors) fails loudly instead of hanging a script. |
| `--auto-resume` | `true` | On boot, scan for sessions a prior shutdown interrupted and drive a continuation turn for each eligible one (see [boot-time auto-resume](#boot-time-auto-resume)). `--auto-resume=false` disables. Serve mode only; needs `--session-db` (in-memory sessions never survive a restart). |
| `--auto-resume-window` | `1h` | Only auto-resume sessions interrupted within this window; older interruptions are left for an operator. `0` disables the freshness gate. Serve mode only. |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` (JSON logs on stderr). |
| `--version` | — | Print version and exit. |

Set `MAST_INJECT_TOKEN` to require bearer auth on the HTTP endpoints;
unset means unauthenticated (dev only, warned at startup). The attach
surface has its own token, `MAST_ATTACH_TOKEN` — attach can read
transcripts and inject operator messages, so treat it as a separate
trust boundary from the inject webhook. The A2A surface has its own
token too, `MAST_A2A_TOKEN` — a different auth model (per-skill scopes
for external agent callers), so treat it as a third trust boundary.

## `mast sessions` (operator surface)

```
mast sessions list   --session-db=... [--user=...] [--state=paused|aborted|interrupted|idle]
mast sessions show   <session-id> --session-db=...
mast sessions resume <session-id> --interrupt=<iid> --response='{"approved":true}' [--ack-effects] [--addr=...]
mast sessions resume --token=<mrt_...> [--response='<json>'] [--ack-effects] [--addr=... | --session-db=...]
mast sessions pause  <session-id> --reason=<enum> [--message=...] [--resume-at=RFC3339 | --resume-after=15m]
                     [--interrupt] [--ttl=48h] [--addr=...]
mast sessions extend-token <mrt_...> --ttl=<duration> [--addr=...]
mast sessions abort  <session-id> [--reason=...] [--addr=...]
mast sessions ack-effects <session-id> [--reason=...] [--addr=... | --session-db=...]
```

The split is deliberate:

- **`list` / `show` read the session DB directly** (SQLite path) — they
  work with or without a running daemon. `list` prints one row per session
  with state (`paused` / `aborted` / `interrupted` / `idle`) and pending
  interrupt IDs;
  `show` prints session detail including each pending interrupt's message,
  response schema, and a copy-pasteable resume command.
- **`resume` / `abort` go through a running daemon** (`--addr`, default
  `http://127.0.0.1:7777`) — resume must be executed by the runner that
  owns the workflow, and routing abort through the daemon keeps a single
  SQLite writer. Both send `MAST_INJECT_TOKEN` as a bearer when set.

### Pause and resume tokens (v0.2)

`pause` **gate-pauses** a session: the daemon refuses every subsequent
turn on it — inject, attach, resume, timer — until the pause is
resumed, and prints a **resume token** (`mrt_...`). Token possession is
the resume capability: `resume --token=...` needs nothing else (the
daemon resolves the session), works for gate pauses and for the
planner's own `pause_session` parks alike, and consumes the token —
a replay is a no-op. Add `--interrupt` to a pause to also cancel the
session's in-flight turn (hard pause).

`--resume-at` / `--resume-after` arm the daemon's timed-pause
scheduler: the session auto-resumes at that time through the same
budget-metered paths an operator resume takes. Tokens expire after 7
days by default (`--ttl` can only shorten that); an expired token
refuses to resume but **the pause stays** — `extend-token` is the
audited recovery, then resume again.

`resume --token --session-db=...` (no daemon) clears **gate pauses
only**, and only on a DB no daemon is serving; an interrupt-pause
resume always needs the daemon that owns the runner.

### Abort (terminal since v0.2)

`abort` writes a durable abort marker with a reason, **cancels the
session's in-flight turn**, and the daemon refuses every further turn
kind on the session — inject and attach included, not just resume
(that narrower contract was v0.1's). Aborting also voids the session's
pause tokens and timers. Markers
live in a companion row beside the session, not in its transcript — an
in-flight turn is never disturbed by an abort (or by a shutdown
marker), and the model never sees marker events.

Resume takes either keying: **interrupt ID** (`--interrupt` +
`--response`, printed by `show`) or a **resume token** (`--token`, the
v0.2 programmatic-pause surface above).

`interrupted` marks a session whose turn was cut short by a daemon
shutdown: on SIGTERM the daemon stops accepting new work (`/inject`
and `/resume` answer **503 + Retry-After**; new attach turns are
refused; requests naming a reserved `…:mast-ops` ID answer **400** on
all three write endpoints), drains in-flight turns for up to the workload's
`budget.max_wallclock_seconds` (30s without a budget), durably
marking their sessions *before* waiting, and clearing the marker for
turns that finish inside the window. The marker survives even a
SIGKILL mid-drain. It is state, not preemption — the next turn on the
session proceeds normally, and a session that reached a HITL pause
reports `paused`, not `interrupted`.

### Boot-time auto-resume

On boot (and on by default — `--auto-resume`), the daemon scans for
`interrupted` sessions and drives a continuation turn for each eligible
one through the same chokepoint every other turn kind uses, so
unattended work cut short by a restart finishes on its own. It needs a
durable `--session-db`; in-memory sessions never survive a restart, so
there is nothing to resume.

**The guarantee is the operational form of exactly-once: auto-resume
never double-fires a mutation.** A session carrying **any** dangling
mutating tool call (an ambiguous prior effect — see below) is skipped
and left for an operator `ack-effects`, not resumed. A dangling
read-only call is repaired with a synthetic
`interrupted before completion` response and the turn re-runs; a
transcript that already ended on a completed model turn just has its
stale marker cleared (no spurious turn). Rails: `--auto-resume-window`
(default `1h`) skips work already stale at crash; a per-session
restart-loop breaker plus a per-boot cap bound a poison session; and a
session advanced by a concurrent inject between the scan and the turn is
skipped. Slice-1 drives `coordinator` dispatch only. Every decision is
counted in `mast_autoresume_total{workload,outcome}`. `--pause-sessions`
on `mast stop` (below) opts a session out — a gate pause outranks
`interrupted`, so those sessions are handed back to the operator instead
of continued.

## `mast stop` (planned stop)

```
mast stop [--addr=...] [--reason=...] [--pause-sessions]
```

Asks a running daemon to drain and exit — exactly the SIGTERM path,
with the interruption markers classified **`operator stop`** (plus
`--reason`) instead of `daemon shutdown`, so transcripts distinguish a
planned stop from a crash forever. `--pause-sessions` additionally
gate-pauses every session the drain marks, so boot-time auto-resume
hands them back to the operator instead of continuing them.

**Daemon exit codes** encode whether work was cut short, not who
initiated: `0` = clean drain (all in-flight turns finished), `3` =
drain window expired with interrupted survivors, `1` = error, `2` =
usage, `4` = teardown watchdog fired (the post-drain unwind — an OTel
flush or a `Close` — deadlocked past its 15s bound; the daemon dumps
goroutine stacks to stderr and force-exits so a wedged shutdown
surfaces a diagnostic instead of hanging until SIGKILL). `Restart=always`
remains the default systemd guidance; `Restart=on-failure` now composes
too — exit 3 revives the daemon exactly when boot-time repair has work
to do, and a cleanly-drained stop stays down.

An interrupted turn can leave a **dangling mutating tool call** — a
call whose outcome the log cannot prove. The recorded-effect outbox
then runs the session's next turn in *ambiguous-effect mode*:
mutating and sub-run-spawning tool calls are refused with a structured
`ambiguous_prior_effect` error (read-only work proceeds) until an
operator acknowledges — asserting they checked whether the dangling
calls took effect externally — or aborts the session. The
acknowledgement surface is **`mast sessions ack-effects <id>`**
(through the daemon by default, which serializes it against in-flight
turns; `--session-db` writes directly when no daemon serves that DB,
e.g. a one-shot task session), or **`resume --ack-effects`** when the
session is also paused on an interrupt. Unknown tools — MCP tools
included — count as mutating unless the workload's
`tool_catalog.tools` overrides them (see the
[workload bundle reference](/mast/reference/workload-bundle/)); the
acknowledgement is recorded durably beside the session and covers
only calls persisted up to that moment. Task delegations, HITL
interrupts, and long-running calls never count as dangling effects.

Two related contracts: the daemon runs **one turn per session at a
time** — a second inject or resume for the same session queues behind
the in-flight turn (bounded by the wallclock budget) rather than
corrupting it; and every SQLite session store the tooling opens
(serve, one-shot, the sessions CLI) carries the same write hardening,
so concurrent access waits instead of failing.

## A2A server

With `--a2a-listen`, the daemon exposes its workloads to the
[A2A](https://a2a-protocol.org) ecosystem — other agents can discover
mast's skills from an agent card and drive them over a standard
JSON-RPC endpoint. It runs on its own listener, separate from the
inject webhook and the operator attach surface.

A workload opts in per bundle (exposure has real ops implications —
auth setup, an external contract, cross-org discoverability, so it is
never automatic):

```yaml
# .agents/workloads/incident-triage.yaml
a2a:
  expose: true
  skill_name: incident-triage
  skill_description: Investigate GKE pod-failure incidents.
  auth:
    scopes: [incident-triage.invoke]
```

**Discovery** is public. An **aggregated agent card** at
`/.well-known/agent-card.json` lists every exposed workload as a skill;
**per-workload cards** at `/.well-known/agent-card/<name>.json` serve
registries that require a distinct endpoint per agent. Cards advertise
JSON-RPC as the preferred transport and the tested-against A2A spec
line (`A2A-Version` header).

**Invocation** is the single `POST /a2a` JSON-RPC 2.0 endpoint. It serves:

- `message/send` — run a turn (a task id **is** a mast session id).
  Execution is synchronous: the call blocks and the reply is a terminal
  task. The agent's answer surfaces as a `result` text artifact; the
  in-process task registry then reports `completed` (which a transcript
  read cannot prove). A message with a `taskId` continues that task;
  without one it routes to the single exposed skill and mints a fresh
  task (an endpoint exposing more than one skill refuses a selector-less
  fresh send with `-32004`). The server assigns a `contextId` when the
  caller omits one, returned on the task so follow-ups can be grouped.
  Text-only for now — a message with no text parts is rejected
  (`-32602`). If the turn pauses for a HITL interrupt the task returns
  `input-required`; if the session is aborted or gate-paused the call is
  refused at the chokepoint and the task reports its durable state; while
  draining for shutdown, new sends are refused with the retryable
  `-32000`.
- `tasks/get` — snapshot a task's state. The registry is consulted first
  (it alone can prove `completed`/`failed`), falling back to the
  session's log-proven state projected onto the A2A lifecycle (`working`,
  `input-required` when a HITL interrupt is pending, `canceled` when
  aborted). A transcript-only read never reports `completed` — the event
  log cannot prove a turn finished versus is still in flight.
- `tasks/cancel` — cancel a task idempotently, routing to the same
  terminal-abort path the operator `abort` uses. Cancel is authoritative:
  a task canceled as its turn finishes still reports `canceled`, never a
  leaked answer.

All three verbs address **only tasks this server minted** (ids carrying
the `a2a-` prefix); any other session id — an operator incident, an
attach or autoresume session — is reported as not found (`-32001`), so a
caller cannot reach another surface's session through the A2A endpoint.
- `message/stream` — recognized but not yet served; it answers the A2A
  "unsupported operation" error (`-32004`) until SSE streaming lands.

**Distributed tracing.** A2A calls propagate W3C trace context: an
inbound `traceparent`/`baggage` header is adopted so the turn's spans
parent under the caller's span, and mast's own outbound A2A client calls
inject the current trace context. This is a no-op when tracing is
disabled.

**Auth.** Card endpoints are always public (a card is a descriptor, not
a capability). The `/a2a` endpoint is authenticated when `MAST_A2A_TOKEN`
is set — a request without a valid bearer is refused `401`, and a call
whose token lacks a skill's declared `auth.scopes` is refused `403`.
Unset means unauthenticated (dev only, warned at startup) — and because
`tasks/cancel` is destructive, a **non-loopback** `--a2a-listen` bind
(anything but `127.0.0.1`/`localhost`/`::1`) is *refused at startup*
without a token; bind loopback or set `MAST_A2A_TOKEN`. The token
model is deliberately separate from the inject and attach tokens: A2A
callers are external agents scoped per skill, not operators.
