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

// Package vertexcacheerr answers one question about a Vertex error:
// is the explicit context cache we hold a handle to still there?
//
// It exists because two packages have to agree on the answer and
// neither should import the other. pkg/providers/vertexcache owns the
// cache's lifecycle and sees the question on a Caches.Update; the
// pkg/providers/gemini wrapper sees it on a GenerateContent that
// stamped the cache reference, and reaches the manager through a
// callback precisely so it does not depend on it. When the two
// disagree — which is what #325 was — the manager keeps handing out a
// name for content that no longer exists, and every later turn fails
// as a config error until the process restarts.
//
// Matching on message text is fragile by nature; the genai SDK exposes
// no typed error for either shape. The mitigation is that the
// substrings are pinned by a test, so a provider wording change fails
// a build rather than a daemon.
package vertexcacheerr

import "strings"

// Gone reports whether err is Vertex saying the cache is no longer
// there. Two spellings, because Vertex has two, and the difference is
// the whole bug:
//
//   - A reaped handle comes back as NOT_FOUND naming "cached content"
//     — the shape the retry-once path was written for.
//   - An elapsed TTL comes back as 400 INVALID_ARGUMENT naming "cache
//     content" (no "d") as "expired". Neither substring of the first
//     shape appears in it.
//
// The status code is deliberately not part of the match. It is the
// part that differs between the two shapes — 404 against 400 — while
// the phrase names the cause in both, and a predicate keyed on the
// code is one Vertex revision away from the failure it exists to
// prevent. The cache-content phrase carries the specificity instead: a
// plain NOT_FOUND (missing model, wrong region) and a plain 400 (a
// genuine misconfiguration, which is what a bare INVALID_ARGUMENT
// usually is) are both left alone.
func Gone(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if !strings.Contains(s, "cache content") && !strings.Contains(s, "cached content") {
		return false
	}
	return strings.Contains(s, "not_found") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "expired")
}
