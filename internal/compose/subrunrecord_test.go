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
	"iter"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

const subRecApp = "compose_subrun_test"

// dispatchMutateModel drives one planner dispatch whose specialist then
// makes one mutating call. The planner side is recognized by the
// presence of invoke_specialist in the request's tools, the same way
// dispatchOnceModel tells the two apart.
type dispatchMutateModel struct {
	mu     sync.Mutex
	outer  int
	inner  int
	mutate bool // when false the specialist finishes without mutating
}

func (m *dispatchMutateModel) Name() string { return "dispatch-mutate" }

func (m *dispatchMutateModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	_, canDispatch := req.Tools[planner.ToolInvokeSpecialist]
	m.mu.Lock()
	var part *genai.Part
	switch {
	case canDispatch:
		m.outer++
		if m.outer == 1 {
			part = genai.NewPartFromFunctionCall(planner.ToolInvokeSpecialist,
				map[string]any{"name": "alpha", "input": "scale it"})
		} else {
			part = genai.NewPartFromFunctionCall("finish_task", map[string]any{"result": "done"})
		}
	default:
		m.inner++
		if m.inner == 1 && m.mutate {
			part = genai.NewPartFromFunctionCall("scale_up", map[string]any{"delta": 3})
		} else {
			part = genai.NewPartFromFunctionCall("finish_task", map[string]any{"result": "scaled"})
		}
	}
	m.mu.Unlock()

	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}},
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}, nil)
	}
}

// recordingObserver is the host end of the seam wired exactly as
// cmd/mast and the library wire it: one effects.SubRunRecorder per
// dispatch, over the ops-row store.
type recordingObserver struct {
	store SubRunIntentStore
	pred  effects.Predicate
	t     *testing.T
}

func (o recordingObserver) SubRun(sessionID, specialist string) planner.SubRunSink {
	rec, err := effects.NewSubRunRecorder(effects.SubRunRecorderConfig{
		Store:      o.store,
		SessionID:  sessionID,
		Specialist: specialist,
		Predicate:  o.pred,
	})
	if err != nil {
		o.t.Fatalf("NewSubRunRecorder: %v", err)
	}
	return rec
}

type scaleUpArgs struct {
	Delta int `json:"delta"`
}

// subRunRecordHarness composes a planner root whose one specialist can
// reach a mutating tool, with the recording seam attached.
type subRunRecordHarness struct {
	svc    session.Service
	store  *transcript.Store
	sub    SubRunIntentStore
	runner *runner.Runner
	model  *dispatchMutateModel

	// hang, when non-nil, blocks the mutating tool until the test
	// cancels the run — the stand-in for a SIGKILL landing between the
	// call and its response.
	entered chan struct{}
	hang    bool

	mu     sync.Mutex
	scaled int
}

func newSubRunRecordHarness(t *testing.T, mutate, hang bool) *subRunRecordHarness {
	t.Helper()
	h := &subRunRecordHarness{
		svc:     session.InMemoryService(),
		model:   &dispatchMutateModel{mutate: mutate},
		entered: make(chan struct{}),
		hang:    hang,
	}
	h.store = transcript.NewStore(h.svc, subRecApp)
	h.sub = SubRunIntentStore{Store: h.store, UserID: "op"}

	scaleTool, err := functiontool.New(functiontool.Config{
		Name:        "scale_up",
		Description: "mutating test tool",
	}, func(ctx adkagent.Context, args scaleUpArgs) (map[string]any, error) {
		close(h.entered)
		if h.hang {
			// Never completes: the process "dies" here.
			<-ctx.Done()
			return nil, ctx.Err()
		}
		h.mu.Lock()
		h.scaled++
		h.mu.Unlock()
		return map[string]any{"scaled": args.Delta}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New(scale_up): %v", err)
	}

	root, _, err := BuildRoot(context.Background(), RootConfig{
		Bundle: workload.Bundle{
			Name:        "triage",
			ToolCatalog: readOnlyCatalog(),
			Planner:     workload.Planner{Enabled: true},
		},
		Specs: []specialists.Spec{{
			Name:        "alpha",
			Instruction: "scale things",
			Mode:        specialists.ModeTask,
			Tools:       specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{}},
		}},
		Model:           h.model,
		ModelName:       "echo",
		SpecialistTools: []tool.Tool{scaleTool},
		SubRunObserver: recordingObserver{
			store: h.sub,
			pred:  effects.NewPredicate(nil),
			t:     t,
		},
	})
	if err != nil {
		t.Fatalf("BuildRoot: %v", err)
	}
	h.runner, err = runner.New(runner.Config{
		AppName:           subRecApp,
		Agent:             root,
		SessionService:    h.svc,
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return h
}

// outerEvents reads the outer session's log back through a fresh handle.
func (h *subRunRecordHarness) outerEvents(t *testing.T, sessionID string) []*session.Event {
	t.Helper()
	resp, err := h.svc.Get(context.Background(), &session.GetRequest{
		AppName: subRecApp, UserID: "op", SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Get outer session: %v", err)
	}
	var out []*session.Event
	for ev := range resp.Session.Events().All() {
		out = append(out, ev)
	}
	return out
}

func hasCall(events []*session.Event, name string) bool {
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.Name == name {
				return true
			}
		}
	}
	return false
}

// TestInterruptedDispatchLeavesAScannableIntent is #235's end-to-end.
//
// A planner dispatch is killed while its specialist's mutating call is
// in flight. Nothing about that call is in the outer session's log —
// that is the isolation the dispatch boundary exists for, and it is
// asserted here rather than assumed — so a boot pass reading the log
// alone sees a clean session and would let the workload mutate again.
// The out-of-band record is what makes the interrupted call visible.
func TestInterruptedDispatchLeavesAScannableIntent(t *testing.T) {
	h := newSubRunRecordHarness(t, true, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range h.runner.Run(ctx, "op", "outer-1",
			genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		}
	}()

	select {
	case <-h.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the specialist's mutating tool never ran")
	}
	cancel() // the kill, landing between the call and its response
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not unwind after cancellation")
	}

	// (1) The negative assertion. The planner's own log — which is also
	// the planner model's context on the next round — must not carry the
	// specialist's individual calls; isolation is what is being traded
	// against here, and a record that leaked into the log would have
	// bought the guarantee by giving that up.
	events := h.outerEvents(t, "outer-1")
	if hasCall(events, "scale_up") {
		t.Error("the outer session's log carries the specialist's scale_up call; the dispatch boundary's isolation is gone")
	}
	if !hasCall(events, planner.ToolInvokeSpecialist) {
		t.Fatal("the outer log has no invoke_specialist call; the dispatch never happened")
	}

	// (2) A scan of the log alone sees nothing — this is the pre-fix
	// behavior, and it is the bug.
	pred := effects.NewPredicate(nil)
	scan := effects.ScanDangling(sessionEvents(events), pred, map[string]bool{"alpha": true})
	for _, in := range scan.Mutating {
		if in.ToolName == "scale_up" {
			t.Fatal("scale_up is in the log's own dangling scan; this test is not measuring what it claims to")
		}
	}

	// (3) The record is there, and folding it in makes the interrupted
	// mutation visible to exactly the scan auto-resume and the outbox
	// consult.
	external := h.sub.Dangling(context.Background(), "outer-1")
	if len(external) != 1 || external[0].ToolName != "scale_up" {
		t.Fatalf("recorded dangling intents = %+v, want the one scale_up call", external)
	}
	if external[0].CallID == "" {
		t.Error("the recorded intent has no call ID; nothing could ever pair it")
	}
	folded := scan.WithExternal(external)
	var found bool
	for _, in := range folded.Mutating {
		if in.ToolName == "scale_up" {
			found = true
		}
	}
	if !found {
		t.Fatal("WithExternal did not surface the interrupted dispatch's mutation")
	}
}

// The completing case is the other half of exactly-once: a dispatch
// that finishes must leave nothing behind, or every planner workload
// would wedge itself into ambiguous-effect mode on its own success.
func TestCompletedDispatchLeavesNothingDangling(t *testing.T) {
	h := newSubRunRecordHarness(t, true, false)
	for _, err := range h.runner.Run(context.Background(), "op", "outer-2",
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
	}

	h.mu.Lock()
	scaled := h.scaled
	h.mu.Unlock()
	if scaled != 1 {
		t.Fatalf("scale_up executed %d times, want 1", scaled)
	}
	if got := h.sub.Dangling(context.Background(), "outer-2"); len(got) != 0 {
		t.Fatalf("dangling after a completed dispatch = %+v, want none", got)
	}
}

// sessionEvents adapts a slice to session.Events for ScanDangling.
type sessionEvents []*session.Event

func (e sessionEvents) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, ev := range e {
			if !yield(ev) {
				return
			}
		}
	}
}
func (e sessionEvents) Len() int                { return len(e) }
func (e sessionEvents) At(i int) *session.Event { return e[i] }
