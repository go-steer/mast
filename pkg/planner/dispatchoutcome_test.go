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
	"iter"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/planner"
)

// rejectingModel yields an error instead of a response, which is what a
// provider rejection looks like from inside a run: the iterator's error
// leg, no event, nothing for a sink watching the stream to observe.
//
// scriptedModel cannot do this — it always yields (resp, nil), the same
// shape internal/evals' rig has — which is the reason a dispatch lost to
// a provider went unnoticed for so long: no test in the tree could
// produce one.
type rejectingModel struct {
	name string
	err  error

	mu    sync.Mutex
	calls int
}

func (m *rejectingModel) Name() string { return m.name }

func (m *rejectingModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		m.calls++
		m.mu.Unlock()
		yield(nil, m.err)
	}
}

func (m *rejectingModel) rounds() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// dispatchWith runs one planner turn that dispatches to a specialist
// driven by spModel, and returns the sink outcomes the observer
// collected.
func dispatchWith(t *testing.T, obs *recordingObserver, spModel model.LLM) []planner.DispatchOutcome {
	t.Helper()
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "sp", "input": "x"}),
	)
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
	for _, err := range r.Run(context.Background(), "op", "outer-1",
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			// The outer turn surviving a dead dispatch is the premise of
			// #452, not an accident: if this ever fires, the failure is
			// already visible and the rest of this file is moot.
			t.Fatalf("the OUTER turn failed, which is not the shape #452 is about: %v", err)
		}
	}
	return obs.closeOutcomes()
}

// TestASubRunnerFailureReachesTheSinkThatWasWatchingIt is the #452
// regression. A provider rejection inside invoke_specialist ends the
// sub-run without producing an event, so a sink watching the dispatch
// saw a stream that simply stopped — indistinguishable from a
// specialist that finished quickly.
//
// Fails before the fix at the compiler: Close took no argument, so
// there was nothing for this to read. Before that signature existed the
// failure had no record anywhere in the process — no log line, no
// metric, no attach frame.
func TestASubRunnerFailureReachesTheSinkThatWasWatchingIt(t *testing.T) {
	spModel := &rejectingModel{
		name: "sp-model",
		err:  errors.New("googleapi: Error 429: Resource has been exhausted (e.g. check quota)., RESOURCE_EXHAUSTED"),
	}
	obs := &recordingObserver{}

	outcomes := dispatchWith(t, obs, spModel)

	if spModel.rounds() == 0 {
		t.Fatal("the specialist's model was never called; this test would pass for the wrong reason")
	}
	if len(outcomes) != 1 {
		t.Fatalf("sink closed %d times, want exactly 1", len(outcomes))
	}
	out := outcomes[0]
	if out.Err == nil {
		t.Fatal("DispatchOutcome.Err is nil: the sink was told the dispatch ended and not that it failed, " +
			"which is the silence #452 is about")
	}
	if !strings.Contains(out.Err.Error(), "RESOURCE_EXHAUSTED") {
		// The provider's own text is what a host classifies on. A
		// wrapped-away cause would leave every failure looking alike.
		t.Errorf("DispatchOutcome.Err = %q, want the provider's rejection text", out.Err)
	}
	if out.Halted != nil {
		t.Errorf("DispatchOutcome.Halted = %v, want nil: nothing halted this dispatch, it broke", out.Halted)
	}
}

// TestACompletedDispatchReportsNeitherFailureNorHalt is the floor under
// the test above: a zero DispatchOutcome has to mean "it worked", or
// every host reporting on Err would report on every dispatch.
func TestACompletedDispatchReportsNeitherFailureNorHalt(t *testing.T) {
	obs := &recordingObserver{}
	outcomes := dispatchWith(t, obs, &scriptedModel{name: "sp-model", script: specialistScript})
	if len(outcomes) != 1 {
		t.Fatalf("sink closed %d times, want exactly 1", len(outcomes))
	}
	if out := outcomes[0]; out.Err != nil || out.Halted != nil {
		t.Errorf("DispatchOutcome = {Err: %v, Halted: %v}, want both nil on a dispatch that completed",
			out.Err, out.Halted)
	}
}

// TestAHaltedDispatchIsReportedAsAHaltAndNotAFailure keeps the two
// apart. A halt is the host's own decision, already logged with its
// reason by whichever consumer made it; a sub-runner error is news. A
// host that could not tell them apart would either log every budget
// ceiling twice or stay quiet about the one case it should not.
//
// It also pins the exclusion: halting cancels the sub-context, and the
// runner's ensuing "context canceled" must not arrive as a second,
// invented failure.
func TestAHaltedDispatchIsReportedAsAHaltAndNotAFailure(t *testing.T) {
	stop := errors.New("workload ceiling reached")
	obs := &recordingObserver{err: stop}

	outcomes := dispatchWith(t, obs, &scriptedModel{name: "sp-model", script: specialistScript})

	if len(outcomes) != 1 {
		t.Fatalf("sink closed %d times, want exactly 1", len(outcomes))
	}
	out := outcomes[0]
	if !errors.Is(out.Halted, stop) {
		t.Errorf("DispatchOutcome.Halted = %v, want the error the sink returned (%v)", out.Halted, stop)
	}
	if out.Err != nil {
		t.Errorf("DispatchOutcome.Err = %v, want nil: the sink stopped this dispatch, nothing broke under it", out.Err)
	}
}
