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

// The W10.3 acceptance test: a spent specialist closes one path, not the
// session.
//
// pkg/agent's TestARefusedTaskAgentReportsThroughFinishTask proves the
// agent half against a stub gate — a refused Task specialist reports
// through finish_task, so the caller above it has something to read. What
// it cannot prove is the half this file is about, because that one lives
// in the turn driver: through v0.5 the driver cancelled the run context
// the moment a specialist's own ceiling fired, so the report the
// specialist had just produced went to a coordinator that no longer
// existed. The routing was possible and never happened.
//
// So the fixture here is a real roster on a real meter through the
// daemon's own runTurn, and the assertions are the three things that all
// have to hold together: the capped specialist made no further call, the
// workload finished anyway through another specialist, and an operator
// can find out — on the meter, on the trip counter, and in the
// transcript. Any two of those without the third is a failure mode:
// finishing without the trip is a silently degraded workload, and the
// trip without finishing is v0.5.

package main

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/watchdog"
	"github.com/go-steer/mast/pkg/workload"
)

// routingModel scripts a coordinator: delegate to each name in order,
// one per model call, then answer. A Task specialist's own model answers
// through finish_task instead, which is how ADK ends a sub-agent's turn.
//
// Delegation is driven by a counter rather than by scanning the request
// for who has already replied, because the interesting script here
// dispatches the *same* specialist twice — once while it has budget and
// once after — and a history scan cannot express that.
type routingModel struct {
	name   string
	order  []string
	tokens int32

	mu    sync.Mutex
	calls int
}

func (m *routingModel) Name() string { return m.name }

func (m *routingModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()

	usage := &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: m.tokens}
	return func(yield func(*model.LLMResponse, error) bool) {
		if _, ok := req.Tools[mastagent.FinishTaskToolName]; ok {
			yield(&model.LLMResponse{
				Content:       fnCall(mastagent.FinishTaskToolName, map[string]any{"result": "handled by " + m.name}),
				UsageMetadata: usage,
			}, nil)
			return
		}
		if n <= len(m.order) {
			yield(&model.LLMResponse{
				Content:       fnCall(m.order[n-1], map[string]any{"request": "go"}),
				UsageMetadata: usage,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content:       genai.NewContentFromText("done", genai.RoleModel),
			UsageMetadata: usage,
		}, nil)
	}
}

func (m *routingModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func fnCall(name string, args map[string]any) *genai.Content {
	return &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)},
	}
}

// rosterHarness is cappedTurnHarness with a roster instead of a bare
// coordinator: a real meter pool carrying per-specialist ceilings derived
// the way the daemon derives them, and sub-agents built through the same
// constructors buildRoot uses — which is what installs the gate.
func rosterHarness(t *testing.T, coord *routingModel, subs map[string]*routingModel, caps map[string]specialists.Budget) *turnHarness {
	t.Helper()

	specs := make([]specialists.Spec, 0, len(subs))
	agents := make([]adkagent.Agent, 0, len(subs))
	// coord.order fixes the iteration order, so the roster is built the
	// same way on every run whatever the map does.
	seen := map[string]bool{}
	for _, name := range coord.order {
		if seen[name] {
			continue
		}
		seen[name] = true
		specs = append(specs, specialists.Spec{Name: name, Budget: caps[name]})
		a, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
			Name: name, Description: name, Instruction: "do the work", Model: subs[name],
		})
		if err != nil {
			t.Fatalf("NewTaskAgent(%q): %v", name, err)
		}
		agents = append(agents, a)
	}

	h := &turnHarness{
		svc:   adksession.InMemoryService(),
		locks: newSessionTurnLocks(),
		// No workload ceiling: the only budget in this run is the
		// roster's, so anything that stops the session is a bug rather
		// than a second cap firing.
		meters: newMeterPool(&workload.Bundle{}, specs, "", "test-model"),
		wds:    newWatchdogPool(watchdog.ModeWarn),
		obs:    observability.New(),
	}
	h.obs.Prime("(test)")
	h.store = transcript.NewStore(h.svc, appName)
	h.tracker = newTurnTracker(h.store, discardLogger(), h.obs, "(test)")

	root, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name:        "roster_root",
		Description: "roster test coordinator",
		Instruction: "coordinate",
		Model:       coord,
		SubAgents:   agents,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	h.runner, err = runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    h.svc,
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return h
}

// sessionText flattens what the session actually holds — text parts and
// the string arguments of every function call, which is where a refused
// Task specialist's report lives. Read off the session service rather
// than off the events runTurn happened to see, because the transcript is
// what an operator opens after the fact.
func sessionText(t *testing.T, h *turnHarness, sid string) string {
	t.Helper()
	resp, err := h.svc.Get(context.Background(), &adksession.GetRequest{
		AppName: appName, UserID: "mast-inject", SessionID: sid,
	})
	if err != nil {
		t.Fatalf("session Get(%q): %v", sid, err)
	}
	var b strings.Builder
	for ev := range resp.Session.Events().All() {
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			switch {
			case p == nil:
			case p.Text != "":
				b.WriteString(p.Text + "\n")
			case p.FunctionCall != nil:
				for _, v := range p.FunctionCall.Args {
					if s, ok := v.(string); ok {
						b.WriteString(s + "\n")
					}
				}
			}
		}
	}
	return b.String()
}

// The plan's acceptance test. The coordinator dispatches alpha, comes
// back for more, and finds alpha out of budget; the workload has to
// finish through beta.
//
// Both arms run the identical script. The uncapped one is not a fixture
// check but the counterfactual: it is what the roster does when nothing
// refuses, and the difference between the columns is the whole of the
// behaviour under test.
func TestASpentSpecialistClosesOnePathNotTheSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps map[string]specialists.Budget

		wantAlphaCalls int
		wantTrips      int
	}{
		{name: "no ceilings", caps: nil, wantAlphaCalls: 2, wantTrips: 0},
		// One call each: alpha's second dispatch cannot be served.
		{name: "alpha capped at one call",
			caps:           map[string]specialists.Budget{"alpha": {MaxTurns: 1}},
			wantAlphaCalls: 1, wantTrips: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coord := &routingModel{name: "coord", order: []string{"alpha", "alpha", "beta"}, tokens: 100}
			alpha := &routingModel{name: "alpha", tokens: 100}
			beta := &routingModel{name: "beta", tokens: 100}
			h := rosterHarness(t, coord, map[string]*routingModel{"alpha": alpha, "beta": beta}, tc.caps)

			const sid = "s-roster"
			if err := h.turn(context.Background(), sid); err != nil {
				t.Fatalf("the turn returned %v; a specialist's ceiling is not the session's", err)
			}

			if got := alpha.count(); got != tc.wantAlphaCalls {
				t.Errorf("alpha made %d model calls, want %d", got, tc.wantAlphaCalls)
			}
			// The other path is what "route around" means. Without this
			// the capped arm passes for a run that quietly stopped early.
			if got := beta.count(); got != 1 {
				t.Errorf("beta made %d model calls, want 1: the workload has to finish through the path that still has budget", got)
			}
			if got := tripCount(t, h.obs); got != tc.wantTrips {
				t.Errorf("mast_budget_trips_total = %d, want %d", got, tc.wantTrips)
			}

			m := h.meters.meter(sid)
			refusals, first := m.Refusals()
			sessionRefusals, _ := m.SessionRefusals()
			if sessionRefusals != 0 {
				t.Errorf("the workload itself was refused %d time(s) under no workload ceiling", sessionRefusals)
			}
			if len(m.PrecludedSession()) != 0 {
				t.Errorf("the session reports itself out of budget: %+v", m.PrecludedSession())
			}

			if tc.caps == nil {
				if refusals != 0 {
					t.Errorf("the uncapped roster was refused %d time(s): %v", refusals, first)
				}
				return
			}

			// The trip is on the meter, attributed.
			if refusals != 1 {
				t.Fatalf("the meter recorded %d refusals, want exactly 1", refusals)
			}
			if scope, ok := budget.Scope(first); !ok || scope != "alpha" {
				t.Errorf("the refusal is attributed to %q (scoped=%v), want \"alpha\"", scope, ok)
			}
			if len(m.Precluded("alpha")) == 0 {
				t.Error("alpha is not reported out of budget, so nothing tells an operator the roster is short a path")
			}
			if len(m.Precluded("beta")) != 0 {
				t.Errorf("beta is reported out of budget: %+v", m.Precluded("beta"))
			}

			// And in the transcript, with the arithmetic, naming alpha.
			text := sessionText(t, h, sid)
			if !strings.Contains(text, mastagent.RefusalMarker) {
				t.Errorf("the refusal is not in the transcript an operator reads:\n%s", text)
			}
			if !strings.Contains(text, "alpha") || !strings.Contains(text, "cap of 1") {
				t.Errorf("the transcript does not say whose ceiling stopped what, or by how much:\n%s", text)
			}
			if !strings.Contains(text, "handled by beta") {
				t.Errorf("the transcript does not show the work completing through the other path:\n%s", text)
			}
		})
	}
}
