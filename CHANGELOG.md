# Changelog

## Unreleased

- (nothing yet)

## v0.1.0-pre (2026-07-26)

Phase-1 pre-release: nine of fork-design's eleven v0.1 exit criteria are
green; `--task` profiles and attach-mode reachability remain gated on the
P1.3 adapter ports per the revised trigger. Highlights below; details in
the per-item entries that follow.

- Workflow-graph and SubAgents dispatch on ADK v2.1.0; durable HITL
  surviving process death; budget metering with cost + turn caps; the
  full 13-specialist GKE triage roster; `.agents/` config discovery;
  sessions operator surface (CLI + HTTP); observability v0.1; A2A v0.3
  client + federation adapter + `invoke_remote_agent`; planner scaffold;
  forkable workflow starters; the slim-embed guarantee with CI
  enforcement; presubmits-as-CI; deploy starters incl. Cloud Run with
  Postgres session store; top-level `mast` library API.

- **P1.4 interop slice: A2A client + federation surface (2026-07-26).** The
  v0.1 slice of the 2026-07-25 re-cut ([`docs/fork-design.md`](./docs/fork-design.md)
  P1.4): `pkg/federation` with the frozen `Adapter`/`Handle` interface
  (chosen so v0.2 streaming/HITL propagation is additive, not breaking —
  rationale in the package doc), `a2a://<name>/<skill>` reference parsing, a
  scheme-keyed adapter registry, and the planner tool
  `invoke_remote_agent(reference, inputs)`; `pkg/a2a` with the synchronous
  A2A v0.3 client (agent-card fetch + cache, JSON-RPC 2.0 `message/send`
  with `A2A-Version` header and bearer auth from env-var references,
  direct-message and task-opened replies, bounded `tasks/get` polling,
  `tasks/cancel` on cancellation/timeout) and static `.agents/a2a/*.yaml`
  registrations wired into `pkg/config` root scanning. Fork-design exit
  criterion 9 is covered by an httptest round-trip through
  `invoke_remote_agent`. Server, streaming, registry discovery: v0.2 per
  [`docs/a2a-design.md`](./docs/a2a-design.md) phasing.

- **P1.1 bootstrap (2026-07-26).** The repo grows code: the spike-validated
  prototype graduates from the standalone `mast-prototype` repo
  (tags `spike1`/`spike2`; verified findings in
  [`docs/spike-findings.md`](./docs/spike-findings.md)) — workload-bundle +
  specialists loaders, workflow-graph and SubAgents dispatch shapes, durable
  HITL on ADK's SQLite session service, budget metering, per-specialist MCP
  tool allowlists, GKE MCP wiring, inject/resume HTTP endpoints, and the GKE
  triage example workload. Pinned to `google.golang.org/adk/v2 v2.1.0`.
  Provenance per [`docs/fork-design.md`](./docs/fork-design.md): attribution
  is by reference, not git history — prototype history remains in
  `mast-prototype`.
