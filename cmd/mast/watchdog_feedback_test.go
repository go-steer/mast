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

// Daemon-side tests for --watchdog=feedback: the observation reaching
// the one party that can act on it. The package half (queue bounds,
// block wording) lives in pkg/watchdog; what these pin is the wiring —
// that the block lands in the *next* turn's prompt, that warn mode
// injects nothing, and that an operator reset does not throw the
// correction away along with the halt.
package main

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/go-steer/mast/pkg/watchdog"
)

// recordingLoopModel is loopingModel that also keeps the message that
// opened each call, so a test can ask what the model actually read.
//
// The *last* content, not the whole request: a prompt assembled from
// session history necessarily replays the block that was injected two
// turns ago, and a test that searched the whole request would call that
// a redelivery.
type recordingLoopModel struct {
	loopingModel

	mu      sync.Mutex
	prompts []string
}

func (m *recordingLoopModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	var b strings.Builder
	if n := len(req.Contents); n > 0 {
		for _, p := range req.Contents[n-1].Parts {
			b.WriteString(p.Text)
		}
	}
	m.prompts = append(m.prompts, b.String())
	m.mu.Unlock()
	return m.loopingModel.GenerateContent(ctx, req, stream)
}

// at returns the message that opened the model's i-th call. The first
// call of a turn is where a prepend would appear; later calls in the
// same turn open on the tool results.
func (m *recordingLoopModel) at(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= len(m.prompts) {
		return ""
	}
	return m.prompts[i]
}

func (m *recordingLoopModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.prompts)
}

// The whole point: the model that looped is told it looped, on the turn
// where it can still do something else. Under warn it never finds out.
func TestWatchdogFeedbackReachesTheNextTurnsPrompt(t *testing.T) {
	m := &recordingLoopModel{loopingModel: loopingModel{rounds: 6}}
	h := newTurnHarnessOpts(t, m, watchdog.ModeFeedback, pokeTool(t))
	ctx := context.Background()

	// Turn one loops and trips the repeat detector. Feedback does not
	// halt, so the turn runs to the model's own giving-up point.
	if err := h.turn(ctx, "s-fb"); err != nil {
		t.Fatalf("feedback mode stopped the turn: %v", err)
	}
	if h.wds.alerts("s-fb").count == 0 {
		t.Fatal("no alert fired; there is nothing to feed back")
	}
	// Nothing was injected into the turn that produced the observation —
	// its prompt was assembled before the loop existed.
	if got := m.at(0); strings.Contains(got, watchdog.FeedbackHeader) {
		t.Errorf("the observing turn's own prompt carries the block:\n%s", got)
	}
	before := m.count()

	if err := h.turn(ctx, "s-fb"); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	next := m.at(before)
	if !strings.Contains(next, watchdog.FeedbackHeader) {
		t.Fatalf("the next turn's prompt carries no watchdog block:\n%s", next)
	}
	for _, want := range []string{
		"repeated-tool-call",
		"not a message from the user",
		"Adjust your approach on this turn accordingly.",
		"hello", // the operator's actual prompt survives the prepend
	} {
		if !strings.Contains(next, want) {
			t.Errorf("prompt is missing %q:\n%s", want, next)
		}
	}
	// The block reads first: it is what has to change how the rest is
	// read.
	if !strings.HasPrefix(next, watchdog.FeedbackHeader) {
		t.Errorf("the block does not open the message:\n%s", next)
	}
}

// Delivered once. A block that re-appears every turn until something
// succeeds is a prompt leak, and by the third turn it describes behavior
// the model has already moved on from.
func TestWatchdogFeedbackIsDeliveredOnce(t *testing.T) {
	m := &recordingLoopModel{loopingModel: loopingModel{rounds: 6}}
	h := newTurnHarnessOpts(t, m, watchdog.ModeFeedback, pokeTool(t))
	ctx := context.Background()

	if err := h.turn(ctx, "s-once"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	second := m.count()
	if err := h.turn(ctx, "s-once"); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	third := m.count()
	if err := h.turn(ctx, "s-once"); err != nil {
		t.Fatalf("third turn: %v", err)
	}
	if !strings.Contains(m.at(second), watchdog.FeedbackHeader) {
		t.Fatal("the second turn got no block; the once-only check is vacuous")
	}
	if got := m.at(third); strings.Contains(got, watchdog.FeedbackHeader) {
		t.Errorf("the block was redelivered on the third turn:\n%s", got)
	}
}

// warn is unchanged by this port. Altering it would silently rewrite the
// context of every operator already running the default.
func TestWatchdogWarnInjectsNothing(t *testing.T) {
	m := &recordingLoopModel{loopingModel: loopingModel{rounds: 6}}
	h := newTurnHarnessOpts(t, m, watchdog.ModeWarn, pokeTool(t))
	ctx := context.Background()

	if err := h.turn(ctx, "s-warn-fb"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if h.wds.alerts("s-warn-fb").count == 0 {
		t.Fatal("no alert fired under warn; the check is vacuous")
	}
	before := m.count()
	if err := h.turn(ctx, "s-warn-fb"); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if got := m.at(before); strings.Contains(got, watchdog.FeedbackHeader) {
		t.Errorf("warn mode injected a block:\n%s", got)
	}
}

// Enforce implies feedback, and the reset must not undo it. A reset that
// clears the halt and drops the observation resumes a model whose
// context still ends in the loop it was stopped for — the same five
// calls, the same halt, one operator round-trip later.
func TestWatchdogResetKeepsTheQueuedObservation(t *testing.T) {
	m := &recordingLoopModel{loopingModel: loopingModel{rounds: 6}}
	h := newTurnHarnessOpts(t, m, watchdog.ModeEnforce, pokeTool(t))
	ctx := context.Background()

	if err := h.turn(ctx, "s-reset-fb"); !watchdog.IsTripped(err) {
		t.Fatalf("first turn err = %v, want a watchdog halt", err)
	}
	if n := h.wds.feedback("s-reset-fb").Pending(); n == 0 {
		t.Fatal("the halting alert queued no observation; enforce must imply feedback")
	}

	h.wds.reset("s-reset-fb")
	if n := h.wds.feedback("s-reset-fb").Pending(); n == 0 {
		t.Fatal("the reset dropped the queued observation — the next turn re-drives the loop")
	}

	before := m.count()
	if err := h.turn(ctx, "s-reset-fb"); err != nil {
		t.Fatalf("post-reset turn: %v", err)
	}
	if got := m.at(before); !strings.Contains(got, watchdog.FeedbackHeader) {
		t.Errorf("the post-reset turn carries no correction:\n%s", got)
	}
}
