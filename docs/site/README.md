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
dev/tools/docs-site.sh build    # exactly what CI runs
dev/tools/docs-site.sh preview  # serve the built site
dev/tools/docs-site.sh check    # build incl. Starlight's link validation
```

Requires Node 22+ (Astro 7 needs >= 22.12). Extra args pass through to
astro, e.g. `dev/tools/docs-site.sh dev --port 4322`.

Equivalent raw commands, from this directory: `npm ci`, then
`npm run dev` / `npm run build` / `npm run preview`.

## CI

[`.github/workflows/ci-docs.yml`](../../.github/workflows/ci-docs.yml)
runs `dev/tools/docs-site.sh build` on PRs and main pushes touching
`docs/site/**` — build only, no deploy. The site build is deliberately
**not** part of `dev/ci/presubmits/all.sh`: Go contributors shouldn't need
a Node toolchain.

## Deploy: deliberately deferred

The repo is private until the fork lands plus sanity checks pass
(AGENTS.md, "Operational facts"). Publishing this site is deferred until
the repo goes public; when that happens, the deploy lands as a GitHub
Pages workflow mirroring core-agent's `docs.yml` pattern
(configure-pages + upload-pages-artifact + deploy-pages), and
`astro.config.mjs` grows the `site` + `base` pair (plus core-agent's
remark-prepend-base plugin) so links resolve identically in dev and prod.
