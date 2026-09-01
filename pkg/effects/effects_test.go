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

	mastagent "github.com/go-steer/mast/pkg/agent"
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
	return newHarnessWith(t, script, nil)
}

// newHarnessWith is newHarness plus the external-dangling hook (#235):
// the dispatched mutations the log cannot hold, folded in at beforeRun.
func newHarnessWith(t *testing.T, script func(*model.LLMRequest) *model.LLMResponse, external func(context.Context, string) []DanglingIntent) *harness {
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
		ExternalDangling: external,
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
	st := scanHistory(events, pred, nil)
	got := st.dangling
	if len(got) != 2 {
		t.Fatalf("scanHistory found %d intents (%v), want 2", len(got), got)
	}
	if got[0].CallID != "c2" || got[1].CallID != "c3" {
		t.Fatalf("scanHistory order = %s,%s want c2,c3", got[0].CallID, got[1].CallID)
	}
	// The paired mutating call's completion is recorded for replay.
	if resp, ok := st.completions["c1"]; !ok || resp["ok"] != true {
		t.Fatalf("completions[c1] = %v,%v want the recorded payload", resp, ok)
	}
}

func TestScanDanglingBuckets(t *testing.T) {
	// list_pods is an ordinary read-only tool via override; scale_up is
	// mutating (default); triage_bot is a composed sub-agent delegation;
	// adk_request_input and a long-running park are excluded.
	pred := NewPredicate(map[string]bool{"list_pods": false})
	subAgents := map[string]bool{"triage_bot": true}
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

	t.Run("split", func(t *testing.T) {
		lrEvent := mk("inv0", call("scale_up", "lr1")) // idx0
		lrEvent.LongRunningToolIDs = []string{"lr1"}
		events := eventList{
			lrEvent,                                     // idx0 deferred (long-running)
			mk("inv1", call("scale_up", "m1")),          // idx1 mutating dangling
			mk("inv1", call("list_pods", "r1")),         // idx2 repairable dangling
			mk("inv1", call("triage_bot", "d1")),        // idx3 deferred (sub-agent)
			mk("inv1", call("adk_request_input", "x1")), // idx4 deferred (control)
			mk("inv1", call("scale_up", "p1")),          // idx5 paired (last call event)
			mk("inv1", respPart("scale_up", "p1")),      // idx6 response
		}
		ds := ScanDangling(events, pred, subAgents)
		if len(ds.Mutating) != 1 || ds.Mutating[0].CallID != "m1" {
			t.Fatalf("Mutating = %+v, want [m1]", ds.Mutating)
		}
		if len(ds.Repairable) != 1 || ds.Repairable[0].CallID != "r1" || ds.Repairable[0].EventIndex != 2 {
			t.Fatalf("Repairable = %+v, want [r1@idx2]", ds.Repairable)
		}
		if len(ds.Deferred) != 3 {
			t.Fatalf("Deferred = %+v, want 3 (long-running, sub-agent, control)", ds.Deferred)
		}
		// A later PAIRED call event (p1@idx5) is still the last function
		// call event: repair of an earlier read-only call would span
		// events and must be blocked.
		if ds.LastCallEventIndex != 5 {
			t.Fatalf("LastCallEventIndex = %d, want 5 (the paired p1 event)", ds.LastCallEventIndex)
		}
	})

	t.Run("clean-single-event-repair", func(t *testing.T) {
		// Two read-only calls in the SAME (and last) function-call event:
		// answerable by a single repair message.
		events := eventList{
			mk("inv0", textPart("thinking")),                             // idx0, no calls
			mk("inv1", call("list_pods", "r1"), call("list_pods", "r2")), // idx1
		}
		ds := ScanDangling(events, pred, subAgents)
		if len(ds.Mutating) != 0 || len(ds.Deferred) != 0 {
			t.Fatalf("unexpected mutating/deferred: %+v / %+v", ds.Mutating, ds.Deferred)
		}
		if len(ds.Repairable) != 2 {
			t.Fatalf("Repairable = %+v, want 2", ds.Repairable)
		}
		if ds.LastCallEventIndex != 1 {
			t.Fatalf("LastCallEventIndex = %d, want 1", ds.LastCallEventIndex)
		}
		for _, r := range ds.Repairable {
			if r.EventIndex != ds.LastCallEventIndex {
				t.Fatalf("repairable %s at event %d, not the last call event %d", r.CallID, r.EventIndex, ds.LastCallEventIndex)
			}
		}
	})

	t.Run("no-calls", func(t *testing.T) {
		ds := ScanDangling(eventList{mk("inv0", textPart("done"))}, pred, subAgents)
		if len(ds.Mutating)+len(ds.Repairable)+len(ds.Deferred) != 0 {
			t.Fatalf("expected empty scan, got %+v", ds)
		}
		if ds.LastCallEventIndex != -1 {
			t.Fatalf("LastCallEventIndex = %d, want -1 (no function-call events)", ds.LastCallEventIndex)
		}
	})
}

func textPart(s string) *genai.Part { return genai.NewPartFromText(s) }

func TestScanHistoryCompletions(t *testing.T) {
	mk := func(parts ...*genai.Part) *adksession.Event {
		ev := adksession.NewEvent(context.Background(), "inv")
		ev.Content = &genai.Content{Role: genai.RoleModel, Parts: parts}
		return ev
	}
	p := genai.NewPartFromFunctionResponse("scale_up", map[string]any{"scaled": 7})
	p.FunctionResponse.ID = "c9"
	empty := genai.NewPartFromFunctionResponse("scale_up", map[string]any{"x": 1})
	empty.FunctionResponse.ID = ""
	st := scanHistory(eventList{mk(p, empty)}, NewPredicate(nil), nil)

	resp, ok := st.completions["c9"]
	if !ok || resp["scaled"] != float64(7) && resp["scaled"] != 7 {
		t.Fatalf("completions[c9] = %v,%v want the recorded payload", resp, ok)
	}
	if _, ok := st.completions[""]; ok {
		t.Fatal("an empty-ID FunctionResponse must not be recorded as a completion")
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

// fixedIDCallResponse scripts a FunctionCall with a PRESET ID —
// ADK's PopulateClientFunctionCallID fills only empty IDs, so the
// preset survives to callTool. This is how a re-fire of a recorded
// call is simulated end-to-end.
func fixedIDCallResponse(name, id string, args map[string]any) *model.LLMResponse {
	r := callResponse(name, args)
	r.Content.Parts[0].FunctionCall.ID = id
	return r
}

// seedPair appends a completed FunctionCall+FunctionResponse pair for
// callID to an existing or new session.
func seedPair(t *testing.T, svc adksession.Service, sessionID, toolName, callID string, response map[string]any) {
	t.Helper()
	ctx := context.Background()
	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: testApp, UserID: testUser, SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", sessionID, err)
	}
	callEv := adksession.NewEvent(ctx, "prior-inv")
	callEv.Author = "effects_agent"
	cp := genai.NewPartFromFunctionCall(toolName, map[string]any{"delta": 1})
	cp.FunctionCall.ID = callID
	callEv.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{cp}}
	if err := svc.AppendEvent(ctx, created.Session, callEv); err != nil {
		t.Fatalf("AppendEvent(call): %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	respEv := adksession.NewEvent(ctx, "prior-inv")
	respEv.Author = "effects_agent"
	rp := genai.NewPartFromFunctionResponse(toolName, response)
	rp.FunctionResponse.ID = callID
	respEv.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{rp}}
	if err := svc.AppendEvent(ctx, created.Session, respEv); err != nil {
		t.Fatalf("AppendEvent(response): %v", err)
	}
	time.Sleep(5 * time.Millisecond)
}

// TestExactKeyReplayEndToEnd drives a re-fire of an already-completed
// call THROUGH THE RUNNER (the prior test suite only unit-tested the
// helper — which left the replay branch dead code when the tool
// context turned out to have no session access; adversarial finding 1).
func TestExactKeyReplayEndToEnd(t *testing.T) {
	h := newHarness(t, roundScript(
		fixedIDCallResponse("scale_up", "call-done-1", map[string]any{"delta": 9}),
	))
	seedPair(t, h.svc, "replayed", "scale_up", "call-done-1", map[string]any{"scaled": 42})

	events := h.run(t, "replayed", "do it again")

	h.mu.Lock()
	scale := h.scale
	h.mu.Unlock()
	if scale != 0 {
		t.Fatalf("scale_up executed %d times, want 0 (recorded completion must replay, not re-execute)", scale)
	}
	resps := toolResponses(events)
	got := resps["scale_up"]
	if len(got) != 1 {
		t.Fatalf("scale_up responses = %v, want exactly the replayed one", got)
	}
	if v, ok := got[0]["scaled"]; !ok || (v != float64(42) && v != 42) {
		t.Fatalf("replayed response = %v, want the recorded payload {scaled: 42}", got[0])
	}
}

// TestExactKeyReplayNilResponse: a recorded completion whose payload is
// nil must still count as a hit — returning nil from the callback
// means "proceed and execute", the exact double-mutation the record
// exists to prevent (adversarial finding 4).
func TestExactKeyReplayNilResponse(t *testing.T) {
	h := newHarness(t, roundScript(
		fixedIDCallResponse("scale_up", "call-done-2", map[string]any{"delta": 1}),
	))
	seedPair(t, h.svc, "nilresp", "scale_up", "call-done-2", nil)

	events := h.run(t, "nilresp", "again")

	h.mu.Lock()
	scale := h.scale
	h.mu.Unlock()
	if scale != 0 {
		t.Fatalf("scale_up executed %d times, want 0 (nil recorded payload is still a completion)", scale)
	}
	resps := toolResponses(events)
	if got := resps["scale_up"]; len(got) != 1 || got[0]["mast_replayed_effect"] != true {
		t.Fatalf("scale_up response = %v, want the explicit nil-payload replay marker", got)
	}
}

// TestPostAckIntentStillRefused: the watermark covers only intents
// persisted at or before it — an intent recorded AFTER the ack must
// still trip ambiguous-effect mode.
func TestPostAckIntentStillRefused(t *testing.T) {
	h := newHarness(t, roundScript(callResponse("scale_up", map[string]any{"delta": 1})))
	// Ack first (creates the session's ops row with the watermark),
	// then the intent lands after it.
	ctx := context.Background()
	if _, err := h.svc.Create(ctx, &adksession.CreateRequest{
		AppName: testApp, UserID: testUser, SessionID: "postack",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.store.AckEffects(ctx, testUser, "postack", "premature ack"); err != nil {
		t.Fatalf("AckEffects: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	resp, err := h.svc.Get(ctx, &adksession.GetRequest{AppName: testApp, UserID: testUser, SessionID: "postack"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	ev := adksession.NewEvent(ctx, "later-inv")
	ev.Author = "effects_agent"
	part := genai.NewPartFromFunctionCall("scale_up", map[string]any{"delta": 2})
	part.FunctionCall.ID = "call-after-ack"
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}}
	if err := h.svc.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	h.run(t, "postack", "continue")

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scale != 0 {
		t.Fatalf("scale_up executed %d times, want 0 (post-ack intents are NOT covered by the watermark)", h.scale)
	}
}

func TestScanHistorySkipsEmptyIDsAndDelegations(t *testing.T) {
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
	events := eventList{
		mk("inv1", call("scale_up", "")),            // empty ID: unkeyable, skipped
		mk("inv1", call("triage_bot", "d1")),        // delegation (sub-agent name): skipped
		mk("inv1", call("transfer_to_agent", "t1")), // control: skipped
		mk("inv1", call("exit_loop", "t2")),         // control: skipped
		mk("inv1", call("scale_up", "c1")),          // genuinely dangling
	}
	st := scanHistory(events, pred, map[string]bool{"triage_bot": true})
	if len(st.dangling) != 1 || st.dangling[0].CallID != "c1" {
		t.Fatalf("scanHistory dangling = %+v, want exactly c1", st.dangling)
	}
}

// TestCoordinatorDelegationDoesNotTripMode reproduces adversarial
// finding 2 on the REAL default-composition wire shape: ADK's
// coordinator emits task delegations as FunctionCalls named after the
// sub-agent and deliberately leaves them unresolved across user turns
// when the specialist replies without finishing (a clarifying
// question — the most ordinary thing a HITL workload does). That
// dangling delegation must not put the next turn in ambiguous-effect
// mode; without the SubAgentNames exclusion this test fails with the
// mutating call refused.
func TestCoordinatorDelegationDoesNotTripMode(t *testing.T) {
	svc := sqliteService(t)
	store := transcript.NewStore(svc, testApp)

	var mu sync.Mutex
	scale := 0
	scaleTool, err := functiontool.New(functiontool.Config{
		Name:        "scale_up",
		Description: "mutating test tool",
	}, func(ctx adkagent.Context, args scaleArgs) (map[string]any, error) {
		mu.Lock()
		scale++
		mu.Unlock()
		return map[string]any{"scaled": args.Delta}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	// Specialist: first invocation asks a question (plain text, no
	// finish_task — the delegation stays unresolved); the turn-2
	// re-dispatch finishes.
	specialistRound := 0
	specialistModel := &scriptedModel{name: "specialist", script: func(*model.LLMRequest) *model.LLMResponse {
		specialistRound++
		if specialistRound == 1 {
			return textResponse("which namespace did you mean?")
		}
		return callResponse("finish_task", map[string]any{"result": "triage done"})
	}}
	specialist, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:        "triage_bot",
		Description: "test specialist",
		Instruction: "triage",
		Model:       specialistModel,
	})
	if err != nil {
		t.Fatalf("NewTaskAgent: %v", err)
	}

	coordRound := 0
	coordModel := &scriptedModel{name: "coord", script: func(*model.LLMRequest) *model.LLMResponse {
		coordRound++
		switch coordRound {
		case 1:
			return callResponse("triage_bot", map[string]any{"request": "triage the incident"})
		case 2:
			return callResponse("scale_up", map[string]any{"delta": 1})
		default:
			return textResponse("done")
		}
	}}
	root, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name:        "coord",
		Description: "test coordinator",
		Instruction: "coordinate",
		Model:       coordModel,
		SubAgents:   []adkagent.Agent{specialist},
		Tools:       []tool.Tool{scaleTool},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	p, err := New(Config{
		Predicate:     NewPredicate(nil),
		SubAgentNames: SubAgentNames(root),
		AckedAt: func(ctx context.Context, sid string) (time.Time, bool) {
			return store.EffectsAckedAt(ctx, "", sid)
		},
	})
	if err != nil {
		t.Fatalf("effects.New: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{p}},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	runOne := func(text string) {
		t.Helper()
		msg := genai.NewContentFromText(text, genai.RoleUser)
		for _, err := range r.Run(context.Background(), testUser, "coord-session", msg, adkagent.RunConfig{}) {
			if err != nil {
				t.Fatalf("runner.Run: %v", err)
			}
		}
	}
	runOne("triage this incident")   // delegation dangles (specialist asked a question)
	runOne("I meant namespace prod") // must NOT be in ambiguous-effect mode

	mu.Lock()
	defer mu.Unlock()
	if scale != 1 {
		t.Fatalf("scale_up executed %d times, want 1 — a dangling task delegation wedged the session in ambiguous-effect mode", scale)
	}
}

// TestCheckNameCollisions_RealRoot proves the SubAgentNames walk feeds
// CheckNameCollisions correctly on a real composed root: a specialist
// named after a declared mutating tool (gate finding N2's fail-open) is
// detected. The delegation-exclusion that TestCoordinatorDelegationDoes-
// NotTripMode relies on is the very mechanism that would hide this tool's
// dangling calls, which is why the collision is refused at construction.
func TestCheckNameCollisions_RealRoot(t *testing.T) {
	specialist, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:        "deploy", // operator specialist sharing a tool verb
		Description: "test specialist",
		Instruction: "deploy things",
		Model:       &scriptedModel{name: "s", script: func(*model.LLMRequest) *model.LLMResponse { return textResponse("ok") }},
	})
	if err != nil {
		t.Fatalf("NewTaskAgent: %v", err)
	}
	root, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name:        "coord",
		Description: "test coordinator",
		Instruction: "coordinate",
		Model:       &scriptedModel{name: "c", script: func(*model.LLMRequest) *model.LLMResponse { return textResponse("done") }},
		SubAgents:   []adkagent.Agent{specialist},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	tr := true
	policies := []ToolPolicy{{Name: "deploy", Mutating: &tr}}
	pred := NewPredicate(Overrides(nil, policies))
	got := CheckNameCollisions(SubAgentNames(root), pred, policies)
	if len(got) != 1 || got[0] != "deploy" {
		t.Fatalf("CheckNameCollisions on real root = %v, want [deploy]", got)
	}
}
