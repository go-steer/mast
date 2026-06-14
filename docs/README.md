# mast: design docs

Design documentation for `mast` — the agent-infrastructure substrate for unattended, library-embedded, multi-provider workloads. These docs describe the project as it's *currently designed*; the code lives in [`core-agent`](https://github.com/go-steer/core-agent) until the fork executes (per [`./fork-design.md`](./fork-design.md)'s trigger conditions).

## Reading order for someone landing cold

1. **[`./positioning.md`](./positioning.md)** — the thesis. What `mast` is, what it isn't, what gets kept / cut / reshaped from core-agent's surface. Strategy, not implementation.
2. **[`./fork-design.md`](./fork-design.md)** — the mechanics. How the fork actually happens: phasing, trigger conditions, sync discipline under (E)-sibling-products motivation, resolved decisions.
3. **[mast-web's `web-design.md`](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md)** — the operator-facing UI. Why web (not terminal TUI); what we reuse from cogo-wasm2 and what we don't; stack decisions; deployment options.
4. **[`./specialists-design.md`](./specialists-design.md)** — the subagent-as-tool subsystem replacing core-agent's skills. Schema, loader shape, composition with existing patterns.

Each doc has a Resolved-decisions section at the bottom listing what's been settled in conversation; the rest is open for discussion.

## Status (2026-06-13)

This repo holds the design corpus for the future `mast` project. The thesis (E) — *sibling products with divergent agendas* — is the current resolved framing: mast = platform-agent product; core-agent = experimentation/integration substrate (cogo-shaped consumers). Both maintained indefinitely.

Current state:

| Repo | Status |
|---|---|
| [`go-steer/mast`](https://github.com/go-steer/mast) (this repo) | Design-corpus-only, pre-fork. Docs land here as drafts evolve. |
| [`go-steer/mast-web`](https://github.com/go-steer/mast-web) | Initialized 2026-06-12 with main scaffolding + four stacked PRs (A/B/C/C+/docs). Operator UI ships independently of the code fork. |
| [`go-steer/core-agent`](https://github.com/go-steer/core-agent) | Holds mast's code until the fork executes. Continues as the experimentation/integration substrate under (E). |

## Fork trigger

Per [`./fork-design.md`](./fork-design.md), the code fork executes after these in-flight items land in core-agent:

1. Issues [#158-#161](https://github.com/go-steer/core-agent/issues?q=is%3Aissue+158+OR+159+OR+160+OR+161) (bash search-gate, watchdog→model routing, `--task=debug` profile extensions, gemini-3.5-flash probe).
2. The shared-memory stack (PRs #13/14/15 against core-agent).

When both are done, phase 1 of the fork begins (hard-fork-then-prune, single squash commit, per the design doc).

## Resolved decisions cross-reference

A consolidated view (each doc's local resolved section is authoritative for its own scope):

| Decision | Where it's resolved |
|---|---|
| Strategic motivation = (E) sibling products | [`./fork-design.md`](./fork-design.md) |
| Project name = `mast`, repo = `github.com/go-steer/mast` | [`./fork-design.md`](./fork-design.md) |
| ADK dependency stays | [`./fork-design.md`](./fork-design.md) |
| In-flight work lands in core-agent first | [`./fork-design.md`](./fork-design.md) |
| Phase 1 trigger = after #158-#161 + shared-memory stack | [`./fork-design.md`](./fork-design.md) |
| CI/release infra = independent at start | [`./fork-design.md`](./fork-design.md) |
| Interactive UI = web, not terminal | [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) + [`./positioning.md`](./positioning.md) |
| Skills replaced by specialists subsystem | [`./specialists-design.md`](./specialists-design.md) + [`./fork-design.md`](./fork-design.md) |
| Web UI = thin client over attach mode, not WASM-as-agent | [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) |
| `mast-web` phases A+B+C don't gate on the fork trigger | [mast-web's web-design.md](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) |
| Docs split: mast-design here, mast-web design in mast-web repo | This README + redirect stubs in core-agent's `docs/mast/` |

## Open questions still on the table

Each doc has its own open-questions section. The strategic ones (cross-doc impact):

- **AX boundary audit** ([`./positioning.md`](./positioning.md), [`./fork-design.md`](./fork-design.md)). Some of what core-agent does today (background agents? cross-process inbox?) may belong up at AX. Needs a dedicated audit.
- **core-agent's own positioning rewrite** ([`./fork-design.md`](./fork-design.md)). Under (E), core-agent needs its own positioning doc — sibling to [`./positioning.md`](./positioning.md) — written for its actual audience (experimentation/integration/cogo).
- **Small-tier-parent classifier aging** ([`./positioning.md`](./positioning.md)). As `gemini-3.5-flash` lands and `gemini-3.5-pro` GAs, the substring matcher needs revisiting (filed core-agent issue #161 begins this).
- **Canonical positioning name** ([`./positioning.md`](./positioning.md)). "Agent infrastructure" / "platform agent runtime" / "agent substrate" — pick one and use consistently in the README sweep.

The doc-local open questions (specialist-API details for `agenttool`, sync discipline edge cases, etc.) live in each doc and don't affect the others.
