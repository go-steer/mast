---
title: Roadmap
description: What v0.1.1 ships, and what lands in v0.2 — honestly.
---

mast is at **v0.1.1**. **All eleven v0.1 exit criteria from the fork
design are green.** The `--task` profile criterion cleared with the
P1.3a/P1.3b adapter ports and was verified against live endpoints on
2026-07-29 (gemini one-shot with grounded search on the tier defaults,
Claude on Vertex completing the Task-mode tool loop, sessions durable
across runs and providers). The final criterion — attach-mode
reachability from mast-web — cleared the same day with the P1.3c port:
the real mast-web SPA, served in proxy mode against a live
`mast serve --attach-listen` daemon, connected, listed sessions, and
round-tripped a prompt through a real turn over SSE. Next stop:
v0.1.0.

## Stability, precisely

Semver stability from v0.1 is reserved for the **five packages the four
pillars stand on**: the root `mast` package, `agent`, `transcript` (the
session operator surface — named `session` pre-v0.1.0, renamed to avoid
colliding with ADK's own `session` package), and the `provider` and
`tool` interfaces. Everything else is explicitly
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
- **Attach mode + mast-web reachability — shipped.** P1.3c ported the
  attach HTTP/SSE transport (protocol v1.4.0: session listing, seq'd
  replay + live tail, inject/wake/interrupt, capabilities frames, agent
  card) plus `pkg/auth` and the eventlog overlay, pinned at
  `core-agent@25d8531c`. `mast serve --attach-listen` binds the surface
  (requires `--session-db`; bearer auth via `MAST_ATTACH_TOKEN`;
  loopback-only without auth), and the
  [mast-web](https://github.com/go-steer/mast-web) operator UI connects
  to it — verified end-to-end in a real browser session. Attach runs
  single-user in v0.1: multi-session auth, the session ACL store, and
  operator session creation (`POST /sessions`) are v0.2 work.

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
