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
| `--model` | `echo` | `echo` (offline fake, no credentials) or a Gemini model id like `gemini-2.5-flash`. |
| `--listen` | `:7777` | HTTP bind address for `/inject`, `/resume`, `/abort`, `/metrics`. |
| `--session-db` | (empty) | SQLite file path (default driver) or Postgres DSN/URL with `--session-db-driver=postgres`. Empty = in-memory sessions, **no durability**. |
| `--session-db-driver` | `sqlite` | `sqlite` or `postgres`. `postgres` with an empty `--session-db` is a startup error, never a silent in-memory downgrade. |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` (JSON logs on stderr). |
| `--version` | — | Print version and exit. |

Set `MAST_INJECT_TOKEN` to require bearer auth on the HTTP endpoints;
unset means unauthenticated (dev only, warned at startup).

## `mast sessions` (operator surface)

```
mast sessions list   --session-db=... [--user=...] [--state=paused|aborted|idle]
mast sessions show   <session-id> --session-db=...
mast sessions resume <session-id> --interrupt=<iid> --response='{"approved":true}' [--addr=...]
mast sessions abort  <session-id> [--reason=...] [--addr=...]
```

The split is deliberate:

- **`list` / `show` read the session DB directly** (SQLite path) — they
  work with or without a running daemon. `list` prints one row per session
  with state (`paused` / `aborted` / `idle`) and pending interrupt IDs;
  `show` prints session detail including each pending interrupt's message,
  response schema, and a copy-pasteable resume command.
- **`resume` / `abort` go through a running daemon** (`--addr`, default
  `http://127.0.0.1:7777`) — resume must be executed by the runner that
  owns the workflow, and routing abort through the daemon keeps a single
  SQLite writer. Both send `MAST_INJECT_TOKEN` as a bearer when set.

`abort` writes a durable abort marker with a reason; the daemon then
refuses resumes for that session. It's a marker plus resume-refusal, not
engine preemption (engine-level terminal state is v0.2 work).

Resume is keyed by **interrupt ID** (`--interrupt` + `--response`), not by
token — resume tokens are the v0.2 programmatic-pause surface.
