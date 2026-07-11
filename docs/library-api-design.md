# mast library API: design

**Status:** draft, 2026-07-01. Companion to [`./positioning.md`](./positioning.md) ("library-embedded" is a pillar; this doc scopes what the library-consumer sees), [`./fork-design.md`](./fork-design.md) (bucket 1's lean core defines the public surface; bucket 2's ports plumb through it), [`./durable-execution-design.md`](./durable-execution-design.md) (session inspection + resume are library API surface), and [`./orchestration-design.md`](./orchestration-design.md) (workloads and bundles are configurable programmatically, not just via `.agents/*.yaml`). Covers the public Go API surface plus the extension points third parties can plug into.

## Why this is its own doc

Mast has two consumer shapes: binary (`mast` CLI + `mast-web` UI) and library (`import "github.com/go-steer/mast/…"` inside a larger Go service). Both are load-bearing per positioning, but they have genuinely different needs:

- **Binary consumers** want configuration via files, CLI flags, and env; sensible defaults; a working system out of the box.
- **Library consumers** want programmatic construction; embeddable config (values, not files); lifecycle hooks; the ability to inject providers / permission gates / eventlog stores; import stability.

Bucket 1's lean core defines the answer *by construction*. If we don't design the library API explicitly, we design it by accident during bucket 1 — and once shipped, we're locked in to whatever surface we accidentally created. This doc scopes the surface deliberately so bucket 1 can implement to it.

## Import surface

Public packages under `github.com/go-steer/mast/`:

| Package | Purpose | Stability |
|---|---|---|
| `github.com/go-steer/mast` | Top-level convenience API: `mast.Run`, `mast.RunWorkload`, `mast.Serve`, `mast.ListSessions`, `mast.Resume` | Stable from v0.1 |
| `github.com/go-steer/mast/agent` | Core types: `Agent`, `Config`, `Context`, `Result`, mode constants | Stable from v0.1 |
| `github.com/go-steer/mast/workload` | Workload bundle types: `Bundle`, `Loader`, `Resolver`, `Registry` | Stable from v0.1 |
| `github.com/go-steer/mast/specialist` | Specialist types: `Spec`, `Registry`, `Loader` | Stable from v0.1 |
| `github.com/go-steer/mast/provider` | Provider interface + built-in registrations | Interface stable; built-ins may add |
| `github.com/go-steer/mast/tool` | Tool interface + built-in tools | Interface stable |
| `github.com/go-steer/mast/permission` | Permission gate types + scope helpers | Stable from v0.1 |
| `github.com/go-steer/mast/session` | Session types: `Store` interface, `Event`, `PauseSpec`, `ResumeSpec` | Stable from v0.1 |
| `github.com/go-steer/mast/eventlog` | Built-in eventlog store implementations (SQLite; postgres in v0.2) | Interface stable; may add adapters |
| `github.com/go-steer/mast/attach` | Attach mode server + client | Stable from v0.1 |
| `github.com/go-steer/mast/observability` | Metric registry; OTel wiring; standard attribute keys | Stable from v0.1 |
| `github.com/go-steer/mast/mcp` | MCP client + transparent wrap | Stable from v0.1 |
| `github.com/go-steer/mast/a2a` | A2A server + client + `TokenValidator` interface (see [`./a2a-design.md`](./a2a-design.md)) | Stable from v0.1 |
| `github.com/go-steer/mast/federation` | Federation adapter interface + built-in adapters (A2A / mast-native / HTTP/RPC) (see [`./federation-design.md`](./federation-design.md)) | Interface stable from v0.1; built-in adapters may add |

**Internal / churnable** packages under `github.com/go-steer/mast/internal/…`. These are the bucket 1 shim + runtime glue; not for library consumers.

**Compatibility promise:** stable-marked packages follow semver. Breaking changes to stable packages between major versions get a migration doc and a deprecation cycle in the preceding minor version. Internal packages have no compatibility promise.

## Top-level convenience API

The 90% path for library-embedded consumers. Small; delegates to lower packages for detail.

### Simple invocation

```go
import "github.com/go-steer/mast"

result, err := mast.Run(ctx, mast.Config{
    TaskClass: mast.TaskDebug,
    Provider:  mast.Provider{Name: "gemini", Model: "gemini-2.5-pro"},
    Input:     "diagnose the latest failed deploy",
})
if err != nil {
    return fmt.Errorf("mast: %w", err)
}
fmt.Println(result.Output)
```

`mast.Config` bundles the common knobs: task class, provider, input, optional tool overrides, optional specialist registry, optional observability config. Anything not set uses sensible defaults + env-inherited config.

### Workload invocation

```go
result, err := mast.RunWorkload(ctx, "incident-triage", input,
    mast.WithBundleDir("./.agents/workloads"),  // optional; env-default if unset
    mast.WithSpecialistDir("./.agents/specialists"),
)
```

`RunWorkload` is the entry for unattended-style consumers whose input maps to a named workload. Bundle discovery is convention-based (dir scan) by default; can be overridden with programmatic bundle registration (see below).

### Programmatic bundle registration

For consumers who don't want file-based bundle discovery:

```go
registry := workload.NewRegistry()
registry.Register(workload.Bundle{
    Name: "custom-triage",
    TaskClass: workload.TaskOrchestrate,
    Specialists: []string{"MyChecker", "MyResolver"},
    ToolCatalog: workload.ToolCatalog{
        Builtin: []string{"read_file", "grep"},
        MCP: []workload.MCPAllow{{Server: "k8s"}},
    },
    Planner: workload.PlannerConfig{Enabled: true},
    Budget:  workload.Budget{MaxCostUSD: 3.00},
})

result, err := mast.RunWorkload(ctx, "custom-triage", input,
    mast.WithWorkloadRegistry(registry),
)
```

Same for specialists — programmatic registration bypasses file discovery. Useful for library consumers who ship their own specialists inside the binary rather than requiring operators to author `.tmpl` files.

### Server mode

```go
srv := mast.NewServer(mast.ServerConfig{
    Attach: attach.Config{Listen: ":8080"},
    Metrics: observability.Config{Prometheus: observability.PrometheusConfig{Listen: ":9100"}},
    WorkloadRegistry: registry,
    SpecialistRegistry: specRegistry,
})
if err := srv.Serve(ctx); err != nil {
    log.Fatal(err)
}
```

Server mode runs the attach listener + metrics endpoint + optional CLI dispatcher inside the host process. Useful for library consumers who want to embed mast alongside their own HTTP server / gRPC server / Kubernetes operator.

### Session inspection + resume

```go
// List paused sessions
sessions, err := mast.ListSessions(ctx, mast.SessionFilter{
    State:    mast.StatePaused,
    Workload: "incident-triage",
})

// Get pause details
sess, err := mast.GetSession(ctx, sessionID)
fmt.Println(sess.PauseReason, sess.PauseMessage, sess.ResumeToken)

// Resume
err = mast.Resume(ctx, sessionID, resumeToken, resumeInput)
```

Covered in more detail in [`./durable-execution-design.md`](./durable-execution-design.md); the library-facing surface mirrors the CLI surface exactly.

## Lifecycle hooks

Library consumers embedding mast in a larger service need hooks to integrate with the host's lifecycle:

```go
srv := mast.NewServer(mast.ServerConfig{
    // ... other config
    Hooks: mast.Hooks{
        BeforeSessionStart: func(ctx context.Context, s *session.StartInfo) error {
            // e.g. inject request ID, check quotas, log
            return nil
        },
        AfterSessionEnd: func(ctx context.Context, s *session.EndInfo) {
            // e.g. emit business metrics, notify
        },
        BeforeToolCall: func(ctx context.Context, t *tool.CallInfo) error {
            // e.g. per-tool authorization
            return nil
        },
        BeforePause: func(ctx context.Context, p *session.PauseInfo) error {
            // e.g. persist to external system before mast's own store
            return nil
        },
        OnHITLRequest: func(ctx context.Context, h *session.HITLInfo) error {
            // e.g. forward to Slack/PagerDuty in addition to mast-web
            return nil
        },
    },
})
```

Hooks are:
- **Called synchronously** — they can veto (return error) actions they don't allow.
- **Named by lifecycle phase** — no wildcard "any-event" hook (that's what event log subscription is for).
- **Composable** — multiple hooks chain in registration order; first error aborts.
- **Cancellable** — hook that returns `context.Canceled` cleanly aborts without erroring.

Hooks are the primary integration point for library consumers who need to enforce host-side policies on mast execution. They compose with (do not replace) the permission gate — the gate is mast's internal authorization; hooks are the host's.

## Embeddable config vs. file-loaded config

The library API accepts both file-loaded and programmatic config, but the two are equivalent at the type level:

- `.agents/workloads/*.yaml` files are parsed by `workload.Loader` into `workload.Bundle` values.
- Programmatic `workload.Bundle` values are registered directly.
- Both end up in the same `workload.Registry` and are indistinguishable at execution time.

Same story for specialists (`.agents/specialists/*.tmpl` → `specialist.Spec` values; programmatic `specialist.Spec` registration is equivalent) and MCP config (`.agents/mcp.json` → `mcp.ServerConfig` values).

**Implications:**
- Library consumers can mix file-loaded and programmatic — load defaults from files, override specific bundles programmatically.
- Library consumers can test bundle behavior in Go tests without needing YAML fixtures.
- Library consumers can generate bundles from their own config sources (a Kubernetes ConfigMap, an external CMS, etc.) at process start.

## Extension points

Third parties (library consumers, operators with custom needs) can plug into mast at several seams. Each seam is a Go interface in a stable package.

### Provider

```go
// github.com/go-steer/mast/provider
type Provider interface {
    Name() string
    Call(ctx context.Context, req Request) (Response, error)
    Stream(ctx context.Context, req Request) (<-chan StreamChunk, error)
    // ... other v2-required methods
}
```

Built-in: Gemini, Vertex, Anthropic, Anthropic-Vertex, echo, scripted. Third-party providers register:

```go
provider.Register("ollama", &myOllamaProvider{})
```

Registrations are process-global. For test isolation, use a per-config provider map.

### Session store

```go
// github.com/go-steer/mast/session
type Store interface {
    Create(ctx context.Context, s *StartInfo) (SessionID, error)
    AppendEvent(ctx context.Context, id SessionID, e Event) error
    LoadEvents(ctx context.Context, id SessionID) ([]Event, error)
    Pause(ctx context.Context, id SessionID, spec PauseSpec) error
    Resume(ctx context.Context, id SessionID, token string, input any) error
    List(ctx context.Context, filter Filter) ([]SessionInfo, error)
    // ... etc; Python-ADK-schema-compatible where possible
}
```

Built-in: `eventlog.SQLite` (v0.1), `eventlog.Postgres` (v0.2). Third-party stores implement `session.Store` and pass via config:

```go
mast.Run(ctx, mast.Config{
    SessionStore: &mySpannerStore{...},
    // ...
})
```

Storage schema stays Python-ADK-schema-readable per [`./durable-execution-design.md`](./durable-execution-design.md) cross-runtime constraints.

### Permission plugin

The permission gate is composable — the built-in path-scope + URL-scope checks compose with custom checks:

```go
// github.com/go-steer/mast/permission
type Checker interface {
    Check(ctx context.Context, action Action) Decision
}

gate := permission.NewGate(
    permission.PathScope(allowedPaths),
    permission.URLScope(allowedHosts),
    &myTenantIsolationChecker{tenant: tenantID},
    &myComplianceChecker{policy: policy},
)

mast.Run(ctx, mast.Config{PermissionGate: gate})
```

Checkers run in order; first `Deny` short-circuits; all `Allow` (or no explicit decision) means allow. This mirrors network policy engines.

### Tool

Third parties register custom built-in tools:

```go
// github.com/go-steer/mast/tool
type Tool interface {
    Name() string
    Description() string
    Schema() Schema
    Call(ctx context.Context, args any) (Result, error)
}

tool.Register(&myCustomTool{...})
```

Custom tools compose with the tool-catalog allowlist in workload bundles — a tool that isn't registered can't appear in an allowlist.

### Specialist source

Specialists don't have to come from files:

```go
// github.com/go-steer/mast/specialist
type Source interface {
    Load(ctx context.Context) ([]Spec, error)
    Watch(ctx context.Context) (<-chan []Spec, error)  // v0.2: hot-reload
}

registry := specialist.NewRegistry(
    specialist.DirectorySource(".agents/specialists"),      // built-in
    &myK8sConfigMapSource{namespace: "ops", name: "specialists"},  // custom
)
```

Multiple sources compose; last-load-wins on name conflict (with a warning log).

### Workload source

Same shape:

```go
// github.com/go-steer/mast/workload
type Source interface {
    Load(ctx context.Context) ([]Bundle, error)
    Watch(ctx context.Context) (<-chan []Bundle, error)
}
```

### Attach transport

The attach protocol (HTTP/SSE per `pkg/attach/` port) is the default; consumers wanting to expose mast over a different transport (gRPC, WebSockets, JSON-RPC over Unix socket) implement:

```go
// github.com/go-steer/mast/attach
type Transport interface {
    Serve(ctx context.Context, handler Handler) error
}
```

Only relevant for library consumers who need mast reachable over their organization's preferred transport. Rare; but the seam exists.

### Observability sinks

Metrics + logs + traces all export via OTel by default, but consumers can add extra sinks:

```go
// github.com/go-steer/mast/observability
observability.RegisterMetricSink(&myStatsdSink{})
observability.RegisterLogSink(&myBusinessLogSink{})
```

Sinks are additive to OTel exports, not replacements.

### A2A token validator

Per [`./a2a-design.md`](./a2a-design.md), A2A server auth uses a pluggable token validator:

```go
// github.com/go-steer/mast/a2a
type TokenValidator interface {
    Validate(ctx context.Context, token string) (Principal, error)
}

a2a.RegisterTokenValidator("google-iam", &myGoogleIAMValidator{})
```

Built-in: JWT (with configured issuer + JWKS), static bearer tokens, Google IAM (Workload Identity in-cluster), OAuth 2.0 introspection.

### Federation adapter

Per [`./federation-design.md`](./federation-design.md), custom federation adapters (for proprietary protocols, corporate agent systems, framework-specific adapters):

```go
// github.com/go-steer/mast/federation
type Adapter interface {
    Name() string                    // e.g. "a2a", "mast", "http", "grpc", or custom
    Resolve(reference string) (RemoteAgent, error)
    Invoke(ctx context.Context, ref RemoteAgent, inputs any) (Result, error)
}

federation.RegisterAdapter(&myLangGraphAdapter{})
```

Custom adapters compose with the built-ins; adapter selected by reference protocol scheme.

## Config precedence

For any given config value, precedence is:

1. **Explicit programmatic** — value passed via `mast.Config` or option func.
2. **Programmatic default** — value set on `mast.NewServer(defaults{...})` for library consumers building a wrapping API.
3. **Env var** — `MAST_*` env vars for common values (`MAST_PROVIDER_MODEL`, `MAST_SESSION_STORE`, etc.).
4. **Config file** — `mast.yaml` in `$MAST_CONFIG_DIR` (default `.mast/` in CWD).
5. **Built-in default** — the sensible-default value.

Documented per config field; consistent across all packages. Config-file schema deliberately stays small — file-loaded config is for values operators change, not for programmatic-only knobs.

## Testability

Library consumers writing tests against mast:

- **Echo provider** for deterministic input/output: `provider.Register("echo", provider.Echo)`.
- **Scripted provider** for turn-by-turn deterministic responses: `provider.Register("scripted", provider.Scripted(scriptFile))`.
- **In-memory session store** for tests: `session.NewMemoryStore()`.
- **Test hook harness**: `mast.NewTestServer(t)` returns a server with lifecycle hooks that record all events for assertion.
- **Snapshot-based golden tests**: `mast.RunGolden(t, snapshotDir, input)` — replays a captured snapshot against current code, diffs against captured output.

Test harness is `pkg/masttest/` (library-facing) rather than `internal/testing/`; consumers can use the same primitives we do.

## Library-embedded attach

Special case: a library consumer wants attach mode reachable from `mast-web` (or programmatic clients) while running inside their own process, sharing the host's HTTP server.

```go
// Register mast attach handlers on an existing mux
srv := mast.NewServer(...)
mast.RegisterAttachHandlers(hostMux, srv, "/mast")

// Now the host serves mast attach at /mast/... alongside its own routes
```

Same for metrics:

```go
mast.RegisterMetricsHandler(hostMux, "/mast/metrics")
```

Standard Go `http.Handler` shapes; no bespoke framework needed on the host side.

## Composition with other subsystems

| Subsystem | Library-API interaction |
|---|---|
| **Durable execution** | `mast.ListSessions`, `mast.GetSession`, `mast.Resume`, `mast.Abort` — full programmatic surface. Custom `session.Store` implementations plug in via extension point. |
| **Orchestration** | Programmatic `workload.Bundle` registration; custom `workload.Source` for non-file bundle sources; `mast.RunWorkload` entry point. |
| **Specialists** | Programmatic `specialist.Spec` registration; custom `specialist.Source`. |
| **Observability** | `observability.Config` in `ServerConfig`; extra metric/log/trace sinks via extension points. |
| **Attach mode** | Custom `attach.Transport`; library-embedded attach via `RegisterAttachHandlers`. |
| **Permission gate** | Composable `permission.Checker` extension point. |
| **Provider layer** | `provider.Register` for custom providers; `provider.Provider` interface stability guarantee. |
| **Tool surface** | `tool.Register` for custom built-in tools; workload bundle allowlist gate. |
| **Session store (eventlog)** | `session.Store` extension point; built-in SQLite / Postgres adapters. |
| **MCP** | Programmatic MCP server registration via `mcp.RegisterServer(cfg)`; per-request overrides via `mast.Config.MCP`. |
| **Memory** | Programmatic memory reader access (`memory.Get(ctx, key)`); write access restricted to derivation pipeline. |

## Phasing

| Version | Scope |
|---|---|
| **v0.1** | Top-level API (`mast.Run`, `mast.RunWorkload`, `mast.Serve`, `mast.ListSessions`, `mast.Resume`). All extension-point interfaces defined. All stable-marked packages present with initial API. Programmatic registration for providers / stores / permission-checkers / tools / specialists / workloads. Library-embedded attach via `RegisterAttachHandlers`. |
| **v0.2** | Hot-reload sources (`Watch()` on `specialist.Source` + `workload.Source`); postgres session store; scripted provider improvements. Extended test harness. |
| **v0.3** | Observability sink extension points fully wired; memory read API stabilized; custom attach transports. |
| **v0.4+** | Cross-runtime interop shim (mast library calling Python ADK sub-agents); plugin loading (Go plugin mechanism or WASM). |

## Open questions

1. **`mast` top-level vs. `mast/agent` split.** Should convenience functions live in the top-level `mast` package (as designed) or under `mast/agent`? Bias: top-level for the 90% path; `mast/agent` for lower-level construction. Same-import convenience matters for the "hello world" story.
2. **Error taxonomy.** Do we surface v2's error types (`ErrInvalidResumeResponse`, etc.) directly or wrap them in `mast.*` errors? Bias: wrap in `mast.*` errors that `errors.Is` correctly against the v2 originals — gives us space to add mast-specific context without leaking ADK types.
3. **Context propagation for hooks.** Should hooks receive the same `context.Context` as the request, or a derived one with hook-specific metadata? Bias: same context; hook-specific metadata via a well-known context key.
4. **Concurrency guarantees on hooks.** Are hooks called from the goroutine handling the request, or from a background goroutine? Bias: request goroutine — matches Go convention; hooks that need to be non-blocking spawn their own goroutine.
5. **Config-file discovery vs. env.** Env-first vs. file-first? Bias: file for readability + review-in-source-control; env for secrets + per-environment overrides. Precedence per section above.
6. **Deprecation cycle length.** How many minor versions between deprecation announcement and removal? Bias: 2 minor versions (announce in v0.N, remove in v0.N+2) for pre-1.0; 1 major version for post-1.0.
7. **Plugin loading (Go plugins / WASM).** Library consumers want to load specialists / tools / providers from binary artifacts at runtime. Go plugins are OS-specific + fragile; WASM is cleaner but limited. Defer to v0.4+; capture as a future direction.
8. **In-process multi-tenant.** A single mast library instance serving multiple tenants — does `WithIsolationScope` need to reach into every extension point (session store, permission gate, MCP)? Bias: yes; tenant scope is a first-class context value.

## Out of scope

- **Multi-language SDKs.** Go only. Python / TypeScript library consumers use the attach protocol as a client, not a library.
- **Auto-generated OpenAPI / gRPC surfaces.** The library API is Go-native. HTTP surface is attach mode (documented separately).
- **A REPL / interactive Go shell.** `go doc` + examples in godoc are the discovery surface.
- **A "framework" wrapping mast for opinionated Go web frameworks.** Consumers use raw handlers; framework-specific wrappers are third-party.
- **Backwards-compat shims for pre-fork core-agent API.** Mast starts fresh at v0.1; core-agent library consumers stay on core-agent.

## Related

- [`./positioning.md`](./positioning.md) — library-embedded is a pillar
- [`./fork-design.md`](./fork-design.md) — bucket 1's lean core exposes this surface
- [`./durable-execution-design.md`](./durable-execution-design.md) — session inspection + resume are library API
- [`./orchestration-design.md`](./orchestration-design.md) — programmatic workload bundle registration
- [`./specialists-design.md`](./specialists-design.md) — programmatic specialist registration
- [`./observability-design.md`](./observability-design.md) — observability config in library API
- [`./deployment-design.md`](./deployment-design.md) — library-embedded deployments (mast inside operator's own service)
- [`./config-layout-design.md`](./config-layout-design.md) — where files live when file-based config is used
- ADK v2 stable API — the underlying runtime we expose
