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

// Originally derived from go-steer/core-agent@6510a65b54ead93b5f2c8c31f478443376203360:pkg/agent/watchdog_enforce_test.go

package watchdog

import (
	"iter"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// callPart builds a FunctionCall part with a fresh ID, so a stream of
// them reads as distinct calls rather than as one re-emitted part.
func callPart(id, name string, args map[string]any) *genai.Part {
	return &genai.Part{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: args}}
}

// loopStream is a turn that never ends on its own: one identical tool
// call per event, forever. This is the intra-turn shape — the model
// emits a call, the flow runs it and calls the model again, all inside
// a single Run — and it is the shape a turn-boundary-only drain cannot
// see, because the boundary never arrives.
func loopStream(n int) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		for i := range n {
			ev := eventWithCalls(callPart(
				string(rune('a'+i%26))+string(rune('0'+i/26)),
				"list_agents", map[string]any{},
			))
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// TestTap_DrainsInTurn is the acceptance gate for the in-turn arm.
// Pre-fix, alerts landed only on the deferred post-turn drain: an
// operator watching a live incident saw nothing until the turn ended,
// and a turn that never ends produced no alert at all.
func TestTap_DrainsInTurn(t *testing.T) {
	t.Parallel()

	w := NewDefaultWatchdog()
	var alertedAt int
	events := 0
	for range Tap(loopStream(20), w, func(Alert) {
		if alertedAt == 0 {
			alertedAt = events
		}
	}) {
		events++
	}

	// RepeatedToolCallSignal's default threshold is 5, so the alert
	// must land while the stream is still running — not at event 20.
	if alertedAt == 0 {
		t.Fatal("no alert fired during the turn")
	}
	if alertedAt > 6 {
		t.Errorf("first alert after %d events, want it at the threshold (5) — the drain is still at the turn boundary", alertedAt)
	}
}

// The halt an enforcing caller performs: stop consuming the stream the
// moment a Critical alert arrives. Tap must surface the alert early
// enough for that to cut the loop short rather than merely annotate it.
func TestTap_InTurnAlertLetsACallerHaltTheTurn(t *testing.T) {
	t.Parallel()

	w := NewDefaultWatchdog()
	e := NewEnforcer(ModeEnforce, "")
	halted := false

	consumed := 0
	for range Tap(loopStream(500), w, func(a Alert) {
		if e.Observe(a) {
			halted = true
		}
	}) {
		consumed++
		if halted {
			break // the caller's cancel, in miniature
		}
	}

	if !halted {
		t.Fatal("the enforcer never saw a Critical alert")
	}
	if consumed > 10 {
		t.Errorf("consumed %d events before halting, want ~5 — a runaway must be cut short, not merely reported", consumed)
	}
	if err := e.Preflight(); err == nil {
		t.Error("the next turn was not refused after an in-turn halt")
	}
}

// Draining early must not drain twice. Each signal emits once per
// pattern, so a second copy of an alert would mean the drain itself is
// duplicating — which under enforce is a second cancel and in a report
// is a doubled count.
func TestTap_InTurnDrainDoesNotDuplicateAlerts(t *testing.T) {
	t.Parallel()

	w := NewDefaultWatchdog()
	var got []Alert
	for range Tap(loopStream(40), w, func(a Alert) { got = append(got, a) }) {
	}
	if len(got) != 1 {
		t.Fatalf("alerts = %d, want 1 for one stuck pattern: %+v", len(got), got)
	}
}

// Text-only events must not provoke a drain. A signal cannot newly
// trip without an observation, and a turn emits far more text than
// tool calls.
func TestTap_InTurnDrainIsGatedOnAFreshObservation(t *testing.T) {
	t.Parallel()

	w := &countingWatchdog{}
	text := func() (*session.Event, error) {
		return eventWithCalls(&genai.Part{Text: "thinking"}), nil
	}
	for range Tap(seq(text, text, text, text), w, nil) {
	}
	// One Check, from the post-turn drain. Zero would leak a trip into
	// the next turn; four would be mutex traffic per text delta.
	if w.checks != 1 {
		t.Errorf("Check called %d times over a text-only turn, want 1 (the post-turn drain)", w.checks)
	}
}

// The same gate, from the other side: a re-emitted call part is not a
// fresh observation, so it must not provoke a second drain either.
func TestTap_InTurnDrainSkipsReEmittedParts(t *testing.T) {
	t.Parallel()

	w := &countingWatchdog{}
	ev := eventWithCalls(callPart("fc-1", "grep", map[string]any{"pattern": "x"}))
	for range Tap(seq(pair(ev, nil), pair(ev, nil), pair(ev, nil)), w, nil) {
	}
	// One in-turn drain for the first (real) observation, plus the
	// post-turn drain.
	if w.checks != 2 {
		t.Errorf("Check called %d times, want 2 (one fresh observation + the post-turn drain)", w.checks)
	}
}

// countingWatchdog counts Check calls and nothing else — the in-turn
// gate is about how often the drain runs, not what it finds.
type countingWatchdog struct {
	observed int
	checks   int
}

func (c *countingWatchdog) ObserveToolCall(ToolCall) { c.observed++ }
func (c *countingWatchdog) Check() []Alert           { c.checks++; return nil }
func (c *countingWatchdog) Reset()                   {}
