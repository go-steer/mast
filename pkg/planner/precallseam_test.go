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

package planner_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/planner"
)

// W10.1's spike. A cost ceiling that refuses a call has to run BEFORE
// the call is issued, and mast has exactly two places to put such a
// check: an llmagent BeforeModelCallback, or a model.LLM wrapper
// installed at internal/compose.BuildModel. The v0.6 plan picked the
// wrapper on the reasoning that it "travels with the model and
// therefore reaches a dispatched specialist for free", and flagged the
// callback as agent-scoped and unverified across that boundary.
//
// These tests measure both seams on the one shape that discriminates
// them — a planner dispatch — because the plan's reasoning turns out to
// be true of the wrong axis. Reachability is not what separates them.
// Both reach. Identity is.
//
// What the six findings below settle, and what W10.2 and W10.3 are then
// built on:
//
//  1. The callback DOES fire inside a dispatch. Runner plugins are what
//     the boundary drops; agent callbacks ride in the agent object.
//  2. Both seams can name the agent — but the callback is handed
//     agent.Context as a declared parameter, while the wrapper recovers
//     it by asserting on a context.Context ADK is under no interface
//     obligation to populate.
//  3. Neither seam knows the OUTER session inside a dispatch, so a
//     pre-call check cannot resolve its meter from ctx.SessionID()
//     there. Same answer as #226 and W9.3: bind at the dispatch door.
//  4. One model.LLM object serves many agents, because compose caches
//     models by name. A wrapper is per-model; a ceiling is per-agent.
//  5. A refusal must be a synthesized response. Returned as an error it
//     reaches the caller in the field ADK uses for a broken tool.
//  6. Outside a dispatch the callback sees the operator's real session,
//     which is why it is sufficient on its own everywhere else.
//
// The seam is the BeforeModelCallback. Its one weakness is that it
// fails OPEN if a construction site forgets to install it — a finite,
// enumerable wiring problem, unlike the wrapper's reliance on an
// undocumented property of a dependency.

// seamSighting is one observation of a pre-call seam firing.
type seamSighting struct {
	agentName string
	sessionID string
	branch    string
	model     string
}

// callbackSeam records what a BeforeModelCallback can see. It never
// short-circuits: returning a response here would skip the model, and
// this spike is measuring visibility rather than refusal.
type callbackSeam struct {
	mu   sync.Mutex
	seen []seamSighting
}

func (s *callbackSeam) callback() llmagent.BeforeModelCallback {
	return func(ctx adkagent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		sighting := seamSighting{
			agentName: ctx.AgentName(),
			sessionID: ctx.SessionID(),
			branch:    ctx.Branch(),
		}
		if req != nil {
			sighting.model = req.Model
		}
		s.seen = append(s.seen, sighting)
		return nil, nil
	}
}

func (s *callbackSeam) sightings() []seamSighting {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seamSighting(nil), s.seen...)
}

// wrapperSeam is the other candidate: a model.LLM decorator. It sees
// every call the model it wraps is asked to make, from wherever.
type wrapperSeam struct {
	inner model.LLM

	mu   sync.Mutex
	seen []seamSighting
}

func (w *wrapperSeam) Name() string { return w.inner.Name() }

func (w *wrapperSeam) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	w.mu.Lock()
	sighting := seamSighting{}
	if req != nil {
		sighting.model = req.Model
	}
	// Everything a wrapper could possibly learn about *who* is calling
	// has to come off the context it was handed. ADK's agent.Context
	// satisfies context.Context, so if the flow passed it down intact a
	// type assertion would recover the identity. This is the whole
	// question, so it is asked rather than assumed.
	if ac, ok := ctx.(adkagent.ReadonlyContext); ok {
		sighting.agentName = ac.AgentName()
		sighting.sessionID = ac.SessionID()
		sighting.branch = ac.Branch()
	}
	w.seen = append(w.seen, sighting)
	w.mu.Unlock()
	return w.inner.GenerateContent(ctx, req, stream)
}

func (w *wrapperSeam) sightings() []seamSighting {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]seamSighting(nil), w.seen...)
}

// specialistWithCallback builds a Task specialist carrying a
// BeforeModelCallback, which buildSpecialist does not.
func specialistWithCallback(t *testing.T, name string, m model.LLM, cb llmagent.BeforeModelCallback) adkagent.Agent {
	t.Helper()
	a, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:                 name,
		Description:          name + " (test specialist)",
		Instruction:          "test",
		Model:                m,
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{cb},
	})
	if err != nil {
		t.Fatalf("NewTaskAgent(%q): %v", name, err)
	}
	return a
}

// runOneDispatch drives a planner turn that dispatches to "sp" once.
func runOneDispatch(t *testing.T, sp adkagent.Agent, plModel *scriptedModel) {
	t.Helper()
	plModel.script = planScript(plModel,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "sp", "input": "x"}),
	)
	root, err := planner.NewRoot(planner.Config{
		Name:        "w",
		Model:       plModel,
		Specialists: map[string]adkagent.Agent{"sp": sp},
	})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, session.InMemoryService())
	for _, err := range r.Run(context.Background(), "op", "outer-1",
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
	}
}

// Finding 1: the plan's reachability premise is wrong, in the
// callback's favour.
//
// The plan assumed a BeforeModelCallback is "agent-scoped and needs
// checking against the dispatch path". It is agent-scoped, and that is
// exactly why it crosses: invoke_specialist builds a private RUNNER,
// but newDispatcher wraps the specialist AGENT that was built at
// startup, callbacks and all. Runner plugins are what the boundary
// drops (#235); agent callbacks ride along in the object.
//
// So reachability does not discriminate the two seams. Both reach.
func TestABeforeModelCallbackFiresInsideAPlannerDispatch(t *testing.T) {
	seam := &callbackSeam{}
	spModel := &scriptedModel{name: "sp-model", script: specialistScript}
	sp := specialistWithCallback(t, "sp", spModel, seam.callback())

	runOneDispatch(t, sp, &scriptedModel{name: "pl-model"})

	if len(spModel.requests()) == 0 {
		t.Fatal("the specialist never called its model; this test would pass for the wrong reason")
	}
	sightings := seam.sightings()
	if len(sightings) == 0 {
		t.Fatal("the BeforeModelCallback never fired inside the dispatch; " +
			"a pre-call check installed there would not bind on a planner-dispatched specialist")
	}
	if got, want := len(sightings), len(spModel.requests()); got != want {
		t.Errorf("callback fired %d time(s) for %d model call(s); a pre-call check has to see every call", got, want)
	}
}

// Finding 2: both seams can name the agent, but only one of them is
// promised it.
//
// pkg/budget keys per-specialist scopes on the agent name, so a
// pre-call seam that cannot name the agent cannot enforce a
// per-specialist ceiling — the entire subject of W10.3. The expectation
// going in was that a model.LLM wrapper could not: its signature is
// (context.Context, *LLMRequest, bool), and LLMRequest carries Model,
// Contents, Config and Tools — no agent, no session.
//
// Measured, that is wrong. ADK hands GenerateContent its
// agent.InvocationContext as the ctx argument
// (internal/llminternal/base_flow.go generateContent), so a wrapper
// recovers the full identity with a type assertion.
//
// The seams therefore differ in the STRENGTH of the guarantee, not in
// what they can see. model.LLM's parameter is declared context.Context;
// the identity is an implementation detail of the caller, unmentioned
// in the interface mast codes against, and an ADK bump could pass a
// plain context without breaking a single signature. BeforeModelCallback
// takes agent.Context as its declared first parameter. One is a
// contract and the other is a coincidence that currently holds.
//
// This test pins the coincidence rather than relying on it: if ADK ever
// stops threading the context, the wrapper option dies loudly here
// instead of quietly fail-opening in whatever gets built on it.
func TestBothSeamsCanNameTheAgentButOnlyOneIsPromisedIt(t *testing.T) {
	seam := &callbackSeam{}
	spModel := &scriptedModel{name: "sp-model", script: specialistScript}
	wrapped := &wrapperSeam{inner: spModel}
	sp := specialistWithCallback(t, "sp", wrapped, seam.callback())

	runOneDispatch(t, sp, &scriptedModel{name: "pl-model"})

	callbacks, wrappers := seam.sightings(), wrapped.sightings()
	if len(callbacks) == 0 || len(wrappers) == 0 {
		t.Fatalf("both seams must fire to be compared: %d callback(s), %d wrapper call(s)",
			len(callbacks), len(wrappers))
	}

	// The callback names the specialist, from a declared parameter.
	if got := callbacks[0].agentName; got != "sp" {
		t.Errorf("BeforeModelCallback saw agent %q, want %q — the scope key", got, "sp")
	}

	// The wrapper names it too, from an assertion on a context ADK is
	// under no interface obligation to pass.
	if got := wrappers[0].agentName; got != "sp" {
		t.Errorf("the model.LLM wrapper could not recover the agent (saw %q): ADK stopped "+
			"threading agent.InvocationContext into GenerateContent. W10.1 recorded this as "+
			"the wrapper's weak point and it has now given way — the callback is the seam", got)
	}
}

// Finding 3: neither seam can name the session it should bill, and the
// callback is wrong in a way that would be worse than silent.
//
// invoke_specialist runs its sub-runner with AutoCreateSession against
// an in-memory service under the session id "invoke-<name>". So inside
// a dispatch ctx.SessionID() is the throwaway, not the operator's
// session. mast keeps one meter per session in a pool keyed by exactly
// that id, so a pre-call check that resolved its meter from the
// callback context would, inside a dispatch, mint a fresh empty meter
// and cheerfully approve every call — enforcing a ceiling against a
// budget that starts at zero each time.
//
// This is the same problem #226 solved for accounting and W9.3 solved
// for recording, and it has the same answer: bind the outer session at
// the DISPATCH DOOR, where the tool context still belongs to the outer
// turn, and hand it inward. The SubRunObserver seam already does this
// and TestSubRunSpendReachesTheSessionMeter pins it.
func TestNeitherSeamKnowsTheOuterSessionInsideADispatch(t *testing.T) {
	seam := &callbackSeam{}
	spModel := &scriptedModel{name: "sp-model", script: specialistScript}
	wrapped := &wrapperSeam{inner: spModel}
	sp := specialistWithCallback(t, "sp", wrapped, seam.callback())

	runOneDispatch(t, sp, &scriptedModel{name: "pl-model"})

	for _, s := range append(seam.sightings(), wrapped.sightings()...) {
		if s.sessionID == "outer-1" {
			t.Fatal("a pre-call seam saw the OUTER session id inside a dispatch; " +
				"it could then resolve its meter directly and W10.2 gets simpler — recheck the design")
		}
		if s.sessionID == "" {
			t.Errorf("seam saw an empty session id; expected the sub-run's own throwaway")
		}
	}
	// Named, so the fix is obvious from the failure: the sub-runner
	// calls Run with "invoke-"+specialist.
	for _, s := range seam.sightings() {
		if s.sessionID != "invoke-sp" {
			t.Errorf("callback saw session %q inside the dispatch, want %q", s.sessionID, "invoke-sp")
		}
	}
}

// Finding 4, and the one that actually decides W10.1: a model.LLM
// wrapper is per-MODEL and a ceiling is per-AGENT.
//
// compose's per-tier builder caches by model name, so every specialist
// resolving to the same id shares one model.LLM object — and an
// un-tiered roster shares the root's. One wrapper therefore serves N
// agents and can only tell them apart through the context assertion
// finding 2 measured. A BeforeModelCallback is installed per agent at
// construction, so the identity is structural: the closure knows which
// specialist it was built for whether or not ADK threads anything.
//
// The wrapper's compensating strength is coverage — it cannot be
// forgotten at a construction site, and a pre-call refusal that misses
// a site fails open. That is the real trade, and it is a wiring problem
// with a finite, enumerable answer rather than a contract problem.
func TestOneModelObjectServesManyAgents(t *testing.T) {
	shared := &wrapperSeam{inner: &scriptedModel{name: "sp-model", script: specialistScript}}
	seam := &callbackSeam{}

	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "sp", "input": "x"}),
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "sp2", "input": "y"}),
	)
	root, err := planner.NewRoot(planner.Config{
		Name:  "w",
		Model: plModel,
		Specialists: map[string]adkagent.Agent{
			"sp":  specialistWithCallback(t, "sp", shared, seam.callback()),
			"sp2": specialistWithCallback(t, "sp2", shared, seam.callback()),
		},
	})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, session.InMemoryService())
	for _, err := range r.Run(context.Background(), "op", "outer-1",
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
	}

	agents := map[string]bool{}
	for _, s := range shared.sightings() {
		agents[s.agentName] = true
	}
	if len(agents) < 2 {
		t.Fatalf("one wrapper saw %d distinct agent(s) (%v); the test needs two specialists "+
			"sharing a model for the point to be measured", len(agents), agents)
	}
	// The claim: identity is not a property of the wrapper, it is a
	// property of each call passing through it.
	if got := len(shared.sightings()); got < 2 {
		t.Errorf("the shared model was called %d time(s); expected both specialists to use it", got)
	}
}

// Finding 5, which settles the other half of W10.1: a refusal must be a
// synthesized response, not an error.
//
// A BeforeModelCallback can stop a call two ways — return an error, or
// return a response — and ADK skips the provider for both, which is the
// property that makes either one a valid PRE-call check. They are not
// interchangeable from the caller's chair, and W10.3 turns entirely on
// the difference.
//
// Measured on the dispatch path, the error shape reaches the planner as
//
//	{"error": "invoke_specialist \"sp\": workflow: dynamic child
//	 sp_dispatch@1/dispatch@1/sp@1: workflow: dynamic child failed:
//	 budget: refused before the call"}
//
// The reason survives, but it arrives in the field ADK uses for a
// broken tool, wrapped in three layers of workflow plumbing. That is
// precisely the shape dispatch.go already refused to emit for a crossed
// cap: "Not an error return: a cap that fires must not look like a
// broken tool." A pre-call ceiling that returns an error would
// contradict, from one layer down, the decision the halt path made.
//
// The synthesized shape reaches it as an ordinary report, and
// sub_model_calls: 0 is the pre-call property showing up in the
// planner's own view of the dispatch.
//
// The shape is not invented here either. pkg/agent's FinishOnStall
// already ships it: a Task agent's silent turn becomes a finish_task
// call so the specialist costs its own part of the answer rather than
// the whole run. A cost refusal is the same move with a different
// reason attached.
func TestARefusalIsASynthesizedResponseNotAnError(t *testing.T) {
	refuseByError := func(adkagent.Context, *model.LLMRequest) (*model.LLMResponse, error) {
		return nil, errors.New("budget: refused before the call")
	}
	refuseByResponse := func(_ adkagent.Context, _ *model.LLMRequest) (*model.LLMResponse, error) {
		return &model.LLMResponse{Content: &genai.Content{
			Role: genai.RoleModel,
			// "result" because a Task agent with no OutputSchema gets
			// ADK's own single-string schema, and ADK validates an
			// injected call exactly as it validates a model-issued
			// one. Same constraint FinishOnStall documents on
			// StallPayload.
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				Name: mastagent.FinishTaskToolName,
				Args: map[string]any{"result": "stopped: cost ceiling"},
			}}},
		}}, nil
	}

	for _, tc := range []struct {
		name   string
		refuse llmagent.BeforeModelCallback
		// check reads the invoke_specialist response the planner was
		// handed and states what kind of thing it is.
		check func(t *testing.T, resp map[string]any)
	}{{
		name:   "an error arrives as a broken tool",
		refuse: refuseByError,
		check: func(t *testing.T, resp map[string]any) {
			errText, isErr := resp["error"].(string)
			if !isErr {
				t.Fatalf("an erroring callback did not produce an error response: %#v", resp)
			}
			if _, ok := resp["result"]; ok {
				t.Errorf("the error response also carried a result: %#v", resp)
			}
			// The planner cannot distinguish this from a tool that
			// crashed, which is the whole objection.
			if !strings.Contains(errText, "dynamic child failed") {
				t.Errorf("error text %q no longer carries ADK's workflow plumbing; "+
					"if the reason now arrives clean, W10.1's argument for the "+
					"synthesized shape is weaker and should be re-read", errText)
			}
		},
	}, {
		name:   "a synthesized response arrives as a report",
		refuse: refuseByResponse,
		check: func(t *testing.T, resp map[string]any) {
			if _, isErr := resp["error"]; isErr {
				t.Fatalf("a synthesized refusal looked like a failure: %#v", resp)
			}
			if got := resp["specialist"]; got != "sp" {
				t.Errorf("response does not name the specialist: %#v", resp)
			}
			// The pre-call property, visible to the planner itself: the
			// dispatch reports zero model calls because none was made.
			// Compared as text: the response round-trips through JSON on
			// its way to the model, so the count may arrive as a
			// float64 rather than the int dispatch.go put in.
			if got := fmt.Sprint(resp["sub_model_calls"]); got != "0" {
				t.Errorf("dispatch reported %v model calls, want 0 — a pre-call "+
					"refusal must not reach the provider", got)
			}
			if !strings.Contains(fmt.Sprint(resp["result"]), "cost ceiling") {
				t.Errorf("the reason did not survive into the planner's result: %#v", resp)
			}
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			spModel := &scriptedModel{name: "sp-model", script: specialistScript}
			sp := specialistWithCallback(t, "sp", spModel, tc.refuse)

			plModel := &scriptedModel{name: "pl-model"}
			plModel.script = planScript(plModel,
				callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "sp", "input": "x"}),
			)
			root, err := planner.NewRoot(planner.Config{
				Name: "w", Model: plModel,
				Specialists: map[string]adkagent.Agent{"sp": sp},
			})
			if err != nil {
				t.Fatalf("NewRoot: %v", err)
			}
			r := newRunner(t, root, session.InMemoryService())

			var events []*session.Event
			var runErr error
			for ev, err := range r.Run(context.Background(), "op", "outer-1",
				genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
				if err != nil {
					runErr = err
					break
				}
				events = append(events, ev)
			}

			// Both shapes are genuinely pre-call. This is the property
			// W10.2's acceptance test reads off the meter: the provider
			// was never reached, so there is no spend to fold and no
			// phantom ledger row.
			if got := len(spModel.requests()); got != 0 {
				t.Errorf("the specialist's model was called %d time(s), want 0", got)
			}

			// Neither shape kills the outer turn. The session stop
			// W10.3 is about does not live on this path at all — it
			// lives in the post-hoc fold on the coordinator path, which
			// cancels the run. Recorded so W10.3 is not written against
			// the wrong path.
			if runErr != nil {
				t.Fatalf("a refused specialist ended the operator's run: %v", runErr)
			}
			resp := dispatchResponse(events)
			if resp == nil {
				t.Fatal("the planner was handed no invoke_specialist response at all")
			}
			tc.check(t, resp)
		})
	}
}

// dispatchResponse returns the invoke_specialist FunctionResponse the
// planner was handed, which is the only thing about a refusal the
// planner can actually act on.
func dispatchResponse(events []*session.Event) map[string]any {
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == planner.ToolInvokeSpecialist {
				return p.FunctionResponse.Response
			}
		}
	}
	return nil
}

// The counter-case, so finding 3 is read as a statement about the
// dispatch boundary rather than about callbacks. Outside a dispatch the
// callback sees the operator's own session, which is why the seam is
// sufficient on the coordinator and graph paths on its own.
func TestOutsideADispatchTheCallbackSeesTheRealSession(t *testing.T) {
	seam := &callbackSeam{}
	// A Chat-mode coordinator, because a Task agent cannot be a runner
	// root — and because the coordinator path is where W10.2's check
	// does its ordinary work.
	co, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name:                 "sp",
		Description:          "test coordinator",
		Instruction:          "test",
		Model:                &scriptedModel{name: "sp-model", script: specialistScript},
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{seam.callback()},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	r := newRunner(t, co, session.InMemoryService())
	for _, err := range r.Run(context.Background(), "op", "outer-1",
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
	}

	sightings := seam.sightings()
	if len(sightings) == 0 {
		t.Fatal("the callback never fired")
	}
	for _, s := range sightings {
		if s.sessionID != "outer-1" {
			t.Errorf("callback saw session %q, want %q — a pre-call check on the "+
				"non-dispatch paths resolves its meter from here", s.sessionID, "outer-1")
		}
		if s.agentName != "sp" {
			t.Errorf("callback saw agent %q, want %q", s.agentName, "sp")
		}
	}
}
