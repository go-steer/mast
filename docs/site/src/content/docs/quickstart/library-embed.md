---
title: "Quickstart: embed the library"
description: Two embedding paths — the batteries-included root package, or the slim pkg-level slice with the pay-for-what-you-import guarantee.
---

mast ships as a Go library with **two embedding paths**. Pick by dependency
appetite: the root package is the batteries-included one, and the slim slice
trades the dispatch shapes and the heavyweight deps for a minimal import
graph — what it leaves out is listed under [path 2](#path-2-the-slim-slice).

Either way you are embedding the **governance layer**, which is the same
code the daemon runs: the agent loop, the write gate, the effect outbox,
budget ceilings and the behavioral watchdog. What you are not embedding is
what starts a turn when nobody calls — schedules, the monitoring cycle,
notify, auto-resume, drain and the operator listeners are `cmd/mast`, on the
assumption that your service already has a trigger and an HTTP surface. Two
seams inside the gate are also narrower here, and both fail closed: without
the daemon's tool-schema resolver a proposed change is refused rather than
validated, and a change set declaring a freshness precondition mints no
grant, so its calls park one at a time.

```sh
go get github.com/go-steer/mast@latest
```

## Path 1: the batteries-included root package

`github.com/go-steer/mast` (the module root) is the 90% path and the first
of the paths [v1.0 promises](/reference/stability/). The surface is
deliberately minimal:
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
`mast.ResumeSession` to feed the operator verdict back. A token-keyed resume
(`mast.ResumeByToken`) records who spent the token as whatever name you put
on the context with `mast.WithActor`, and records `library ResumeByToken`
when there isn't one. Budgets come from
the bundle's budget block, or override with `Config.Budget`. A session DB
written by an embedded runtime reads identically through `mast sessions`.

The [watchdog](/concepts/interop/#where-the-posture-comes-from) runs here
too, and it is armed by default: a turn that loops on the same tool call
gets a `feedback` observation, and a bundle declaring
`safety.watchdog: enforce` has the runaway turn abandoned with an error
`mast.IsWatchdogHalt` recognizes. What a library call cannot hold, it does
not pretend to — there is no cross-call session state for the "refuse
every later turn" half of `enforce`, and no next turn for a `feedback`
observation to be injected into, so both rungs act within the turn they
fire in.

Which signals run is mast's choice, not the embedder's: the watchdog is
internal, and so is the shipped-but-not-default `dominant-tool-call`
density detector. That changed with
[#301](https://github.com/go-steer/mast/issues/301), which moved every
runtime package out of reach; before it, an embedder composing its own
runner could hand the watchdog a signal list. If your workload needs a
different set — a polling loop that should not trip the cycle detector,
say — open an issue. See [the signals](/concepts/interop/#the-signals).

## Path 2: the slim slice

The root package imports the dispatch subsystems, several of which are
denylisted for the **slim-embed guarantee** ("pay for what you import" — a
tested v0.1 promise, enforced in CI by `scripts/check-slim-deps.sh`). If
your host service needs the minimal dependency graph, do **not** import the
root package; compose the slim slice directly:

| Import | What it buys |
|---|---|
| `pkg/specialists` | Programmatic specialist `Spec`s, and `Build`, which turns one into a governed ADK agent |
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

Read `examples/deploy/slim/README.md` for the import walkthrough and
what grows from there. Durability and MCP tools are a config value
later; metrics, an operator surface, schedules and the rest of the
daemon are not importable — they live under `internal/` — so when you
need them, the path is the `mast` binary rather than reassembling it
from parts.

## Which path?

- Want an agent capability inside a service this afternoon → the root
  package.
- Auditing every transitive dependency, or embedding into something
  size-sensitive → the slim slice.
- Don't need governance or durability and never will → use raw ADK v2
  directly (really — see the routing on the [landing page](/)).
