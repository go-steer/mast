# mast docs site

The end-user documentation site for mast: [Astro](https://astro.build) +
[Starlight](https://starlight.astro.build), mirroring core-agent's
`docs/site` convention. This is the **v0.1 skeleton** — landing, install,
three quickstarts, reference, roadmap; the full site is Phase-4 work per
[`docs/fork-design.md`](../fork-design.md).

This site is the *user-facing* surface. The *design* surface stays in
[`docs/*.md`](../README.md); house rule #4 in [`AGENTS.md`](../../AGENTS.md)
requires user-visible changes to walk both.

## Run it locally

THE way to run the site is the dev-tools script (it checks your Node
version, installs deps when needed, and matches CI exactly):

```sh
dev/tools/docs-site.sh          # dev server, reachable via --host
dev/tools/docs-site.sh build    # astro build
dev/tools/docs-site.sh preview  # serve the built site
dev/tools/docs-site.sh check    # build + link verification — exactly what CI runs
```

Requires Node 22+ (Astro 7 needs >= 22.12); `check` also needs python3.
Extra args pass through to astro, e.g. `dev/tools/docs-site.sh dev --port 4322`.

## Links and the deploy base

The site deploys under `/mast` (project pages). Write content links
root-relative **without** the base — `[text](/reference/cli/)` — and the
`remark-prepend-base` plugin (`src/plugins/remark-prepend-base.mjs`,
ported from core-agent) rewrites them onto the base at build time.
Starlight only prefixes its own chrome; it does not touch Markdown links.

The plugin only sees the Markdown pipeline. Links in component props
(`LinkCard href`), frontmatter (`hero.actions`, `banner`), and raw HTML
need the `/mast` prefix written out by hand.

`scripts/verify-internal-links.py` (run by `check` and by both CI
workflows against the built `dist/`) fails on dead targets **and** on
root-relative URLs missing the base — a green `astro build` alone
guarantees neither.

Equivalent raw commands, from this directory: `npm ci`, then
`npm run dev` / `npm run build` / `npm run preview`.

## CI

[`.github/workflows/ci-docs.yml`](../../.github/workflows/ci-docs.yml)
runs `dev/tools/docs-site.sh check` (build + link verification) on PRs
and main pushes touching `docs/site/**` — no deploy. The site build is
deliberately **not** part of `dev/ci/presubmits/all.sh`: Go contributors
shouldn't need a Node toolchain.

## Deploy

Live at **https://go-steer.github.io/mast/** (the repo went public
2026-07-27). Main pushes touching `docs/site/**` deploy via
`.github/workflows/docs.yml` (configure-pages + upload-pages-artifact +
deploy-pages, mirroring core-agent's pattern); `astro.config.mjs`
carries the `site` + `base` pair. The deploy is gated on the same link
verification: a dead link never ships. PRs get build + verification via
`ci-docs.yml` — they never ship.
