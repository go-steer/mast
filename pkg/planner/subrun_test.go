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
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/planner"
)

// recordingObserver is a SubRunObserver that folds every sub-run event
// into a meter and remembers the session IDs and authors it was handed.
type recordingObserver struct {
	meter *budget.Meter

	mu       sync.Mutex
	sessions []string
	authors  []string
	events   int
	err      error
}

func (o *recordingObserver) ObserveSubRun(sessionID string, ev *session.Event) error {
	o.mu.Lock()
	o.events++
	o.sessions = append(o.sessions, sessionID)
	if ev != nil {
		o.authors = append(o.authors, ev.Author)
	}
	o.mu.Unlock()
	if o.err != nil {
		return o.err
	}
	if o.meter == nil {
		return nil
	}
	return o.meter.Observe(ev)
}

func (o *recordingObserver) snapshot() (sessions, authors []string, events int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.sessions...), append([]string(nil), o.authors...), o.events
}

// dispatchResult digs the invoke_specialist FunctionResponse out of a
// turn's events. There is exactly one per dispatch.
func dispatchResult(t *testing.T, events []*session.Event) map[string]any {
	t.Helper()
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
	t.Fatalf("no %s function response in %d events", planner.ToolInvokeSpecialist, len(events))
	return nil
}

// TestSubRunSpendReachesTheSessionMeter is the #226 regression: a
// specialist dispatched through invoke_specialist runs on a private
// runner, so through v0.4 its model calls were billed to nobody. With
// an observer wired, the same calls land in the same meter the outer
// stream feeds, under the OUTER session's ID.
//
// Fails before the fix for the plainest possible reason — there is no
// seam to wire, and the sub-run's tokens simply are not there.
func TestSubRunSpendReachesTheSessionMeter(t *testing.T) {
	spModel := &scriptedModel{name: "sp-model", script: specialistScript}
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "sp", "input": "x"}),
	)

	meter := budget.NewMeter(budget.Limits{RatePer1K: 1.0})
	obs := &recordingObserver{meter: meter}
	root, err := planner.NewRoot(planner.Config{
		Name:           "w",
		Model:          plModel,
		Specialists:    map[string]adkagent.Agent{"sp": buildSpecialist(t, "sp", spModel)},
		SubRunObserver: obs,
	})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, session.InMemoryService())

	for ev, err := range r.Run(context.Background(), "op", "outer-1",
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
		if berr := meter.Observe(ev); berr != nil {
			t.Fatalf("unexpected budget trip: %v", berr)
		}
	}

	_, _, calls := meter.Snapshot()
	plannerRounds := len(plModel.requests())
	specialistRounds := len(spModel.requests())
	if specialistRounds == 0 {
		t.Fatal("the specialist never ran; this test would pass for the wrong reason")
	}
	if want := plannerRounds + specialistRounds; calls != want {
		t.Errorf("metered calls = %d, want %d (%d planner rounds + %d specialist rounds)",
			calls, want, plannerRounds, specialistRounds)
	}

	sessions, authors, events := obs.snapshot()
	if events == 0 {
		t.Fatal("the observer saw no sub-run events at all")
	}
	for _, s := range sessions {
		// The sub-run's own session is an in-memory throwaway named
		// "invoke-sp". Billing to it would file the spend under a
		// session no operator can name.
		if s != "outer-1" {
			t.Fatalf("observed session id = %q, want the outer session %q", s, "outer-1")
		}
	}
	var sawSpecialist bool
	for _, a := range authors {
		if a == "sp" {
			sawSpecialist = true
		}
	}
	if !sawSpecialist {
		// Not cosmetic: pkg/budget keys per-specialist scopes on
		// Author, so a sub-run event authored by anything else would
		// silently skip the specialist's own ceiling.
		t.Errorf("no sub-run event authored by the specialist; authors = %v", authors)
	}
}

// TestSpecialistCeilingBindsOnThePlannerDoor is the other half of
// #226: because scopes key on Event.Author, folding sub-run events into
// the session meter makes a specialist's DECLARED ceiling bind on the
// planner's dispatch door, not just on a coordinator's.
//
// And it binds the better way. The crossing stops the sub-run and hands
// the planner a labelled partial; the outer turn keeps going and the
// planner still finishes. That is the shape pkg/budget's own doc says
// the event-stream seam cannot give.
func TestSpecialistCeilingBindsOnThePlannerDoor(t *testing.T) {
	spModel := &scriptedModel{name: "sp-model", script: specialistScript}
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "sp", "input": "x"}),
	)

	// No session ceiling at all — only the specialist's, and it is
	// below what one scripted call costs (120 tokens), so the
	// specialist's very first model event crosses it. A trip here can
	// only have come through the scope.
	meter := budget.New(budget.Config{
		Limits: budget.Limits{RatePer1K: 1.0},
		Scopes: map[string]budget.Limits{"sp": {MaxTokens: 50, RatePer1K: 1.0}},
	})
	obs := &recordingObserver{meter: meter}
	root, err := planner.NewRoot(planner.Config{
		Name:           "w",
		Model:          plModel,
		Specialists:    map[string]adkagent.Agent{"sp": buildSpecialist(t, "sp", spModel)},
		SubRunObserver: obs,
	})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, session.InMemoryService())

	var events []*session.Event
	for ev, err := range r.Run(context.Background(), "op", "outer-2",
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
		if berr := meter.Observe(ev); berr != nil {
			t.Fatalf("the session meter tripped; the specialist's scope was supposed to: %v", berr)
		}
		events = append(events, ev)
	}

	res := dispatchResult(t, events)
	if got := res["status"]; got != "halted" {
		t.Errorf("dispatch status = %v, want \"halted\" (result was %v)", got, res)
	}
	reason, _ := res["reason"].(string)
	if !strings.Contains(reason, budget.ErrExceeded.Error()) {
		t.Errorf("halt reason = %q, want it to name %q", reason, budget.ErrExceeded.Error())
	}
	// A cap that fires must not look like a broken tool: the planner
	// gets a result and finishes its turn.
	if len(plModel.requests()) < 2 {
		t.Errorf("planner rounds = %d, want >= 2 (the halt must not have killed the outer turn)",
			len(plModel.requests()))
	}
}

// TestSubRunObserverErrorStopsOnlyTheSubRun pins the containment
// directly, with an error that has nothing to do with budgets: the
// specialist is cut off, the planner is told why, and the turn lives.
func TestSubRunObserverErrorStopsOnlyTheSubRun(t *testing.T) {
	spModel := &scriptedModel{name: "sp-model", script: specialistScript}
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "sp", "input": "x"}),
	)

	obs := &recordingObserver{err: errors.New("host says stop")}
	root, err := planner.NewRoot(planner.Config{
		Name:           "w",
		Model:          plModel,
		Specialists:    map[string]adkagent.Agent{"sp": buildSpecialist(t, "sp", spModel)},
		SubRunObserver: obs,
	})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, session.InMemoryService())
	events := runPlanner(t, r, "outer-3", genai.NewContentFromText("work", genai.RoleUser))

	res := dispatchResult(t, events)
	if got := res["status"]; got != "halted" {
		t.Errorf("dispatch status = %v, want \"halted\"", got)
	}
	if reason, _ := res["reason"].(string); !strings.Contains(reason, "host says stop") {
		t.Errorf("halt reason = %q, want it to carry the observer's error", reason)
	}
	if _, ok := res["specialist"]; !ok {
		t.Error("a halted dispatch still has to say which specialist it was")
	}
}

// TestNilSubRunObserverDispatchesUnchanged: a caller with nothing to
// fold events into gets exactly the v0.4 behavior — a completed
// dispatch, no status key, no halt.
func TestNilSubRunObserverDispatchesUnchanged(t *testing.T) {
	spModel := &scriptedModel{name: "sp-model", script: specialistScript}
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "sp", "input": "x"}),
	)

	root, err := planner.NewRoot(planner.Config{
		Name:        "w",
		Model:       plModel,
		Specialists: map[string]adkagent.Agent{"sp": buildSpecialist(t, "sp", spModel)},
	})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, session.InMemoryService())
	events := runPlanner(t, r, "outer-4", genai.NewContentFromText("work", genai.RoleUser))

	res := dispatchResult(t, events)
	if _, ok := res["status"]; ok {
		t.Errorf("unobserved dispatch reported status %v; the success shape must not change", res["status"])
	}
	if res["result"] == nil {
		t.Errorf("unobserved dispatch returned no result: %v", res)
	}
	if calls, ok := res["sub_model_calls"].(int); ok && calls == 0 {
		t.Error("sub_model_calls = 0; the specialist did not run")
	}
}
