---
title: CLI
description: The mast daemon flags and the mast sessions operator surface.
sidebar:
  order: 3
---

## `mast` (the daemon)

`mast --workload=...` runs the daemon: the HTTP inject endpoint, the
runner, and the session store.

There is **no `serve` subcommand** — serve mode *is* the flag-only
invocation, and adding a positional prompt switches the same binary
into one-shot mode. `mast serve --attach-listen=…` therefore does not
start a daemon; it exits `2` with the misplaced-flag error, because
`serve` was read as the prompt. The only subcommands are
[`sessions`](#mast-sessions-operator-surface) and
[`stop`](#mast-stop-planned-stop).

| Flag | Default | Meaning |
|---|---|---|
| `--workload` | — | Workload to run: a name resolved via [.agents/ discovery](/reference/agents-discovery/), or a path to a workload directory. Empty = trivial single-agent coordinator (inject-endpoint smoke only). |
| `--dispatch` | *(unset)* | Dispatch shape: `coordinator` (SubAgents pattern), `graph` (workflow-graph LLM-as-router), `fanout` (whole roster investigates concurrently, one `_synthesis` specialist merges), `bounded` (one `SingleTurn` specialist, one model call, a report forced to a schema), or `auto` (read the shape off the roster — it never picks `bounded`, because a cost ceiling is declared, never inferred). **Unset takes the bundle's own `dispatch:`, then `coordinator`**; the flag wins only when the operator actually typed it. `fanout` requires a read-only roster and `bounded` a single schema-declaring `SingleTurn` specialist — see [the bundle reference](/reference/workload-bundle/). |
| `--model` | `echo` | `echo` (offline fake, no credentials), `scripted` (JSONL recorded-turn replay; path via `MAST_SCRIPT`, strict matching via `MAST_SCRIPT_STRICT=1`), a Gemini model id like `gemini-2.5-flash`, or a Claude model id like `claude-sonnet-4-6`. |
| `--provider` | — | Provider alias: `echo`, `scripted`, `gemini`, `vertex`, `anthropic`, or `anthropic-vertex`. Validates `--model` when both are set; picks the provider's default model from the `--task` profile's tier when `--model` is unset. The alias also picks the backend within a family, and the two families differ in what happens without one. `vertex` serves `gemini-*` against Vertex AI (`GOOGLE_CLOUD_PROJECT` + ADC; location from `GOOGLE_CLOUD_LOCATION` / `GOOGLE_CLOUD_REGION`, default `global`) — with no alias, only `GOOGLE_GENAI_USE_VERTEXAI=true` gets you there, and a project alone does not. For `claude-*`, no alias means `ANTHROPIC_API_KEY` selects the first-party API, then a Vertex project (`ANTHROPIC_VERTEX_PROJECT_ID` / `GOOGLE_CLOUD_PROJECT`) selects Vertex. See [credentials](/concepts/providers/#credentials). |
| `--listen` | `:7777` | HTTP bind address for `/inject`, `/resume`, `/abort`, `/monitor-ack`, `/metrics`. |
| `--attach-listen` | (empty) | Operator attach surface (HTTP/SSE for [mast-web](https://github.com/go-steer/mast-web) and other attach clients): a TCP address like `127.0.0.1:8484`, or a Unix socket as `unix:/path/mast.sock`. Empty = disabled. Requires `--session-db` (live-tail pumps from the eventlog overlay). Non-loopback TCP binds are refused unless auth is configured — set `MAST_ATTACH_TOKEN`. Serve mode only. |
| `--a2a-listen` | (empty) | [A2A](https://a2a-protocol.org) server surface: a TCP address like `127.0.0.1:7780`. Empty = disabled. Publishes an agent card and a JSON-RPC 2.0 endpoint (`POST /a2a`) for workloads that opt in via the bundle's `a2a.expose` section. Card endpoints (`/.well-known/agent-card.json`, `/.well-known/agent-card/<name>.json`) are public; `/a2a` is authenticated when `MAST_A2A_TOKEN` is set, with per-skill scope enforcement. Non-loopback binds are refused without a token (`tasks/cancel` is destructive). Serve mode only. See [A2A server](#a2a-server). |
| `--agui-listen` | (empty) | [AG-UI](https://docs.ag-ui.com/introduction) server surface: a TCP address like `127.0.0.1:7781`. Empty = disabled. Serves a per-workload HTTP+SSE run endpoint and a `/agui/agents.json` discovery descriptor for workloads that opt in via the bundle's `agui.expose` section. Authenticated when `MAST_AGUI_TOKEN` is set (per-workload scope enforcement); rate limits via `MAST_AGUI_RATE`/`MAST_AGUI_BURST`. Non-loopback binds are refused without a token (a run drives a budgeted turn). Serve mode only. See [AG-UI server](#ag-ui-server). |
| `--notify-url` | (empty) | Chat egress for monitoring cycles: [switchboard](https://github.com/go-steer/switchboard)'s message ingress, as an origin (`http://switchboard:8080`) or the full `/v1/messages` endpoint. Empty = disabled, and a workload whose bundle declares a `monitor.notify` block then **refuses to start**. Requires `MAST_NOTIFY_TOKEN`. Serve mode only — one-shot runs no monitoring cycle. See [chat egress](#chat-egress-for-monitoring-cycles). |
| `--session-db` | (empty) | SQLite file path (default driver) or Postgres DSN/URL with `--session-db-driver=postgres`. Empty = in-memory sessions, **no durability**. |
| `--session-db-driver` | `sqlite` | `sqlite` or `postgres`. `postgres` with an empty `--session-db` is a startup error, never a silent in-memory downgrade. |
| `--timeout` | `5m` | One-shot turn deadline (`2m`, `90s`, …); `0` disables. One-shot only — serve-mode wallclock ceilings come from workload budgets. An unresponsive backend (or a provider SDK silently retrying on quota errors) fails loudly instead of hanging a script. |
| `--auto-resume` | `true` | On boot, scan for sessions a prior shutdown interrupted and drive a continuation turn for each eligible one (see [boot-time auto-resume](#boot-time-auto-resume)). `--auto-resume=false` disables. Serve mode only; needs `--session-db` (in-memory sessions never survive a restart). |
| `--auto-resume-window` | `1h` | Only auto-resume sessions interrupted within this window; older interruptions are left for an operator. `0` disables the freshness gate. Serve mode only. |
| `--watchdog` | *(unset)* | Behavioral-watchdog posture, a ladder where each rung includes the one before it. `warn` logs a detected tool loop and lets the turn run. `feedback` also prepends a `[watchdog]` block to the session's next prompt, telling the model what it is doing. `enforce` also cancels the turn in flight on a **Critical** alert (`repeated-tool-call`, `alternating-tool-cycle`) and refuses the session's every subsequent turn until `POST /sessions/{id}/guardrails/reset`. Detection is identical in all three — only the reaction changes. **Unset takes the workload's `safety.watchdog`, then mast's default (`feedback`)**; the startup line says which won. Applies to serve *and* one-shot mode, except `feedback`, which needs a next turn a one-shot does not have (it behaves as `warn` there, and says so when you asked for it explicitly); a one-shot has no session to refuse either, so an `enforce` halt just ends the turn. With `--attach-listen`, an `enforce` halt is written to the session database and survives a daemon restart (and so does the reset that clears it); without one it lives only in the process, and the daemon warns at startup that it does. See [where the posture comes from](/concepts/interop/#where-the-posture-comes-from) and [a halt outlives the process](/concepts/interop/#a-halt-outlives-the-process-that-observed-it). |
| `--mcp-digest` | `true` | Route MCP tool responses through the structural digest before the model reads them. Responses under 8000 bytes are handed back untouched; larger ones are pruned, and the model gets a `retrieve_raw` tool to pull the original back when the digest dropped something it needs. `--mcp-digest=false` is the kill switch for the whole daemon; `no_digest: true` on a server in [`mcp.json`](/reference/mcp-servers/#digesting-large-tool-responses) opts out one server. See [digesting large tool responses](/reference/mcp-servers/#digesting-large-tool-responses). |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` (JSON logs on stderr). |
| `--version` | — | Print version and exit. |

Set `MAST_INJECT_TOKEN` to require bearer auth on the HTTP endpoints;
unset means unauthenticated (dev only, warned at startup). The attach
surface has its own token, `MAST_ATTACH_TOKEN` — attach can read
transcripts and inject operator messages, so treat it as a separate
trust boundary from the inject webhook. The A2A surface has its own
token too, `MAST_A2A_TOKEN` — a different auth model (per-skill scopes
for external agent callers), so treat it as a third trust boundary. The
AG-UI surface has its own token as well, `MAST_AGUI_TOKEN` — per-workload
scopes for user-facing clients (CopilotKit apps, chat-platform bots), a
fourth trust boundary.

### Chat egress for monitoring cycles

Those four are all **inbound**: they let a caller drive this daemon.
`--notify-url` is the one that points the other way — the daemon posting
into a chat when a [monitoring
cycle](/reference/workload-bundle/#notify--speaking-only-when-something-changed)
has something to report:

```bash
MAST_NOTIFY_TOKEN=… mast --workload=cluster-watch \
  --notify-url=http://switchboard:8080 --listen=:7777 --session-db=/var/lib/mast/sessions.db
```

The bearer comes from `MAST_NOTIFY_TOKEN` and there is deliberately no flag
for it: a flag puts a credential in every `ps` on the node and in the
container spec's args. The daemon **refuses to start** if that token equals
any of the four inbound ones — they authorize opposite directions, and
sharing one means anything that can read the chat bridge's configuration can
inject turns here. Setting the token with no `--notify-url` is a startup
warning, because it usually means half a rollout.

Which conversation to post into, and how long the workload may stay silent,
are the bundle's (`monitor.notify.conversation`, `monitor.notify.digest_after`)
— they are properties of the workload, while the ingress and the credential
are properties of the deployment.

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
mast sessions export-decisions [<session-id>] --session-db=... [--workload=...]
                     [--since=RFC3339] [--until=RFC3339] [--include-approver] [--out=file.jsonl]
```

The split is deliberate:

- **`list` / `show` read the session DB directly** (SQLite path) — they
  work with or without a running daemon. `list` prints one row per session
  with state (`paused` / `aborted` / `interrupted` / `idle`) and pending
  interrupt IDs;
  `show` prints session detail including each pending interrupt's message,
  response schema, and a copy-pasteable resume command — plus, for a
  mutating call an operator edited, the arguments that actually ran and
  who authorized them (see [the write gate](/reference/write-gate/)). When
  the parked call belongs to a [change
  set](/reference/write-gate/#approving-a-whole-change-set), `show` also
  lists the set's other calls, the freshness re-read each one is subject to,
  and the `--response` that approves them all at once; if the call is being
  asked about a second time because an earlier approval stopped covering it,
  a `Stale:` line says why.
- **`export-decisions` reads the session DB directly too**, and writes one
  JSON Lines record per operator adjudication — approve, reject, edit — with
  a `_meta` provenance header. Naming a session exports that session;
  omitting it exports every session in the store. Approver identities are
  digested unless you pass `--include-approver`, and the header records
  which mode produced the file. **Tool arguments are exported verbatim**, so
  an export is as sensitive as the arguments your tools take; `--out` writes
  `0600`, and without it the rows go to stdout for piping. See [exporting
  decisions](/reference/write-gate/#exporting-what-was-decided).
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

### Who a resume names

Every consumed token records **who spent it** — the `consumed_by` on the
pause record, which is what a replay of the same token reports back
(`already consumed at 2026-08-19T14:02:11Z by shared-bearer-token`) and
what the daemon logs. It comes from the **credential the resume presented**,
never from a name in the request body:

| How the resume arrived | `consumed_by` |
|---|---|
| Over HTTP with `MAST_INJECT_TOKEN` set | `shared-bearer-token` |
| Over HTTP with a token from `MAST_INJECT_USERS_FILE` | that row's identity, e.g. `alice@example.com` |
| A relay in `MAST_INJECT_PROXY_IDENTITIES` sending `X-Asserted-Caller` | `alice@example.com (asserted by sa:switchboard)` |
| The daemon's timed-pause scheduler | `timer` |
| `resume --token --session-db=...` (no daemon) | `operator resume --token --session-db` |

`shared-bearer-token` is what a shared credential can honestly prove: that
*someone* holding it resumed the session. Per-person attribution needs a
per-person credential.

#### Naming people on the daemon

Point `MAST_INJECT_USERS_FILE` at a users file and `/resume` starts
accepting each row's token as well as the shared one, recording the person
behind it:

```json
{
  "version": 1,
  "users": [
    {"identity": "alice@example.com", "token": "..."},
    {"identity": "sa:switchboard",    "token": "..."}
  ]
}
```

It holds bearer tokens, so it must be mode `0600` or stricter; the daemon
refuses to start otherwise. `MAST_INJECT_PROXY_IDENTITIES` is a
comma-separated list of identities in that table allowed to answer on
someone else's behalf via `X-Asserted-Caller` — a chat relay with an
approve button is the case it exists for. Both are checked at startup: a
proxy list with no table, or one naming an identity the table doesn't
have, refuses to boot rather than issuing 403s later.

The table says **who did something a name belongs on**; it is not a second
way in. Two routes read it — `/resume`, where the name is who approved, and
[`/monitor-ack`](/reference/workload-bundle/#ack--taking-an-acknowledgement-back),
where it is who silenced a finding. `/inject`, `/abort` and the rest still
take `MAST_INJECT_TOKEN` and nothing else.

`MAST_INJECT_TOKEN` keeps working alongside the table, and a resume that
presents it is still recorded as `shared-bearer-token` — configuring a
table does not retroactively attribute anything, and an emitter you
haven't migrated keeps working. To require attribution, **leave
`MAST_INJECT_TOKEN` unset**: `/resume` then admits only the table's
tokens, and there is no unattributed way to answer a gate. (A request
presenting the shared token *and* `X-Asserted-Caller` is refused outright
rather than silently recorded as the shared credential — a token that
can't name its own holder doesn't get to vouch for someone else's.)

There is deliberately **no approver field in the resume body**. An
attribution a caller writes about itself is worth nothing after an
incident, so mast takes it from the credential instead — the same rule the
[write gate](/reference/write-gate/) applies to its own approver.

`/monitor-ack` goes one step further and **refuses** a body carrying
`ack_by` rather than ignoring it: on that route mast's authentication is
the only thing in front of the producer's suppression, so a client that
believed it had named someone is told it did not.

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
e.g. a one-shot task session — this direct path prints a warning
because it cannot serialize against a running daemon, so use it only
when none serves the DB), or **`resume --ack-effects`** when the
session is also paused on an interrupt. Unknown tools — MCP tools
included — count as mutating unless the workload's
`tool_catalog.tools` overrides them (see the
[workload bundle reference](/reference/workload-bundle/)); the
acknowledgement is recorded durably beside the session and covers
only calls persisted up to that moment. Task delegations, HITL
interrupts, and long-running calls never count as dangling effects.
Task delegations are excluded **by name** (a delegation is a
FunctionCall named after the sub-agent), so a mutating tool that shared
a specialist's name would slip past that exclusion — therefore mast
**refuses to start** when a composed sub-agent name also names a
mutating tool: rename one side (a read-only tool of the same name is
harmless and allowed).

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
- `message/stream` — run a turn like `message/send`, but stream its
  progress as Server-Sent Events (`text/event-stream`), one JSON-RPC
  response per `data:` frame: an initial `Task` snapshot, then a
  `status-update` per model response carrying its text as progress, then
  the closing `artifact-update` (the agent's answer) and a final
  `status-update` (`final: true`) with the terminal state. Updates are
  message-granular (one per model response, not token deltas). Auth,
  scope, and rate-limit refusals are decided *before* the stream opens, so
  a refusal is a normal JSON-RPC error, not a truncated stream. The card
  advertises `capabilities.streaming: true`.

These verbs address **only tasks this server minted** (ids carrying
the `a2a-` prefix); any other session id — an operator incident, an
attach or autoresume session — is reported as not found (`-32001`), so a
caller cannot reach another surface's session through the A2A endpoint.

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

**Rate limiting.** External callers can consume the agent's provider
budget through `message/send` and `message/stream`, so both turn-driving
verbs are rate limited (they share one bucket per caller and workload).
Set `MAST_A2A_RATE` to the allowed requests/second **per caller and
workload**, and optionally `MAST_A2A_BURST` for the bucket depth
(defaults to `ceil(rate)`, minimum 1):

```bash
MAST_A2A_RATE=5 MAST_A2A_BURST=10 mast --a2a-listen=127.0.0.1:7780 ...
```

The caller is the token's tenant claim if present, else its subject; an
unauthenticated endpoint buckets all callers together. A refused send
returns the retryable `-32000` error with an advisory `Retry-After`
header. `MAST_A2A_RATE` unset means no rate limiting; a set-but-malformed
value fails startup. Control-plane verbs (`tasks/get`, `tasks/cancel`)
are never rate limited — an operator can always read or cancel a task.

## AG-UI server

With `--agui-listen`, the daemon exposes its workloads to the
[AG-UI](https://docs.ag-ui.com/introduction) ecosystem — the agent↔user
protocol CopilotKit React apps and chat-platform bots speak. A client
POSTs a `RunAgentInput` and receives the turn back as a Server-Sent
Events (`text/event-stream`) run stream. It runs on its own listener,
separate from the inject webhook, the attach surface, and the A2A
endpoint (mast has no single shared HTTP root — each surface owns its
listener).

A workload opts in per bundle (like A2A, exposure carries real ops
implications — auth setup, a public turn-driving endpoint, user-facing
UX — so it is never automatic):

```yaml
# .agents/workloads/incident-triage.yaml
agui:
  expose: true
  endpoint_path: /agui/incident-triage    # defaults to /agui/<name>
  description: Investigate GKE pod-failure incidents.
  session_model: per_thread               # or per_run
  auth:
    scopes: [incident-triage.run]
```

**Discovery** is public: `GET /agui/agents.json` lists every exposed
workload as `{name, endpoint, description, input_schema, auth: {scopes}}`.
AG-UI has no standardized well-known path (unlike A2A's agent card), so
this descriptor is a mast convention for clients that want to
discover-then-connect.

**Invocation** is a `POST` to each workload's `endpoint_path`. The body
is an AG-UI `RunAgentInput`; the response is the run streamed as SSE
frames: `RunStarted`, then a `StateSnapshot` echoing the client's input
state, then the model's answer as a `TextMessage` triad
(`TextMessageStart`/`Content`/`End`) with `ToolCallStart`/`Args`/`End`
and `ToolCallResult` frames for any tool activity, then exactly one
terminal frame — `RunFinished` on success (`outcome: {type: "success"}`)
or on a HITL pause (`outcome: {type: "interrupt", interrupts: […]}`), or
`RunError` (`aborted` when the session was aborted or gate-paused,
`internal` on a runner error). Updates are
message-granular (one text message per model response, not token deltas).
The session id is always **derived by the daemon** from the AG-UI
`threadId`/`runId` and namespaced under `agui-` — a client never supplies
a raw session id, and an id that would collide with a reserved
`…:mast-ops` row is refused before the stream opens. `session_model`
picks the mapping: `per_thread` (the default — one continuing session per
thread, matching chat UX) or `per_run` (a fresh session per run, for
stateless one-shots).

**HITL.** A turn that parks for human input closes the stream with a
terminal `RunFinished` whose `outcome.type` is `interrupt`, listing each
pending interrupt as `{id, message, responseSchema?, expiresAt?}`
(projected from the durable session's pending-interrupt state — not a
fabricated success). The client **resumes** by starting a new run whose
`RunAgentInput.resume` carries one entry per interrupt (`{interruptId,
status: "resolved" | "cancelled", payload?}`); the daemon reconciles each
entry against the session's open interrupt ids and drives the resume turn
through the same chokepoint. A resume that references no open interrupt
(or an unknown id) is refused `409` rather than silently forking a fresh
turn. A resume run may carry an empty `input` — the `resume` array alone
drives it. Under `session_model: per_run` (where the session is keyed on
`runId`) the resume must also carry `parentRunId` naming the run that
parked, so the daemon can reach the parked session; under the default
`per_thread` the shared `threadId` already reaches it.

Per-key state deltas, client-declared tools, and the `agui://` federation
client are follow-on stages.

**Auth.** The discovery descriptor is always public. Each run endpoint is
authenticated when `MAST_AGUI_TOKEN` is set — a request without a valid
bearer is refused `401`, and a token lacking a workload's declared
`auth.scopes` is refused `403`. Unset means unauthenticated (dev only,
warned at startup) — and because a run drives a budgeted turn, a
**non-loopback** `--agui-listen` bind is *refused at startup* without a
token; bind loopback or set `MAST_AGUI_TOKEN`.

**Rate limiting.** A run consumes the agent's provider budget, so runs are
rate limited per caller and workload. Set `MAST_AGUI_RATE` to the allowed
requests/second, and optionally `MAST_AGUI_BURST` for the bucket depth
(defaults to `ceil(rate)`, minimum 1):

```bash
MAST_AGUI_RATE=5 MAST_AGUI_BURST=10 mast --agui-listen=127.0.0.1:7781 ...
```

A refused run returns HTTP `429` with an advisory `Retry-After` header
(AG-UI has no JSON-RPC error envelope, so refusals ride HTTP status).
`MAST_AGUI_RATE` unset means no rate limiting; a set-but-malformed value
fails startup.
