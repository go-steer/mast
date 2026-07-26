# Slim embed — reference consumer

This is the reference consumer for the **slim-embed guarantee**
([`docs/library-api-design.md`](../../../docs/library-api-design.md),
"Slim-embed guarantee"): a single-file host service embedding exactly
one mast control loop in-process — a SingleTurn classifier feeding one
Task specialist over in-memory sessions — and nothing else.

```
host process
  └─ ADK runner
       └─ slim_triage (workflow graph root)
            START → classify (SingleTurn) → run_triage (Task specialist)
```

What matters here is what this program **imports**, not what it
serves. The loop runs over hardcoded sample inputs because the point
is the dependency graph, not the transport.

## Run it

```sh
go run ./examples/deploy/slim
```

No credentials, no network: the loop runs on mast's offline echo
model. Swapping in a real model is one ADK import
(`google.golang.org/adk/v2/model/gemini`) — see the comment in
`main.go`.

## Pay for what you import

The slim slice this example pulls:

| Import | What it buys |
|---|---|
| `pkg/agent` | Task / SingleTurn / Chat constructors over ADK's `llmagent` |
| `pkg/specialists` | Programmatic specialist `Spec`s + `Build` (file loading via `LoadDir` optional) |
| `pkg/budget` | In-process usage meter + cost ceilings (optional) |
| ADK v2 (`runner`, `session`, `workflow`, …) + stdlib | The loop itself |

What it must **not** pull — enforced in CI by
[`scripts/check-slim-deps.sh`](../../../scripts/check-slim-deps.sh),
which fails any PR whose changes grow this example's transitive
dependency graph:

- `pkg/inject` (HTTP inject/resume/abort server)
- `pkg/observability` (Prometheus registry, OTel SDK wiring) — and with
  it `github.com/prometheus/...`, the OTel SDK, and the OTLP exporters
- `pkg/mcp` — and with it `github.com/modelcontextprotocol/go-sdk`
- `pkg/graph`, `pkg/router` (workload dispatch shapes)
- `pkg/config` (`.agents/` discovery)

One honest caveat: the OTel **API** (`go.opentelemetry.io/otel`,
`/trace`, `/metric`) is in the graph regardless, because ADK v2's
model path (`google.golang.org/genai`) imports it directly. Those are
no-op stubs unless an SDK is installed; the SDK and exporters stay
out. The CI check denylists the heavy parts and documents this.

## The upgrade path is additive

Each subsystem you later need is one import plus a config value — not
a migration:

- **Durability:** swap `session.InMemoryService()` for ADK's
  `session/database` service over SQLite in the same
  `runner.Config` field.
- **Budgets:** already here — `pkg/budget` is part of the slim slice.
- **Metrics / traces:** import `pkg/observability`, register the
  fixed metric families, observe the same runner events this loop
  already iterates.
- **HITL pauses:** emit `RequestInput` interrupts from the workflow
  nodes (see `pkg/graph` for the full pattern) and feed operator
  verdicts back through the runner.
- **An operator surface:** import `pkg/inject` and mount its
  inject/resume/abort endpoints in your existing HTTP mux.
- **MCP tools:** import `pkg/mcp` and add toolsets to the
  specialists' `BuildOptions`.

That is the strategic point of the guarantee: slim consumers start
*inside* mast and grow by adding imports, not by migrating off a
side-car framework. If you don't need any of mast's governance or
durability and never will, use raw ADK v2 — see the routing note in
[`docs/positioning.md`](../../../docs/positioning.md), "Smaller
agents, slim embeds".
