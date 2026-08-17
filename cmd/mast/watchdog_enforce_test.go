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

// Daemon-side tests for --watchdog=enforce: the in-flight halt at the
// event tap, the structural refusal at the runTurnPre chokepoint, and
// the guardrail projection an operator reads to find out why.
package main

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/watchdog"
)

// loopingModel is a stuck agent: the same tool call, the same
// arguments, round after round, until it has emitted `rounds` of them
// and gives up on its own. The self-imposed ceiling is what makes the
// warn-mode arm of these tests terminate — under enforce the watchdog
// is supposed to cut it off long before.
type loopingModel struct {
	rounds int32
	calls  atomic.Int32
}

func (m *loopingModel) Name() string { return "looping" }

func (m *loopingModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		n := m.calls.Add(1)
		resp := &model.LLMResponse{
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2,
			},
		}
		if n > m.rounds {
			resp.Content = genai.NewContentFromText("giving up", genai.RoleModel)
			resp.TurnComplete = true
			resp.FinishReason = genai.FinishReasonStop
			yield(resp, nil)
			return
		}
		// A fresh ID per call: these are distinct calls that happen to be
		// identical, not one re-emitted part (which the tap dedups).
		resp.Content = &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: fmt.Sprintf("fc-%d", n),
				// Same name, same args — the runaway shape.
				Name: "poke", Args: map[string]any{"target": "pod-1"},
			}}},
		}
		resp.FinishReason = genai.FinishReasonStop
		yield(resp, nil)
	}
}

// pokeTool always succeeds, so the only thing wrong with the loop is
// that it is a loop — a failing tool would trip a different signal.
func pokeTool(t *testing.T) tool.Tool {
	t.Helper()
	poke, err := functiontool.New(functiontool.Config{
		Name:        "poke",
		Description: "poke something and report that it was poked",
	}, func(adkagent.Context, struct {
		Target string `json:"target"`
	},
	) (map[string]any, error) {
		return map[string]any{"poked": true}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}
	return poke
}

// TestWatchdogEnforceCutsAnIntraTurnLoopShort is the whole point of the
// posture. The loop lives *inside* one turn — model calls tool, tool
// returns, model calls it again — so a turn-boundary-only reaction
// never fires while the money is being spent.
func TestWatchdogEnforceCutsAnIntraTurnLoopShort(t *testing.T) {
	m := &loopingModel{rounds: 60}
	h := newTurnHarnessOpts(t, m, watchdog.ModeEnforce, pokeTool(t))

	err := h.turn(context.Background(), "s-enforce")
	if !watchdog.IsTripped(err) {
		t.Fatalf("turn err = %v, want a watchdog halt", err)
	}
	// RepeatedToolCallSignal trips at 5. A couple of rounds of slack for
	// the tool round-trip; 60 would mean nothing stopped it.
	if got := m.calls.Load(); got > 10 {
		t.Errorf("the model was called %d times, want ~5 — the halt did not cut the loop short", got)
	}
	if !strings.Contains(err.Error(), "guardrails/reset") {
		t.Errorf("halt error = %q, does not tell the operator how to clear it", err)
	}
}

// The same loop under the default posture must run: warn annotates, it
// does not stop. A watchdog that halts when it was asked to log is the
// failure mode that makes operators turn the whole thing off.
func TestWatchdogWarnLetsTheLoopRun(t *testing.T) {
	m := &loopingModel{rounds: 12}
	h := newTurnHarnessOpts(t, m, watchdog.ModeWarn, pokeTool(t))

	if err := h.turn(context.Background(), "s-warn"); err != nil {
		t.Fatalf("warn mode stopped the turn: %v", err)
	}
	if got := m.calls.Load(); got < 12 {
		t.Errorf("the model was called %d times, want all 12 rounds — warn mode interfered", got)
	}
	// It still noticed, which is warn mode's entire job.
	if a := h.wds.alerts("s-warn"); a.count == 0 {
		t.Error("warn mode logged no alert on a 12-round loop")
	}
	if halted, _ := h.wds.halted("s-warn"); halted {
		t.Error("warn mode recorded a halt")
	}
}

// A halt has to survive the turn that caused it. Every turn kind —
// auto-resume, a scheduled fire, an attach inject — funnels through
// runTurnPre, and each would otherwise re-drive the loop that tripped.
func TestWatchdogEnforceRefusesEverySubsequentTurn(t *testing.T) {
	m := &loopingModel{rounds: 60}
	h := newTurnHarnessOpts(t, m, watchdog.ModeEnforce, pokeTool(t))
	ctx := context.Background()

	if err := h.turn(ctx, "s-refuse"); !watchdog.IsTripped(err) {
		t.Fatalf("first turn err = %v, want a watchdog halt", err)
	}
	before := m.calls.Load()
	events := h.eventCount(t, "s-refuse")

	err := h.turn(ctx, "s-refuse")
	if !errors.Is(err, inject.ErrConflict) {
		t.Fatalf("turn on a halted session: err = %v, want ErrConflict", err)
	}
	if !watchdog.IsTripped(err) {
		t.Errorf("the refusal does not classify as a watchdog halt: %v", err)
	}
	// Refused before the model, not after it: a refusal that still pays
	// for a round-trip is not a refusal.
	if got := m.calls.Load(); got != before {
		t.Errorf("the refused turn called the model %d more times", got-before)
	}
	if got := h.eventCount(t, "s-refuse"); got != events {
		t.Errorf("the refused turn appended events: %d -> %d", events, got)
	}
}

// And the reset re-arms it, rather than leaving the session permanently
// halted or permanently disarmed.
func TestWatchdogResetClearsTheHaltAndRuns(t *testing.T) {
	m := &loopingModel{rounds: 6}
	h := newTurnHarnessOpts(t, m, watchdog.ModeEnforce, pokeTool(t))
	ctx := context.Background()

	if err := h.turn(ctx, "s-reset"); !watchdog.IsTripped(err) {
		t.Fatalf("first turn err = %v, want a watchdog halt", err)
	}
	h.wds.reset("s-reset")

	if halted, _ := h.wds.halted("s-reset"); halted {
		t.Fatal("reset left the session halted")
	}
	// The model gives up after 6 rounds, so this turn is short and
	// clean — proving the reset cleared the signal run too. Had only the
	// trip been cleared, the retained run would re-halt immediately.
	if err := h.turn(ctx, "s-reset"); err != nil {
		t.Fatalf("turn after reset: %v", err)
	}
}

// The guardrail surface is where an operator finds out. Under enforce
// the watchdog is not advisory even before it fires — "will this thing
// stop my agent?" is the question, and the answer is yes.
func TestGuardrailProjectionUnderEnforce(t *testing.T) {
	g := newGuardrailViewMode(watchdog.ModeEnforce, budget.Limits{}, nil)

	got := g.info(gsid)
	if got.Watchdog.Mode != string(watchdog.ModeEnforce) || got.Watchdog.Advisory {
		t.Errorf("watchdog = %+v, want a non-advisory enforce posture", got.Watchdog)
	}
	if got.Watchdog.Tripped || got.Halted {
		t.Error("an armed-but-quiet watchdog reported as tripped")
	}

	alert := watchdog.Alert{
		Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical,
		Reason: "poke called 6 times with identical arguments.",
	}
	g.wds.note(gsid, alert)
	g.wds.enforcer(gsid).Observe(alert)

	got = g.info(gsid)
	if !got.Watchdog.Tripped || !got.Halted {
		t.Errorf("watchdog = %+v halted = %v, want a halted session", got.Watchdog, got.Halted)
	}
	// The halt reason wins over the raw alert text: it names the signal
	// AND the way out, which is what the operator is reading for.
	if !strings.Contains(got.Watchdog.Reason, "guardrails/reset") {
		t.Errorf("reason = %q, does not say how to clear the halt", got.Watchdog.Reason)
	}

	resp, err := g.reset(gsid, attach.GuardrailResetRequest{Guardrail: attach.GuardrailWatchdog, Caller: "test"})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(resp.Reset) != 1 || resp.Reset[0] != attach.GuardrailWatchdog {
		t.Errorf("reset = %v, want the watchdog cleared", resp.Reset)
	}
	if resp.Guardrails.Watchdog.Tripped || resp.Guardrails.Halted {
		t.Errorf("post-reset guardrails = %+v, still halted", resp.Guardrails)
	}
}
