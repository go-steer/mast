# mast

The agent-infrastructure substrate for **unattended, library-embedded, multi-provider, durable** agent workloads — built for platform and SRE teams deploying agents into Cloud Run, Kubernetes, and their own Go services. Lean fork of [`go-steer/core-agent`](https://github.com/go-steer/core-agent), native to [ADK v2](https://google.golang.org/adk/v2).

> **Status: v0.4.0 — an operator approves the exact call that will fire, the loop runs on a schedule without an orchestrator, and every verdict becomes a labelled eval row.** v0.3 put an operator in front of every mutating call; v0.4 makes what they approve a typed object rather than a paragraph. A finding carries the proposed `(tool, arguments)`, checked against that tool's own input schema before the report is accepted, so the executor is reachable from a diagnosis through a structural rule instead of a prompt. One answer can cover a whole change set — bound to an exact call signature, single-use, durable across a restart, and voided by the cluster moving (a declared freshness re-read) rather than only by a clock. A bundle wakes itself on an interval whose anchor survives the daemon, and a missed tick is skipped, not caught up. `dispatch: bounded` spends exactly one cheap-tier call on a schema-forced report, with the step count read off the meter rather than inferred. Every adjudication — including mast's own refusals — is durable and exports as JSONL, because *"the operator edited 10 replicas down to 4"* is the highest-signal thing the system produces. A specialist declares `tier: small | mid | frontier` rather than a vendor's model id, and a nightly on two providers checks that a cheap tier is actually billed cheaply. On the v0.3.0 write gate and the v0.2.0 durable-execution spine. Docs: [go-steer.github.io/mast](https://go-steer.github.io/mast/).

## Is mast for you?

Thirty seconds of honesty before anything else:

- **You're a platform / SRE team putting agents where no human is watching** — incident triage in a Cloud Run pod, a scheduled drift monitor, runbook automation behind a webhook, an agent compiled into your own service. **You're in the right place; keep reading.**
- **You want one simple agent loop in Go — no governance, no durability, no operator surface.** Use [raw ADK v2](https://google.golang.org/adk/v2); that niche is ADK's, and mast would be overhead. Come back when the loop must survive restarts, needs budget or permission governance, needs to switch providers without code changes, or stops having a human watching it.
- **You're after an interactive coding tool for your own laptop.** That's a different product shape; mast is built for agents running in your infrastructure, not in your editor.

## The four pillars

1. **Unattended.** Runs without a human watching: workload bundles declare specialists, tool catalogs, budgets, and HITL policy; envelopes dispatch turns via webhook; watchdog + cost ceilings guard the loop; the operator surface (attach + mast-web, `mast sessions`) is for looking in, not for babysitting.
2. **Library-embedded.** A Go library first (`mast.RunWorkload(ctx, ...)` from your own service, with a CI-enforced slim-embed guarantee — pay only for what you import) and a standalone binary second (`mast serve` for Cloud Run / GKE / systemd). Same subsystems in both shapes.
3. **Multi-provider.** The same config runs Gemini or Claude (first-party or Vertex) — switch with a flag, not a rewrite. Task-class profiles pick sensible model tiers per job; budget metering prices both.
4. **Durable.** Sessions live in SQLite or Postgres via ADK's session store; HITL pauses survive `kill -9`, pod restarts, and cluster migrations, and resume where they stopped — verified, not aspirational.

Audit and governance run through all four: an append-only event log behind every session, permission gating, per-workload cost ceilings, and structured JSON logs with session correlation.

## Quick start

```bash
# Unattended daemon: workload bundle + durable sessions + operator surface
mast --workload=examples/workloads/gke-triage \
     --session-db=/var/lib/mast/sessions.db \
     --attach-listen=127.0.0.1:8484

# One-shot, same binary: task-class profile picks the model tier
mast --task=research --provider=gemini "what changed in the last deploy?"

# Operator surface
mast sessions list --session-db=/var/lib/mast/sessions.db
```

Point [mast-web](https://github.com/go-steer/mast-web) at the attach address for the browser operator UI. Full walkthroughs — unattended triage, forking a workflow starter, library embedding — live on the [docs site](https://go-steer.github.io/mast/).

## What ships in v0.1

Workflow-graph and SubAgents dispatch on ADK v2; specialists (subagent-as-tool with budgets and tool allowlists); workload bundles + `.agents/` discovery; durable HITL; budget metering with cost and turn caps; the provider adapters (Gemini built-in-tool layer, Anthropic first-party + Vertex, Vertex context caching, scripted replay); the attach operator surface (HTTP/SSE) with mast-web reachability; sessions CLI; observability (Prometheus counters + env-gated OTel trace export); the synchronous A2A v0.3 client and federation interface; forkable workflow starters; Cloud Run / GKE deploy recipes. Details: [CHANGELOG](./CHANGELOG.md) and the [design corpus](./docs/README.md).

## What `mast` is *not*

- **Not an interactive coding tool.** No LSP integration, no AST tooling, no syntax-aware diff UI — the file-editing tools exist to serve unattended workloads, not an editor. A deliberate scope decision, not a gap.
- **Not the successor to core-agent.** Sibling products with different jobs: `mast` is the platform-agent runtime; [`core-agent`](https://github.com/go-steer/core-agent) stays the experimentation + integration substrate. Both are maintained.
- **Not a framework sampler.** The interop surfaces (MCP now; A2A server, AG-UI in v0.2) exist so workloads compose with the ecosystem, not to chase every protocol.

## Related repos

| Repo | Role |
|---|---|
| [`go-steer/core-agent`](https://github.com/go-steer/core-agent) | Parent project and sibling product: the experimentation/integration substrate. Adapter packages port from here with per-file derivation headers. |
| [`go-steer/mast-web`](https://github.com/go-steer/mast-web) | Operator-facing web UI over the attach protocol (works with `mast` and any attach-mode core-agent variant). |
| [`go-steer/core-tui`](https://github.com/go-steer/core-tui) | Terminal UI for developer / experimentation workflows. Paired with core-agent, not mast. |

## Contributing

PRs against `main`; run `dev/ci/presubmits/all.sh` before pushing (CI runs the identical scripts). House rules in [`AGENTS.md`](./AGENTS.md); scope questions resolve through the [design corpus](./docs/README.md) — check the resolved-decisions table before re-proposing something settled.

> **Early-access note:** some sibling repos linked here are private during early access; those links may 404 until they open up.

## License

Apache 2.0 — see [LICENSE](./LICENSE).
