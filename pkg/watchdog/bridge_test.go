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
//
// Originally derived from go-steer/core-agent@b8dd225e9ae7fdeb3ff23772cc5be25eed34b818

package watchdog

import (
	"errors"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// Bridge tests — focused on the event-side wiring (event-tap →
// Watchdog.ObserveToolCall, post-turn → Watchdog.Check). The
// watchdog's own behavior is exercised in watchdog_test.go. Here we
// verify the *plumbing*: the bridge correctly extracts tool calls
// from session events, serializes args stably, and fans alerts to
// the callback.

// fakeWatchdog records every observation and lets a test inject alerts
// to be returned from the next Check. Keeps the test independent of
// the real signal logic — we're verifying the bridge, not the signal.
type fakeWatchdog struct {
	observed []ToolCall
	pending  []Alert
	checks   int
	resets   int
}

func (f *fakeWatchdog) ObserveToolCall(tc ToolCall) {
	f.observed = append(f.observed, tc)
}

func (f *fakeWatchdog) Check() []Alert {
	f.checks++
	out := f.pending
	f.pending = nil
	return out
}

func (f *fakeWatchdog) Reset() { f.resets++ }

// eventWithCalls builds a session event whose content carries the
// given parts. In ADK v2 session.Event embeds model.LLMResponse, so
// FunctionCall parts live at ev.Content.Parts via genai types.
func eventWithCalls(parts ...*genai.Part) *session.Event {
	return &session.Event{
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{Parts: parts},
		},
	}
}

// seq builds a one-turn event stream from (event, error) pairs.
func seq(pairs ...func() (*session.Event, error)) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		for _, p := range pairs {
			if !yield(p()) {
				return
			}
		}
	}
}

func pair(ev *session.Event, err error) func() (*session.Event, error) {
	return func() (*session.Event, error) { return ev, err }
}

func TestObserveEvent_ExtractsFunctionCalls(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}
	ev := eventWithCalls(
		&genai.Part{Text: "I'll read the file."},
		&genai.Part{FunctionCall: &genai.FunctionCall{
			Name: "read_file",
			Args: map[string]any{"path": "main.go"},
		}},
		&genai.Part{FunctionCall: &genai.FunctionCall{
			Name: "grep",
			Args: map[string]any{"pattern": "foo"},
		}},
	)
	ObserveEvent(w, ev, map[string]struct{}{})
	if got, want := len(w.observed), 2; got != want {
		t.Fatalf("observed %d calls, want %d", got, want)
	}
	if w.observed[0].Name != "read_file" {
		t.Errorf("[0].Name = %q, want read_file", w.observed[0].Name)
	}
	if !strings.Contains(w.observed[0].Args, "main.go") {
		t.Errorf("[0].Args should embed path arg; got %q", w.observed[0].Args)
	}
	if w.observed[1].Name != "grep" {
		t.Errorf("[1].Name = %q, want grep", w.observed[1].Name)
	}
}

func TestObserveEvent_NilSafe(t *testing.T) {
	t.Parallel()
	// All the no-op paths: nil watchdog, nil event, nil content,
	// empty parts, nil part. None should panic. (Bridge runs from
	// the streaming event loop — a panic here would tear down the
	// run mid-turn.)
	ObserveEvent(nil, nil, map[string]struct{}{}) // nil watchdog AND nil ev
	w := &fakeWatchdog{}
	ObserveEvent(w, nil, map[string]struct{}{})
	ObserveEvent(w, &session.Event{}, map[string]struct{}{}) // nil content
	ObserveEvent(w, eventWithCalls(), map[string]struct{}{}) // empty parts
	ObserveEvent(w, eventWithCalls(nil, &genai.Part{Text: "x"}), map[string]struct{}{})
	if len(w.observed) != 0 {
		t.Errorf("no-op paths should observe nothing; got %d", len(w.observed))
	}
}

func TestSerializeArgs_StableAcrossMapOrder(t *testing.T) {
	t.Parallel()
	// Go's map iteration is randomized. The serializer MUST produce
	// the same string for the same logical args every call — otherwise
	// the watchdog's literal-string-compare detector would see every
	// call as distinct and never trip on a real loop.
	args := map[string]any{
		"path":      "main.go",
		"max_lines": 100,
		"recursive": false,
		"glob":      "*.go",
	}
	first := serializeArgs(args)
	for i := 0; i < 20; i++ {
		if got := serializeArgs(args); got != first {
			t.Fatalf("iteration %d: got %q, want stable %q", i, got, first)
		}
	}
}

func TestSerializeArgs_EmptyArgs(t *testing.T) {
	t.Parallel()
	if got := serializeArgs(nil); got != "{}" {
		t.Errorf("nil args → %q, want %q", got, "{}")
	}
	if got := serializeArgs(map[string]any{}); got != "{}" {
		t.Errorf("empty args → %q, want %q", got, "{}")
	}
}

func TestSerializeArgs_UnmarshalableFallback(t *testing.T) {
	t.Parallel()
	// Marshal failure must yield the recognizable placeholder, not
	// skip the observation — the watchdog needs *some* comparable
	// string per call.
	args := map[string]any{"fn": func() {}} // funcs don't JSON-marshal
	if got := serializeArgs(args); got != "<unmarshalable-args>" {
		t.Errorf("unmarshalable args → %q, want %q", got, "<unmarshalable-args>")
	}
}

// TestObserveEvent_DedupsAggregatorReEmission is the #363 regression
// gate. ADK's streaming aggregator can re-emit the same FunctionCall
// part across more than one event (intermediate aggregate + final);
// without per-turn dedup each real call counted up to twice and the
// repeated-tool-call signal tripped at ~half the configured
// threshold. Same-ID re-emission dedups; a legitimate parallel call
// with identical args but a distinct ID still counts; and a fresh
// turn (fresh seen set) counts again — cross-turn repetition IS the
// watchdog's signal.
func TestObserveEvent_DedupsAggregatorReEmission(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}

	call := &genai.FunctionCall{ID: "fc-1", Name: "grep", Args: map[string]any{"pattern": "foo"}}
	evIntermediate := eventWithCalls(&genai.Part{FunctionCall: call})
	evFinal := eventWithCalls(&genai.Part{FunctionCall: call})

	seen := map[string]struct{}{}
	ObserveEvent(w, evIntermediate, seen)
	ObserveEvent(w, evFinal, seen)
	if got := len(w.observed); got != 1 {
		t.Fatalf("re-emitted part observed %d times, want 1", got)
	}

	// A legitimate parallel call: same name+args, DIFFERENT ID.
	evParallel := eventWithCalls(&genai.Part{FunctionCall: &genai.FunctionCall{
		ID: "fc-2", Name: "grep", Args: map[string]any{"pattern": "foo"},
	}})
	ObserveEvent(w, evParallel, seen)
	if got := len(w.observed); got != 2 {
		t.Fatalf("distinct-ID parallel call observed total %d, want 2", got)
	}

	// Next turn: fresh seen set — the SAME call must count again
	// (cross-turn repetition is the runaway signal).
	ObserveEvent(w, evFinal, map[string]struct{}{})
	if got := len(w.observed); got != 3 {
		t.Fatalf("cross-turn repeat observed total %d, want 3 (dedup must not span turns)", got)
	}
}

// TestObserveEvent_IDLessDedupsByNameArgs covers the ID-less
// provider path: within one turn, identical name+args dedup
// (aggregator artifact); different args still count.
func TestObserveEvent_IDLessDedupsByNameArgs(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}

	mk := func(pattern string) *session.Event {
		return eventWithCalls(&genai.Part{FunctionCall: &genai.FunctionCall{
			Name: "grep", Args: map[string]any{"pattern": pattern},
		}})
	}
	seen := map[string]struct{}{}
	ObserveEvent(w, mk("foo"), seen)
	ObserveEvent(w, mk("foo"), seen) // re-emission
	ObserveEvent(w, mk("bar"), seen) // different args
	if got := len(w.observed); got != 2 {
		t.Fatalf("observed %d, want 2 (foo once, bar once)", got)
	}
}

func TestTap_PassesEventsAndErrorsThrough(t *testing.T) {
	t.Parallel()
	// Tap is a pass-through observer: every (event, error) pair the
	// inner stream yields must reach the consumer unchanged, in order.
	ev1 := eventWithCalls(&genai.Part{Text: "hello"})
	ev2 := eventWithCalls(&genai.Part{FunctionCall: &genai.FunctionCall{Name: "grep"}})
	wantErr := errors.New("stream error")

	var gotEvents []*session.Event
	var gotErrs []error
	for ev, err := range Tap(seq(pair(ev1, nil), pair(ev2, nil), pair(nil, wantErr)), &fakeWatchdog{}, nil) {
		gotEvents = append(gotEvents, ev)
		gotErrs = append(gotErrs, err)
	}
	if len(gotEvents) != 3 {
		t.Fatalf("consumer saw %d pairs, want 3", len(gotEvents))
	}
	if gotEvents[0] != ev1 || gotEvents[1] != ev2 || gotEvents[2] != nil {
		t.Errorf("events not passed through unchanged: %v", gotEvents)
	}
	if gotErrs[0] != nil || gotErrs[1] != nil || !errors.Is(gotErrs[2], wantErr) {
		t.Errorf("errors not passed through unchanged: %v", gotErrs)
	}
}

func TestTap_ObservesAndDrainsAlerts(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{pending: []Alert{
		{Signal: "repeated-tool-call", Severity: SeverityWarn, Reason: "looping on read_file"},
		{Signal: "repeated-tool-call", Severity: SeverityWarn, Reason: "looping on grep"},
	}}
	var got []Alert
	stream := Tap(seq(
		pair(eventWithCalls(&genai.Part{FunctionCall: &genai.FunctionCall{
			Name: "read_file", Args: map[string]any{"path": "main.go"},
		}}), nil),
	), w, func(al Alert) { got = append(got, al) })

	for range stream {
	}
	if len(w.observed) != 1 || w.observed[0].Name != "read_file" {
		t.Errorf("Tap should feed function calls to the watchdog; observed %+v", w.observed)
	}
	// Alerts drained only after the stream ended.
	if len(got) != 2 {
		t.Fatalf("expected 2 alerts dispatched post-turn; got %d", len(got))
	}
	if got[0].Signal != "repeated-tool-call" || got[1].Reason != "looping on grep" {
		t.Errorf("unexpected dispatched alerts: %+v", got)
	}
}

func TestTap_AlertsDrainAfterStreamEndsNotDuring(t *testing.T) {
	t.Parallel()
	// drainAlerts semantics: Check fires once, after the last event
	// — an alert returned there is "for the turn just ended."
	w := &fakeWatchdog{pending: []Alert{{Signal: "x"}}}
	alertsSeenMidStream := -1
	var dispatched int
	stream := Tap(seq(pair(eventWithCalls(), nil), pair(eventWithCalls(), nil)),
		w, func(Alert) { dispatched++ })
	for range stream {
		alertsSeenMidStream = dispatched
	}
	if alertsSeenMidStream != 0 {
		t.Errorf("alerts dispatched mid-stream (%d); drain must run post-turn", alertsSeenMidStream)
	}
	if dispatched != 1 || w.checks != 1 {
		t.Errorf("post-turn drain: dispatched=%d checks=%d, want 1/1", dispatched, w.checks)
	}
}

func TestTap_NilCallbackDrainsButDoesNotPanic(t *testing.T) {
	t.Parallel()
	// Bridge contract (matches core-agent's drainWatchdogAlerts): if
	// no callback is wired, alerts are pulled (so they don't leak
	// into the next turn) but silently discarded.
	w := &fakeWatchdog{pending: []Alert{{Signal: "x"}}}
	for range Tap(seq(pair(eventWithCalls(), nil)), w, nil) {
	}
	if w.checks != 1 {
		t.Errorf("Check should still drain with nil callback; checks = %d", w.checks)
	}
	if len(w.pending) != 0 {
		t.Errorf("pending alerts should be drained; %d remain", len(w.pending))
	}
}

func TestTap_NilWatchdogIsPassThrough(t *testing.T) {
	t.Parallel()
	// Should NOT panic, should NOT call the callback (no watchdog,
	// nothing to drain — pure pass-through).
	ev := eventWithCalls(&genai.Part{FunctionCall: &genai.FunctionCall{Name: "grep"}})
	n := 0
	for range Tap(seq(pair(ev, nil)), nil, func(Alert) {
		t.Error("callback must not fire with nil watchdog")
	}) {
		n++
	}
	if n != 1 {
		t.Errorf("consumer saw %d events, want 1", n)
	}
}

func TestTap_DrainsOnEarlyConsumerBreak(t *testing.T) {
	t.Parallel()
	// The drain is deferred — same guarantee core-agent's
	// wrapWithCleanup gives its post-turn hooks. A consumer that
	// stops consuming mid-turn must not leave the alert buffer to
	// leak into the next turn.
	w := &fakeWatchdog{pending: []Alert{{Signal: "x"}}}
	var got []Alert
	for range Tap(seq(pair(eventWithCalls(), nil), pair(eventWithCalls(), nil)),
		w, func(al Alert) { got = append(got, al) }) {
		break // consumer bails after the first event
	}
	if w.checks != 1 || len(got) != 1 {
		t.Errorf("early break: checks=%d dispatched=%d, want 1/1", w.checks, len(got))
	}
}

func TestTap_DoesNotResetWatchdog(t *testing.T) {
	t.Parallel()
	// Per-turn Reset would wipe the signals' run state — cross-turn
	// repetition is exactly what the watchdog exists to count. Reset
	// is a caller decision at logical session boundaries, never
	// Tap's.
	w := &fakeWatchdog{}
	for range Tap(seq(pair(eventWithCalls(), nil)), w, nil) {
	}
	if w.resets != 0 {
		t.Errorf("Tap must not call Reset; resets = %d", w.resets)
	}
}

// TestTap_PerTurnDedupScopedToOneTap is the #363 regression gate at
// the Tap level: within one Tap (one turn) a re-emitted same-ID
// FunctionCall part is observed once; wrapping the next turn in a
// fresh Tap makes the SAME call count again, so cross-turn
// repetition still accumulates toward the threshold.
func TestTap_PerTurnDedupScopedToOneTap(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}
	call := &genai.FunctionCall{ID: "fc-1", Name: "grep", Args: map[string]any{"pattern": "foo"}}
	turn := func() iter.Seq2[*session.Event, error] {
		// Intermediate aggregate + final event carrying the same part.
		return seq(
			pair(eventWithCalls(&genai.Part{FunctionCall: call}), nil),
			pair(eventWithCalls(&genai.Part{FunctionCall: call}), nil),
		)
	}

	for range Tap(turn(), w, nil) {
	}
	if got := len(w.observed); got != 1 {
		t.Fatalf("turn 1: re-emitted part observed %d times, want 1", got)
	}
	// Turn 2: fresh Tap → fresh seen set → the same call counts again.
	for range Tap(turn(), w, nil) {
	}
	if got := len(w.observed); got != 2 {
		t.Fatalf("after turn 2: observed total %d, want 2 (dedup must not span turns)", got)
	}
}

// TestTap_EndToEndWithDefaultWatchdog wires the real DefaultWatchdog
// through Tap across turns: the double-emission that motivated #363
// must NOT trip the threshold early, and genuine cross-turn
// repetition must still trip it.
func TestTap_EndToEndWithDefaultWatchdog(t *testing.T) {
	t.Parallel()
	w := &DefaultWatchdog{signals: []Signal{NewRepeatedToolCallSignal(3)}}
	call := &genai.FunctionCall{ID: "fc-1", Name: "read_file", Args: map[string]any{"path": "loop.go"}}
	turn := func() iter.Seq2[*session.Event, error] {
		return seq(
			pair(eventWithCalls(&genai.Part{FunctionCall: call}), nil), // intermediate
			pair(eventWithCalls(&genai.Part{FunctionCall: call}), nil), // final re-emission
		)
	}
	var alerts []Alert
	onAlert := func(al Alert) { alerts = append(alerts, al) }

	// Turns 1 + 2: two real calls (double-emission deduped). Without
	// the #363 dedup these would already count 4 and trip at
	// threshold 3.
	for range Tap(turn(), w, onAlert) {
	}
	for range Tap(turn(), w, onAlert) {
	}
	if len(alerts) != 0 {
		t.Fatalf("threshold 3 must not trip after 2 real calls; got %+v", alerts)
	}
	// Turn 3: third real call — trips.
	for range Tap(turn(), w, onAlert) {
	}
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert after 3 cross-turn repeats; got %d", len(alerts))
	}
	if alerts[0].Signal != "repeated-tool-call" || !strings.Contains(alerts[0].Reason, "read_file") {
		t.Errorf("unexpected alert: %+v", alerts[0])
	}
}
