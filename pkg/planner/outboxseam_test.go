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
	"reflect"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/planner"
)

// W9.3 probe (#235). The effect outbox's durable record is not a store
// of its own: pkg/effects derives intents and completions from the
// SESSION EVENT LOG (beforeRun → scanHistory → pairScan). So installing
// the outbox plugin on the dispatch sub-runner would buy nothing — its
// substrate is the same in-memory session that dies with the tool call.
//
// That leaves one candidate seam for the `apply` half of #235: carry the
// sub-run's mutating intents out-of-band on SubRunSink, the channel
// spend and the watchdog already cross (#226), and record them against
// something durable the host owns. Unlike the write gate, recording
// needs no round trip, so the objection that killed out-of-band delivery
// for approvals does not apply here.
//
// It has its own precondition, and this file measures it: an outbox
// intent is only worth writing if it lands BEFORE the effect. If the
// sink is handed the FunctionCall only after the tool body has already
// run, the "intent" is a post-hoc log entry and the exactly-once
// guarantee it is supposed to underwrite does not exist.
//
// This is a measurement, not a fix. Nothing here changes behaviour.

// orderLog records the interleaving of the sub-run event stream and the
// dispatched specialist's tool body.
type orderLog struct {
	mu  sync.Mutex
	seq []string
}

func (l *orderLog) add(s string) {
	l.mu.Lock()
	l.seq = append(l.seq, s)
	l.mu.Unlock()
}

func (l *orderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.seq...)
}

func (l *orderLog) indexOf(s string) int {
	for i, got := range l.snapshot() {
		if got == s {
			return i
		}
	}
	return -1
}

// orderingObserver logs where in the sub-run event stream the mutating
// call and its response become visible to a host.
type orderingObserver struct {
	log  *orderLog
	tool string
}

func (o *orderingObserver) SubRun(_, _ string) planner.SubRunSink { return &orderingSink{obs: o} }

type orderingSink struct{ obs *orderingObserver }

func (s *orderingSink) Observe(ev *session.Event) error {
	if ev == nil || ev.Content == nil {
		return nil
	}
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		if p.FunctionCall != nil && p.FunctionCall.Name == s.obs.tool {
			s.obs.log.add("sink_saw_call")
		}
		if p.FunctionResponse != nil && p.FunctionResponse.Name == s.obs.tool {
			s.obs.log.add("sink_saw_response")
		}
	}
	return nil
}

func (s *orderingSink) Close() {}

type scaleArgs struct {
	Deployment string `json:"deployment"`
	Replicas   int    `json:"replicas"`
}

// buildMutatingSpecialist is buildSpecialist with a tool whose body
// announces when it actually ran.
func buildMutatingSpecialist(t *testing.T, name string, m model.LLM, log *orderLog) adkagent.Agent {
	t.Helper()
	scale, err := func() (tool.Tool, error) {
		return functiontool.New(functiontool.Config{
			Name:        "scale_deployment",
			Description: "Scale a deployment. Mutating.",
		}, func(_ adkagent.Context, args scaleArgs) (map[string]any, error) {
			log.add("tool_body_ran")
			return map[string]any{"status": "scaled", "deployment": args.Deployment}, nil
		})
	}()
	if err != nil {
		t.Fatalf("functiontool.New(scale_deployment): %v", err)
	}
	a, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:        name,
		Description: name + " (test change executor)",
		Instruction: "test",
		Model:       m,
		Tools:       []tool.Tool{scale},
	})
	if err != nil {
		t.Fatalf("NewTaskAgent(%q): %v", name, err)
	}
	return a
}

// TestSubRunSinkOrderingAgainstTheToolBody measures whether a host
// watching a dispatch through SubRunSink learns about a mutating call
// before or after that call has already hit the world.
//
// The answer decides whether the `apply` half of #235 can be closed
// out-of-band at all:
//
//   - call BEFORE body: a host can write a durable intent from the sink
//     and the outbox's intent-before-effect contract survives the
//     dispatch boundary.
//   - call AFTER body: the sink can only ever produce an after-the-fact
//     record, and W9.3 needs the sub-run's own session to be durable —
//     which is option 1's difficulty, not a cheaper alternative to it.
func TestSubRunSinkOrderingAgainstTheToolBody(t *testing.T) {
	log := &orderLog{}

	spModel := &scriptedModel{name: "sp-model"}
	spModel.script = planScript(spModel,
		callResponse("scale_deployment", map[string]any{"deployment": "frontend", "replicas": 20}),
	)
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "exec", "input": "scale it"}),
	)

	obs := &orderingObserver{log: log, tool: "scale_deployment"}
	root, err := planner.NewRoot(planner.Config{
		Name:           "w",
		Model:          plModel,
		Specialists:    map[string]adkagent.Agent{"exec": buildMutatingSpecialist(t, "exec", spModel, log)},
		SubRunObserver: obs,
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

	seq := log.snapshot()
	body := log.indexOf("tool_body_ran")
	call := log.indexOf("sink_saw_call")
	resp := log.indexOf("sink_saw_response")

	if body < 0 {
		t.Fatalf("the mutating tool never ran; this probe would pass for the wrong reason. seq = %v", seq)
	}
	if call < 0 {
		t.Fatalf("the sink never saw the mutating call at all, so a host cannot record an intent "+
			"from this seam under any ordering. seq = %v", seq)
	}
	if resp < 0 {
		t.Errorf("the sink never saw the mutating call's response; a completion record has no seam here either. seq = %v", seq)
	}

	// The finding. Asserted rather than logged so it cannot drift
	// unnoticed under an ADK bump: the whole out-of-band outbox design
	// rests on this ordering, and ADK owns it, not mast.
	if call > body {
		t.Errorf("MEASURED: SubRunSink is handed the mutating FunctionCall only AFTER the tool body ran "+
			"(seq = %v). An out-of-band outbox intent written from this seam would be post-hoc, so the "+
			"`apply` half of #235 cannot be closed this way and needs a durable sub-run session instead.", seq)
	}
	t.Logf("sub-run seam ordering: %v (call=%d body=%d response=%d)", seq, call, body, resp)
}

// TestDispatchSubSessionHasNoInjectionSeam pins the other half of the
// W9.3 precondition, and it is an API fact rather than a behaviour:
// there is no way for a host to give the dispatch sub-runner a session
// service. pkg/planner constructs one internally
// (session.InMemoryService(), AppName "planner_dispatch"), so any design
// that depends on the sub-run's log outliving the tool call is a change
// to pkg/planner's surface, not a wiring change at a call site.
//
// Guarding it as a test because "just make the sub-session durable" is
// the sentence most likely to be said about #235, and it is currently
// not something a caller can do.
func TestDispatchSubSessionHasNoInjectionSeam(t *testing.T) {
	var names []string
	ct := reflect.TypeOf(planner.Config{})
	for i := range ct.NumField() {
		f := ct.Field(i)
		if !f.IsExported() {
			continue
		}
		names = append(names, f.Name)
		if strings.Contains(f.Name, "Session") {
			t.Fatalf("planner.Config now exposes %q: W9.3's precondition changed and the design that "+
				"depends on the sub-session being unreachable must be re-derived", f.Name)
		}
	}
	t.Logf("planner.Config exposes no session seam; the dispatch sub-session is unreachable by construction. fields = %v", names)
}
