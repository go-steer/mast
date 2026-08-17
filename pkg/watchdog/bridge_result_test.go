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

// Originally derived from go-steer/core-agent@ef7dfb652b080a95f8595eeb2307bf93155d730a:pkg/agent/watchdog_result_test.go

package watchdog

import (
	"errors"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// Tool-outcome bridge. fakeWatchdog (bridge_test.go) deliberately does
// NOT implement ToolResultObserver — that is the "call-only watchdog"
// case, and it must stay silent rather than panic. resultWatchdog is
// the opted-in shape.
type resultWatchdog struct {
	fakeWatchdog
	results []ToolResult
}

func (f *resultWatchdog) ObserveToolResult(tr ToolResult) {
	f.results = append(f.results, tr)
}

// respPart builds a FunctionResponse part; eventWithCalls (bridge_test.go)
// carries it, since ADK puts calls and responses in the same part list.
func respPart(id, name string, resp map[string]any) *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		ID: id, Name: name, Response: resp,
	}}
}

func TestObserveToolResults_SplitsSuccessFromFailure(t *testing.T) {
	t.Parallel()
	w := &resultWatchdog{}

	ObserveToolResults(w, eventWithCalls(
		respPart("1", "read_file", map[string]any{"content": "hi"}),
		respPart("2", "gke_get_pod", map[string]any{"error": "PermissionDenied"}),
	), map[string]struct{}{})

	if got := len(w.results); got != 2 {
		t.Fatalf("observed %d results, want 2", got)
	}
	if w.results[0].Failed() {
		t.Errorf("[0] = %+v, want success", w.results[0])
	}
	if !w.results[1].Failed() || w.results[1].Error != "PermissionDenied" {
		t.Errorf("[1] = %+v, want the ADK error key surfaced", w.results[1])
	}
	if w.results[1].Name != "gke_get_pod" {
		t.Errorf("[1].Name = %q, want gke_get_pod", w.results[1].Name)
	}
}

// A watchdog that only counts calls must be left alone, not crashed and
// not silently required to grow a method. Result observation is an
// optional extension at the interface level precisely so a third-party
// watchdog doesn't break at a minor version.
func TestObserveToolResults_SkipsNonObservers(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}
	ObserveToolResults(w, eventWithCalls(
		respPart("1", "x", map[string]any{"error": "boom"}),
	), map[string]struct{}{})
	if len(w.observed) != 0 {
		t.Errorf("call observations = %d, want 0: results are not calls", len(w.observed))
	}
}

func TestObserveToolResults_NilSafe(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	ObserveToolResults(nil, nil, seen) // nil watchdog and nil event
	w := &resultWatchdog{}
	ObserveToolResults(w, nil, seen)
	ObserveToolResults(w, &session.Event{}, seen) // nil content
	ObserveToolResults(w, eventWithCalls(), seen)
	ObserveToolResults(w, eventWithCalls(nil, &genai.Part{Text: "x"}), seen)
	ObserveToolResults(w, eventWithCalls(
		&genai.Part{FunctionResponse: &genai.FunctionResponse{Name: "x"}}, // nil Response map
	), seen)
	// The last one IS a response part, so it counts — as a success.
	if len(w.results) != 1 || w.results[0].Failed() {
		t.Errorf("results = %+v, want one success from the empty response", w.results)
	}
}

// The streaming aggregator re-emits response parts exactly as it
// re-emits call parts; a double-counted failure would trip the streak
// signal at half its threshold.
func TestObserveToolResults_DedupsAggregatorReEmission(t *testing.T) {
	t.Parallel()
	w := &resultWatchdog{}
	seen := map[string]struct{}{}

	ev := eventWithCalls(respPart("call-1", "gke_get_pod", map[string]any{"error": "boom"}))
	ObserveToolResults(w, ev, seen)
	ObserveToolResults(w, ev, seen) // re-emission on the final aggregate
	if got := len(w.results); got != 1 {
		t.Fatalf("observed %d, want 1 (deduped on ID)", got)
	}
	// A genuinely different call with the same error is a separate ID.
	ObserveToolResults(w, eventWithCalls(
		respPart("call-2", "gke_get_pod", map[string]any{"error": "boom"}),
	), seen)
	if got := len(w.results); got != 2 {
		t.Errorf("observed %d, want 2: a distinct call ID is a distinct result", got)
	}
}

// Calls and results share one per-turn dedup set. Distinct key spaces
// keep a call's ID from suppressing its own response.
func TestObserveToolResults_SharesSeenSetWithoutColliding(t *testing.T) {
	t.Parallel()
	w := &resultWatchdog{}
	seen := map[string]struct{}{}

	ObserveEvent(w, eventWithCalls(&genai.Part{FunctionCall: &genai.FunctionCall{
		ID: "call-1", Name: "gke_get_pod", Args: map[string]any{},
	}}), seen)
	ObserveToolResults(w, eventWithCalls(
		respPart("call-1", "gke_get_pod", map[string]any{"error": "boom"}),
	), seen)

	if len(w.observed) != 1 {
		t.Errorf("call observations = %d, want 1", len(w.observed))
	}
	if len(w.results) != 1 {
		t.Errorf("result observations = %d, want 1 — the shared key space swallowed the response", len(w.results))
	}
}

// ID-less responses fall back to name+error. Two different failures of
// the same tool are still two observations; the same failure twice is
// one, which is the safe direction to be wrong in.
func TestObserveToolResults_IDLessFallback(t *testing.T) {
	t.Parallel()
	w := &resultWatchdog{}
	seen := map[string]struct{}{}

	ObserveToolResults(w, eventWithCalls(
		respPart("", "gke_get_pod", map[string]any{"error": "boom"}),
		respPart("", "gke_get_pod", map[string]any{"error": "boom"}),
		respPart("", "gke_get_pod", map[string]any{"error": "other"}),
	), seen)

	if got := len(w.results); got != 2 {
		t.Errorf("observed %d, want 2: identical failures collapse, distinct ones don't", got)
	}
}

func TestToolResponseError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		resp map[string]any
		want string
	}{
		{"nil map", nil, ""},
		{"no error key", map[string]any{"content": "hi"}, ""},
		{"nil error value", map[string]any{"error": nil}, ""},
		{"empty string error", map[string]any{"error": ""}, ""},
		{"string error", map[string]any{"error": "PermissionDenied"}, "PermissionDenied"},
		{"error value", map[string]any{"error": errors.New("boom")}, "boom"},
		// An unrecognized shape counts as a failure rather than being
		// dropped: a tool returning a structured error object is failing,
		// and silently reading it as success would drop exactly the
		// observation the streak signal exists to make.
		{"structured error", map[string]any{"error": map[string]any{"code": 7}}, "map[code:7]"},
		{"numeric error", map[string]any{"error": 500}, "500"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := toolResponseError(tc.resp); got != tc.want {
				t.Errorf("toolResponseError(%v) = %q, want %q", tc.resp, got, tc.want)
			}
		})
	}
}

// End-to-end through Tap with the real default watchdog: a run whose
// tools all fail must surface the streak alert on the post-turn drain.
// This is the wiring that #639 is actually about — the calls looked
// ordinary; the results were the story.
func TestTap_FeedsResultsIntoTheDefaultWatchdog(t *testing.T) {
	t.Parallel()

	w := NewDefaultWatchdog()
	var got []Alert

	var pairs []func() (*session.Event, error)
	for i, name := range []string{"gke_get_pod", "gke_list_events", "gke_get_deployment"} {
		pairs = append(pairs, pair(eventWithCalls(
			respPart(string(rune('a'+i)), name, map[string]any{"error": "PermissionDenied"}),
		), nil))
	}
	for range Tap(seq(pairs...), w, func(a Alert) { got = append(got, a) }) {
	}

	if len(got) != 1 || got[0].Signal != "tool-failure-streak" {
		t.Fatalf("alerts = %+v, want one tool-failure-streak", got)
	}
}

// Tap must not feed results to a call-only watchdog, and must not
// double-count a response the aggregator re-emits across events.
func TestTap_DedupsResultsAcrossEvents(t *testing.T) {
	t.Parallel()

	w := &resultWatchdog{}
	ev := eventWithCalls(respPart("call-1", "x", map[string]any{"error": "boom"}))
	for range Tap(seq(pair(ev, nil), pair(ev, nil)), w, nil) {
	}
	if len(w.results) != 1 {
		t.Errorf("results = %d, want 1 across the re-emitted event", len(w.results))
	}
}
