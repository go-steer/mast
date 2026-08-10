---
title: MCP servers (mcp.json)
description: The mcp.json catalog mast parses to wire MCP toolsets — HTTP and local stdio transports, credential handling, and how workloads reference servers.
sidebar:
  order: 5
---

mast connects agents to tools over the **Model Context Protocol (MCP)**.
Server definitions live in a catalog file, `mcp.json`; a
[workload bundle](/reference/workload-bundle/) references them by name via
`tool_catalog.mcp[].server`. mast wires each referenced server generically,
dispatched by transport kind — no server is special-cased.

## Where mcp.json lives

- **Directory mode** (`--workload=<path>`) — next to the workload:
  `<path>/mcp.json`.
- **Name mode** (`--workload=<name>`) — at the config root selected by
  [.agents/ discovery](/reference/agents-discovery/).

A workload that references a server absent from `mcp.json` is a **fatal
load error** — mast refuses to start rather than silently drop a tool.

:::caution[mcp.json is a control-plane file]
A `stdio` entry names a local command mast will execute, so editing
`mcp.json` grants code execution. Treat it as trusted operator
configuration on par with `config.json`. Each stdio launch is logged
(resolved command **and** args) so operators can audit what runs.

The permission gate write-protects a catalog that lives inside an
`.agents/` tree — a model with file-write access cannot rewrite it to
register a malicious server. A catalog loaded from any other location (a
path-mode workload directory, `/etc/mast/agents`, `$MAST_CONFIG_DIR`) is
**not** covered by the `.agents/` heuristic; the daemon registers such a
catalog's resolved path with the gate explicitly once the gate is
runtime-wired, and until then it relies on filesystem permissions.

Two catalog-level defenses harden stdio further, independent of the gate:

- **`command_allowlist`** bounds which executables any stdio server may
  launch — a non-empty list makes an out-of-allowlist `command` a fatal
  load error, so even an edited catalog cannot introduce a new binary.
- **`env_mode: "clean"`** stops a stdio child from inheriting the daemon's
  full environment (API keys, cloud credentials), passing through only the
  variables you name in `env_passthrough`.
:::

## Schema

```json
{
  "version": 1,
  "command_allowlist": ["/usr/local/bin/fs-mcp-server"],
  "servers": {
    "gke": {
      "transport": "http",
      "url": "https://container.googleapis.com/mcp",
      "auth": {
        "google_oauth": {
          "scopes": ["https://www.googleapis.com/auth/cloud-platform"]
        }
      }
    },
    "filesystem": {
      "transport": "stdio",
      "command": "/usr/local/bin/fs-mcp-server",
      "args": ["--root", "${WORKSPACE}/data"],
      "env_mode": "clean",
      "env_passthrough": ["PATH", "HOME"],
      "env": {
        "FS_MCP_TOKEN": "${FS_MCP_TOKEN}"
      }
    }
  }
}
```

| Field | Type | Notes |
|---|---|---|
| `version` | int | Required. Must be `1` — the only schema version this build accepts. |
| `command_allowlist` | list of strings | Optional, catalog-level. When non-empty, every stdio server's resolved `command` must appear here or the catalog fails to load. Both sides are `${VAR}`-expanded before comparison. Empty (the default) imposes no restriction. |
| `servers` | map | Server name → definition. The map key is the name workloads reference; it must be non-empty. |
| `servers.<name>.transport` | string | Required. `http` or `stdio`. |
| `servers.<name>.url` | string | Required for `http`. The streamable-HTTP endpoint. |
| `servers.<name>.auth.google_oauth.scopes` | list of strings | `http` only. When present, requests carry an Application Default Credentials (ADC) bearer token with these scopes. Omit for an unauthenticated endpoint. Empty scopes default to `cloud-platform`. |
| `servers.<name>.command` | string | Required for `stdio`. Executable to launch — a bare name resolved on `PATH` or an absolute path. |
| `servers.<name>.args` | list of strings | `stdio` only. Command arguments. |
| `servers.<name>.env_mode` | string | `stdio` only. `inherit` (default) — the child inherits the full daemon environment. `clean` — the child starts from an empty environment and receives only `env_passthrough` variables plus `env`. |
| `servers.<name>.env_passthrough` | list of strings | `stdio` only, and only under `env_mode: "clean"`. Names of daemon environment variables to copy through to the child (each copied only if set). Rejected under `inherit`, where the child already sees everything. |
| `servers.<name>.env` | map | `stdio` only. Environment variables layered on top (they override an inherited or passed-through variable of the same name). |

Unknown fields are tolerated (forward-compatibility with richer catalogs);
the loader validates the version, each server name, and the per-transport
required fields, reporting the first problem it finds.

## Transports

### http

Speaks MCP over a streamable-HTTP endpoint. When the entry declares
`auth.google_oauth`, mast attaches an ADC bearer token and **fails fast at
startup** if credentials cannot be loaded — surfacing a misconfiguration as
a clear load error rather than a mid-run tool failure. Without an `auth`
block the endpoint is called unauthenticated.

### stdio

mast launches `command` (with `args` and `env`) as a local child process
and speaks MCP over its stdin/stdout. This is the path for local or
sidecar MCP servers that authenticate through their own environment rather
than a bearer token.

- **Variable expansion.** `${VAR}` references in `command`, each `args`
  entry, and each `env` value are expanded against the daemon environment.
  Expansion follows Go's `os.ExpandEnv`, so a bare `$name` expands too and
  there is no escape for a literal `$` — a value that must contain a
  literal dollar (for example a secret) should be passed through the
  inherited daemon environment rather than written into the catalog.
- **Environment.** Under the default `env_mode: "inherit"` the child
  inherits the daemon's environment and the configured `env` entries are
  layered on top (they override, applied in a deterministic order). Under
  `env_mode: "clean"` the child starts from an empty environment and sees
  only the `env_passthrough` daemon variables that are set, plus `env` — so
  a local tool server never receives the daemon's provider API keys or
  cloud credentials unless you name them explicitly.
- **Command allowlist.** The catalog-level `command_allowlist`, when
  non-empty, restricts which executables stdio servers may launch: an
  out-of-allowlist `command` is a fatal load error. This bounds the blast
  radius of an edited catalog even where the file itself is not
  gate-protected. The match is a literal string comparison after `${VAR}`
  expansion — it does **not** resolve `PATH` or canonicalize symlinks, so
  list a `command` exactly as it is written in the server entry (a bare
  `node` and an absolute `/usr/bin/node` are distinct entries).
- **Lifecycle.** The process is launched lazily on first tool use, not at
  startup. It then lives for the duration of the daemon (mast holds the
  toolset for the process lifetime and does not tear individual toolsets
  down); the child exits when it closes its own stdio or the daemon exits.

## How wiring interacts with `--model`

MCP is **not** wired under the default `echo` model, which never emits tool
calls — wiring it there would be pure startup cost and would surface
credential problems as workload-load failures. The `scripted` model and
real providers do wire MCP. Because a `stdio` server needs no cloud
credentials, you can drive real tool calls fully offline with
`--model scripted`.

## Tool policy

Per-workload and per-specialist allowlists narrow which of a server's tools
an agent may call; see
[`tool_catalog`](/reference/workload-bundle/#fields). MCP tools default to
**mutating** for the recorded-effect outbox unless a `tool_catalog.tools[]`
override marks them read-only.
