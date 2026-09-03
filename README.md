# mast

The agent-infrastructure substrate for **unattended, library-embedded, multi-provider, durable** agent workloads — built for platform and SRE teams deploying agents into Cloud Run, Kubernetes, and their own Go services. Lean fork of [`go-steer/core-agent`](https://github.com/go-steer/core-agent), native to [ADK v2](https://google.golang.org/adk/v2).

**Current release: v0.6.0.** Install, quickstarts, concepts, and the full reference live on the **[docs site](https://go-steer.github.io/mast/)**. What changed: [CHANGELOG](./CHANGELOG.md). What's next: [roadmap](https://go-steer.github.io/mast/roadmap/).

## Is mast for you?

Thirty seconds of honesty — the long version is [Why mast](https://go-steer.github.io/mast/why-mast/):

- **Yes**, if you're a platform / SRE team putting agents where no human is watching: incident triage in a Cloud Run pod, a scheduled drift monitor, runbook automation behind a webhook, an agent compiled into your own service.
- **No**, if you want one simple agent loop in Go with no governance, durability, or operator surface. Use [raw ADK v2](https://google.golang.org/adk/v2); mast would be overhead. Come back when the loop must survive restarts, needs budget or permission governance, needs to switch providers without a rewrite, or stops having a human watching it.
- **No**, if you're after an interactive coding tool for your laptop. Different product shape: mast runs agents in your infrastructure, not in your editor — no LSP integration, no AST tooling, no syntax-aware diff UI. A scope decision, not a gap.
- **Not the successor to core-agent**, either. Sibling products with different jobs: `mast` is the platform-agent runtime, [`core-agent`](https://github.com/go-steer/core-agent) stays the experimentation + integration substrate, and both are maintained.

## The four pillars

1. **Unattended.** Workload bundles declare specialists, tool catalogs, budgets, and HITL policy; webhooks and schedules dispatch turns; a behavioral watchdog and cost ceilings guard the loop. The operator surface is for looking in, not for babysitting.
2. **Library-embedded.** `mast.RunWorkload(ctx, ...)` from inside your own service, with a CI-enforced slim-embed guarantee — pay only for what you import. Same subsystems as a standalone daemon.
3. **Multi-provider.** The same config runs Gemini or Claude (first-party or Vertex). A specialist declares `tier: small | mid | frontier` rather than a vendor's model id, and budget metering prices every provider.
4. **Durable.** Sessions live in SQLite or Postgres; approvals and pauses survive `kill -9`, pod restarts, and cluster migrations, then resume where they stopped — verified, not aspirational.

Audit and governance run through all four: an append-only event log behind every session, an operator gate in front of every mutating call, per-workload cost ceilings, and structured JSON logs with session correlation.

## Quick start

[Install](https://go-steer.github.io/mast/install/) the binary, then give it a provider — an API key or a Vertex project:

```bash
# Gemini API key
export GEMINI_API_KEY=...

# ...or Gemini on Vertex AI, on the service account's own credentials
export GOOGLE_CLOUD_PROJECT=my-project
export GOOGLE_CLOUD_LOCATION=global   # and run with --provider=vertex
```

```bash
# Unattended daemon: workload bundle + durable sessions + operator surface
mast --workload=examples/workloads/gke-triage \
     --provider=gemini \
     --session-db=/var/lib/mast/sessions.db \
     --attach-listen=127.0.0.1:8484

# One-shot, same binary: a task-class profile picks the model tier
mast --task=research --provider=gemini "what changed in the last deploy?"

# Operator surface
mast sessions list --session-db=/var/lib/mast/sessions.db
```

Claude is the same shape: `--provider=anthropic` with `ANTHROPIC_API_KEY`, or `--provider=anthropic-vertex` on the Vertex variables above. `--provider` alone picks the tier's model; `--model` pins a specific one; the `-vertex` half of a pair picks the backend. With neither, mast runs its built-in `echo` model — which is how the [unattended triage](https://go-steer.github.io/mast/quickstart/unattended-triage/) quickstart walks the whole inject → approve → `kill -9` → resume loop with no credentials and no network.

Point [mast-web](https://github.com/go-steer/mast-web) at the attach address for the browser operator UI. More walkthroughs: [library embedding](https://go-steer.github.io/mast/quickstart/library-embed/), [forking a starter](https://go-steer.github.io/mast/quickstart/fork-a-starter/).

## Related repos

| Repo | Role |
|---|---|
| [`go-steer/core-agent`](https://github.com/go-steer/core-agent) | Parent project and sibling product: the experimentation/integration substrate. Adapter packages port from here with per-file derivation headers. |
| [`go-steer/mast-web`](https://github.com/go-steer/mast-web) | Operator-facing web UI over the attach protocol (works with `mast` and any attach-mode core-agent variant). |
| [`go-steer/core-tui`](https://github.com/go-steer/core-tui) | Terminal UI for developer / experimentation workflows. Paired with core-agent, not mast. |

## Contributing

PRs against `main`; run `dev/ci/presubmits/all.sh` before pushing (CI runs the identical scripts). Architecture map: [`DESIGN.md`](./DESIGN.md). House rules: [`AGENTS.md`](./AGENTS.md). Scope questions resolve through the [design corpus](./docs/README.md) — check the resolved-decisions table before re-proposing something settled.

> **Early-access note:** some sibling repos linked here are private during early access; those links may 404 until they open up.

## License

Apache 2.0 — see [LICENSE](./LICENSE).
