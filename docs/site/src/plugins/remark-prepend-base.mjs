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

import { visit } from 'unist-util-visit';

/**
 * Prepend Astro's `base` to any Markdown link starting with `/`.
 * Keeps content files decoupled from the deploy base URL: authors
 * write `[text](/reference/cli/)`, at build time the plugin turns
 * it into `[text](/mast/reference/cli/)` (or whatever `base`
 * resolves to). Change the base in astro.config.mjs and content
 * doesn't move.
 *
 * Ported from go-steer/core-agent's docs/site plugin of the same
 * name — Starlight only prefixes its own chrome (sidebar, nav) with
 * `base`; raw Markdown links pass through untouched, which is how
 * root-relative content links 404 on project-pages hosting.
 *
 * Skipped:
 *   - external URLs (any scheme://)
 *   - protocol-relative (//foo)
 *   - anchor-only links (#foo)
 *   - links already prefixed with the base
 */
export function remarkPrependBase(base) {
  const normalizedBase = base.replace(/\/$/, '');
  if (!normalizedBase) {
    return () => (tree) => tree;
  }
  return () => (tree) => {
    visit(tree, ['link', 'linkReference'], (node) => {
      const url = node.url;
      if (typeof url !== 'string' || url === '') return;
      if (/^[a-z][a-z0-9+.-]*:\/\//i.test(url)) return;
      if (url.startsWith('//')) return;
      if (url.startsWith('#')) return;
      if (!url.startsWith('/')) return;
      if (url === normalizedBase || url.startsWith(normalizedBase + '/')) return;
      node.url = normalizedBase + url;
    });
  };
}
