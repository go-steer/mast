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

// The library-embedded surface's runaway backstop. Internal (package
// mast, not mast_test) because libraryWatchdogMode is unexported and
// the posture resolution is worth asserting on its own, not only
// through a turn.
package mast

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/watchdog"
	"github.com/go-steer/mast/pkg/workload"
)

// libraryLoopModel is a stuck agent: the same tool call with the same
// arguments, round after round, until it has emitted `rounds` of them
// and stops on its own. The self-imposed ceiling is what makes the
// warn-mode arm terminate — under enforce the watchdog is supposed to
// cut it off long before.
type libraryLoopModel struct {
	rounds int32
	calls  atomic.Int32
}

func (m *libraryLoopModel) Name() string { return "library-loop" }

func (m *libraryLoopModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		n := m.calls.Add(1)
		resp := &model.LLMResponse{
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2,
			},
			FinishReason: genai.FinishReasonStop,
		}
		if n > m.rounds {
			resp.Content = genai.NewContentFromText("giving up", genai.RoleModel)
			resp.TurnComplete = true
			yield(resp, nil)
			return
		}
		// A fresh ID per call: distinct calls that happen to be
		// identical, not one re-emitted part (which the tap dedups).
		resp.Content = &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID:   fmt.Sprintf("fc-%d", n),
				Name: "poke", Args: map[string]any{"target": "pod-1"},
			}}},
		}
		yield(resp, nil)
	}
}

// loopWorkload is a one-specialist coordinator roster: the root model
// call goes straight to the fake, so whatever it emits is what the tap
// sees.
func loopWorkload(posture string) (workload.Bundle, []specialists.Spec) {
	return workload.Bundle{
			Name:        "library_loop",
			Description: "A workload whose model does nothing but repeat itself.",
			Specialists: []string{"poker"},
			Safety:      workload.Safety{Watchdog: posture},
		}, []specialists.Spec{{
			Name:        "poker",
			Description: "Pokes things.",
			Mode:        specialists.ModeTask,
			Instruction: "Poke the pod until something changes.",
		}}
}

// TestLibraryEmbedEnforceHaltsARunawayTurn is the gap this closes:
// before it, mast.RunWorkload iterated the runner's event stream
// directly, with no watchdog anywhere on the library path. A bundle
// that asks for enforce now gets the same halt the daemon and the
// one-shot give it.
func TestLibraryEmbedEnforceHaltsARunawayTurn(t *testing.T) {
	bundle, specs := loopWorkload(workload.WatchdogEnforce)
	m := &libraryLoopModel{rounds: 60}

	_, err := RunWorkload(context.Background(), Config{Model: m}, bundle, specs, "go")
	if !watchdog.IsTripped(err) {
		t.Fatalf("RunWorkload err = %v, want a watchdog halt", err)
	}
	// RepeatedToolCallSignal trips at 5; a few rounds of slack for the
	// tool round-trip. 60 would mean nothing stopped it.
	if got := m.calls.Load(); got > 15 {
		t.Errorf("the model was called %d times, want ~5 — the halt did not cut the loop short", got)
	}
}

// The same loop under warn must run to completion: warn annotates, it
// does not stop. A backstop that halts when it was asked to log is the
// failure mode that makes consumers turn the whole thing off.
func TestLibraryEmbedWarnLetsTheLoopRun(t *testing.T) {
	bundle, specs := loopWorkload(workload.WatchdogWarn)
	m := &libraryLoopModel{rounds: 8}

	if _, err := RunWorkload(context.Background(), Config{Model: m}, bundle, specs, "go"); err != nil {
		t.Fatalf("warn mode stopped the turn: %v", err)
	}
	if got := m.calls.Load(); got < 8 {
		t.Errorf("the model was called %d times, want all 8 rounds — warn mode interfered", got)
	}
}

// The default posture must not halt either. This is the assertion that
// keeps mast.DefaultMode honest for library consumers: the daemon can
// afford a rung that stops a session because it has an operator surface
// to clear it through, and this surface has none.
func TestLibraryEmbedDefaultPostureDoesNotHalt(t *testing.T) {
	bundle, specs := loopWorkload("")
	m := &libraryLoopModel{rounds: 8}

	if _, err := RunWorkload(context.Background(), Config{Model: m}, bundle, specs, "go"); err != nil {
		t.Fatalf("the default posture stopped the turn: %v", err)
	}
	if got := m.calls.Load(); got < 8 {
		t.Errorf("the model was called %d times, want all 8 rounds — the default posture interfered", got)
	}
}

func TestLibraryWatchdogMode(t *testing.T) {
	tests := []struct {
		name    string
		bundle  *workload.Bundle
		want    watchdog.Mode
		wantErr bool
	}{
		{"nil bundle takes the default", nil, watchdog.DefaultMode, false},
		{"unset takes the default", &workload.Bundle{}, watchdog.DefaultMode, false},
		{"declared warn", &workload.Bundle{Safety: workload.Safety{Watchdog: "warn"}}, watchdog.ModeWarn, false},
		{"declared feedback", &workload.Bundle{Safety: workload.Safety{Watchdog: "feedback"}}, watchdog.ModeFeedback, false},
		{"declared enforce", &workload.Bundle{Safety: workload.Safety{Watchdog: "enforce"}}, watchdog.ModeEnforce, false},
		{"a typo is refused, not defaulted", &workload.Bundle{Name: "x", Safety: workload.Safety{Watchdog: "enfoce"}}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := libraryWatchdogMode(tc.bundle)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("libraryWatchdogMode = %q, want an error", got)
				}
				// The message has to name the field, or the consumer is
				// left grepping a Go struct for a YAML key.
				if !strings.Contains(err.Error(), "safety.watchdog") {
					t.Errorf("error = %q, does not name safety.watchdog", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("libraryWatchdogMode: %v", err)
			}
			if got != tc.want {
				t.Errorf("libraryWatchdogMode = %q, want %q", got, tc.want)
			}
		})
	}
}
