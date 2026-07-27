# mast

The agent-infrastructure substrate for **unattended, library-embedded, multi-provider** workloads. Lean fork of [`go-steer/core-agent`](https://github.com/go-steer/core-agent).

> **Status: Phase 1 in progress (since 2026-07-26).** The design corpus lives under [`docs/`](./docs/README.md); the code rebuild has begun — the spike-validated prototype graduated in P1.1 (`cmd/mast/`, `pkg/`, GKE triage example, CI). Adapter ports from [`core-agent`](https://github.com/go-steer/core-agent) land when its code-cleanup milestones close (revised trigger in [`docs/fork-design.md`](./docs/fork-design.md)).

## What `mast` is

- **Headless / unattended.** Runs as Cloud Run pods, Kubernetes services, scheduled monitors, daemons behind attach sockets.
- **Library-embeddable.** A Go library you compile into your own service, not just a CLI.
- **Multi-provider.** Same config, Claude or Gemini — switch without code changes.
- **Audit + governance first.** Session DB, event log, permission gate, cost ceilings as first-class citizens.

## What `mast` is *not*

- **Not a Claude Code competitor.** Developer-laptop interactive coding is downstream of model + IDE + training investment we can't match. Use Claude Code, Antigravity, or Cursor for that shape.
- **Not a one-tool-for-everything.** Sibling to [`core-agent`](https://github.com/go-steer/core-agent) under the (E) — sibling products with divergent agendas — motivation: `mast` targets platform-agent runtime; `core-agent` stays the experimentation + integration substrate for embedded consumers.

## Related repos

| Repo | Role |
|---|---|
| [`go-steer/core-agent`](https://github.com/go-steer/core-agent) | Parent project. Until the fork executes, `mast`'s code lives there. Stays alive as the experimentation/integration substrate. |
| [`go-steer/mast-web`](https://github.com/go-steer/mast-web) | Operator-facing web UI for `mast` (and any attach-mode core-agent variant). Already initialized; ships independently. |
| [`go-steer/core-tui`](https://github.com/go-steer/core-tui) | Terminal UI alternative for developer / experimentation workflows. Stays paired with core-agent, not mast. |

## Contributing

Phase 1 is in flight: this repo takes both design-doc PRs and Go code PRs for Phase-1 workstreams (see [`AGENTS.md`](./AGENTS.md) for house rules and [`docs/fork-design.md`](./docs/fork-design.md) for what's in scope). Adapter ports from core-agent (P1.3) remain gated on its cleanup milestones — runtime changes that belong there still go there first.

> **Early-access note:** the sibling repos linked here are private during early access; those links may 404 until they open up.

## License

Apache 2.0 — see [LICENSE](./LICENSE).
