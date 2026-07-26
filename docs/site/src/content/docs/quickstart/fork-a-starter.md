---
title: "Quickstart: fork a starter"
description: Copy a self-contained workflow starter out of examples/workflows/ and make it your own — fork-and-forget by design.
---

`examples/workflows/` holds **forkable starters**: standalone, self-contained
reference implementations of the canonical workflow shapes from the design
corpus. The lifecycle is deliberately fork-and-forget — copy the directory
out, and it's your code. No upgrade path, no compatibility promise; mast's
CI only keeps the in-repo originals building. Starters never import each
other, and duplication between them is accepted on purpose.

v0.1 ships two shapes:

## LLM-as-router

A cheap SingleTurn classifier reads the input, and the graph dispatches to
the matching Task-mode specialist — with a `Default` fallback for anything
the classifier can't place. Runs offline with deterministic in-process
fakes:

```sh
go run ./examples/workflows/llm-as-router
```

Or pipe your own tickets, one per line:

```sh
echo "I was double-charged on my last invoice" | go run ./examples/workflows/llm-as-router
```

Customization points, in the order you'll hit them: the categories table
(the whole routing domain), the two model seams (swap the fakes for e.g.
`gemini.NewModel(...)` — keep the classifier SingleTurn: one call, cheap
tier), per-specialist tools, and an optional HITL approval gate.

## Fan-out-fan-in

A planner emits N independent work items, workers execute them in parallel
under a concurrency cap, a join barrier collects results, and a summarizer
folds N results into one output. Every node is deterministic, so it runs
with no model at all:

```sh
go run ./examples/workflows/fan-out-fan-in
```

Or pipe a fleet of service names:

```sh
echo "web, api, worker" | go run ./examples/workflows/fan-out-fan-in
```

The README in the starter documents the two constraints this shape lives
under (no HITL inside parallel branches — escalation belongs after the
join — and which of the three concurrency knobs actually binds).

## How to fork one

```sh
cp -r examples/workflows/llm-as-router ~/src/my-router
cd ~/src/my-router
go mod init example.com/my-router && go mod tidy
go run .
```

Each starter's README lists its customization points and the design docs
that govern the shape. Smoke tests come along for free
(`go test ./...`).

## When you outgrow a fork

That's the tell you want the full runtime: wrap your graph in a [workload
bundle](/reference/workload-bundle/), get budgets, durable HITL, the
sessions surface, and metrics from the daemon — or embed via the
[library](/quickstart/library-embed/). More shapes (supervisor+workers,
sequential pipeline, map-reduce, adversarial verifier, autonomous loop)
land with the reference-graph library — see the [roadmap](/roadmap/).
