---
title: "Quickstart: embed the library"
description: Two embedding paths — the batteries-included root package, or the slim pkg-level slice with the pay-for-what-you-import guarantee.
---

mast ships as a Go library with **two embedding paths**. Pick by dependency
appetite, not by feature list — both run the same subsystems.

```sh
go get github.com/go-steer/mast@v0.1.0-pre
```

## Path 1: the batteries-included root package

`github.com/go-steer/mast` (the module root) is the 90% path and one of the
five stable-from-v0.1 packages. The v0.1 surface is deliberately minimal:
`Config`, `Result`, `Run`, `RunWorkload`, `ListSessions`, `ResumeSession`.

The "hello world" — one agent, one turn:

```go
package main

import (
	"context"
	"fmt"

	"github.com/go-steer/mast"
)

func main() {
	res, err := mast.Run(context.Background(),
		mast.Config{ModelName: "echo"}, // "echo" = offline fake; or "gemini-2.5-flash"
		"You acknowledge incidents briefly.",
		"pod web-1 is in CrashLoopBackOff")
	if err != nil {
		panic(err)
	}
	fmt.Println(res.Output, res.Usage.CostUSD)
}
```

A full workload — programmatic bundle registration, no filesystem, no
`.agents/` directory. The dispatch shape is chosen from the roster
automatically (planner / workflow-graph router / SubAgents coordinator):

```go
res, err := mast.RunWorkload(ctx, mast.Config{ModelName: "echo"},
	workload.Bundle{Name: "triage", Specialists: []string{"classify", "_fallback"}},
	[]specialists.Spec{
		{Name: "classify", Mode: specialists.ModeSingleTurn, Instruction: "..."},
		{Name: "_fallback", Mode: specialists.ModeTask, Instruction: "..."},
	},
	`{"reason":"CrashLoopBackOff"}`)
```

Durability is one config field: pass a shared ADK `session/database`
service (SQLite or Postgres) as `Config.Sessions` and you get durable
pause/resume — `mast.ListSessions` to find pending interrupts,
`mast.ResumeSession` to feed the operator verdict back. Budgets come from
the bundle's budget block, or override with `Config.Budget`. A session DB
written by an embedded runtime reads identically through `mast sessions`.

## Path 2: the slim slice

The root package imports the dispatch subsystems, several of which are
denylisted for the **slim-embed guarantee** ("pay for what you import" — a
tested v0.1 promise, enforced in CI by `scripts/check-slim-deps.sh`). If
your host service needs the minimal dependency graph, do **not** import the
root package; compose the slim slice directly:

| Import | What it buys |
|---|---|
| `pkg/agent` | Task / SingleTurn / Chat constructors over ADK's `llmagent` |
| `pkg/specialists` | Programmatic specialist `Spec`s + `Build` |
| `pkg/budget` | In-process usage meter + cost ceilings (optional) |
| ADK v2 (`runner`, `session`, `workflow`, …) | The loop itself |

What stays out of your binary: the HTTP inject server, the Prometheus
registry and OTel SDK wiring, the MCP SDK, the workload dispatch shapes,
and `.agents/` discovery.

The reference consumer is a complete single-file host service — a
SingleTurn classifier feeding one Task specialist over in-memory sessions:

```sh
go run ./examples/deploy/slim
```

Read `examples/deploy/slim/README.md` for the import walkthrough and the
additive upgrade path: durability, metrics, HITL, an operator surface, and
MCP tools are each **one import plus a config value** later — not a
migration.

## Which path?

- Want an agent capability inside a service this afternoon → the root
  package.
- Auditing every transitive dependency, or embedding into something
  size-sensitive → the slim slice.
- Don't need governance or durability and never will → use raw ADK v2
  directly (really — see the routing on the [landing page](/)).
