// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// v0.1 skeleton config, mirroring core-agent's docs/site Astro +
// Starlight setup (the go-steer convention; fork-design's earlier Hugo
// references were stale — corrected 2026-07-26).
//
// Deliberately NOT set yet: `site` + `base` for GitHub Pages. Deploy is
// deferred until the repo goes public (see docs/site/README.md); when
// the Pages workflow lands, add site/base here plus core-agent's
// remark-prepend-base plugin so relative links resolve identically in
// dev and prod.
export default defineConfig({
  integrations: [
    starlight({
      title: 'mast',
      description:
        'Agent infrastructure for unattended, library-embedded, multi-provider, durable workloads.',
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/go-steer/mast',
        },
      ],
      editLink: {
        baseUrl: 'https://github.com/go-steer/mast/edit/main/docs/site/',
      },
      // Palette + typography live in one file so the whole visual
      // system is swappable (same pattern as core-agent).
      customCss: ['./src/styles/theme.css'],
      sidebar: [
        {
          label: 'Overview',
          items: [
            { label: 'Introduction', link: '/' },
            { label: 'Install', link: '/install/' },
            { label: 'Roadmap', link: '/roadmap/' },
          ],
        },
        {
          label: 'Quickstarts',
          items: [
            { label: 'Unattended triage (offline)', link: '/quickstart/unattended-triage/' },
            { label: 'Embed the library', link: '/quickstart/library-embed/' },
            { label: 'Fork a starter', link: '/quickstart/fork-a-starter/' },
          ],
        },
        {
          label: 'Reference',
          items: [{ autogenerate: { directory: 'reference' } }],
        },
      ],
    }),
  ],
});
