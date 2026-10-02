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

package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRetrieveRawNeedsAStore(t *testing.T) {
	t.Parallel()
	// A registered tool that refuses every call teaches the model
	// nothing except that the escape hatch does not work, so the
	// wiring site is made to notice at construction.
	if _, err := NewRetrieveRawTool(nil); err == nil {
		t.Fatal("NewRetrieveRawTool(nil) should be a construction error")
	}
	tl, err := NewRetrieveRawTool(&memStore{})
	if err != nil {
		t.Fatalf("NewRetrieveRawTool: %v", err)
	}
	if tl.Name() != RetrieveRawToolName {
		t.Errorf("Name() = %q, want %q", tl.Name(), RetrieveRawToolName)
	}
}

func TestRetrieveRawReturnsTheStoredPayload(t *testing.T) {
	t.Parallel()
	store := &memStore{}
	payload := strings.Repeat("kubectl output ", 100)
	if err := store.Put(context.Background(), "call-42", []byte(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := retrieveRawFunc(store)(newStubToolContext("c1"), retrieveRawArgs{CallID: "call-42"})
	if err != nil {
		t.Fatalf("retrieve_raw: %v", err)
	}
	if got.Raw != payload {
		t.Errorf("raw = %q…, want the stored payload", truncate(got.Raw))
	}
	if got.Bytes != len(payload) {
		t.Errorf("bytes = %d, want %d", got.Bytes, len(payload))
	}
}

func TestRetrieveRawKeepsTheModelInTheLoopOnFailure(t *testing.T) {
	t.Parallel()
	// Every failure comes back as a normal tool response: aborting the
	// turn over a failed spot-check is worse than telling the model the
	// spot-check is unavailable. The two messages differ because "I do
	// not have that" and "I am broken" call for different next moves.
	tests := []struct {
		name  string
		store *memStore
		id    string
		want  string
	}{
		{
			name:  "an empty call_id",
			store: &memStore{},
			id:    "",
			want:  "non-empty call_id",
		},
		{
			name:  "an unknown call_id",
			store: &memStore{},
			id:    "call-nope",
			want:  `no raw payload stored for call_id "call-nope"`,
		},
		{
			name:  "a broken store",
			store: &memStore{getErr: errors.New("disk on fire")},
			id:    "call-42",
			want:  "disk on fire",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := retrieveRawFunc(tc.store)(newStubToolContext("c1"), retrieveRawArgs{CallID: tc.id})
			if err != nil {
				t.Fatalf("the handler must not return a Go error, got %v", err)
			}
			if !strings.HasPrefix(got.Raw, "(error:") {
				t.Errorf("raw = %q, want an (error: ...) response", got.Raw)
			}
			if !strings.Contains(got.Raw, tc.want) {
				t.Errorf("raw = %q, want it to mention %q", got.Raw, tc.want)
			}
			if got.Bytes != 0 {
				t.Errorf("bytes = %d, want 0 on a failure", got.Bytes)
			}
		})
	}
}

func TestTheRetrieveRawDescriptionTalksTheModelOutOfCallingIt(t *testing.T) {
	t.Parallel()
	// Upstream measured this on a live demo (2026-07-17): Flash called
	// retrieve_raw to "double-check" a digest, re-inflated ~28k tokens,
	// and ran the same triage ~6x more expensive. A wrap that saves
	// context and a tool that hands it straight back cancel out unless
	// the description does the work — so the load-bearing clauses are
	// pinned here rather than left to a future prose tidy-up.
	for _, clause := range []string{
		"DO NOT call retrieve_raw to spot-check",
		"Treat the digest as authoritative by default",
		"re-inflates the full payload back into your context",
		"prefer a narrower follow-up call to the underlying tool",
	} {
		if !strings.Contains(retrieveRawDescription, clause) {
			t.Errorf("the description no longer discourages re-inflation: missing %q", clause)
		}
	}
}

func truncate(s string) string {
	if len(s) <= 60 {
		return s
	}
	return s[:60]
}
