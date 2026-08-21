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

package watchdog

import (
	"strings"
	"testing"
)

// newDominant is the constructor these tests exercise, at its defaults.
func newDominant() *DominantToolCallSignal {
	return NewDominantToolCallSignal(DefaultDominantWindow, DefaultDominantThreshold)
}

// TestDominantToolCallSignal_TripsOnTheInterleavedLoop is the acceptance
// test for #227. The shape is one call repeated with occasional others
// wedged in — a a a b a a a c a a a — which the repeat detector cannot
// see (every interloper resets its run) and the cycle detector hands
// back (no repeating block).
//
// Fails on pre-#227 code, where neither wired signal produced anything
// for this sequence until the interleaves happened to stop.
func TestDominantToolCallSignal_TripsOnTheInterleavedLoop(t *testing.T) {
	t.Parallel()

	s := newDominant()
	alerts := feed(s, interleaved()...)

	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want exactly 1: %+v", len(alerts), alerts)
	}
	got := alerts[0]
	if got.Signal != "dominant-tool-call" {
		t.Errorf("Signal = %q, want dominant-tool-call", got.Signal)
	}
	if got.Severity != SeverityCritical {
		t.Errorf("Severity = %v, want Critical — this is a runaway, like the other two loop detectors", got.Severity)
	}
	if !strings.Contains(got.Reason, "gke_list_clusters") {
		t.Errorf("Reason omits the dominant call, so an operator can't tell what looped: %q", got.Reason)
	}
	if !strings.Contains(got.Reason, "interrupt") {
		t.Errorf("Reason names no mast affordance: %q", got.Reason)
	}
	if !strings.Contains(got.Guidance, "gke_list_clusters") {
		t.Errorf("Guidance omits the dominant call, so the [watchdog] block steers toward nothing: %q", got.Guidance)
	}
}

// interleaved is the #227 shape: nine of twelve calls are the same one,
// with two interlopers placed so that no run ever reaches five and no
// block ever repeats.
func interleaved() []ToolCall {
	dom := call("gke_list_clusters", `{"project":"p"}`)
	return []ToolCall{
		dom, dom, dom,
		call("gke_get_cluster", `{"name":"c1"}`),
		dom, dom, dom,
		call("gke_list_nodepools", `{"cluster":"c1"}`),
		dom, dom, dom,
		dom,
	}
}

// TestDominantToolCallSignal_TheOtherTwoDetectorsStayQuietOnIt is the
// other half of the acceptance claim, and the reason this signal
// exists. Asserting only that the new detector fires would not show
// there was a hole; this asserts the hole.
func TestDominantToolCallSignal_TheOtherTwoDetectorsStayQuietOnIt(t *testing.T) {
	t.Parallel()

	seq := interleaved()
	if alerts := feed(NewRepeatedToolCallSignal(DefaultRepeatThreshold), seq...); len(alerts) != 0 {
		t.Errorf("the repeat detector raised %d alerts on the interleaved shape; #227 says it cannot see it: %+v", len(alerts), alerts)
	}
	if alerts := feed(NewAlternatingCycleSignal(DefaultCycleMaxPeriod, DefaultCycleRepeats), seq...); len(alerts) != 0 {
		t.Errorf("the cycle detector raised %d alerts on the interleaved shape; #227 says it hands it back: %+v", len(alerts), alerts)
	}
}

// TestDominantToolCallSignal_ConvergesInsideOneWindow pins the property
// the port is for. The repeat detector eventually catches this loop —
// the cost is how long it takes, and upstream measured 22 calls before
// it did. Density reaches the verdict on the twelfth observation, which
// is the first one where a full window exists.
func TestDominantToolCallSignal_ConvergesInsideOneWindow(t *testing.T) {
	t.Parallel()

	s := newDominant()
	seq := interleaved()
	for i, c := range seq[:len(seq)-1] {
		if a := s.ObserveToolCall(c); a != nil {
			t.Fatalf("alert at observation %d, before the window was full: %+v", i+1, a)
		}
	}
	if a := s.ObserveToolCall(seq[len(seq)-1]); a == nil {
		t.Fatalf("no alert on observation %d, the first full window", len(seq))
	}
}

// TestDominantToolCallSignal_DefersToTheRepeatDetector covers the
// de-duplication that the issue calls the part worth porting carefully.
// DefaultWatchdog appends every signal's alert, so a plain run of one
// call must not produce both this alert and the repeat detector's —
// under `feedback` that is two paragraphs of steering for one behavior.
func TestDominantToolCallSignal_DefersToTheRepeatDetector(t *testing.T) {
	t.Parallel()

	dom := call("read_file", `{"path":"main.go"}`)
	var seq []ToolCall
	for i := 0; i < DefaultDominantWindow; i++ {
		seq = append(seq, dom)
	}

	if alerts := feed(newDominant(), seq...); len(alerts) != 0 {
		t.Errorf("got %d alerts on a plain consecutive run; the repeat detector owns that shape: %+v", len(alerts), alerts)
	}
	// And the shape really is the repeat detector's — otherwise the
	// deferral would be handing it to nobody.
	if alerts := feed(NewRepeatedToolCallSignal(DefaultRepeatThreshold), seq...); len(alerts) != 1 {
		t.Errorf("the repeat detector raised %d alerts on a 12-call run, want 1", len(alerts))
	}
}

// TestDominantToolCallSignal_DefersToTheCycleDetector is the same
// contract for the a → b → a → b shape. A period-2 cycle is 6 of 12 for
// each call, below the threshold, so this test uses a period-3 cycle
// with a dominant member: a a b a a b a a b a a b is 8 of 12 for `a`
// and a clean repetition of a 3-call block.
func TestDominantToolCallSignal_DefersToTheCycleDetector(t *testing.T) {
	t.Parallel()

	a := call("list_pods", `{"ns":"prod"}`)
	b := call("get_events", `{"ns":"prod"}`)
	var seq []ToolCall
	for i := 0; i < 4; i++ {
		seq = append(seq, a, a, b)
	}

	if alerts := feed(newDominant(), seq...); len(alerts) != 0 {
		t.Errorf("got %d alerts on a clean 3-call cycle; the cycle detector owns that shape: %+v", len(alerts), alerts)
	}
	if alerts := feed(NewAlternatingCycleSignal(DefaultCycleMaxPeriod, DefaultCycleRepeats), seq...); len(alerts) != 1 {
		t.Errorf("the cycle detector raised %d alerts on the 3-call cycle, want 1 — the deferral would be handing the shape to nobody", len(alerts))
	}
}

// TestDominantToolCallSignal_ZeroDeferralsAlertAnyway pins that the
// deferrals are for a signal wired *alongside* the other two. An
// embedder wiring this one alone sets them to zero and sees every
// dominant loop, including the two shapes the others would have owned.
func TestDominantToolCallSignal_ZeroDeferralsAlertAnyway(t *testing.T) {
	t.Parallel()

	dom := call("read_file", `{"path":"main.go"}`)
	var run []ToolCall
	for i := 0; i < DefaultDominantWindow; i++ {
		run = append(run, dom)
	}

	s := newDominant()
	s.DeferToRepeatRun = 0
	s.DeferToCyclePeriod = 0
	if alerts := feed(s, run...); len(alerts) != 1 {
		t.Fatalf("got %d alerts with deferrals disabled, want 1: %+v", len(alerts), alerts)
	}
}

// TestDominantToolCallSignal_OneAlertPerBehavior pins that a loop that
// keeps going produces one alert, not one per call past the threshold.
func TestDominantToolCallSignal_OneAlertPerBehavior(t *testing.T) {
	t.Parallel()

	s := newDominant()
	seq := interleaved()
	// Keep the loop running well past the first full window, still
	// interleaved so the repeat deferral never engages.
	dom := seq[0]
	for i := 0; i < 3; i++ {
		seq = append(seq, dom, dom, dom, call("gke_get_cluster", `{"name":"c1"}`))
	}
	if alerts := feed(s, seq...); len(alerts) != 1 {
		t.Fatalf("got %d alerts for one continuing loop, want 1: %+v", len(alerts), alerts)
	}
}

// TestDominantToolCallSignal_ReArmsAfterTheWindowDiversifies is the
// other side of that: a session that recovers and later loops again
// gets a second alert. A signal that latches would go quiet for the
// rest of a long-running unattended session.
func TestDominantToolCallSignal_ReArmsAfterTheWindowDiversifies(t *testing.T) {
	t.Parallel()

	s := newDominant()
	if alerts := feed(s, interleaved()...); len(alerts) != 1 {
		t.Fatalf("first loop: got %d alerts, want 1", len(alerts))
	}
	// Twelve distinct calls flush the window and clear the trip.
	var varied []ToolCall
	for i := 0; i < DefaultDominantWindow; i++ {
		varied = append(varied, call("read_file", `{"path":"f`+string(rune('a'+i))+`.go"}`))
	}
	if alerts := feed(s, varied...); len(alerts) != 0 {
		t.Fatalf("varied work raised %d alerts: %+v", len(alerts), alerts)
	}
	if alerts := feed(s, interleaved()...); len(alerts) != 1 {
		t.Fatalf("second loop: got %d alerts, want 1 — the signal latched", len(alerts))
	}
}

// TestDominantToolCallSignal_OrdinaryWorkIsQuiet is the false-positive
// floor. Reading four files with a list between each is the normal
// shape of an investigating agent; the most frequent call there is 4 of
// 12 and must not trip.
func TestDominantToolCallSignal_OrdinaryWorkIsQuiet(t *testing.T) {
	t.Parallel()

	s := newDominant()
	var seq []ToolCall
	for _, f := range []string{"a.go", "b.go", "c.go", "d.go"} {
		seq = append(seq,
			call("list_dir", `{"path":"/src"}`),
			call("read_file", `{"path":"`+f+`"}`),
			call("grep", `{"q":"`+f+`"}`),
		)
	}
	if alerts := feed(s, seq...); len(alerts) != 0 {
		t.Errorf("ordinary investigation raised %d alerts: %+v", len(alerts), alerts)
	}
}

// TestDominantToolCallSignal_CanonicalizesArgs pins that the same file
// under three spellings is one call, the way the other two detectors
// read it. An agent re-reading one file under three paths is as stuck
// as one re-reading it under the same path.
func TestDominantToolCallSignal_CanonicalizesArgs(t *testing.T) {
	t.Parallel()

	spellings := []string{`{"path":"main.go"}`, `{"path":"./main.go"}`, `{"path":"main.go"}`}
	var seq []ToolCall
	for i := 0; i < DefaultDominantWindow; i++ {
		if i == 3 || i == 7 {
			seq = append(seq, call("list_dir", `{"path":"/src"}`))
			continue
		}
		seq = append(seq, call("read_file", spellings[i%len(spellings)]))
	}

	s := newDominant()
	alerts := feed(s, seq...)
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1 — three spellings of one path must count as one call: %+v", len(alerts), alerts)
	}
}

// TestDominantToolCallSignal_PartialWindowIsQuiet pins that the signal
// says nothing until it has a full window. A density claim over four
// observations is not a density claim.
func TestDominantToolCallSignal_PartialWindowIsQuiet(t *testing.T) {
	t.Parallel()

	s := newDominant()
	dom := call("gke_list_clusters", `{"project":"p"}`)
	for i := 0; i < DefaultDominantWindow-1; i++ {
		c := dom
		if i%4 == 3 {
			c = call("gke_get_cluster", `{"name":"c1"}`)
		}
		if a := s.ObserveToolCall(c); a != nil {
			t.Fatalf("alert at observation %d of a %d-call window: %+v", i+1, DefaultDominantWindow, a)
		}
	}
}

// TestDominantToolCallSignal_ConstructorClamps pins the degenerate
// configurations, each of which would turn the detector into something
// other than a detector.
func TestDominantToolCallSignal_ConstructorClamps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                      string
		window, threshold         int
		wantWindow, wantThreshold int
	}{
		{"defaults pass through", 12, 8, 12, 8},
		{"window below the floor", 2, 2, 4, 2},
		{"threshold below the floor", 12, 0, 12, 2},
		{"threshold above the window", 12, 40, 12, 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := NewDominantToolCallSignal(tc.window, tc.threshold)
			if s.Window != tc.wantWindow {
				t.Errorf("Window = %d, want %d", s.Window, tc.wantWindow)
			}
			if s.Threshold != tc.wantThreshold {
				t.Errorf("Threshold = %d, want %d", s.Threshold, tc.wantThreshold)
			}
		})
	}
}

// TestDominantToolCallSignal_NotInTheDefaultSet pins the deferral #227
// asks for: the signal lands as a constructor, and whether it joins the
// default set is a posture decision that belongs with the open
// watchdog-governance question. A change here should be a deliberate
// one, with this test's message read first.
func TestDominantToolCallSignal_NotInTheDefaultSet(t *testing.T) {
	t.Parallel()

	for _, s := range NewDefaultWatchdog().signals {
		if s.Name() == "dominant-tool-call" {
			t.Fatal("dominant-tool-call joined the default signal set. That is a posture change, not a port: mast defaults to feedback, so a third Critical detector changes what every unattended workload is told about itself, and a polling workload is the known false positive. If it was decided deliberately, update this test, docs/site concepts/interop.md, and the watchdog-governance note in docs/sibling-sync.md.")
		}
	}
}

// TestDominantToolCallSignal_ResetClearsState pins Signal.Reset, which
// DefaultWatchdog calls on a logical session boundary.
func TestDominantToolCallSignal_ResetClearsState(t *testing.T) {
	t.Parallel()

	s := newDominant()
	if alerts := feed(s, interleaved()...); len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	s.Reset()
	if len(s.history) != 0 || len(s.names) != 0 || s.tripped != "" {
		t.Fatalf("Reset left state behind: history=%d names=%d tripped=%q", len(s.history), len(s.names), s.tripped)
	}
	if alerts := feed(s, interleaved()...); len(alerts) != 1 {
		t.Fatalf("after Reset: got %d alerts, want 1", len(alerts))
	}
}
