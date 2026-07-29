---
title: Roadmap
description: What v0.1.0-pre ships, what's gated on the adapter ports, and what lands in v0.2 — honestly.
---

mast is at **v0.1.0-pre**. Ten of the eleven v0.1 exit criteria from the
fork design are green — the `--task` profile criterion cleared with the
P1.3a/P1.3b adapter ports (a live-credential provider smoke remains on the
checklist); this page is the honest account of the rest.

## Stability, precisely

Semver stability from v0.1 is reserved for the **five packages the four
pillars stand on**: the root `mast` package, `agent`, `session`, and the
`provider` and `tool` interfaces. Everything else is explicitly
**experimental** — API may change without a deprecation cycle until the
version named in the
[library API design's import-surface table](https://github.com/go-steer/mast/blob/main/docs/library-api-design.md).

## Shipped in v0.1.0-pre

Workflow-graph and SubAgents dispatch on ADK v2.1.0; durable HITL surviving
process death; budget metering with cost + turn caps; the full GKE
triage roster; `.agents/` discovery; the sessions operator surface (CLI +
HTTP); observability v0.1 (seven fixed counter families + env-gated OTel
trace export); the synchronous A2A v0.3 client, `federation.Adapter`, and
the `invoke_remote_agent` planner tool; the planner scaffold; two forkable
workflow starters; the CI-enforced slim-embed guarantee; deploy starters
including Cloud Run with Postgres sessions; and the top-level `mast`
library API.

## Gated on the P1.3 adapter ports

The staged ports are landing as core-agent's cleanup milestones close (all
four closed 2026-07-28; the rule is *don't port moving code* — the revised
trigger in
[`docs/fork-design.md`](https://github.com/go-steer/mast/blob/main/docs/fork-design.md)):

- **`--task` profiles — shipped.** P1.3a landed the task-class,
  permission, pricing, and model-tier packages; P1.3b landed the provider
  adapters (Anthropic first-party + Vertex, the Gemini builtin-tool layer,
  Vertex context caching, scripted replay) and the watchdog. One-shot
  `--task` runs now take `echo`, `scripted`, `gemini-*`, or `claude-*`
  models.
- **Attach mode + mast-web reachability — still gated.** The attach
  HTTP/SSE transport ports as P1.3c once core-agent's attach surface stops
  moving (it kept absorbing fixes after the milestones closed), and with
  it the [mast-web](https://github.com/go-steer/mast-web) operator UI.

## v0.2

Per the 2026-07-25 scope re-cut and the per-subsystem design docs:

- **A2A server** — expose workloads as skills to registries (Google Agent
  Registry, kagent); v0.1 ships the client only.
- **AG-UI** (server + client) — CopilotKit apps and chat-platform bots;
  waits on the interrupt-lifecycle spec extension stabilizing.
- **Per-specialist cost attribution** — branch/node-attributed cost on top
  of the v0.1 workload-level counters.
- **Planner shapes** — the `run_shape_*` vocabulary tools wired to the
  reference-graph library (they return `not_implemented` in the v0.1
  scaffold), plus more starters: supervisor+workers, sequential pipeline,
  map-reduce, adversarial verifier, autonomous loop.
- **Programmatic pause / resume tokens** — today's resume surface is
  HITL-interrupt-keyed.
- **Multi-session substrate** — `mode: multi_session` bundles honored.

## Further out (v0.3+)

Shared memory + audit-derived memory, multi-tenant isolation scopes, MCP
credential resolution, full mast-native federation, bundle learning.

## The design corpus

Every claim above traces to a design doc in the repo —
[`docs/README.md`](https://github.com/go-steer/mast/blob/main/docs/README.md)
is the index and carries the resolved-decisions table. Start with
[`positioning.md`](https://github.com/go-steer/mast/blob/main/docs/positioning.md)
(the thesis) and
[`fork-design.md`](https://github.com/go-steer/mast/blob/main/docs/fork-design.md)
(the plan this roadmap is cut from).
