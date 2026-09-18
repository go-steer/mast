# mast

The agent-infrastructure substrate for **unattended, library-embedded, multi-provider, durable** agent workloads — built for platform and SRE teams deploying agents into Cloud Run, Kubernetes, and their own Go services. Lean fork of [`go-steer/core-agent`](https://github.com/go-steer/core-agent), native to [ADK v2](https://google.golang.org/adk/v2).

**Current release: v0.9.0.** Install, quickstarts, concepts, and the full reference live on the **[docs site](https://go-steer.github.io/mast/)**. What changed: [CHANGELOG](./CHANGELOG.md). What's next: [roadmap](https://go-steer.github.io/mast/roadmap/).

## Is mast for you?

Thirty seconds of honesty — the long version is [Why mast](https://go-steer.github.io/mast/why-mast/):

- **Yes**, if you're a platform / SRE team putting agents where no human is watching: incident triage in a Cloud Run pod, a scheduled drift monitor, runbook automation behind a webhook, an agent compiled into your own service.
- **No**, if you want one simple agent loop in Go with no governance, durability, or operator surface. Use [raw ADK v2](https://google.golang.org/adk/v2); mast would be overhead. Come back when the loop must survive restarts, needs budget or permission governance, needs to switch providers without a rewrite, or stops having a human watching it.
- **No**, if you're after an interactive coding tool for your laptop. Different product shape: mast runs agents in your infrastructure, not in your editor — no LSP integration, no AST tooling, no syntax-aware diff UI. A scope decision, not a gap.
- **Not the successor to core-agent**, either. Sibling products with different jobs: `mast` is the platform-agent runtime, [`core-agent`](https://github.com/go-steer/core-agent) stays the experimentation + integration substrate, and both are maintained.

### What installing it looks like today

mast is built to be a thing *you* install, not a service we run — that is a decision, recorded in [`docs/positioning.md`](./docs/positioning.md#who-installs-mast-answered-2026-09-11-closing-291). It is also a decision the product has not finished paying for, and the honest version before you start:

- **The install is a kustomize base**, not a chart or a module: `deploy/` plus `scripts/setup-wif.sh`. There is no `helm install`, no Terraform, no Homebrew tap, and release artifacts are not signed yet ([#342](https://github.com/go-steer/mast/issues/342)).
- **Run one replica for anything scheduled.** Extra replicas no longer duplicate the work — one instance takes a lease over the session store and drives the schedules, timed resumes and boot auto-resume, and the others log at `ERROR` that they are not — but they do not share it either: a passive replica never takes over, so a restart is what brings the cadence back ([#345](https://github.com/go-steer/mast/issues/345)).
- **One mast, one tenant.** There is no isolation scope on a workload bundle ([#344](https://github.com/go-steer/mast/issues/344)).
- **Config drift is diagnosed, not reconciled.** The ConfigMap name is stable and there is no hot reload, so an edit can land on disk and change nothing until you restart the pod. The daemon logs what it loaded and warns when the mounted files stop matching it ([#343](https://github.com/go-steer/mast/issues/343)). There is no CRD, and that one is not a gap — see the doc.

## The four pillars

1. **Unattended.** Workload bundles declare specialists, tool catalogs, budgets, and HITL policy; webhooks and schedules dispatch turns; a behavioral watchdog and cost ceilings guard the loop. The operator surface is for looking in, not for babysitting.
2. **Library-embedded.** `mast.RunWorkload(ctx, ...)` from inside your own service, with a CI-enforced slim-embed guarantee — pay only for what you import. **The governance layer is the same code the daemon runs**, not a reduced copy of it: the agent loop, the dispatch shapes, the write gate, the effect outbox, budget ceilings and the behavioral watchdog. What you do not get is the unattended plumbing *around* a turn — schedules, the monitoring cycle, notifications, auto-resume, drain and the operator listeners live in `cmd/mast`, not in `pkg/`, because a host service already has its own. You bring the trigger; mast brings the governance.
3. **Multi-provider.** Two vendors — Gemini and Claude, each first-party or on Vertex — and the claim is substitutability, not the count. A specialist declares `tier: small | mid | frontier` rather than a vendor's model id, the same bundle runs on either backend with no code change, budget metering prices the (backend, model) pair, and a judged eval corpus runs nightly against both so "it still works over there" is measured rather than asserted. A third backend is [#312](https://github.com/go-steer/mast/issues/312) — and it is the first real test of whether `tier:` survives a vendor the abstraction was not designed against.
4. **Durable.** Sessions live in SQLite or Postgres; approvals and pauses survive `kill -9`, pod restarts, and cluster migrations, then resume where they stopped — verified, not aspirational.

Audit and governance run through all four: an append-only event log behind every session, an operator gate in front of every mutating call, per-workload cost ceilings, and structured JSON logs with session correlation.

**One combination is refused at startup rather than gated**, and it is worth knowing before you write a bundle: a workload that enables the `planner:` *and* carries a `change_executor` specialist while `hitl.on_mutation` asks for approval. `invoke_specialist` runs its specialist on a runner it constructs itself, and the write gate and the effect outbox are runner plugins — so a mutating call made inside a planner dispatch would neither park nor dry-run, on a bundle that asked for both. The startup error names the specialist and the three ways out: run the same roster under `dispatch: coordinator` or `dispatch: graph`, where the gate reaches it, or set `hitl.on_mutation: apply` if those writes are genuinely meant to fire unattended (which records each dispatched mutation and stops none of them). That refusal is the design rather than containment — it closed [#235](https://github.com/go-steer/mast/issues/235) on the answer, not on a fix, because an approval comes back through the session event log and a dispatch's session dies with the tool call.

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
