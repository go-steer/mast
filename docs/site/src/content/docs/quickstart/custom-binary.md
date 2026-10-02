---
title: "Quickstart: build your own mast"
description: A main.go that is the mast binary plus your own models and tools — same flags, same servers, same governance.
---

The `mast` binary is one line of Go over the `github.com/go-steer/mast/cli`
package:

```go
func main() { os.Exit(cli.Main(context.Background(), os.Args[1:])) }
```

Your own binary is the same line with options. What you get is the
whole binary — serve mode, one-shot mode, `mast sessions`, `mast stop`,
every flag and exit code in the [CLI reference](/reference/cli/), every
listener — plus the models and tools you add. You don't rebuild the
daemon out of parts, and nothing you add skips the governance mast's
own models and tools go through.

```sh
go get github.com/go-steer/mast@latest
```

## A main.go with a model and a tool

```go
package main

import (
	"context"
	"os"
	"strings"

	"google.golang.org/adk/v2/model"

	"github.com/go-steer/mast/cli"
	"example.com/acme/llm"     // your model client
	"example.com/acme/tickets" // your tools
)

func main() {
	os.Exit(cli.Main(context.Background(), os.Args[1:],
		cli.WithModels(func(name string) (model.LLM, bool) {
			if strings.HasPrefix(name, "acme-") {
				return llm.New(name), true
			}
			return nil, false // anything else goes to mast's own providers
		}),
		cli.WithTools("tickets", tickets.Open, tickets.Close),
	))
}
```

Build it like any Go program, then run it like `mast`:

```sh
go build -o mast-acme .
./mast-acme --model=acme-large --workload=triage
```

Every option takes [ADK](https://google.golang.org/adk)'s own types —
`model.LLM`, `tool.Tool`, `tool.Toolset` — so nothing you write against
it is a mast type beyond the option functions.

## Models

`cli.WithModels` takes a function from a model name to a model. It is
asked first, for every model name the binary meets: the `--model` value
and each specialist's `model:` in the [bundle](/reference/workload-bundle/).
Return `false` and the name goes to mast's own providers (`gemini-*`,
`claude-*`), which refuse a name they do not know, exactly as before. So
a bundle can run its coordinator on your model and one specialist on
Claude, or the other way round.

Three things to know:

- **Your model is used as you built it.** mast wraps its own providers
  in a retry for a rate-limited call. It does not wrap yours, because
  your client owns its retries.
- **Budgets meter what your model reports.** The budget meter folds the
  token usage on each response. A model that reports none can't be held
  to a cost ceiling by anything. If mast's pricing catalog doesn't know
  your model, its calls are priced at the fallback rate and counted as
  unpriced, not treated as free (see
  [budgets](/concepts/budgets/#where-the-dollar-figure-comes-from)).
- **`--model=echo` still makes everything a fake.** Under an offline
  test model the whole process is a test double, and every specialist's
  `model:` collapses to it, yours included. Offline tests of a bundle
  keep working in your binary.

## Tools

`cli.WithTools(name, tools...)` adds your tools as one toolset called
`name`. `cli.WithToolset` adds a `tool.Toolset` you already have. Either
way, a specialist reaches it the way it reaches an
[MCP server](/concepts/tools-and-mcp/), by naming it in its allowlist:

```yaml
# specialists/ticket-filer.specialist.md
---
name: ticket-filer
mode: Task
capability: change_executor
tools:
  mcp:
    - server: tickets      # the name you gave WithTools
      tools: [open_ticket]
---
```

Because it works like an MCP server, everything mast already enforces on
an allowlist applies:

- **A mistyped toolset name is refused at startup.** The name is checked
  against the workload's MCP servers plus the toolsets your binary
  supplies; an allowlist is applied by dropping what does not match, so
  a typo would otherwise grant nothing in silence.
- **A read-only specialist still enumerates.** It must list the tools it
  takes, and each has to be classified read-only in the workload's
  `tool_catalog.tools`. A read-only specialist with no `tools.mcp` at all
  would inherit every toolset, so startup refuses that too.
- **Your tools are mutating until you say otherwise.** mast classifies a
  tool it does not know as mutating, so its calls go through the
  [write gate](/reference/write-gate/) under the workload's `on_mutation`
  policy. By default that means each call parks for an operator's approval.
  Mark a tool `mutating: false` in `tool_catalog.tools` once you've
  decided it only reads.
- **Names must be distinct.** A toolset with an empty name, two toolsets
  sharing one, or a toolset named like one of the workload's MCP servers
  is refused, because an allowlist naming it couldn't say which it meant.

## Version stamping

Without any stamping, `--version` — and the version the attach
capabilities frame and the agent card report — is the mast module your
binary was built against: `mast 1.0.0` for a `go.mod` requiring
`github.com/go-steer/mast v1.0.0`. (With mast replaced by a local
directory there is no version to report, and it says `dev`.) To put
your own release identity there instead, stamp it the way mast's
release does:

```sh
go build -ldflags "\
  -X github.com/go-steer/mast/internal/version.Version=v1.2.3-acme \
  -X github.com/go-steer/mast/internal/version.Commit=$(git rev-parse --short HEAD) \
  -X github.com/go-steer/mast/internal/version.Date=$(date -u +%F)" -o mast-acme .
```

## What isn't an option yet

These aren't oversights. Each waits on something specific, and each
would be added later without breaking a `main.go` written today:

- **A session store of your own.** The daemon's durable machinery — the
  spend ledger, guardrail halts, the scheduling lease, the attach
  surface's live tail — sits on the SQL connection behind
  `--session-db`, not on ADK's session interface. A custom session
  service would quietly lose all of that. Use `--session-db` with SQLite
  or Postgres.
- **Your own authentication.** Caller identity is moving to
  [go-steer/purser](https://github.com/go-steer/purser), and an option
  can't name its types until purser reaches v1.
- **Your own runner plugins.** A plugin has to be ordered relative to
  the effect outbox and the write gate, and that needs a design, not a
  flag.

If one of these is what's stopping you, [open an
issue](https://github.com/go-steer/mast/issues) saying what you're
building. A named use case is what turns a deferred option into an
option.
