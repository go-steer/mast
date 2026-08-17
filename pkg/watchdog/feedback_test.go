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

// Originally derived from go-steer/core-agent@6510a65b54ead93b5f2c8c31f478443376203360:pkg/watchdog/watchdog_test.go

package watchdog

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func guided(signal string) Alert {
	return Alert{
		Signal:   signal,
		Severity: SeverityCritical,
		Reason:   "operator text for " + signal + ". POST /sessions/{id}/interrupt.",
		Guidance: "model text for " + signal + ".",
	}
}

func TestFormatFeedback_Empty(t *testing.T) {
	t.Parallel()
	if got := FormatFeedback(nil); got != "" {
		t.Errorf("FormatFeedback(nil) = %q, want empty so callers can skip the prepend", got)
	}
	if got := FormatFeedback([]Alert{}); got != "" {
		t.Errorf("FormatFeedback([]) = %q, want empty", got)
	}
}

// The block has one job beyond carrying the text: making it unmistakable
// that this is the model's own last turn being described, not the user
// speaking. A model that reads it as a user complaint apologizes instead
// of changing what it does.
func TestFormatFeedback_Shape(t *testing.T) {
	t.Parallel()
	got := FormatFeedback([]Alert{guided("repeated-tool-call"), guided("alternating-tool-cycle")})

	if !strings.HasPrefix(got, FeedbackHeader) {
		t.Errorf("block does not open with %q:\n%s", FeedbackHeader, got)
	}
	for _, want := range []string{
		"your own previous turn",
		"not a message from the user",
		"- repeated-tool-call: model text for repeated-tool-call.",
		"- alternating-tool-cycle: model text for alternating-tool-cycle.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("block is missing %q:\n%s", want, got)
		}
	}
	// An instruction, not a description. A description of the loop is
	// what the model already had.
	if !strings.HasSuffix(got, "Adjust your approach on this turn accordingly.") {
		t.Errorf("block does not end in an instruction:\n%s", got)
	}
	// The operator half must not leak: it names controls the model
	// cannot use, and naming them invites a hallucinated call.
	if strings.Contains(got, "/interrupt") {
		t.Errorf("operator affordance leaked into the model-facing block:\n%s", got)
	}
}

// A third-party signal that sets no Guidance still feeds. Reason is the
// worse text — it may name operator controls — but dropping the alert
// would make a custom signal silently inert under feedback mode, which
// is the harder failure to notice.
func TestFormatFeedback_FallsBackToReason(t *testing.T) {
	t.Parallel()
	got := FormatFeedback([]Alert{{Signal: "custom", Reason: "third-party observation."}})
	if !strings.Contains(got, "- custom: third-party observation.") {
		t.Errorf("a Guidance-less alert produced no line:\n%s", got)
	}
}

// Every built-in signal owes the model a usable sentence. This is the
// test that keeps a new signal from shipping with only the operator
// half — which would pass every other test in the package.
func TestBuiltinSignalsCarryModelFacingGuidance(t *testing.T) {
	t.Parallel()

	w := NewDefaultWatchdog()
	for i := 0; i < 10; i++ {
		w.ObserveToolCall(ToolCall{Name: "kubectl_get", Args: `{"resource":"pods"}`})
	}
	for i := 0; i < 12; i++ {
		w.ObserveToolCall(ToolCall{Name: fmt.Sprintf("t%d", i%3), Args: "{}"})
		w.ObserveToolResult(ToolResult{Name: fmt.Sprintf("t%d", i%3), Error: "permission denied"})
	}

	alerts := w.Check()
	if len(alerts) < 3 {
		t.Fatalf("got %d alerts, want all three built-in signals to have fired: %v", len(alerts), alerts)
	}
	seen := map[string]bool{}
	for _, a := range alerts {
		seen[a.Signal] = true
		if strings.TrimSpace(a.Guidance) == "" {
			t.Errorf("signal %q sets no Guidance — it would feed the operator text to the model", a.Signal)
			continue
		}
		// The two things the model-facing half must not do: name a
		// control the model does not have, or address a third party.
		for _, banned := range []string{"/interrupt", "guardrails/reset", "budget ceiling", "the agent"} {
			if strings.Contains(a.Guidance, banned) {
				t.Errorf("signal %q Guidance contains operator-facing %q: %s", a.Signal, banned, a.Guidance)
			}
		}
	}
	for _, want := range []string{"repeated-tool-call", "alternating-tool-cycle", "tool-failure-streak"} {
		if !seen[want] {
			t.Errorf("signal %q did not fire; the guidance invariant went unchecked for it", want)
		}
	}
}

// Nothing is queued below feedback, so flipping a long-running
// deployment up a rung cannot deliver a backlog of observations about
// turns that ended hours ago.
func TestFeedback_QueuesNothingUnderWarn(t *testing.T) {
	t.Parallel()
	f := NewFeedback(ModeWarn)
	f.Queue([]Alert{guided("repeated-tool-call")})
	if n := f.Pending(); n != 0 {
		t.Errorf("warn mode queued %d alerts, want 0", n)
	}
	if got := f.Drain(); got != nil {
		t.Errorf("Drain = %v, want nil", got)
	}
}

func TestFeedback_ZeroModeIsWarn(t *testing.T) {
	t.Parallel()
	f := NewFeedback("")
	f.Queue([]Alert{guided("repeated-tool-call")})
	if n := f.Pending(); n != 0 {
		t.Errorf("the zero mode queued %d alerts, want 0", n)
	}
}

// Enforce feeds too. Without this the turn after an operator reset
// starts with the model knowing nothing about why it was stopped.
func TestFeedback_EnforceQueues(t *testing.T) {
	t.Parallel()
	f := NewFeedback(ModeEnforce)
	f.Queue([]Alert{guided("repeated-tool-call")})
	if n := f.Pending(); n != 1 {
		t.Errorf("enforce mode queued %d alerts, want 1", n)
	}
}

func TestFeedback_DrainDeliversOnceAndEmpties(t *testing.T) {
	t.Parallel()
	f := NewFeedback(ModeFeedback)
	f.Queue([]Alert{guided("a"), guided("b")})

	got := f.Drain()
	if len(got) != 2 {
		t.Fatalf("Drain = %d alerts, want 2", len(got))
	}
	if n := f.Pending(); n != 0 {
		t.Errorf("Pending = %d after Drain, want 0", n)
	}
	// Once, not every turn until some turn succeeds: a block that
	// re-appears indefinitely is a prompt leak.
	if again := f.Drain(); again != nil {
		t.Errorf("second Drain = %v, want nil", again)
	}
}

// The queue only grows when a host observes turns without starting new
// ones. The bound keeps that case from becoming an ever-growing prompt
// prefix, and it drops from the front because the newest observation
// describes the behavior about to repeat.
func TestFeedback_BoundedOldestDropped(t *testing.T) {
	t.Parallel()
	f := NewFeedback(ModeFeedback)
	for i := 0; i < MaxPendingFeedback+3; i++ {
		f.Queue([]Alert{guided(fmt.Sprintf("s%d", i))})
	}
	if n := f.Pending(); n != MaxPendingFeedback {
		t.Fatalf("Pending = %d, want the queue capped at %d", n, MaxPendingFeedback)
	}
	got := f.Drain()
	if got[0].Signal != "s3" {
		t.Errorf("oldest surviving alert = %q, want s3 — the wrong end was dropped", got[0].Signal)
	}
	if last := got[len(got)-1].Signal; last != "s6" {
		t.Errorf("newest alert = %q, want s6", last)
	}
}

func TestFeedback_EmptyQueueIsANoOp(t *testing.T) {
	t.Parallel()
	f := NewFeedback(ModeFeedback)
	f.Queue(nil)
	f.Queue([]Alert{})
	if n := f.Pending(); n != 0 {
		t.Errorf("Pending = %d, want 0", n)
	}
}

// A host that never wired a queue must not panic on the turn path.
func TestFeedback_NilIsInert(t *testing.T) {
	t.Parallel()
	var f *Feedback
	f.Queue([]Alert{guided("repeated-tool-call")})
	if got := f.Drain(); got != nil {
		t.Errorf("Drain on a nil queue = %v, want nil", got)
	}
	if n := f.Pending(); n != 0 {
		t.Errorf("Pending on a nil queue = %d, want 0", n)
	}
}

// Alerts arrive from the event tap while the turn path drains.
func TestFeedback_ConcurrentUse(t *testing.T) {
	t.Parallel()
	f := NewFeedback(ModeFeedback)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f.Queue([]Alert{guided(fmt.Sprintf("s%d", i))})
			f.Pending()
			f.Drain()
		}(i)
	}
	wg.Wait()
}
