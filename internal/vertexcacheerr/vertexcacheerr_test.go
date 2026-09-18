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

package vertexcacheerr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// notFoundText is the reaped-handle shape, verbatim from a Vertex
// response (the same literal pkg/providers/gemini has pinned since the
// retry-once path was written).
const notFoundText = "Error 404, Message: Not found: cached content metadata for 6116704758662168576., Status: NOT_FOUND, Details: []"

// expiredText is the elapsed-TTL shape. Honest provenance: this is
// reconstructed from #325's description, not copied out of a captured
// transcript, so the framing around the phrase may not be character
// exact. What the fix depends on is narrower than the whole string —
// the two substrings pinned by TestBothSpellingsArePinnedBySubstring —
// and those are what #325 reports directly.
const expiredText = "Error 400, Message: The cache content projects/p/locations/l/cachedContents/123 has expired., Status: INVALID_ARGUMENT, Details: []"

func TestGone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"reaped handle, 404 NOT_FOUND", errors.New(notFoundText), true},
		{"elapsed TTL, 400 INVALID_ARGUMENT", errors.New(expiredText), true},
		{"lowercase not found without a code", errors.New("cached content not found"), true},
		{"mixed case", errors.New("Cached Content missing, Status: NOT_FOUND"), true},

		// The phrase carries the specificity, not the status code:
		// both codes appear on errors that are nothing to do with the
		// cache, and treating either as decisive is how a
		// misconfiguration gets silently retried uncached.
		{"404 on a missing model", errors.New("Error 404, Message: publisher model not found, Status: NOT_FOUND"), false},
		{"404 on a wrong region", errors.New("resource not found: NOT_FOUND"), false},
		{"400 on an oversized request", errors.New("Error 400, Message: The input token count exceeds the maximum, Status: INVALID_ARGUMENT"), false},
		{"400 naming a different expired thing", errors.New("Error 400, Message: credentials have expired, Status: INVALID_ARGUMENT"), false},

		// Names the cache but reports something recoverable about it.
		{"quota, not absence", errors.New("cached content quota exceeded"), false},
		{"permission, not absence", errors.New("Error 403, Message: permission denied on cached content, Status: PERMISSION_DENIED"), false},

		{"wrapped", fmt.Errorf("generate content: %w", errors.New(expiredText)), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Gone(tc.err); got != tc.want {
				t.Errorf("Gone(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestBothSpellingsArePinnedBySubstring states the dependency in the
// narrowest form it actually has. Gone matches on message text because
// the SDK exposes no typed error for either shape; that is tolerable
// only while a provider wording change fails a build instead of a
// daemon, and these are the four substrings such a change would move.
//
// If Vertex renames one, this test is where you find out, and the fix
// is to add the new spelling rather than to loosen the phrase check —
// the phrase is what keeps a plain 400 from being read as an eviction.
func TestBothSpellingsArePinnedBySubstring(t *testing.T) {
	t.Parallel()
	for _, want := range []struct{ text, substr string }{
		{notFoundText, "cached content"},
		{notFoundText, "NOT_FOUND"},
		{expiredText, "cache content"},
		{expiredText, "expired"},
	} {
		if !containsFold(want.text, want.substr) {
			t.Errorf("fixture %q no longer contains %q — the fixture drifted, not the predicate", want.text, want.substr)
		}
	}

	// The two shapes are disjoint: neither substring pair matches the
	// other message. This is the reason #325 existed at all — a
	// predicate written against the first shape cannot accidentally
	// cover the second.
	if containsFold(expiredText, "cached content") {
		t.Error("expired shape contains \"cached content\"; the two spellings are no longer distinct and the #325 framing is stale")
	}
	if containsFold(notFoundText, "expired") {
		t.Error("not-found shape contains \"expired\"; the two spellings are no longer distinct and the #325 framing is stale")
	}
}

// containsFold mirrors what Gone does to the message before looking:
// lowercase, then plain substring. Spelled out here rather than called
// through Gone, so this test pins the fixtures independently of the
// predicate it is checking the fixtures for.
func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
