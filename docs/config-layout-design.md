# mast config layout: design

**Status:** draft, 2026-07-01. Companion to [`./specialists-design.md`](./specialists-design.md) (`.agents/specialists/*.tmpl`), [`./orchestration-design.md`](./orchestration-design.md) (`.agents/workloads/*.yaml`), [`./mcp-catalog-design.md`](./mcp-catalog-design.md) (`.agents/mcp/*.json`), [`./library-api-design.md`](./library-api-design.md) (config precedence surface), and [`./deployment-design.md`](./deployment-design.md) (config injection patterns per topology). Small doc that ties together the `.agents/` file layout across subsystems and defines precedence rules, naming conflicts, hot-reload semantics, and env-var overrides.

## Why this needs its own doc

Multiple subsystems own file layouts under `.agents/` (specialists, workloads, MCP config, planned: reducers). Without a coordinating design, operators face:

- Ambiguity about where `.agents/` lives (project root? config dir? user home?).
- Ambiguity about file discovery (recursive glob? single directory? explicit list?).
- Ambiguity about precedence when the same name appears in multiple locations.
- Ambiguity about hot-reload — does editing a specialist file take effect immediately or need restart?
- Ambiguity about env-var overrides — which config values can be overridden and how.

Individual subsystem docs punt on these ("`.agents/specialists/*.tmpl`" without saying where `.agents/` is). This doc gives them one answer.

## Layout

The canonical layout under `.agents/`:

```
.agents/
  specialists/                  # specialist definitions (see specialists-design.md)
    ImagePullBackOff.tmpl
    CrashLoopBackOff.tmpl
    ...
  workloads/                    # workload bundles (see orchestration-design.md)
    incident-triage.yaml
    drift-detection.yaml
    ...
  mcp/                          # MCP server wiring (see mcp-catalog-design.md)
    gke.json
    prometheus.json
    ...
  a2a/                          # A2A client configs (see a2a-design.md)
    external-triage-agent.yaml
    registry.yaml
    ...
  remote/                       # federation configs — mast-native + HTTP/RPC (see federation-design.md)
    internal-scanner.yaml
    peer-mast-fleet.yaml
    ...
  skills/                       # SKILL.md skill bundles (see skills-design.md)
    gke-triage.skill/
      SKILL.md
    k8s-upgrade.skill/
      SKILL.md
    registry.yaml                # optional; for registry discovery
    ...
  memory/                       # (future) memory reducers if we ever add file-based ones
  reducers/                     # (future) reserved
mast.yaml                       # runtime config (or .mast/mast.yaml; both accepted)
```

Directory names are canonical — mast looks for `specialists/`, `workloads/`, `mcp/`, `a2a/`, `remote/`, `skills/` (and any future additions) by those exact names under `.agents/`.

## Discovery locations

`.agents/` and the runtime config file are looked up in a specific order:

1. **`$MAST_CONFIG_DIR`** (env-var override) — if set, this is the canonical location; nothing else is consulted.
2. **`./.agents/` in the process working directory** — the primary location for CLI + library-embedded consumers running from a project root.
3. **`~/.config/mast/agents/`** (XDG-compliant on Linux; equivalent on macOS/Windows) — the fallback location for user-level defaults.
4. **`/etc/mast/agents/`** (Linux only; system-level) — the fallback for system deployments that don't use env-var config.

**Merging strategy across locations:** last-write-wins is *not* the default. Locations are consulted independently:

- If `$MAST_CONFIG_DIR` is set: only that location is used. Others ignored.
- Otherwise: the *first* location that exists (`./`, `~`, `/etc`) is used exclusively. No merging across locations.

Rationale: cross-location merging invites subtle bugs (specialist X in `~/.config/...` overrides specialist X in `./.agents/`? or vice versa?) and hides discoverability. One location per invocation, deterministically chosen.

**Explicit override for library-embedded consumers:** the library API accepts `WithBundleDir(...)`, `WithSpecialistDir(...)`, `WithMCPDir(...)` options that bypass the discovery order entirely. Library consumers embedding mast inside their own service typically use these to point at their host's config layout.

## File discovery within a directory

Each subsystem directory is scanned per its own rules:

| Directory | Glob | Recursive? |
|---|---|---|
| `specialists/` | `*.tmpl` | No; flat directory |
| `workloads/` | `*.yaml` (also `.yml`) | No; flat directory |
| `mcp/` | `*.json` | No; flat directory |
| `a2a/` | `*.yaml` (also `.yml`) | No; flat directory |
| `remote/` | `*.yaml` (also `.yml`) | No; flat directory |
| `skills/` | `*.skill/` (directories); `SKILL.md` inside each; also `registry.yaml` at top level | No skill-directory recursion; one skill per bundle dir |

**Flat, not recursive.** Nested subdirectories are ignored (allows operators to organize source repos with subdirs like `specialists/archive/` for retired-but-kept files).

**Filename-derived names.** For specialists and workloads, the filename (minus extension) is the default name; explicit `name:` in the file overrides. For MCP config, `name:` is required in the file (JSON doesn't have a natural filename→name mapping).

## Name collisions

Two files defining the same name in the same directory:

- **v0.1:** load-time error. Fail-fast; operator resolves.
- **v0.2+:** may add explicit `namespace` support for large operator setups if the flat-directory + fail-on-collision pattern creates friction. Not v0.1 concern.

Same-name collisions across directories (e.g., a specialist and a workload with the same name) are not collisions — they live in different namespaces.

## Runtime config file

`mast.yaml` (top-level) or `.mast/mast.yaml` — the runtime config. Discovery order:

1. `$MAST_CONFIG_FILE` (env-var override) — full path to a config file; wins over everything else.
2. `./mast.yaml` or `./.mast/mast.yaml` in process working directory.
3. `~/.config/mast/mast.yaml`.
4. `/etc/mast/mast.yaml`.

Same first-match-wins rule as `.agents/` discovery.

**Config file schema** stays deliberately small — file-loaded config is for values operators change (session store DSN, observability endpoints, resource limits), not for programmatic knobs (custom providers, hooks, extension points). Full schema documented in [`./library-api-design.md`](./library-api-design.md).

## Env-var overrides

Any config value can be overridden via env var following a convention:

- Config key `session_store.type` → env var `MAST_SESSION_STORE_TYPE`.
- Config key `observability.otel.endpoint` → env var `MAST_OBSERVABILITY_OTEL_ENDPOINT`.
- Config key `deployment.instance_id` → env var `MAST_DEPLOYMENT_INSTANCE_ID`.

Uppercase; dots replaced with underscores; `MAST_` prefix.

**Sensitive values (credentials, tokens) should always be env-supplied**, not file-configured — never put secrets in `.agents/*` or `mast.yaml`. The credential resolvers (per [`./mcp-catalog-design.md`](./mcp-catalog-design.md)) reference env vars for token retrieval.

**Env vars override file config unconditionally.** Order of resolution per config field:

1. Explicit programmatic (via library API `mast.Config{...}`).
2. Env var.
3. File.
4. Built-in default.

## Config file references to `.agents/` contents

`mast.yaml` can reference `.agents/` contents by name (not by path). Example:

```yaml
# mast.yaml
default_workload: incident-triage    # resolves against workloads/*.yaml
```

References resolve at load time; missing references are load-time errors.

## Hot-reload

Hot-reload semantics per file class:

| File class | Hot-reload | Trigger | v0.X |
|---|---|---|---|
| Specialists (`.tmpl`) | Not v0.1; may add | `SIGHUP` in v0.2 | v0.2 |
| Workloads (`.yaml`) | Not v0.1; may add | `SIGHUP` in v0.2 | v0.2 |
| MCP config (`.json`) | Not v0.1 | Restart required in v0.1; may add SIGHUP later | v0.3+ |
| Runtime config (`mast.yaml`) | Not v0.1 | Restart required | v0.3+ |

**In v0.1, all file changes require mast restart.** File changes during a session don't affect the running session (which loaded its config at start).

**In v0.2**, `SIGHUP` triggers a re-scan of `specialists/` and `workloads/` directories:
- New files → loaded and registered.
- Modified files → replace existing (only for new sessions; existing sessions retain their loaded version).
- Deleted files → deregister for new sessions.
- Errors during reload → log + retain previous state.

**Programmatic hot-reload for library consumers** via `specialist.Source.Watch(ctx)` and `workload.Source.Watch(ctx)` (per [`./library-api-design.md`](./library-api-design.md)). Consumers implementing custom sources (Kubernetes ConfigMaps, external CMS) get watch semantics via their source implementation.

## Validation

Config validation happens at load time — not at session start.

- **Specialists** load-validate: YAML frontmatter parses; required fields present; `mode` value is legal; referenced tool names exist in the registry.
- **Workloads** load-validate: YAML parses; required fields present; `task_class` is legal; `specialists[]` references exist in the specialist registry; `tool_catalog.mcp[].server` references exist in the MCP config; `budget` fields have legal ranges.
- **MCP config** load-validate: JSON parses; `name` present; `transport` is legal; `command` (for stdio transport) or `url` (for HTTP transport) present.
- **Runtime config** load-validate: YAML parses; unknown top-level keys warn (not error, to allow forward-compat evolution); values that must be one-of-enum enforce.

**Load-time errors are fatal** — mast refuses to start with invalid config. Session-start errors (e.g., a bundle references a specialist that was deleted after last successful load) are per-session errors.

## Precedence summary (one place)

Config value precedence, highest wins:

1. **Explicit programmatic** — passed via library API.
2. **Env var** — `MAST_*` prefix.
3. **Runtime config file** — `mast.yaml` (via discovery order).
4. **Subsystem file** — `.agents/*/*.{tmpl,yaml,json}` (via discovery order).
5. **Built-in default** — hard-coded fallback.

`.agents/` discovery order:
1. `$MAST_CONFIG_DIR` (exclusive if set).
2. `./.agents/`.
3. `~/.config/mast/agents/`.
4. `/etc/mast/agents/`.

Runtime config discovery order (same shape):
1. `$MAST_CONFIG_FILE` (exclusive if set).
2. `./mast.yaml` or `./.mast/mast.yaml`.
3. `~/.config/mast/mast.yaml`.
4. `/etc/mast/mast.yaml`.

## Deployment injection patterns

Different topologies have different natural config-injection shapes:

| Topology | `.agents/` injection | `mast.yaml` injection |
|---|---|---|
| Standalone / laptop | Files in project directory | Same |
| Cloud Run | Bundled in container image (build-time) OR mounted secret / config | Env vars primarily |
| GKE | ConfigMap volume mount → `/etc/mast/agents/` | ConfigMap → `/etc/mast/mast.yaml`; env from Secret |
| Library-embedded | Programmatic registration bypasses file layout | Programmatic `mast.Config{...}` |
| Bundled | `.agents/` alongside binary in tarball | `mast.yaml` alongside |

Container images built via `examples/deploy/*/Dockerfile` bake in sensible defaults but leave `.agents/` overridable via mount for operator customization.

## Composition with subsystems

| Subsystem | Config-layout dependency |
|---|---|
| **Specialists** | `.agents/specialists/*.tmpl` per [`./specialists-design.md`](./specialists-design.md) |
| **Orchestration** | `.agents/workloads/*.yaml` per [`./orchestration-design.md`](./orchestration-design.md) |
| **MCP catalog** | `.agents/mcp/*.json` per [`./mcp-catalog-design.md`](./mcp-catalog-design.md); wiring templates under `.agents/mcp/*.example.json` |
| **Library API** | Programmatic registration bypasses file layout entirely; precedence rules apply uniformly |
| **Deployment** | Injection patterns per topology; ConfigMap volume mounts for GKE |
| **Observability** | `mast.yaml`'s `observability.*` section |
| **Durable execution** | `mast.yaml`'s `session_store.*` section; env-supplied credentials |
| **Memory** | `mast.yaml`'s `memory.*` section |

## Phasing

| Version | Scope |
|---|---|
| **v0.1** | Discovery order (env / project / user / system) established. Load-time validation. Env-var override convention. No hot-reload; restart-required for all changes. |
| **v0.2** | `SIGHUP` hot-reload for specialists + workloads. `specialist.Source.Watch()` + `workload.Source.Watch()` library API. |
| **v0.3+** | MCP config hot-reload. Runtime config hot-reload for the safe subset (log levels, observability endpoints). Full-runtime-config hot-reload remains out-of-scope. |

## Open questions

1. **Cross-location merging as an opt-in.** Some operators may want to merge (e.g., system-level defaults + project-level overrides). Bias: not v0.1; add as `MAST_CONFIG_MERGE=true` opt-in in v0.3+ if operators ask.
2. **Config file format.** YAML for `mast.yaml`, YAML for workloads, JSON for MCP config, YAML-frontmatter for specialists. Consistent enough for the pattern to be predictable; JSON for MCP because that matches typical MCP client config conventions upstream. Consider migrating MCP to YAML in v0.2 for consistency? Bias: keep JSON — upstream convention wins over internal consistency.
3. **Windows-friendly paths.** `$HOME/.config/mast/`, `C:\ProgramData\mast\` — need windows-adapted defaults. Bias: yes; use `os.UserConfigDir()` and `os.TempDir()` for cross-platform correctness.
4. **Secrets in `.agents/`.** Should we detect and warn on secret-shaped values in `.agents/*` (things matching common secret patterns)? Bias: yes; load-time warning; documented anti-pattern.
5. **Config schema versioning.** As mast evolves, config file schema will change. How do we communicate breaking changes? Bias: `mast.yaml` gets a top-level `version:` field starting v0.2; missing version = v0.1 assumed.
6. **In-cluster config from ConfigMap CRDs.** Should we ship a Kubernetes CRD for mast config (WorkloadDef, SpecialistDef)? Bias: no v0.1; standard ConfigMap volume mounts suffice; CRD is v1.0+ if a real operator use case surfaces.

## Out of scope

- **A dedicated config UI.** mast-web has session views + workload views but is not a config editor. Operators edit files.
- **Config migration tools between versions.** Documented migration steps in CHANGELOG; not automated.
- **Config schema in a separate file (e.g., JSON Schema doc)**. Doc-first; schema-file consideration for v0.3+ if consumers ask.
- **Encrypted config files at rest.** Storage encryption (disk-level, secret-manager) is the deployment's job; mast doesn't add a layer.

## Related

- [`./specialists-design.md`](./specialists-design.md) — file layout for specialists
- [`./orchestration-design.md`](./orchestration-design.md) — file layout for workloads
- [`./mcp-catalog-design.md`](./mcp-catalog-design.md) — file layout for MCP config
- [`./library-api-design.md`](./library-api-design.md) — programmatic bypass of file layout; precedence rules
- [`./deployment-design.md`](./deployment-design.md) — injection patterns per topology
- [XDG Base Directory Specification](https://specifications.freedesktop.org/basedir-spec/basedir-spec-latest.html) — the Linux/macOS convention we follow
