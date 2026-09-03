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

package agent_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"

	mastagent "github.com/go-steer/mast/pkg/agent"
)

// countingModel answers every request and counts how many it was asked
// to make. The count is the only thing these tests really assert: a
// pre-call refusal that still reaches the provider has bought nothing,
// however good the response looks.
type countingModel struct {
	name string
	// delegate, when set, names a sub-agent tool this model calls once
	// before answering. ADK exposes a Task sub-agent under its own
	// name, so this is the specialist's name.
	delegate string

	mu    sync.Mutex
	calls int
}

func (m *countingModel) Name() string { return m.name }

func (m *countingModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {
		// A Task agent has to finish or ADK ends the caller's turn; a
		// Chat agent delegates once if it was told to, then answers.
		if _, ok := req.Tools[mastagent.FinishTaskToolName]; ok {
			yield(call(mastagent.FinishTaskToolName, map[string]any{"result": "did the work"}), nil)
			return
		}
		if m.delegate != "" && !answered(req, m.delegate) {
			yield(call(m.delegate, map[string]any{"request": "go"}), nil)
			return
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText("answered", genai.RoleModel)}, nil)
	}
}

func call(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)},
	}}
}

// answered reports whether name has already returned into this request's
// history, so a scripted coordinator delegates once rather than forever.
func answered(req *model.LLMRequest, name string) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				return true
			}
		}
	}
	return false
}

func (m *countingModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// stubGate refuses every call by the named agents and permits the rest.
type stubGate struct {
	refuse map[string]string

	mu    sync.Mutex
	asked []string
}

func (g *stubGate) Allow(agentName string) error {
	g.mu.Lock()
	g.asked = append(g.asked, agentName)
	g.mu.Unlock()
	if reason, ok := g.refuse[agentName]; ok {
		return errors.New(reason)
	}
	return nil
}

func (g *stubGate) questions() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.asked...)
}

// runWith drives one turn through root, optionally with a gate on the
// context, and returns the events it produced.
func runWith(t *testing.T, root adkagent.Agent, gate mastagent.CallGate) []*session.Event {
	t.Helper()
	r, err := runner.New(runner.Config{
		AppName:           "precall-test",
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	ctx := mastagent.WithCallGate(context.Background(), gate)
	var out []*session.Event
	for ev, err := range r.Run(ctx, "user", "s1",
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

// transcript flattens every text part and finish_task argument a turn
// produced, which is what an operator reading the session sees.
func transcript(events []*session.Event) string {
	var b strings.Builder
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
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

func taskAgent(t *testing.T, name string, m model.LLM, cbs ...llmagent.BeforeModelCallback) adkagent.Agent {
	t.Helper()
	a, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name: name, Description: name, Instruction: "do the work",
		Model:                m,
		BeforeModelCallbacks: cbs,
	})
	if err != nil {
		t.Fatalf("NewTaskAgent(%q): %v", name, err)
	}
	return a
}

func coordinator(t *testing.T, name string, m model.LLM, subs ...adkagent.Agent) adkagent.Agent {
	t.Helper()
	a, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name: name, Description: name, Instruction: "coordinate",
		Model: m, SubAgents: subs,
	})
	if err != nil {
		t.Fatalf("NewCoordinator(%q): %v", name, err)
	}
	return a
}

// The load-bearing negative. An embedder who never asked for a budget
// must not have one imposed, and a host that meters some turns and not
// others must not have the unmetered ones refused. The gate is absent,
// not permissive-by-configuration, and absence permits.
func TestAnUngatedTurnRunsUnchanged(t *testing.T) {
	m := &countingModel{name: "m"}
	runWith(t, coordinator(t, "root", m), nil)
	if got := m.count(); got != 1 {
		t.Errorf("an ungated turn made %d model calls, want 1", got)
	}
}

func TestAPermittingGateChangesNothingButIsAsked(t *testing.T) {
	m := &countingModel{name: "m"}
	gate := &stubGate{}
	runWith(t, coordinator(t, "root", m), gate)

	if got := m.count(); got != 1 {
		t.Errorf("a permitting gate changed the call count to %d, want 1", got)
	}
	if got := gate.questions(); len(got) != 1 || got[0] != "root" {
		t.Errorf("the gate was asked %v, want exactly one question naming \"root\" — "+
			"the name it is asked under has to be the one budget scopes are keyed on", got)
	}
}

// The whole point, on the simplest shape: a refused call is not made.
func TestARefusedCoordinatorMakesNoModelCallAndSaysWhy(t *testing.T) {
	m := &countingModel{name: "m"}
	gate := &stubGate{refuse: map[string]string{"root": "$1.02 already at cap $1.00"}}
	events := runWith(t, coordinator(t, "root", m), gate)

	if got := m.count(); got != 0 {
		t.Fatalf("a refused agent still called its model %d time(s)", got)
	}
	text := transcript(events)
	if !mastagent.Refused(strings.TrimSpace(text)) {
		t.Errorf("the turn's transcript is not countable as a refusal:\n%s", text)
	}
	// The arithmetic reaches the operator, not just the fact of a stop.
	// A transcript that says "stopped" without saying by how much sends
	// the reader to the meter to find out whether to raise the cap.
	if !strings.Contains(text, "already at cap $1.00") {
		t.Errorf("the gate's reason did not reach the transcript:\n%s", text)
	}
	if !strings.Contains(text, "root") {
		t.Errorf("the refusal does not name the agent it stopped:\n%s", text)
	}
}

// A Task specialist reports through finish_task rather than answering
// in text, which is what makes a refusal something the caller above it
// can act on. This is the shape W10.3 routes around.
func TestARefusedTaskAgentReportsThroughFinishTask(t *testing.T) {
	// The control first: without the refusal the specialist really is
	// reached, so a zero count below means the gate stopped it rather
	// than the script never getting there.
	ctrlSp := &countingModel{name: "sp"}
	runWith(t, coordinator(t, "root", &countingModel{name: "coord", delegate: "sp"},
		taskAgent(t, "sp", ctrlSp)), &stubGate{})
	if ctrlSp.count() == 0 {
		t.Fatal("the coordinator never delegated even with nothing refused; " +
			"this test would pass for the wrong reason")
	}

	coordModel := &countingModel{name: "coord", delegate: "sp"}
	spModel := &countingModel{name: "sp"}
	root := coordinator(t, "root", coordModel, taskAgent(t, "sp", spModel))
	gate := &stubGate{refuse: map[string]string{"sp": "specialist cap"}}

	events := runWith(t, root, gate)

	if got := spModel.count(); got != 0 {
		t.Fatalf("the refused specialist called its model %d time(s)", got)
	}
	// The coordinator is not refused, so it still runs — a specialist's
	// ceiling is not the workload's.
	if got := coordModel.count(); got == 0 {
		t.Error("the coordinator made no calls; a specialist's refusal must not stop the root")
	}

	var reported bool
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.Name == mastagent.FinishTaskToolName {
				reported = true
				if s, _ := p.FunctionCall.Args["result"].(string); !mastagent.Refused(s) {
					t.Errorf("finish_task carried %q, which Refused does not recognise", s)
				}
			}
		}
	}
	if !reported {
		t.Error("the refused specialist did not report through finish_task, so the " +
			"caller above it has nothing to route around")
	}
}

// The un-forgettability claim, and the reason the constructors install
// this rather than the eleven call sites.
//
// W10.1 chose a BeforeModelCallback over a model.LLM wrapper knowing
// the callback's weakness was that a site could omit it and fail open.
// This asserts the weakness was closed rather than merely noted: an
// agent built with no callbacks at all is still gated.
func TestEveryConstructorGatesWithoutBeingAsked(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		build func(m model.LLM) (adkagent.Agent, error)
	}{{
		mode: "task",
		build: func(m model.LLM) (adkagent.Agent, error) {
			return mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
				Name: "a", Description: "d", Instruction: "i", Model: m})
		},
	}, {
		mode: "chat",
		build: func(m model.LLM) (adkagent.Agent, error) {
			return mastagent.NewCoordinator(mastagent.CoordinatorConfig{
				Name: "a", Description: "d", Instruction: "i", Model: m})
		},
	}, {
		mode: "single_turn",
		build: func(m model.LLM) (adkagent.Agent, error) {
			return mastagent.NewSingleTurnAgent(mastagent.SingleTurnAgentConfig{
				Name: "a", Description: "d", Instruction: "i", Model: m})
		},
	}} {
		t.Run(tc.mode, func(t *testing.T) {
			// A Task agent cannot be a runner root, and a SingleTurn
			// agent is not one either. Both are reached as sub-agents
			// of a coordinator the gate permits, so the only thing
			// refused is the agent under test.
			build := func(t *testing.T, m model.LLM) adkagent.Agent {
				t.Helper()
				a, err := tc.build(m)
				if err != nil {
					t.Fatalf("build: %v", err)
				}
				if tc.mode == "chat" {
					return a
				}
				return coordinator(t, "root", &countingModel{name: "root-m", delegate: "a"}, a)
			}

			// The control. Without it, "made zero calls" is equally
			// consistent with the agent never having been reached.
			ctrl := &countingModel{name: "m"}
			runWith(t, build(t, ctrl), &stubGate{})
			if ctrl.count() == 0 {
				t.Fatalf("a permitted %s agent was never called; the fixture does not "+
					"reach it and the assertion below would pass for the wrong reason", tc.mode)
			}

			m := &countingModel{name: "m"}
			runWith(t, build(t, m), &stubGate{refuse: map[string]string{"a": "no budget"}})

			if got := m.count(); got != 0 {
				t.Errorf("a %s agent built with no BeforeModelCallbacks made %d model "+
					"call(s) under a refusing gate; the constructor did not install the "+
					"gate and this seam fails open", tc.mode, got)
			}
		})
	}
}

// Ordering. A caller's own callback must not be able to run ahead of
// the ceiling and short-circuit past it — that would be a supported way
// to spend money the gate refused.
func TestTheGateIsAskedBeforeACallersOwnCallback(t *testing.T) {
	var order []string
	own := func(adkagent.Context, *model.LLMRequest) (*model.LLMResponse, error) {
		order = append(order, "caller")
		return nil, nil
	}
	gate := &gateRecorder{order: &order}

	m := &countingModel{name: "m"}
	root, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name: "root", Description: "d", Instruction: "i", Model: m,
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{own},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	runWith(t, root, gate)

	if len(order) != 2 || order[0] != "gate" || order[1] != "caller" {
		t.Errorf("callback order was %v, want [gate caller]", order)
	}
}

type gateRecorder struct{ order *[]string }

func (g *gateRecorder) Allow(string) error {
	*g.order = append(*g.order, "gate")
	return nil
}

// A refusal has to be distinguishable from a stall, because they call
// for different operator actions: a stall is a model that went quiet
// and may work on retry, while a refusal is a budget decision that will
// repeat until a ceiling moves.
func TestARefusalIsNotAStall(t *testing.T) {
	refusal := mastagent.RefusalText("sp", "cap")
	stall := mastagent.StallText("sp", "last words")

	if !mastagent.Refused(refusal) {
		t.Error("Refused does not recognise its own text")
	}
	if mastagent.Stalled(refusal) {
		t.Error("a refusal counts as a stall; the two would be filed together")
	}
	if mastagent.Refused(stall) {
		t.Error("a stall counts as a refusal")
	}
	if !mastagent.Stalled(stall) {
		t.Error("Stalled does not recognise its own text")
	}
}

func TestTheDefaultPayloadCarriesTheMarkerWhereAReaderSeesIt(t *testing.T) {
	args := mastagent.DefaultRefusalPayload("sp", "$2 over $1")
	got, _ := args["result"].(string)
	if !mastagent.Refused(got) {
		t.Errorf("default payload %q is not countable", got)
	}
	if !strings.Contains(got, "$2 over $1") {
		t.Errorf("default payload dropped the reason: %q", got)
	}
}
