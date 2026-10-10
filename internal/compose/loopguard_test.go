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

package compose

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/go-steer/mast/internal/watchdog"
	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/workload"
)

// loopingModel is the #514 model: it calls one tool with one set of
// arguments on every round, for at most limit rounds, and records the
// function responses it was shown.
type loopingModel struct {
	limit int

	mu     sync.Mutex
	rounds int
	seen   []map[string]any // the latest function response in each request
}

func (m *loopingModel) Name() string { return "looping" }

func (m *loopingModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		m.rounds++
		round := m.rounds
		if n := len(req.Contents); n > 0 {
			for _, p := range req.Contents[n-1].Parts {
				if p.FunctionResponse != nil {
					m.seen = append(m.seen, p.FunctionResponse.Response)
				}
			}
		}
		m.mu.Unlock()
		if round > m.limit {
			yield(&model.LLMResponse{Content: genai.NewContentFromText("giving up", genai.RoleModel), TurnComplete: true}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					Name: "k8s_resource_spec",
					Args: map[string]any{"scope": "data/pvc-data-postgres-0"},
				},
			}}},
			TurnComplete: true,
		}, nil)
	}
}

// recordingStop is approval.TurnStop: it records the first reason and
// cancels the turn, as the daemon's refusalStop does.
type recordingStop struct {
	mu     sync.Mutex
	reason error
	cancel context.CancelFunc
}

func (s *recordingStop) StopTurn(reason error) {
	s.mu.Lock()
	if s.reason == nil {
		s.reason = reason
	}
	s.mu.Unlock()
	s.cancel()
}

type specArgs struct {
	Scope string `json:"scope"`
}

type guardProbe struct {
	executions int
	model      *loopingModel
	stop       *recordingStop
}

func runGuardProbe(t *testing.T, mode watchdog.Mode, maxCalls int, withStop bool, limit int) *guardProbe {
	t.Helper()
	probe := &guardProbe{model: &loopingModel{limit: limit}}

	spec, err := functiontool.New(functiontool.Config{
		Name:        "k8s_resource_spec",
		Description: "reads a resource spec",
	}, func(_ adkagent.Context, _ specArgs) (map[string]any, error) {
		probe.executions++
		return map[string]any{"phase": "Pending"}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}
	root, err := llmagent.New(llmagent.Config{
		Name:        "loop_agent",
		Description: "loop-guard probe",
		Instruction: "act",
		Model:       probe.model,
		Tools:       []tool.Tool{spec},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	var plugins []*plugin.Plugin
	guard, err := LoopGuard(LoopGuardConfig{Mode: mode, MaxModelCallsPerTurn: maxCalls, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("LoopGuard: %v", err)
	}
	if guard != nil {
		plugins = append(plugins, guard)
	}
	r, err := runner.New(runner.Config{
		AppName:           "loopguard-test",
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: plugins},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe.stop = &recordingStop{cancel: cancel}
	if withStop {
		ctx = approval.WithTurnStop(ctx, probe.stop)
	}
	msg := genai.NewContentFromText("why is postgres pending?", genai.RoleUser)
	for _, err := range r.Run(ctx, "u", "s", msg, adkagent.RunConfig{}) {
		// A stopped turn ends in "context canceled"; that is the
		// feature, so errors are not fatal here.
		_ = err
	}
	return probe
}

// TestLoopGuardNotesThenEndsTheTurn is #514 end to end, through a real
// runner: the model is told on the threshold call's result, keeps going,
// and the turn ends before the call that would have been the
// threshold+stopAfter-th execution.
func TestLoopGuardNotesThenEndsTheTurn(t *testing.T) {
	p := runGuardProbe(t, watchdog.ModeFeedback, 0, true, 100)

	var loop *watchdog.LoopStopError
	if !errors.As(p.stop.reason, &loop) {
		t.Fatalf("turn ended with %v, want a *watchdog.LoopStopError — nothing stopped the loop", p.stop.reason)
	}
	want := watchdog.DefaultRepeatThreshold + watchdog.DefaultStopAfterNote - 1
	if p.executions != want {
		t.Errorf("tool ran %d times, want %d", p.executions, want)
	}
	if p.model.rounds > want+1 {
		t.Errorf("model called %d times after a stop at call %d — the turn did not end", p.model.rounds, want+1)
	}

	noted := 0
	for i, resp := range p.model.seen {
		note, _ := resp[watchdog.LoopNoteKey].(string)
		if i+1 < watchdog.DefaultRepeatThreshold && note != "" {
			t.Errorf("result %d carried a note below the threshold", i+1)
		}
		if note != "" {
			noted++
			if resp["phase"] != "Pending" {
				t.Errorf("result %d lost the tool's own fields: %v", i+1, resp)
			}
			if !strings.Contains(note, "ends this turn") {
				t.Errorf("note %q does not say what happens next", note)
			}
		}
	}
	if noted != watchdog.DefaultStopAfterNote {
		t.Errorf("%d results carried the note, want %d", noted, watchdog.DefaultStopAfterNote)
	}
}

// TestLoopGuardWithoutATurnStopKeepsRefusing: a library embed may run
// with no TurnStop. The guard cannot end the turn there, but it must not
// hand the model a fresh allowance either — every later repeat is
// refused without running.
func TestLoopGuardWithoutATurnStopKeepsRefusing(t *testing.T) {
	p := runGuardProbe(t, watchdog.ModeFeedback, 0, false, 20)
	want := watchdog.DefaultRepeatThreshold + watchdog.DefaultStopAfterNote - 1
	if p.executions != want {
		t.Errorf("tool ran %d times over 20 identical calls, want %d", p.executions, want)
	}
	last := p.model.seen[len(p.model.seen)-1]
	if last[loopStoppedKey] != loopStopped {
		t.Errorf("last response = %v, want the guard's refusal", last)
	}
}

// TestLoopGuardIsOffBelowFeedback: warn means nothing intervenes.
func TestLoopGuardIsOffBelowFeedback(t *testing.T) {
	if g, err := LoopGuard(LoopGuardConfig{Mode: watchdog.ModeWarn}); g != nil || err != nil {
		t.Fatalf("LoopGuard(warn, no cap) = %v, %v; want nil, nil", g, err)
	}
	p := runGuardProbe(t, watchdog.ModeWarn, 0, true, 12)
	if p.executions != 12 {
		t.Errorf("tool ran %d times under warn, want all 12", p.executions)
	}
}

// TestModelCallCapEndsTheTurnUnderEveryPosture is #519: the cap is a
// budget, so it holds under warn, where the repeated-call guard is off.
// The probe's model repeats one call, but the cap is set below the
// repeat guard's threshold so it is the cap that fires.
func TestModelCallCapEndsTheTurnUnderEveryPosture(t *testing.T) {
	for _, mode := range []watchdog.Mode{watchdog.ModeWarn, watchdog.ModeFeedback, watchdog.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			p := runGuardProbe(t, mode, 3, true, 100)
			var capped *watchdog.TurnCallsError
			if !errors.As(p.stop.reason, &capped) {
				t.Fatalf("turn ended with %v, want a *watchdog.TurnCallsError", p.stop.reason)
			}
			if p.model.rounds != 3 {
				t.Errorf("model called %d times, want exactly the cap of 3", p.model.rounds)
			}
			if !watchdog.IsLoopStop(p.stop.reason) {
				t.Error("a cap stop does not classify as loop_stop")
			}
		})
	}
}

// TestModelCallCapWithoutATurnStopStillEndsTheTurn: the refused call is
// answered with text and no function call, so a library embed with no
// TurnStop ends the turn too, rather than the flow asking again.
func TestModelCallCapWithoutATurnStopStillEndsTheTurn(t *testing.T) {
	p := runGuardProbe(t, watchdog.ModeWarn, 3, false, 100)
	if p.model.rounds != 3 {
		t.Errorf("model called %d times, want 3", p.model.rounds)
	}
}

func TestMaxModelCallsPerTurnResolution(t *testing.T) {
	b := func(n int) *workload.Bundle {
		return &workload.Bundle{Budget: workload.Budget{MaxModelCallsPerTurn: n}}
	}
	for _, tc := range []struct {
		name string
		b    *workload.Bundle
		want int
	}{
		{"no bundle takes the default", nil, DefaultMaxModelCallsPerTurn},
		{"unset takes the default", b(0), DefaultMaxModelCallsPerTurn},
		{"declared", b(40), 40},
		{"negative is uncapped", b(-1), 0},
	} {
		if got := MaxModelCallsPerTurn(tc.b); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}
