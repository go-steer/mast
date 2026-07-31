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
| `--session-db` | (empty) | SQLite file path (default driver) or Postgres DSN/URL with `--session-db-driver=postgres`. Empty = in-memory sessions, **no durability**. |
| `--session-db-driver` | `sqlite` | `sqlite` or `postgres`. `postgres` with an empty `--session-db` is a startup error, never a silent in-memory downgrade. |
| `--timeout` | `5m` | One-shot turn deadline (`2m`, `90s`, …); `0` disables. One-shot only — serve-mode wallclock ceilings come from workload budgets. An unresponsive backend (or a provider SDK silently retrying on quota errors) fails loudly instead of hanging a script. |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` (JSON logs on stderr). |
| `--version` | — | Print version and exit. |

Set `MAST_INJECT_TOKEN` to require bearer auth on the HTTP endpoints;
unset means unauthenticated (dev only, warned at startup). The attach
surface has its own token, `MAST_ATTACH_TOKEN` — attach can read
transcripts and inject operator messages, so treat it as a separate
trust boundary from the inject webhook.

## `mast sessions` (operator surface)

```
mast sessions list   --session-db=... [--user=...] [--state=paused|aborted|interrupted|idle]
mast sessions show   <session-id> --session-db=...
mast sessions resume <session-id> --interrupt=<iid> --response='{"approved":true}' [--addr=...]
mast sessions abort  <session-id> [--reason=...] [--addr=...]
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

`abort` writes a durable abort marker with a reason; the daemon then
refuses resumes for that session. It's a marker plus resume-refusal, not
engine preemption (engine-level terminal state is v0.2 work). Markers
live in a companion row beside the session, not in its transcript — an
in-flight turn is never disturbed by an abort (or by a shutdown
marker), and the model never sees marker events.

Resume is keyed by **interrupt ID** (`--interrupt` + `--response`), not by
token — resume tokens are the v0.2 programmatic-pause surface.

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
reports `paused`, not `interrupted`. Boot-time auto-resume of
interrupted sessions is deliberately v0.2 work (it gates on the
recorded-effect outbox for mutating-tool idempotency).

Two related contracts: the daemon runs **one turn per session at a
time** — a second inject or resume for the same session queues behind
the in-flight turn (bounded by the wallclock budget) rather than
corrupting it; and every SQLite session store the tooling opens
(serve, one-shot, the sessions CLI) carries the same write hardening,
so concurrent access waits instead of failing.
