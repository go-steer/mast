# AGENTS.md

Instructions for AI coding agents (Claude Code, Cursor, Codex, etc.) working in this repo. Read this in full before touching anything substantive.

This file is mirrored verbatim to [`CLAUDE.md`](./CLAUDE.md) so tools that look for either name find the same content.

---

## What this repo is — and isn't, yet

**`mast`** is the agent-infrastructure substrate for unattended, library-embedded, multi-provider workloads — the platform-agent product designed as a lean fork of [`go-steer/core-agent`](https://github.com/go-steer/core-agent).

**Right now this repo is design-only.** The code lives in `core-agent` (and the pre-fork `mast-prototype` spike repo) until the fork executes. Per the trigger condition in [`docs/fork-design.md`](./docs/fork-design.md), revised 2026-07-26: Phase 1's rebuild work (lean core + mast-native subsystems) starts immediately; only the adapter ports (P1.3) wait, on core-agent's three code cleanup milestones (*Correctness & durability*, *Security hardening*, *Substrate & API structure*) closing.

See [`docs/fork-design.md`](./docs/fork-design.md) for the rebuild-lean-core mechanics, naming, sync discipline, and resolved decisions.

**What you can do here today (Phase 1 in progress since 2026-07-26):**

- Read + improve the design corpus under [`docs/`](./docs/).
- Ship Go code for Phase-1 workstreams (lean core, bucket-3 subsystems, examples) — `cmd/mast/` + `pkg/` landed with the P1.1 bootstrap. Follow the design doc for the subsystem you touch; `docs/adk-v2-usage.md` is the substrate reference.
- Cross-reference between docs and to the sibling repos (core-agent and mast-web).

**What you cannot do here yet:**

- **Port adapter packages from core-agent (P1.3 / bucket 2).** The ports gate on core-agent's three code cleanup milestones closing (see the revised trigger in [`docs/fork-design.md`](./docs/fork-design.md)) — porting earlier means porting code core-agent is actively restructuring. Runtime changes that belong in core-agent still go to [`core-agent`](https://github.com/go-steer/core-agent) first.

---

## Reading order before changing anything

If you've never seen this project before, read in this order:

1. [`README.md`](./README.md) — the public face. Status, related repos, how to contribute pre-fork.
2. [`docs/README.md`](./docs/README.md) — index of the design corpus + reading order for the design docs.
3. [`docs/positioning.md`](./docs/positioning.md) — the thesis. What mast is, what it isn't, the kept/cut/change-shape decisions.
4. [`docs/fork-design.md`](./docs/fork-design.md) — the mechanics of the fork itself + the resolved-decisions table.
5. [`docs/specialists-design.md`](./docs/specialists-design.md) — the specialists subsystem (replacing skills).
6. [mast-web's `docs/web-design.md`](https://github.com/go-steer/mast-web/blob/main/docs/web-design.md) — the web UI design (lives in the sibling mast-web repo).

The resolved-decisions cross-reference at the bottom of [`docs/README.md`](./docs/README.md) is the quickest way to check whether something you're about to propose has already been settled.

---

## Sibling repos

`mast` does not live alone. Three related repos make up the picture:

| Repo | Role | Notes |
|---|---|---|
| [`go-steer/core-agent`](https://github.com/go-steer/core-agent) | Holds mast's code until the fork executes. Stays alive as the experimentation/integration substrate under (E). | Most runtime PRs that anticipate landing in mast should go here first. |
| [`go-steer/mast-web`](https://github.com/go-steer/mast-web) | Operator-facing web UI for mast (and any attach-mode core-agent variant). | Already initialized; ships independently of the code fork. |
| [`go-steer/core-tui`](https://github.com/go-steer/core-tui) | Terminal UI alternative for developer / experimentation workflows. | Stays paired with core-agent, not mast. |

**Under the (E) — sibling-products motivation:** mast and core-agent have *different jobs*, not just different user cohorts. Don't propose merging them; don't propose dropping core-agent. Both are maintained indefinitely.

---

## House rules

These are non-negotiable. They reflect lessons from prior incidents or hard-won design discipline.

### 1. No AI-assistant attribution on commits, PRs, or artifacts

Work in this repo is committed by its human author, regardless of which tooling helped produce it. When committing on behalf of a human, do **not** add:

- `Co-Authored-By:` lines naming any AI assistant, agent, or model — Claude, Gemini, Copilot, Codex, or anything that comes later
- "Generated with <tool>" badges, footers, emoji trailers, or tool-marketing links of any kind
- Any other marketing-style trailer in commit messages, PR titles/bodies, release notes, or docs

This applies to every agent and every artifact, not just the tools named above. Use the human's `user.email` and `user.name` (configurable via `git -c user.email=... -c user.name=...`). The change is theirs; you're the typing.

### 2. Apache 2.0 license header on every new source file

Every new `.go` / `.sh` / `.yaml` / `.js` file (when code lands) gets the 13-line header:

```
// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// ...
```

Match the syntax-comment style for the file type. The `LICENSE` file at the repo root is the canonical text.

### 3. Public names only — no internal codenames

Every name written into a committed artifact (docs, PR titles/bodies, commit messages, code, file names) must be a public name. Internal codenames — for this project, for adjacent projects, or ones you're tempted to invent — never get committed. For the adjacent interactive-IDE work referenced in this corpus, the public name is **Antigravity**; write that. If you only know something by an internal name and don't know its public one, stop and ask rather than committing it. This is a standing decision; respect it.

### 4. Docs-site pages alongside README/DESIGN changes — NOW ACTIVE

**Active as of 2026-07-26:** `docs/site/` exists. The stack is **Astro + Starlight** — core-agent's actual `docs/site` convention; this rule's earlier "Hugo" wording (and fork-design's "Hugo + Docsy" references) were stale and were corrected the same day. User-visible changes must update both:

- `README.md` and/or `docs/*.md` (design surface)
- `docs/site/src/content/docs/...` (user-facing Astro/Starlight content)

Walk both when shipping a feature. Build the site locally with `dev/tools/docs-site.sh build` (Node 22+; see `docs/site/README.md`) — CI runs the identical build via `.github/workflows/ci-docs.yml`. The site build is deliberately not a Go presubmit.

### 5. UAT / scratch files under `/tmp`, never `$HOME`

When generating temporary state for testing — session DBs, cache dirs, log files — default to `os.TempDir()/<app>/...` (or the shell equivalent). Don't litter the user's home directory with throwaway state.

### 6. Run presubmits before pushing (when they exist)

When the fork lands and `dev/ci/presubmits/*` exist, those scripts are the same scripts CI runs. Run them locally before push. Don't ship preventable red builds.

### 7. Respect DESIGN.md / fork-design.md deferrals

If a design doc explicitly *defers* a feature (e.g. "this is out of scope for v0.1" / "added at v0.5"), don't re-propose surfacing it without a concrete consumer use case. Deferrals are decisions, not invitations.

---

## How to commit + push

Default git config: use the human's name + email. **Do not** set `user.email`/`user.name` in repo config; pass them per-command:

```bash
git -c user.email="<human's email>" -c user.name="<human's name>" commit -m "..."
```

**Commit message style:** read recent commits and match. Pre-fork this repo is small; default to:

- `docs(positioning): refine the (E) sibling-products motivation`
- `docs(fork-design): add C★ phase for agent --ui flag`
- `chore: bump LICENSE year`

Body: a short paragraph or two on *why*. The diff describes *what*. Avoid trailing AI-attribution footers per rule #1.

**PR shape:**

- Title: short, imperative (`docs(...): ...`).
- Body: motivation + summary of what changed + how to verify (which for docs-only changes is "read the rendered markdown on the PR diff view").
- Stack PRs when changes build on each other; mark base branches clearly.

---

## How to contribute pre-fork

*(Historical note: this section governed the docs-only period, which ended 2026-07-26 when the P1.1 bootstrap landed under the revised trigger. Docs-PR conventions below still apply to design-doc changes.)*

Typical design-doc PR shapes:

1. **Refining a design doc** — clarifying language, adding context, capturing a resolved decision in the cross-reference table. Most common; low-friction.
2. **Adding a new design doc** — for a subsystem the existing three don't cover. Add it under `docs/<topic>-design.md`, update `docs/README.md`'s reading order + cross-reference table.
3. **Updating the trigger condition** — if events in core-agent or mast-web change what the fork depends on. Coordinate with the human; this is a strategic decision.

**What pre-fork PRs are NOT for:**

- Adding Go modules / source code. The fork executes a specific `chore: prune to lean scope (forked from go-steer/core-agent@<SHA>)` commit; pre-empting that with hand-curated Go code complicates the squash. If you have runtime-level proposals, draft them in [`core-agent`](https://github.com/go-steer/core-agent) first.
- Adding CI workflows for code. CI is set up at fork time per the established go-steer pattern (see mast-web for the template).

---

## How to contribute post-fork (active as of 2026-07-26)

Phase 1 is in progress; the repo has code. Current shape (per [`docs/fork-design.md`](./docs/fork-design.md)):

- `cmd/mast/` — the binary
- `pkg/agent/`, `pkg/providers/`, `pkg/attach/`, etc. — runtime
- `dev/ci/presubmits/` + `dev/tools/` — same convention as core-agent
- `.github/workflows/{ci,ci-docs,docs,release}.yml` — same convention as mast-web / core-agent
- `docs/site/` — Astro + Starlight mirror of core-agent's setup *(corrected 2026-07-26 — the earlier "Hugo + Docsy" reference was stale; skeleton shipped, deploy deferred until the repo is public)*

Working rules while Phase 1 is in flight: build + vet + gofmt + test green before pushing (CI runs exactly that per `.github/workflows/ci.yml`); every new source file gets the Apache header (house rule #2); one workstream per PR, stacked on `main` or the current integration branch; consult `docs/spike-findings.md` before touching graph/HITL/budget code — the resume contract and allowlist semantics there are verified behavior, not suggestions.

---

## Operational facts you should know

- **Repo visibility:** private until the fork lands + a few sanity checks pass. Don't reference URLs as if they're public.
- **Branch policy:** PRs against `main`. No long-lived feature branches pre-fork (the repo is small).
- **License:** Apache 2.0. Compatible with the parent core-agent project.
- **The `Antigravity` rule:** see house rule #3.

---

## Common foot-guns

Issues prior agents have hit that you should sidestep:

- **Editing a design doc without updating the resolved-decisions table** in `docs/README.md`. If you settle something new, capture it there too so the next agent doesn't re-litigate.
- **Cross-referencing the doc using a wrong path** — sibling docs use `./foo.md`; refs to mast-web or core-agent need full GitHub URLs (`https://github.com/go-steer/<repo>/blob/main/...`).
- **Treating mast as "the future core-agent"** — it's the *sibling*, not the *successor*. core-agent stays alive. Decisions that assume "mast replaces core-agent" are wrong.
- **Proposing a feature the design explicitly defers** — read the "Out of scope" sections in each design doc before adding anything new.
- **Modifying `LICENSE`** — don't. It's pinned at Apache 2.0; bumping the year is the only legitimate change.

---

## Where to find more context

- **Design corpus:** [`docs/`](./docs/) (this repo).
- **Web UI design + code:** [`go-steer/mast-web`](https://github.com/go-steer/mast-web).
- **Current code (pre-fork):** [`go-steer/core-agent`](https://github.com/go-steer/core-agent).
- **Sibling design docs in core-agent:** [`go-steer/core-agent/docs/`](https://github.com/go-steer/core-agent/tree/main/docs) — attach-mode, multi-session, shared-memory, context-management, etc. Most of mast's runtime contracts are documented there.

When in doubt, read the doc that the relevant decision was settled in (use the resolved-decisions table in [`docs/README.md`](./docs/README.md) as the index).
