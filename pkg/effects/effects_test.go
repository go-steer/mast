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

package effects

import (
	"context"
	"iter"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/go-steer/mast/pkg/transcript"
)

const (
	testApp  = "effects-test"
	testUser = "op"
)

// sqliteService opens a real database-backed session service in a temp
// dir (house rule #5 — t.TempDir() lives under os.TempDir()). The
// durability and scan semantics under test are exactly the ones the
// in-memory service can't exercise.
func sqliteService(t *testing.T) adksession.Service {
	t.Helper()
	svc, err := database.NewSessionService(sqlite.Open(filepath.Join(t.TempDir(), "sessions.db")))
	if err != nil {
		t.Fatalf("NewSessionService: %v", err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	return svc
}

// scriptedModel mirrors the planner tests' pattern: replies are
// computed per call by a script over the request.
type scriptedModel struct {
	name   string
	script func(req *model.LLMRequest) *model.LLMResponse

	mu   sync.Mutex
	reqs []*model.LLMRequest
}

func (m *scriptedModel) Name() string { return m.name }

func (m *scriptedModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		m.reqs = append(m.reqs, req)
		m.mu.Unlock()
		resp := m.script(req)
		if resp.UsageMetadata == nil {
			resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     10,
				CandidatesTokenCount: 5,
				TotalTokenCount:      15,
			}
		}
		resp.TurnComplete = true
		resp.FinishReason = genai.FinishReasonStop
		yield(resp, nil)
	}
}

func callResponse(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)},
		},
	}
}

func textResponse(s string) *model.LLMResponse {
	return &model.LLMResponse{Content: genai.NewContentFromText(s, genai.RoleModel)}
}

// roundScript returns each canned response once, in order, then text.
func roundScript(calls ...*model.LLMResponse) func(*model.LLMRequest) *model.LLMResponse {
	round := 0
	return func(*model.LLMRequest) *model.LLMResponse {
		defer func() { round++ }()
		if round < len(calls) {
			return calls[round]
		}
		return textResponse("turn done")
	}
}

type scaleArgs struct {
	Delta int `json:"delta"`
}

// harness bundles a runner wired with the outbox plugin over a real
// SQLite store, one mutating tool (scale_up) and one override-read-only
// tool (list_pods), with execution counters.
type harness struct {
	svc     adksession.Service
	store   *transcript.Store
	runner  *runner.Runner
	model   *scriptedModel
	mu      sync.Mutex
	scale   int // times scale_up actually executed
	list    int // times list_pods actually executed
	sawCall map[string]bool
}

func newHarness(t *testing.T, script func(*model.LLMRequest) *model.LLMResponse) *harness {
	t.Helper()
	h := &harness{
		svc:     sqliteService(t),
		model:   &scriptedModel{name: "scripted", script: script},
		sawCall: map[string]bool{},
	}
	h.store = transcript.NewStore(h.svc, testApp)

	scaleTool, err := functiontool.New(functiontool.Config{
		Name:        "scale_up",
		Description: "mutating test tool",
	}, func(ctx adkagent.Context, args scaleArgs) (map[string]any, error) {
		h.mu.Lock()
		h.scale++
		h.mu.Unlock()
		// Verification (a) pinned end-to-end: by the time the tool
		// runs, its own FunctionCall event must already be durable — a
		// FRESH read of the session (not the runner's handle) must
		// contain this function-call ID.
		resp, gerr := h.svc.Get(ctx, &adksession.GetRequest{
			AppName: testApp, UserID: testUser, SessionID: ctx.SessionID(),
		})
		if gerr != nil {
			return nil, gerr
		}
		h.mu.Lock()
		h.sawCall[ctx.FunctionCallID()] = hasFunctionCall(resp.Session.Events(), ctx.FunctionCallID())
		h.mu.Unlock()
		return map[string]any{"scaled": args.Delta}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New(scale_up): %v", err)
	}
	listTool, err := functiontool.New(functiontool.Config{
		Name:        "list_pods",
		Description: "read-only test tool (via override)",
	}, func(ctx adkagent.Context, args struct{}) (map[string]any, error) {
		h.mu.Lock()
		h.list++
		h.mu.Unlock()
		return map[string]any{"pods": []string{"a", "b"}}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New(list_pods): %v", err)
	}

	root, err := llmagent.New(llmagent.Config{
		Name:        "effects_agent",
		Description: "outbox test agent",
		Model:       h.model,
		Tools:       []tool.Tool{scaleTool, listTool},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	f := false
	p, err := New(Config{
		Predicate: NewPredicate(Overrides(nil, []ToolPolicy{{Name: "list_pods", Mutating: &f}})),
		AckedAt: func(ctx context.Context, sid string) (time.Time, bool) {
			return h.store.EffectsAckedAt(ctx, "", sid)
		},
	})
	if err != nil {
		t.Fatalf("effects.New: %v", err)
	}
	h.runner, err = runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    h.svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{p}},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return h
}

func hasFunctionCall(events adksession.Events, callID string) bool {
	for ev := range events.All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.ID == callID {
				return true
			}
		}
	}
	return false
}

// run drives one user turn, returning all streamed events.
func (h *harness) run(t *testing.T, sessionID, text string) []*adksession.Event {
	t.Helper()
	var events []*adksession.Event
	msg := genai.NewContentFromText(text, genai.RoleUser)
	for ev, err := range h.runner.Run(context.Background(), testUser, sessionID, msg, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

// seedDangling creates the session and appends one event carrying an
// unpaired FunctionCall — the wire shape a SIGKILL mid-tool-execution
// leaves behind. Returns the seeded call ID.
func seedDangling(t *testing.T, svc adksession.Service, sessionID, toolName, callID string, longRunning bool) {
	t.Helper()
	ctx := context.Background()
	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: testApp, UserID: testUser, SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", sessionID, err)
	}
	ev := adksession.NewEvent(ctx, "prior-inv")
	ev.Author = "effects_agent"
	part := genai.NewPartFromFunctionCall(toolName, map[string]any{"delta": 1})
	part.FunctionCall.ID = callID
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}}
	if longRunning {
		ev.LongRunningToolIDs = []string{callID}
	}
	if err := svc.AppendEvent(ctx, created.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	// Natural forward timestamps for everything that follows (the #54
	// lesson: fixture timestamps that run backwards disarm OCC checks).
	time.Sleep(5 * time.Millisecond)
}

// toolResponses flattens FunctionResponse payloads from a turn's
// events, keyed by tool name.
func toolResponses(events []*adksession.Event) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionResponse != nil {
				out[p.FunctionResponse.Name] = append(out[p.FunctionResponse.Name], p.FunctionResponse.Response)
			}
		}
	}
	return out
}

func TestFunctionCallDurableBeforeToolRun(t *testing.T) {
	h := newHarness(t, roundScript(callResponse("scale_up", map[string]any{"delta": 2})))
	h.run(t, "clean-session", "scale it")

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scale != 1 {
		t.Fatalf("scale_up executed %d times, want 1", h.scale)
	}
	if len(h.sawCall) != 1 {
		t.Fatalf("recorded %d call-durability probes, want 1", len(h.sawCall))
	}
	for id, durable := range h.sawCall {
		if !durable {
			t.Errorf("FunctionCall %s was NOT durable before tool.Run — the log-as-outbox intent property does not hold; the design's ops-row fallback intent carrier is needed", id)
		}
	}
}

func TestDanglingMutatingIntentRefusesMutatingCalls(t *testing.T) {
	h := newHarness(t, roundScript(
		callResponse("scale_up", map[string]any{"delta": 3}),
		callResponse("list_pods", map[string]any{}),
	))
	seedDangling(t, h.svc, "wounded", "scale_up", "call-prior-1", false)

	events := h.run(t, "wounded", "continue the work")

	h.mu.Lock()
	scale, list := h.scale, h.list
	h.mu.Unlock()
	if scale != 0 {
		t.Fatalf("scale_up executed %d times in ambiguous-effect mode, want 0 (fail-closed)", scale)
	}
	if list != 1 {
		t.Fatalf("list_pods executed %d times, want 1 (read-only tools proceed in ambiguous mode)", list)
	}
	resps := toolResponses(events)
	if got := resps["scale_up"]; len(got) != 1 || got[0]["error"] != "ambiguous_prior_effect" {
		t.Fatalf("scale_up response = %v, want one ambiguous_prior_effect refusal", got)
	}
}

func TestPausedHITLSessionDoesNotTripMode(t *testing.T) {
	h := newHarness(t, roundScript(callResponse("scale_up", map[string]any{"delta": 1})))
	// A paused session's wire shape IS an unpaired FunctionCall — for
	// the HITL control surface. It must not read as a dangling effect.
	seedDangling(t, h.svc, "paused", "adk_request_input", "interrupt-1", false)

	h.run(t, "paused", "resume-ish turn")

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scale != 1 {
		t.Fatalf("scale_up executed %d times on a paused-HITL session, want 1 (control calls are not effects)", h.scale)
	}
}

func TestLongRunningCallDoesNotTripMode(t *testing.T) {
	h := newHarness(t, roundScript(callResponse("scale_up", map[string]any{"delta": 1})))
	seedDangling(t, h.svc, "longrun", "scale_up", "call-lr-1", true)

	h.run(t, "longrun", "next turn")

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scale != 1 {
		t.Fatalf("scale_up executed %d times with a pending long-running call, want 1 (long-running calls are pending by design)", h.scale)
	}
}

func TestAckLiftsAmbiguousMode(t *testing.T) {
	h := newHarness(t, roundScript(callResponse("scale_up", map[string]any{"delta": 4})))
	seedDangling(t, h.svc, "acked", "scale_up", "call-prior-2", false)
	if err := h.store.AckEffects(context.Background(), testUser, "acked", "operator checked the cluster"); err != nil {
		t.Fatalf("AckEffects: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	h.run(t, "acked", "continue after ack")

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scale != 1 {
		t.Fatalf("scale_up executed %d times after operator ack, want 1 (ack lifts the refusal)", h.scale)
	}
}

func TestScanDangling(t *testing.T) {
	pred := NewPredicate(nil)
	mk := func(inv string, parts ...*genai.Part) *adksession.Event {
		ev := adksession.NewEvent(context.Background(), inv)
		ev.Content = &genai.Content{Role: genai.RoleModel, Parts: parts}
		return ev
	}
	call := func(name, id string) *genai.Part {
		p := genai.NewPartFromFunctionCall(name, map[string]any{})
		p.FunctionCall.ID = id
		return p
	}
	respPart := func(name, id string) *genai.Part {
		p := genai.NewPartFromFunctionResponse(name, map[string]any{"ok": true})
		p.FunctionResponse.ID = id
		return p
	}

	events := eventList{
		mk("inv1", call("scale_up", "c1")),
		mk("inv1", respPart("scale_up", "c1")),      // paired: not dangling
		mk("inv2", call("scale_up", "c2")),          // dangling mutating
		mk("inv2", call("adk_request_input", "i1")), // control: excluded
		mk("inv2", call("invoke_specialist", "c3")), // dangling spawning
	}
	got := scanDangling(events, pred)
	if len(got) != 2 {
		t.Fatalf("scanDangling found %d intents (%v), want 2", len(got), got)
	}
	if got[0].CallID != "c2" || got[1].CallID != "c3" {
		t.Fatalf("scanDangling order = %s,%s want c2,c3", got[0].CallID, got[1].CallID)
	}
}

func TestRecordedCompletion(t *testing.T) {
	mk := func(parts ...*genai.Part) *adksession.Event {
		ev := adksession.NewEvent(context.Background(), "inv")
		ev.Content = &genai.Content{Role: genai.RoleModel, Parts: parts}
		return ev
	}
	p := genai.NewPartFromFunctionResponse("scale_up", map[string]any{"scaled": 7})
	p.FunctionResponse.ID = "c9"
	events := eventList{mk(p)}

	resp, ok := recordedCompletion(events, "c9")
	if !ok || resp["scaled"] != float64(7) && resp["scaled"] != 7 {
		t.Fatalf("recordedCompletion = %v,%v want the recorded payload", resp, ok)
	}
	if _, ok := recordedCompletion(events, "missing"); ok {
		t.Fatal("recordedCompletion found a completion for an unknown call ID")
	}
	if _, ok := recordedCompletion(events, ""); ok {
		t.Fatal("recordedCompletion must not match an empty call ID")
	}
}

// eventList adapts a slice to session.Events for unit tests.
type eventList []*adksession.Event

func (e eventList) All() iter.Seq[*adksession.Event] {
	return func(yield func(*adksession.Event) bool) {
		for _, ev := range e {
			if !yield(ev) {
				return
			}
		}
	}
}
func (e eventList) Len() int                   { return len(e) }
func (e eventList) At(i int) *adksession.Event { return e[i] }
