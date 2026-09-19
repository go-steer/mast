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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/agui"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/watchdog"
	"github.com/go-steer/mast/pkg/workload"
)

// newAGUIBackendRunner builds an aguiBackend backed by a real turn stack
// (runner + transcript store + tracker + locks + meters over an in-memory
// session service), so RunAgent can drive turns end-to-end through runTurnPre.
// workloadName is "(test)" to match the harness's primed obs.
func newAGUIBackendRunner(t *testing.T, m model.LLM) *aguiBackend {
	t.Helper()
	h := newTurnHarness(t, m)
	return &aguiBackend{turnDeps: h.deps()}
}

// collectEmit returns an emit func plus a pointer to the slice it appends to.
func collectEmit() (func(any), *[]any) {
	var got []any
	return func(ev any) { got = append(got, ev) }, &got
}

// TestAGUIBackendRunHappyPath drives a run end to end: the opening frames
// (RunStarted, StateSnapshot) precede the model's answer as a TextMessage
// triad, and RunResult carries the answer for the server's RunFinished. The
// backend does NOT emit the terminal frame (that is the server's job).
// Neutralize check: drop the onEvent text translation and the triad vanishes;
// drop the opening emits and the first frame is no longer RunStarted.
func TestAGUIBackendRunHappyPath(t *testing.T) {
	b := newAGUIBackendRunner(t, &blockableModel{})
	emit, got := collectEmit()

	res, err := b.RunAgent(context.Background(), agui.RunInput{
		ThreadID: "t1", RunID: "r1", Text: "investigate",
	}, emit)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if res.Aborted || res.Interrupted {
		t.Fatalf("result = %+v, want a clean success", res)
	}
	if res.Text != "done" {
		t.Fatalf("result text = %q, want %q (the model's answer)", res.Text, "done")
	}

	frames := *got
	if len(frames) < 5 {
		t.Fatalf("emitted %d frames, want >= 5 (RunStarted, StateSnapshot, text triad)", len(frames))
	}
	rs, ok := frames[0].(agui.RunStarted)
	if !ok {
		t.Fatalf("frame[0] = %T, want agui.RunStarted", frames[0])
	}
	if rs.ThreadID != "t1" || rs.RunID != "r1" {
		t.Fatalf("RunStarted = %+v, want t1/r1", rs)
	}
	if _, ok := frames[1].(agui.StateSnapshot); !ok {
		t.Fatalf("frame[1] = %T, want agui.StateSnapshot", frames[1])
	}
	// The text triad carries the model's answer.
	var start, content, end bool
	var text string
	for _, f := range frames[2:] {
		switch ev := f.(type) {
		case agui.TextMessageStart:
			start = true
		case agui.TextMessageContent:
			content = true
			text = ev.Delta
		case agui.TextMessageEnd:
			end = true
		case agui.RunFinished, agui.RunError:
			t.Fatalf("backend emitted a terminal frame %T; that is the server's job", f)
		}
	}
	if !start || !content || !end {
		t.Fatalf("text triad incomplete: start=%v content=%v end=%v", start, content, end)
	}
	if text != "done" {
		t.Fatalf("text content = %q, want %q", text, "done")
	}
}

// TestAGUIBackendRunEchoesState pins that the opening StateSnapshot echoes the
// client's input state verbatim, and falls back to {} when absent. Neutralize
// check: drop the in.State echo and the snapshot no longer carries the input.
func TestAGUIBackendRunEchoesState(t *testing.T) {
	b := newAGUIBackendRunner(t, &blockableModel{})

	emit, got := collectEmit()
	if _, err := b.RunAgent(context.Background(), agui.RunInput{
		ThreadID: "t1", RunID: "r1", Text: "hi", State: []byte(`{"count":1}`),
	}, emit); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	snap := (*got)[1].(agui.StateSnapshot)
	if string(snap.Snapshot) != `{"count":1}` {
		t.Fatalf("snapshot = %s, want the input state echoed", snap.Snapshot)
	}

	emit2, got2 := collectEmit()
	if _, err := b.RunAgent(context.Background(), agui.RunInput{
		ThreadID: "t2", RunID: "r2", Text: "hi",
	}, emit2); err != nil {
		t.Fatalf("RunAgent(no state): %v", err)
	}
	if snap := (*got2)[1].(agui.StateSnapshot); string(snap.Snapshot) != `{}` {
		t.Fatalf("absent-state snapshot = %s, want {}", snap.Snapshot)
	}
}

// TestAGUIBackendRunAborted: continuing an aborted session hits the runTurnPre
// chokepoint (ErrConflict), and classifyRun projects the session's durable
// aborted state onto RunResult.Aborted (→ the server's RunError{aborted})
// rather than a generic error. Neutralize check: drop the ErrConflict branch
// in classifyRun and this reports a generic error instead of Aborted.
func TestAGUIBackendRunAborted(t *testing.T) {
	b := newAGUIBackendRunner(t, &blockableModel{})
	ctx := context.Background()
	emit, _ := collectEmit()

	if _, err := b.RunAgent(ctx, agui.RunInput{ThreadID: "t1", RunID: "r1", Text: "hi"}, emit); err != nil {
		t.Fatalf("first RunAgent: %v", err)
	}
	sid := b.sessionIDFor("t1", "r1", nil)
	if err := b.store.Abort(ctx, "", sid, "operator abort"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	res, err := b.RunAgent(ctx, agui.RunInput{ThreadID: "t1", RunID: "r2", Text: "again"}, emit)
	if err != nil {
		t.Fatalf("RunAgent(aborted): %v", err)
	}
	if !res.Aborted {
		t.Fatalf("result = %+v, want Aborted (chokepoint refusal projected from the store)", res)
	}
}

// TestAGUIClassifyRunAbortedMidFlight: an operator abort that lands WHILE the
// turn is running cancels the turn ctx from outside the chokepoint, so
// runTurnPre returns a plain context cancellation (not inject.ErrConflict) and
// the run falls into classifyRun's default branch. The durable abort marker is
// the ground truth, so the run must report Aborted, not a generic internal
// error — the AG-UI analogue of A2A's sticky-Canceled reconciliation. The read
// goes through a detached context because the turn ctx is already canceled.
// Neutralize check: drop the StateAborted consult in the default branch (return
// err directly) and this reports the raw cancellation instead of Aborted.
func TestAGUIClassifyRunAbortedMidFlight(t *testing.T) {
	b := newAGUIBackendRunner(t, &blockableModel{})
	// A completed run first, so the session exists for the abort marker to land
	// on (store.Abort requires an existing session).
	emit, _ := collectEmit()
	if _, err := b.RunAgent(context.Background(), agui.RunInput{ThreadID: "t1", RunID: "r1", Text: "hi"}, emit); err != nil {
		t.Fatalf("seed RunAgent: %v", err)
	}
	sid := b.sessionIDFor("t1", "r1", nil)
	if err := b.store.Abort(context.Background(), "", sid, "operator abort mid-flight"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	// The turn ctx is canceled by the abort sweep; model that here so the test
	// also exercises the detached-context read.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := b.classifyRun(ctx, sid, &aguiEmitter{}, context.Canceled)
	if err != nil {
		t.Fatalf("classifyRun returned err %v, want nil (abort marker projected to Aborted)", err)
	}
	if !res.Aborted {
		t.Fatalf("result = %+v, want Aborted (durable abort marker is ground truth)", res)
	}
}

// TestBuildAGUIServerRejectsBadSessionModel: an unknown agui.session_model
// fails daemon startup (fail-fast) rather than silently falling back to
// per_thread at runtime, which would route runs to a different session model
// than the operator wrote. Neutralize check: drop the session_model guard in
// buildAGUIServer and this constructs a server instead of erroring.
func TestBuildAGUIServerRejectsBadSessionModel(t *testing.T) {
	bundle := &workload.Bundle{
		Name: "wl",
		AGUI: workload.AGUI{Expose: true, SessionModel: "bogus"},
	}
	_, err := buildAGUIServer(discardLogger(), "127.0.0.1:0", bundle, &aguiBackend{}, nil, context.Background())
	if err == nil {
		t.Fatal("buildAGUIServer accepted invalid session_model, want error")
	}
	if !strings.Contains(err.Error(), "session_model") {
		t.Fatalf("error %q does not mention session_model", err)
	}
}

// TestAGUIBackendRunFailed: a genuine runner error (not a chokepoint
// ErrConflict) surfaces as a returned error so the server closes the stream
// with a generic RunError{internal} — never a fabricated success. Neutralize
// check: return a nil error from classifyRun's default branch and this reports
// a clean result.
func TestAGUIBackendRunFailed(t *testing.T) {
	b := newAGUIBackendRunner(t, errModel{})
	emit, _ := collectEmit()
	res, err := b.RunAgent(context.Background(), agui.RunInput{ThreadID: "t1", RunID: "r1", Text: "hi"}, emit)
	if err == nil {
		t.Fatalf("RunAgent(errModel) err = nil, want the runner error surfaced (result=%+v)", res)
	}
	if res.Text != "" || res.Aborted || res.Interrupted {
		t.Fatalf("failed run carried a disposition %+v, want zero", res)
	}
}

// TestAGUIBackendDrainingNoEmit: once shutdown drain begins, a run is refused
// with agui.ErrUnavailable BEFORE any emit, so the server reports a clean HTTP
// 503 rather than an orphaned stream. Neutralize check: move the drain gate
// below the opening emits and a draining run leaks frames.
func TestAGUIBackendDrainingNoEmit(t *testing.T) {
	b := newAGUIBackendRunner(t, &blockableModel{})
	b.tracker.beginDrain(context.Background())
	emit, got := collectEmit()
	_, err := b.RunAgent(context.Background(), agui.RunInput{ThreadID: "t1", RunID: "r1", Text: "hi"}, emit)
	if !errors.Is(err, agui.ErrUnavailable) {
		t.Fatalf("draining RunAgent err = %v, want agui.ErrUnavailable", err)
	}
	if len(*got) != 0 {
		t.Fatalf("draining run emitted %d frames, want 0 (refusal precedes any emit)", len(*got))
	}
}

// TestAGUIBackendRejectsForeignSession pins the ownership fence: a crafted
// threadId that pushes the derived session id into the reserved ops-row
// namespace is refused BEFORE any emit, so a caller cannot drive a turn into a
// reserved marker-storage row. Neutralize check: drop the isAGUISessionID guard
// in RunAgent and the crafted id drives a turn (emitting frames) instead of
// refusing.
func TestAGUIBackendRejectsForeignSession(t *testing.T) {
	b := newAGUIBackendRunner(t, &blockableModel{})
	emit, got := collectEmit()
	// "x:mast-ops" derives to "agui-thread-x:mast-ops", which ends in the
	// reserved ops suffix.
	_, err := b.RunAgent(context.Background(), agui.RunInput{ThreadID: "x:mast-ops", RunID: "r1", Text: "hijack"}, emit)
	if err == nil {
		t.Fatal("RunAgent(reserved-id) err = nil, want a refusal")
	}
	if len(*got) != 0 {
		t.Fatalf("reserved-id run emitted %d frames before refusal, want 0", len(*got))
	}
}

// TestAGUIBackendSessionModel pins session-id derivation for both models: a
// per_thread run keys on threadId, a per_run run keys on runId, and both are
// namespaced under the AG-UI prefix. Neutralize check: drop the prefix and
// isAGUISessionID rejects the derived id; swap the model branches and the
// derived id keys on the wrong field.
func TestAGUIBackendSessionModel(t *testing.T) {
	perThread := &aguiBackend{} // nil bundle → default per_thread
	if got := perThread.sessionIDFor("t1", "r1", nil); got != "agui-thread-t1" {
		t.Fatalf("per_thread session = %q, want agui-thread-t1", got)
	}
	if !isAGUISessionID("agui-thread-t1") {
		t.Fatal("agui-thread-t1 not recognized as an owned session id")
	}

	perRun := &aguiBackend{bundle: &workload.Bundle{AGUI: workload.AGUI{SessionModel: workload.AGUISessionPerRun}}}
	if got := perRun.sessionIDFor("t1", "r1", nil); got != "agui-run-r1" {
		t.Fatalf("per_run session = %q, want agui-run-r1", got)
	}

	// An absent correlation id mints a fresh owned session rather than
	// colliding id-less callers onto one shared session.
	minted := perThread.sessionIDFor("", "", nil)
	if !isAGUISessionID(minted) || minted == "agui-thread-" {
		t.Fatalf("id-less run session = %q, want a fresh minted agui- id", minted)
	}
}

// TestAGUIEmitterOnEvent pins the event→frame translation in isolation: a model
// text event becomes a TextMessage triad with a unique messageId; a model
// FunctionCall becomes a ToolCall start/args/end triple; a FunctionResponse
// (arriving on a non-model event) becomes a ToolCallResult; and a RequestedInput
// marks the run interrupted. Neutralize checks: drop the text triad and no text
// frame appears; drop the FunctionCall branch and no ToolCall frame appears;
// drop the interrupted set and inputRequired stays false.
func TestAGUIEmitterOnEvent(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit}

	// Model text event → triad; captured as lastText.
	e.onEvent(mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "hello"}}}))
	if e.lastText != "hello" {
		t.Fatalf("lastText = %q, want hello", e.lastText)
	}
	// Model function-call event → tool triple parented to the call's message.
	fc := genai.NewPartFromFunctionCall("search", map[string]any{"q": "x"})
	fc.FunctionCall.ID = "call-1"
	e.onEvent(mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{fc}}))
	// Tool result on a non-model (tool) event → ToolCallResult.
	fr := genai.NewPartFromFunctionResponse("search", map[string]any{"hits": 3})
	fr.FunctionResponse.ID = "call-1"
	e.onEvent(mkEvent(&genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{fr}}))
	// A RequestedInput marks the run interrupted.
	e.onEvent(&adksession.Event{RequestedInput: &adksession.RequestInput{Message: "approve?"}})
	if !e.interrupted {
		t.Fatal("RequestedInput did not set interrupted")
	}

	var textFrames, toolStart, toolArgs, toolEnd, toolResult int
	var msgIDs []string
	for _, f := range *got {
		switch ev := f.(type) {
		case agui.TextMessageStart:
			msgIDs = append(msgIDs, ev.MessageID)
		case agui.TextMessageContent:
			textFrames++
		case agui.ToolCallStart:
			toolStart++
			if ev.ToolCallID != "call-1" || ev.ToolCallName != "search" {
				t.Fatalf("ToolCallStart = %+v, want call-1/search", ev)
			}
		case agui.ToolCallArgs:
			toolArgs++
		case agui.ToolCallEnd:
			toolEnd++
		case agui.ToolCallResult:
			toolResult++
			if ev.ToolCallID != "call-1" {
				t.Fatalf("ToolCallResult id = %q, want call-1", ev.ToolCallID)
			}
		}
	}
	if textFrames != 1 {
		t.Fatalf("text content frames = %d, want 1", textFrames)
	}
	if toolStart != 1 || toolArgs != 1 || toolEnd != 1 || toolResult != 1 {
		t.Fatalf("tool frames = start:%d args:%d end:%d result:%d, want 1 each", toolStart, toolArgs, toolEnd, toolResult)
	}
	if len(msgIDs) != 1 || msgIDs[0] == "" {
		t.Fatalf("message ids = %v, want one non-empty id", msgIDs)
	}
}

// TestAGUIEmitterSkipsUserText pins that a user-authored text event is NOT
// re-emitted as an assistant text frame — only model-authored text streams to
// the client. Neutralize check: drop the role==model guard and the user text
// leaks as an assistant frame.
func TestAGUIEmitterSkipsUserText(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit}
	e.onEvent(mkEvent(genai.NewContentFromText("user typed this", genai.RoleUser)))
	if len(*got) != 0 {
		t.Fatalf("user text emitted %d frames, want 0", len(*got))
	}
	if e.lastText != "" {
		t.Fatalf("user text captured as lastText %q, want empty", e.lastText)
	}
}

// TestAGUIEmitterDropsPartials drives the emitter with the sequence a
// streaming model produces — chunk, chunk, chunk, then the aggregate — and
// pins that only the aggregate reaches the wire (#400).
//
// The two things this catches are not equally bad, and the tool half is the
// one that breaks a client: each chunk of a streamed FunctionCall carries
// PartialArgs and no Args, so pre-fix the emitter minted a COMPLETE
// start/args/end triple per chunk with empty arguments, and anything
// dispatching on TOOL_CALL_END would run the tool once per chunk. The text
// half is a rendering bug by comparison — N one-fragment bubbles, then the
// whole answer again.
//
// Neutralize check (performed, both halves red without the guard): remove
// `if ev.Partial { return }` from onEvent and this reports 4 text content
// frames instead of 1 and 3 tool triples instead of 1.
func TestAGUIEmitterDropsPartials(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit}

	// Three text chunks, then the aggregate carrying the whole answer.
	for _, chunk := range []string{"hel", "lo ", "there"} {
		ev := mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: chunk}}})
		ev.Partial = true
		e.onEvent(ev)
	}
	e.onEvent(mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "hello there"}}}))

	// Two arg chunks with no Args at all — the shape the streaming-args path
	// produces — then the assembled call.
	for range 2 {
		part := &genai.Part{FunctionCall: &genai.FunctionCall{ID: "call-9", Name: "search"}}
		ev := mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}})
		ev.Partial = true
		e.onEvent(ev)
	}
	fc := genai.NewPartFromFunctionCall("search", map[string]any{"q": "x"})
	fc.FunctionCall.ID = "call-9"
	e.onEvent(mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{fc}}))

	var textContent, toolStart, toolEnd int
	for _, f := range *got {
		switch f.(type) {
		case agui.TextMessageContent:
			textContent++
		case agui.ToolCallStart:
			toolStart++
		case agui.ToolCallEnd:
			toolEnd++
		}
	}
	if textContent != 1 {
		t.Fatalf("text content frames = %d, want 1 (partials must not emit)", textContent)
	}
	if toolStart != 1 || toolEnd != 1 {
		t.Fatalf("tool frames = start:%d end:%d, want 1 each — a TOOL_CALL_END per chunk is a tool dispatched per chunk", toolStart, toolEnd)
	}
	// The aggregate, not the last chunk, is what the run reports.
	if e.lastText != "hello there" {
		t.Fatalf("lastText = %q, want the aggregate %q", e.lastText, "hello there")
	}
}

// TestAGUIExposedWorkloads: not opted in → nil; opted in → one endpoint with
// defaults filled (endpoint_path=/agui/<name>, description from the bundle).
func TestAGUIExposedWorkloads(t *testing.T) {
	if got := aguiExposedWorkloads(&workload.Bundle{Name: "w"}); got != nil {
		t.Fatalf("expose:false: got %v, want nil", got)
	}
	if got := aguiExposedWorkloads(nil); got != nil {
		t.Fatalf("nil bundle: got %v, want nil", got)
	}
	b := &workload.Bundle{
		Name:        "triage",
		Description: "GKE triage",
		AGUI:        workload.AGUI{Expose: true, Auth: workload.AGUIAuth{Scopes: []string{"triage:run"}}},
	}
	got := aguiExposedWorkloads(b)
	if len(got) != 1 {
		t.Fatalf("got %d workloads, want 1", len(got))
	}
	ew := got[0]
	if ew.WorkloadName != "triage" || ew.EndpointPath != "/agui/triage" || ew.Description != "GKE triage" {
		t.Fatalf("exposed = %+v", ew)
	}
	if len(ew.Scopes) != 1 || ew.Scopes[0] != "triage:run" {
		t.Fatalf("scopes = %v", ew.Scopes)
	}
	// Explicit endpoint path + description win.
	b.AGUI.EndpointPath = "/custom"
	b.AGUI.Description = "custom desc"
	got = aguiExposedWorkloads(b)
	if got[0].EndpointPath != "/custom" || got[0].Description != "custom desc" {
		t.Fatalf("explicit exposed = %+v", got[0])
	}
}

func TestAGUIValidator(t *testing.T) {
	exposed := []agui.ExposedWorkload{
		{WorkloadName: "a", Scopes: []string{"a:run", "shared"}},
		{WorkloadName: "b", Scopes: []string{"b:run", "shared"}},
	}
	logger := newLogger("error")

	t.Setenv("MAST_AGUI_TOKEN", "")
	v, err := aguiValidator(logger, exposed)
	if err != nil {
		t.Fatalf("aguiValidator(unset): %v", err)
	}
	if v != nil {
		t.Fatal("unset token: want nil validator")
	}

	t.Setenv("MAST_AGUI_TOKEN", "secret")
	v, err = aguiValidator(logger, exposed)
	if err != nil {
		t.Fatalf("aguiValidator(set): %v", err)
	}
	p, err := v.Validate(t.Context(), "secret")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, want := range []string{"a:run", "b:run", "shared"} {
		if !p.HasScope(want) {
			t.Fatalf("principal missing scope %q (have %v)", want, p.Scopes)
		}
	}
	if n := countTest(p.Scopes, "shared"); n != 1 {
		t.Fatalf("scope \"shared\" appears %d times, want 1 (de-duplicated)", n)
	}
}

func TestAGUIRateLimiter(t *testing.T) {
	logger := newLogger("error")

	t.Setenv("MAST_AGUI_RATE", "")
	t.Setenv("MAST_AGUI_BURST", "")
	lim, err := aguiRateLimiter(logger)
	if err != nil {
		t.Fatalf("aguiRateLimiter(unset): %v", err)
	}
	if lim != nil {
		t.Fatal("unset rate: want nil limiter")
	}

	t.Setenv("MAST_AGUI_RATE", "2.5")
	if lim, err = aguiRateLimiter(logger); err != nil || lim == nil {
		t.Fatalf("aguiRateLimiter(2.5) = %v, %v; want a limiter", lim, err)
	}

	// Fail closed on hostile input rather than silently disabling.
	t.Setenv("MAST_AGUI_BURST", "")
	for _, bad := range []string{"abc", " 5 ", "Inf", "NaN", "0", "-1", "1e400"} {
		t.Setenv("MAST_AGUI_RATE", bad)
		if _, err := aguiRateLimiter(logger); err == nil {
			t.Errorf("MAST_AGUI_RATE=%q: want error, got nil", bad)
		}
	}
	t.Setenv("MAST_AGUI_RATE", "5")
	for _, bad := range []string{"abc", "0", "-1"} {
		t.Setenv("MAST_AGUI_BURST", bad)
		if _, err := aguiRateLimiter(logger); err == nil {
			t.Errorf("MAST_AGUI_BURST=%q: want error, got nil", bad)
		}
	}
}

// TestAGUIOutcomeVocabulary pins observability's exported AG-UI outcome
// constants to their fixed string values. pkg/agui records outcomes through
// observability.Registry.AGUIRun using its own unexported constants (locked to
// the same literals by pkg/agui's TestOutcomeConstantLiterals); this test locks
// the registry side, so the two pins together prevent either from drifting and
// producing an unprimed scrape series.
func TestAGUIOutcomeVocabulary(t *testing.T) {
	pairs := map[string]string{
		observability.AGUIRunSuccess:     "success",
		observability.AGUIRunError:       "error",
		observability.AGUIRunAborted:     "aborted",
		observability.AGUIRunRejected:    "rejected",
		observability.AGUIRunInterrupted: "interrupted",
		observability.AGUIRunQueueFull:   "queue_full",
	}
	for got, want := range pairs {
		if got != want {
			t.Fatalf("outcome %q != wire value %q (the metric vocabulary drifted from pkg/agui)", got, want)
		}
	}
}

// mkEvent wraps a genai.Content in a runner event for onEvent tests.
func mkEvent(c *genai.Content) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-agui-test")
	ev.Content = c
	return ev
}

// TestInterruptsFromDetailMarshalsSchema pins that a pending input's response
// schema is projected onto the AG-UI interrupt as raw JSON (so a client can
// render an input form), and that a schemaless pending input yields an interrupt
// with no schema (omitted, not a null). Neutralize check: drop the ResponseSchema
// marshal in interruptsFromDetail and the schema key vanishes from the wire.
func TestInterruptsFromDetailMarshalsSchema(t *testing.T) {
	d := &transcript.Detail{
		Pending: []transcript.PendingInput{
			{
				InterruptID:    "int-1",
				Message:        "approve?",
				ResponseSchema: &jsonschema.Schema{Type: "object"},
			},
			{InterruptID: "int-2", Message: "no schema"},
		},
	}
	its := interruptsFromDetail(d)
	if len(its) != 2 {
		t.Fatalf("interrupts = %d, want 2", len(its))
	}
	if its[0].ID != "int-1" || its[0].Message != "approve?" {
		t.Fatalf("interrupt[0] = %+v, want int-1/approve?", its[0])
	}
	if len(its[0].ResponseSchema) == 0 {
		t.Fatal("interrupt[0] lost its response schema")
	}
	var schema map[string]any
	if err := json.Unmarshal(its[0].ResponseSchema, &schema); err != nil {
		t.Fatalf("response schema is not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("response schema type = %v, want object", schema["type"])
	}
	if len(its[1].ResponseSchema) != 0 {
		t.Errorf("schemaless interrupt carried a schema: %s", its[1].ResponseSchema)
	}
}

// TestResumeAnswer pins how a resume entry's payload becomes the value forwarded
// to the parked tool: a payload is forwarded verbatim; a resolved entry with no
// payload forwards an empty object; a cancelled entry with no payload forwards a
// minimal cancellation disposition so the tool can tell it apart from an empty
// resolution. Neutralize check: collapse the cancelled branch and a payloadless
// cancel is indistinguishable from an empty resolve.
func TestResumeAnswer(t *testing.T) {
	// Verbatim payload (an object).
	if got := resumeAnswer(agui.ResumeEntry{Status: agui.ResumeStatusResolved, Payload: json.RawMessage(`{"approved":true}`)}); got.(map[string]any)["approved"] != true {
		t.Fatalf("resolved payload not forwarded verbatim: %#v", got)
	}
	// Verbatim payload (a scalar).
	if got := resumeAnswer(agui.ResumeEntry{Status: agui.ResumeStatusResolved, Payload: json.RawMessage(`"go"`)}); got != "go" {
		t.Fatalf("scalar payload = %#v, want \"go\"", got)
	}
	// Resolved, no payload → empty object (not a cancellation marker).
	got := resumeAnswer(agui.ResumeEntry{Status: agui.ResumeStatusResolved})
	m, ok := got.(map[string]any)
	if !ok || len(m) != 0 {
		t.Fatalf("payloadless resolve = %#v, want empty object", got)
	}
	// Cancelled, no payload → cancellation disposition.
	got = resumeAnswer(agui.ResumeEntry{Status: agui.ResumeStatusCancelled})
	if m, ok := got.(map[string]any); !ok || m["status"] != string(agui.ResumeStatusCancelled) {
		t.Fatalf("payloadless cancel = %#v, want {status: cancelled}", got)
	}
}

// aguiScriptedModel is a model.LLM whose reply per call is chosen by a script
// over the round number and request, used to drive a planner root through a real
// HITL pause/resume from the AG-UI backend. Each response synthesizes usage
// metadata (like a real model) so budget metering does not divide by zero.
type aguiScriptedModel struct {
	script func(round int, req *model.LLMRequest) *model.LLMResponse
	round  int
}

func (m *aguiScriptedModel) Name() string { return "agui-scripted" }

func (m *aguiScriptedModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.round++
		resp := m.script(m.round, req)
		if resp.UsageMetadata == nil {
			resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2,
			}
		}
		resp.TurnComplete = true
		resp.FinishReason = genai.FinishReasonStop
		yield(resp, nil)
	}
}

// aguiCallResponse builds a single-tool-call model response.
func aguiCallResponse(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)},
		},
	}
}

// aguiLastFunctionResponse returns the last FunctionResponse payload visible in a
// request's contents (what the model saw the operator answer with, regardless of
// the name the resume was delivered under — a resume rides adk_request_input, not
// the parked tool's own name).
func aguiLastFunctionResponse(req *model.LLMRequest) map[string]any {
	var last map[string]any
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Response != nil {
				last = p.FunctionResponse.Response
			}
		}
	}
	return last
}

// newAGUIBackendPlanner builds an aguiBackend over a real planner root (which
// carries the request_operator_input long-running tool), so RunAgent can drive a
// genuine HITL pause and resume through runTurnPre — the plain llmagent harness
// (newAGUIBackendRunner) cannot park a turn.
func newAGUIBackendPlanner(t *testing.T, m model.LLM) *aguiBackend {
	t.Helper()
	svc := adksession.InMemoryService()
	obs := observability.New()
	obs.Prime("(test)")
	store := transcript.NewStore(svc, appName)
	root, err := planner.NewRoot(planner.Config{Name: "w", Model: m})
	if err != nil {
		t.Fatalf("planner.NewRoot: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return &aguiBackend{turnDeps: turnDeps{
		store:        store,
		obs:          obs,
		tracker:      newTurnTracker(store, discardLogger(), obs, "(test)"),
		logger:       discardLogger(),
		workloadName: "(test)",
		r:            r,
		meters:       newMeterPool(nil, nil, "", "test-model"),
		wds:          newWatchdogPool(watchdog.ModeWarn),
		turnLocks:    newSessionTurnLocks(),
	}}
}

// TestAGUIBackendHITLPauseAndResume drives the full HITL lifecycle over a planner
// root. Turn 1's request_operator_input call parks the run, so RunAgent returns
// Interrupted carrying the open interrupt (id + operator message + response
// schema) projected from durable state — the server renders that as
// RunFinished{outcome: interrupt}. A resume naming no open interrupt is refused
// with ErrNotResumable before any emit (→ HTTP 409). Turn 2 answers the real
// interrupt id; RunAgent unparks the planner via a FunctionResponse named
// adk_request_input and the run finishes clean.
//
// The resume FunctionResponse name matches the operator-facing resume()
// (adk_request_input, load-bearing for graph roots — a planner root resumes
// under either name, so this test cannot pin the name itself).
//
// Neutralize check: force the pendingInterrupts projection in classifyRun's
// err==nil branch to miss (return ok=false) and turn 1 no longer surfaces its
// open interrupt — it falls through to the em.interrupted guard and RunAgent
// returns an error, failing this test at the pause-turn assertion. (The clean
// resume-less-success misreport is separately pinned by
// TestAGUIClassifyRunInterruptWithoutProjection.)
func TestAGUIBackendHITLPauseAndResume(t *testing.T) {
	var operatorAnswer map[string]any
	m := &aguiScriptedModel{script: func(round int, req *model.LLMRequest) *model.LLMResponse {
		if round == 1 {
			return aguiCallResponse(planner.ToolRequestOperatorInput, map[string]any{
				"message": "approve the rollback?",
				"schema":  map[string]any{"type": "object"},
			})
		}
		// Resume round: the operator's answer must be visible to the planner.
		if fr := aguiLastFunctionResponse(req); fr != nil {
			operatorAnswer = fr
		}
		return aguiCallResponse("finish_task", map[string]any{"result": "rolled back"})
	}}
	b := newAGUIBackendPlanner(t, m)

	// Turn 1: pause.
	emit, _ := collectEmit()
	res, err := b.RunAgent(context.Background(), agui.RunInput{ThreadID: "t1", RunID: "r1", Text: "do risky thing"}, emit)
	if err != nil {
		t.Fatalf("RunAgent(pause turn): %v", err)
	}
	if !res.Interrupted {
		t.Fatalf("pause turn result = %+v, want Interrupted", res)
	}
	if len(res.Interrupts) != 1 {
		t.Fatalf("interrupts = %+v, want exactly one open interrupt", res.Interrupts)
	}
	it := res.Interrupts[0]
	if it.ID == "" {
		t.Fatal("interrupt carries no ID (the resume correlation key)")
	}
	if it.Message != "approve the rollback?" {
		t.Fatalf("interrupt message = %q, want the operator prompt", it.Message)
	}
	// A request_operator_input park projects only id + message (scanPending does
	// not lift the tool's "schema" arg into ResponseSchema — that field is
	// populated only for RequestedInput-source pauses); the schema-marshal path
	// is covered by TestInterruptsFromDetailMarshalsSchema.
	if res.Text != "" {
		t.Errorf("paused turn carried answer text %q, want none", res.Text)
	}

	// A resume naming no open interrupt is refused BEFORE any emit so the server
	// answers HTTP 409 rather than opening an orphaned stream.
	emitBad, gotBad := collectEmit()
	_, err = b.RunAgent(context.Background(), agui.RunInput{
		ThreadID: "t1", RunID: "r2",
		Resume: []agui.ResumeEntry{{InterruptID: "no-such-id", Status: agui.ResumeStatusResolved}},
	}, emitBad)
	if !errors.Is(err, agui.ErrNotResumable) {
		t.Fatalf("resume(unknown id) err = %v, want agui.ErrNotResumable", err)
	}
	if len(*gotBad) != 0 {
		t.Fatalf("unresumable run emitted %d frames, want 0 (refusal precedes any emit)", len(*gotBad))
	}

	// Turn 2: resume the real interrupt — unparks the planner, run finishes.
	emit2, _ := collectEmit()
	res2, err := b.RunAgent(context.Background(), agui.RunInput{
		ThreadID: "t1", RunID: "r3",
		Resume: []agui.ResumeEntry{{
			InterruptID: it.ID,
			Status:      agui.ResumeStatusResolved,
			Payload:     json.RawMessage(`{"approved":true}`),
		}},
	}, emit2)
	if err != nil {
		t.Fatalf("RunAgent(resume turn): %v", err)
	}
	if res2.Interrupted {
		t.Fatalf("resumed run still Interrupted %+v — the resume forked a fresh turn instead of unparking", res2)
	}
	if operatorAnswer == nil {
		t.Fatal("planner never saw the operator answer — the resume did not unpark the parked call")
	}
}

// TestAGUIBackendPerRunResumeViaParentRunID pins the per_run resume path. Under
// the run-keyed session model the parked session is keyed on the ORIGINAL run's
// id, but a resume arrives as a NEW run (new RunID) — so it can only reach the
// parked session by carrying parentRunId. With it, turn 2 unparks the parked
// run; without it the resume derives a fresh (empty) session and is correctly
// refused with ErrNotResumable. (per_thread is covered by
// TestAGUIBackendHITLPauseAndResume, where the shared threadId already reaches
// the parked session.)
//
// Neutralize check: drop the `runID = in.ParentRunID` override in RunAgent and
// the ParentRunID resume derives agui-run-<new id>, reads an empty session, and
// returns ErrNotResumable — the unpark assertion below then fails.
func TestAGUIBackendPerRunResumeViaParentRunID(t *testing.T) {
	var operatorAnswer map[string]any
	m := &aguiScriptedModel{script: func(round int, req *model.LLMRequest) *model.LLMResponse {
		if round == 1 {
			return aguiCallResponse(planner.ToolRequestOperatorInput, map[string]any{
				"message": "approve?",
			})
		}
		if fr := aguiLastFunctionResponse(req); fr != nil {
			operatorAnswer = fr
		}
		return aguiCallResponse("finish_task", map[string]any{"result": "done"})
	}}
	b := newAGUIBackendPlanner(t, m)
	b.bundle = &workload.Bundle{AGUI: workload.AGUI{SessionModel: workload.AGUISessionPerRun}}

	// Turn 1: pause under per_run → session agui-run-r1.
	emit, _ := collectEmit()
	res, err := b.RunAgent(context.Background(), agui.RunInput{ThreadID: "t1", RunID: "r1", Text: "go"}, emit)
	if err != nil {
		t.Fatalf("RunAgent(pause turn): %v", err)
	}
	if len(res.Interrupts) != 1 {
		t.Fatalf("interrupts = %+v, want exactly one open interrupt", res.Interrupts)
	}
	iid := res.Interrupts[0].ID

	// A resume WITHOUT parentRunId lands on a fresh per_run session (agui-run-r2)
	// that holds no open interrupt → refused before any emit.
	emitNo, gotNo := collectEmit()
	_, err = b.RunAgent(context.Background(), agui.RunInput{
		ThreadID: "t1", RunID: "r2",
		Resume: []agui.ResumeEntry{{InterruptID: iid, Status: agui.ResumeStatusResolved}},
	}, emitNo)
	if !errors.Is(err, agui.ErrNotResumable) {
		t.Fatalf("per_run resume without parentRunId err = %v, want agui.ErrNotResumable", err)
	}
	if len(*gotNo) != 0 {
		t.Fatalf("refused resume emitted %d frames, want 0", len(*gotNo))
	}

	// A resume WITH parentRunId=r1 reaches the parked session and unparks it.
	emit2, _ := collectEmit()
	res2, err := b.RunAgent(context.Background(), agui.RunInput{
		ThreadID: "t1", RunID: "r3", ParentRunID: "r1",
		Resume: []agui.ResumeEntry{{
			InterruptID: iid,
			Status:      agui.ResumeStatusResolved,
			Payload:     json.RawMessage(`{"approved":true}`),
		}},
	}, emit2)
	if err != nil {
		t.Fatalf("RunAgent(per_run resume via parentRunId): %v", err)
	}
	if res2.Interrupted {
		t.Fatalf("resumed run still Interrupted %+v — parentRunId did not reach the parked session", res2)
	}
	if operatorAnswer == nil {
		t.Fatal("planner never saw the operator answer — the parentRunId resume did not unpark")
	}
}

// TestAGUIClassifyRunInterruptWithoutProjection pins the degraded-path guard: if
// the in-turn interrupt signal fired (em.interrupted) but the durable pause
// projection is unavailable — a transient store read failure, or a pause whose
// marker did not land — classifyRun closes the stream with an honest internal
// error rather than advertising a resume-less interrupt (an interrupt outcome
// with an empty interrupts list the client could never answer) or fabricating a
// success.
//
// Neutralize check: restore the old `return RunResult{Interrupted: true}` in the
// em.interrupted branch and this test fails (err == nil, Interrupted true, no
// interrupts).
func TestAGUIClassifyRunInterruptWithoutProjection(t *testing.T) {
	b := newAGUIBackendPlanner(t, &aguiScriptedModel{script: func(int, *model.LLMRequest) *model.LLMResponse { return nil }})
	em := &aguiEmitter{interrupted: true}
	// A session that was never created has no durable pause → pendingInterrupts
	// returns ok=false, driving the em.interrupted fallback.
	res, err := b.classifyRun(context.Background(), "agui-thread-never-created", em, nil)
	if err == nil {
		t.Fatalf("classifyRun(interrupt signaled, no projection) = %+v, want an error", res)
	}
	if res.Interrupted || len(res.Interrupts) != 0 {
		t.Fatalf("degraded interrupt path leaked a resume-less interrupt: %+v", res)
	}
}

// mkStateEvent builds a runner event carrying only a state write — no content
// at all, which is the shape pkg/graph stashes a node result on.
func mkStateEvent(delta map[string]any) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-agui-test")
	ev.Actions.StateDelta = delta
	return ev
}

// stateDeltas extracts the decoded patch arrays from the collected frames, in
// emission order.
func stateDeltas(t *testing.T, frames []any) [][]agui.PatchOp {
	t.Helper()
	var out [][]agui.PatchOp
	for _, f := range frames {
		sd, ok := f.(agui.StateDelta)
		if !ok {
			continue
		}
		var ops []agui.PatchOp
		if err := json.Unmarshal(sd.Delta, &ops); err != nil {
			t.Fatalf("StateDelta.Delta %s is not a patch array: %v", sd.Delta, err)
		}
		out = append(out, ops)
	}
	return out
}

// TestAGUIEmitterPublishesNoStateWithoutAProjection is the default-off half of
// #98's state slice, and the one worth having: session state is whatever the
// runtime put there — pkg/graph writes routing keys and judge verdicts,
// pkg/approval writes grants and change sets — and the AG-UI client is a
// browser. A workload that declares no agui.state_projection must publish none
// of it.
//
// Neutralize check, measured rather than assumed: default-off is held by two
// independent guards in emitStateDelta — the len(e.projection) == 0 early
// return and the per-key e.projection[k] filter — and removing either one alone
// leaves this test green. Removing BOTH fails it, with a patch carrying the
// grant. That is the honest report: this test pins the property, and the
// separate TestAGUIEmitterProjectsOnlyAllowlistedKeys is what pins the filter.
func TestAGUIEmitterPublishesNoStateWithoutAProjection(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit}
	e.onEvent(mkStateEvent(map[string]any{
		"plan":                  "step 1",
		"mast:grant:scale/once": map[string]any{"approver": "sre-oncall"},
	}))
	if len(*got) != 0 {
		t.Fatalf("a workload with no state_projection emitted %d frame(s): %v", len(*got), *got)
	}
}

// TestAGUIEmitterProjectsOnlyAllowlistedKeys pins that the projection is a
// filter and not a redaction: an unlisted key produces no op at all, so a
// client cannot even learn that it changed. It also pins the ordering, because
// Go randomizes map iteration and a patch stream a client diffs across runs
// should not reorder for no reason.
//
// Neutralize check: replace the e.projection[k] test with an unconditional
// append and "secret" appears in the patch.
func TestAGUIEmitterProjectsOnlyAllowlistedKeys(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit, projection: map[string]bool{"plan": true, "phase": true}}
	e.onEvent(mkStateEvent(map[string]any{
		"plan":   "step 1",
		"phase":  "diagnose",
		"secret": "do not publish",
	}))

	deltas := stateDeltas(t, *got)
	if len(deltas) != 1 {
		t.Fatalf("state deltas = %d, want 1", len(deltas))
	}
	ops := deltas[0]
	if len(ops) != 2 {
		t.Fatalf("ops = %+v, want exactly the two allowlisted keys", ops)
	}
	if ops[0].Path != "/phase" || ops[1].Path != "/plan" {
		t.Fatalf("ops out of sorted key order: %q then %q", ops[0].Path, ops[1].Path)
	}
	for _, op := range ops {
		if op.Op != "add" {
			t.Fatalf("op %+v: want \"add\", the only op correct against a client state document mast cannot see", op)
		}
	}
	if string(ops[1].Value) != `"step 1"` {
		t.Fatalf("plan value = %s, want the state value verbatim", ops[1].Value)
	}
	for _, f := range *got {
		if sd, ok := f.(agui.StateDelta); ok && strings.Contains(string(sd.Delta), "secret") {
			t.Fatalf("unlisted key leaked into the patch: %s", sd.Delta)
		}
	}
}

// TestAGUIEmitterStateDeltaRidesAnEventWithNoContent pins the ordering bug the
// obvious implementation has: state writes ride on Actions, not Content, and
// pkg/graph stashes a node result on an event carrying no content at all.
//
// Neutralize check: move the emitStateDelta call below onEvent's
// `if ev.Content == nil { return }` and this fails with zero frames.
func TestAGUIEmitterStateDeltaRidesAnEventWithNoContent(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit, projection: map[string]bool{"result": true}}
	ev := mkStateEvent(map[string]any{"result": map[string]any{"ok": true}})
	if ev.Content != nil {
		t.Fatal("fixture is wrong: the event should carry no content")
	}
	e.onEvent(ev)
	if len(stateDeltas(t, *got)) != 1 {
		t.Fatalf("a contentless state event emitted %d frame(s), want 1", len(*got))
	}
}

// TestAGUIEmitterEscapesStateKeysInPointers: mast's own state keys are
// compound and generated — pkg/approval's grant key embeds a tool signature
// with a "/" in it — so an unescaped pointer would address a nested member
// that does not exist rather than the key that changed.
//
// Neutralize check: return "/" + key from agui.StatePointer and this fails.
func TestAGUIEmitterEscapesStateKeysInPointers(t *testing.T) {
	emit, got := collectEmit()
	key := "mast:grant:scale_deployment/once"
	e := &aguiEmitter{emit: emit, projection: map[string]bool{key: true}}
	e.onEvent(mkStateEvent(map[string]any{key: "granted"}))

	deltas := stateDeltas(t, *got)
	if len(deltas) != 1 || len(deltas[0]) != 1 {
		t.Fatalf("deltas = %+v, want one op", deltas)
	}
	if got := deltas[0][0].Path; got != "/mast:grant:scale_deployment~1once" {
		t.Fatalf("pointer = %q, want the slash escaped as ~1", got)
	}
}

// TestStatePointerEscapesBothReservedCharacters covers the half the emitter
// test above cannot reach on its own: RFC 6901 reserves "~" as well as "/", and
// the two escapes are order-dependent — escaping "/" first would turn a literal
// "~1" in a key into "~01" on the second pass and address the wrong member.
//
// Neutralize check: swap the two ReplaceAll calls in StatePointer and the
// "~1 already in the key" case fails.
func TestStatePointerEscapesBothReservedCharacters(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"plan", "/plan"},
		{"", "/"},
		{"a/b", "/a~1b"},
		{"a~b", "/a~0b"},
		{"a~1b", "/a~01b"},
		{"a/~b", "/a~1~0b"},
	} {
		if got := agui.StatePointer(tc.key); got != tc.want {
			t.Errorf("StatePointer(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// TestAGUIEmitterDropsAnUnmarshalableStateValue: null is a legitimate state
// value, so a marshal failure rendered as null would publish a change that did
// not happen. The key is dropped and the run continues.
func TestAGUIEmitterDropsAnUnmarshalableStateValue(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit, projection: map[string]bool{"bad": true, "good": true}}
	e.onEvent(mkStateEvent(map[string]any{
		"bad":  make(chan int), // channels do not marshal
		"good": 1,
	}))
	deltas := stateDeltas(t, *got)
	if len(deltas) != 1 || len(deltas[0]) != 1 {
		t.Fatalf("deltas = %+v, want exactly the one marshalable key", deltas)
	}
	if deltas[0][0].Path != "/good" {
		t.Fatalf("surviving op = %+v, want /good", deltas[0][0])
	}
}

// TestBuildAGUIServerRejectsBadStateProjection: an empty entry matches no key
// forever and a duplicate makes the list uncountable, so both are refused at
// startup rather than at the first state write. A key naming state the
// workload never writes is deliberately accepted — which keys a roster
// produces depends on the dispatch shape and on runtime-resolved tools.
func TestBuildAGUIServerRejectsBadStateProjection(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		want string
	}{
		{"empty entry", []string{"plan", "  "}, "is empty"},
		{"duplicate", []string{"plan", "plan"}, "twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := &workload.Bundle{
				Name: "wl",
				AGUI: workload.AGUI{Expose: true, StateProjection: tc.keys},
			}
			_, err := buildAGUIServer(discardLogger(), "127.0.0.1:0", bundle, &aguiBackend{}, nil, context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
	t.Run("a key nothing writes is accepted", func(t *testing.T) {
		bundle := &workload.Bundle{
			Name: "wl",
			AGUI: workload.AGUI{Expose: true, StateProjection: []string{"never_written"}},
		}
		if _, err := buildAGUIServer(discardLogger(), "127.0.0.1:0", bundle, &aguiBackend{}, nil, context.Background()); err != nil {
			t.Fatalf("buildAGUIServer refused an unwritten key: %v", err)
		}
	})
}

// TestAGUIBackendStateProjection pins the bundle→emitter wiring, which is the
// seam a unit test on the emitter alone cannot reach: a backend whose bundle
// declares no projection must build an emitter that publishes nothing.
func TestAGUIBackendStateProjection(t *testing.T) {
	if got := (&aguiBackend{}).publication().stateSet; got != nil {
		t.Fatalf("nil bundle: projection = %v, want nil", got)
	}
	if got := (&aguiBackend{bundle: &workload.Bundle{Name: "w"}}).publication().stateSet; got != nil {
		t.Fatalf("no state_projection declared: projection = %v, want nil", got)
	}
	b := &aguiBackend{bundle: &workload.Bundle{
		Name: "w",
		AGUI: workload.AGUI{Expose: true, StateProjection: []string{"plan", "phase"}},
	}}
	pub := b.publication()
	if len(pub.stateSet) != 2 || !pub.stateSet["plan"] || !pub.stateSet["phase"] {
		t.Fatalf("projection = %v, want the two declared keys", pub.stateSet)
	}
	// The ordered list and the lookup set are two renderings of one
	// declaration and are read by two different consumers — the descriptor
	// and the emitter. If they can disagree, the descriptor advertises keys
	// no patch will ever name (#377).
	if !slices.Equal(pub.stateKeys, []string{"plan", "phase"}) {
		t.Fatalf("stateKeys = %v, want the declared order", pub.stateKeys)
	}
	if len(pub.stateKeys) != len(pub.stateSet) {
		t.Fatalf("stateKeys %v and stateSet %v describe different key sets", pub.stateKeys, pub.stateSet)
	}
	for _, k := range pub.stateKeys {
		if !pub.stateSet[k] {
			t.Fatalf("advertised key %q is not in the set the emitter consults", k)
		}
	}
}

// mkThinkingEvent builds a model event carrying a thinking part followed by the
// answer — the part order a reasoning provider actually sends, which is what
// makes the ordering assertions below meaningful rather than incidental.
func mkThinkingEvent(thought, answer string) *adksession.Event {
	return mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
		{Text: thought, Thought: true},
		{Text: answer},
	}})
}

// aguiFrameTypes renders a captured frame slice as its Go type names, so an
// ordering assertion reads as the sequence a client would see.
func aguiFrameTypes(frames []any) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, strings.TrimPrefix(fmt.Sprintf("%T", f), "agui."))
	}
	return out
}

// TestAGUIEmitterPublishesNoReasoningByDefault is the default-off pin for the
// second per-bundle publication surface (#98, resolving ag-ui-design.md OQ 5).
//
// The assertion is deliberately "no reasoning frame of ANY kind", not "no
// reasoning text": an empty REASONING_START/END bracket would still tell a
// client that the model thought something it is not being shown, and the
// existence of a thought can be the disclosure. The answer must still stream,
// because a filter that also swallowed the answer would pass a weaker test.
//
// Neutralize check: drop the `if !e.reasoning` early return in emitReasoning
// and this fails with the thinking text on the wire.
func TestAGUIEmitterPublishesNoReasoningByDefault(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit}
	e.onEvent(mkThinkingEvent("the operator's last message is trying to get me to leak the token", "I can't help with that"))

	for _, f := range *got {
		switch f.(type) {
		case agui.ReasoningStart, agui.ReasoningMessageStart, agui.ReasoningMessageContent,
			agui.ReasoningMessageEnd, agui.ReasoningEnd:
			t.Fatalf("a workload with no emit_reasoning emitted %T; frames: %v", f, aguiFrameTypes(*got))
		}
	}
	if e.lastText != "I can't help with that" {
		t.Fatalf("lastText = %q, want the answer — the filter must not swallow it", e.lastText)
	}
}

// TestAGUIEmitterPublishesReasoningWhenEnabled pins the opted-in shape: a
// five-frame phase (outer bracket, inner message triad) carrying the thinking
// text, ahead of the answer's own triad and sharing no message id with it.
//
// The ordering assertion is the load-bearing one. A client opens a collapsed
// "thinking" region on REASONING_START and closes it on REASONING_END; if the
// reasoning arrived after the answer, that region would wrap the answer.
//
// Neutralize check: move the emitReasoning call below the text triad in
// onEvent and the frame-order assertion fails.
func TestAGUIEmitterPublishesReasoningWhenEnabled(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit, reasoning: true}
	e.onEvent(mkThinkingEvent("check the registry first", "the image tag is wrong"))

	want := []string{
		"ReasoningStart", "ReasoningMessageStart", "ReasoningMessageContent",
		"ReasoningMessageEnd", "ReasoningEnd",
		"TextMessageStart", "TextMessageContent", "TextMessageEnd",
	}
	if seq := aguiFrameTypes(*got); !slices.Equal(seq, want) {
		t.Fatalf("frames = %v, want %v", seq, want)
	}

	var reasoningID, answerID, delta string
	for _, f := range *got {
		switch ev := f.(type) {
		case agui.ReasoningMessageStart:
			reasoningID = ev.MessageID
		case agui.ReasoningMessageContent:
			delta = ev.Delta
		case agui.TextMessageStart:
			answerID = ev.MessageID
		}
	}
	if delta != "check the registry first" {
		t.Errorf("reasoning delta = %q, want the thinking text", delta)
	}
	if reasoningID == "" || reasoningID == answerID {
		t.Errorf("reasoning messageId = %q, answer messageId = %q: want a distinct non-empty id", reasoningID, answerID)
	}
	if e.lastText != "the image tag is wrong" {
		t.Errorf("lastText = %q, want the answer — reasoning must never become RunFinished.result", e.lastText)
	}
}

// TestAGUIEmitterNeverPublishesAThoughtSignature is the test the whole opt-in
// is worth having. A thinking block whose payload is entirely its signature —
// which is what claude-opus-5 returns under the request mast sends today, so
// this is the COMMON case for an opted-in workload rather than an edge one —
// must produce no frame at all.
//
// Two ways to get this wrong, and this catches both. Emitting the signature as
// a REASONING_* value would hand a browser a provider replay credential (the
// value Anthropic requires back verbatim on the assistant turn preceding a
// tool_result). Emitting an empty bracket would announce a withheld thought.
//
// Neutralize check: change internal/modeltext.Thought to return
// string(p.ThoughtSignature) when Text is empty, and this fails with the
// signature on the wire while every other reasoning test stays green.
func TestAGUIEmitterNeverPublishesAThoughtSignature(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit, reasoning: true}
	e.onEvent(mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
		{Thought: true, ThoughtSignature: []byte("opaque-replay-credential")},
		{Text: "restarted the deployment"},
	}}))

	for _, f := range *got {
		switch f.(type) {
		case agui.ReasoningStart, agui.ReasoningMessageStart, agui.ReasoningMessageContent,
			agui.ReasoningMessageEnd, agui.ReasoningEnd:
			t.Fatalf("a signature-only thinking block emitted %T; frames: %v", f, aguiFrameTypes(*got))
		}
	}
	blob, err := json.Marshal(*got)
	if err != nil {
		t.Fatalf("Marshal frames: %v", err)
	}
	if strings.Contains(string(blob), "opaque-replay-credential") {
		t.Fatalf("the thought signature reached the wire: %s", blob)
	}
}

// TestAGUIEmitterConcatenatesOneEventsThinking pins that the several thinking
// parts of ONE model event become ONE reasoning message, mirroring the answer
// path directly above it. mast runs StreamingModeNone, so an event is a whole
// model turn; splitting it into several reasoning messages would invent a
// structure the provider did not send.
func TestAGUIEmitterConcatenatesOneEventsThinking(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit, reasoning: true}
	e.onEvent(mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
		{Text: "first, ", Thought: true},
		{Text: "then second.", Thought: true},
		{Text: "done"},
	}}))

	var contents []string
	for _, f := range *got {
		if ev, ok := f.(agui.ReasoningMessageContent); ok {
			contents = append(contents, ev.Delta)
		}
	}
	if len(contents) != 1 || contents[0] != "first, then second." {
		t.Fatalf("reasoning content frames = %v, want one carrying the concatenation", contents)
	}
}

// TestAGUIEmitterSkipsUserThinking pins that the role guard covers reasoning
// too. A thinking part on a non-model event is not the model's reasoning, and
// re-emitting it would publish text from the wrong author under a frame a
// client renders as the agent's own thinking.
func TestAGUIEmitterSkipsUserThinking(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit, reasoning: true}
	e.onEvent(mkEvent(&genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{
		{Text: "not the model's thinking", Thought: true},
	}}))
	if len(*got) != 0 {
		t.Fatalf("a non-model thinking part emitted %d frame(s): %v", len(*got), aguiFrameTypes(*got))
	}
}

// TestAGUIBackendReasoningEnabled pins the bundle→emitter seam a unit test on
// the emitter alone cannot reach: the field defaults false for a nil bundle and
// for a bundle that simply does not mention the key, so a workload only
// publishes reasoning by saying so.
func TestAGUIBackendReasoningEnabled(t *testing.T) {
	if (&aguiBackend{}).publication().reasoning {
		t.Error("nil bundle: reasoning = true, want false")
	}
	if (&aguiBackend{bundle: &workload.Bundle{Name: "w"}}).publication().reasoning {
		t.Error("no emit_reasoning declared: reasoning = true, want false")
	}
	b := &aguiBackend{bundle: &workload.Bundle{
		Name: "w",
		AGUI: workload.AGUI{Expose: true, EmitReasoning: true},
	}}
	if !b.publication().reasoning {
		t.Error("emit_reasoning: true did not reach the backend")
	}
}

// TestAGUIBackendEmitsReasoningEndToEnd drives a real turn through runTurnPre
// with a model that thinks before answering, under both settings. It is the
// only test here that proves the bundle field reaches a live run: the emitter
// tests construct the emitter by hand and so would stay green if RunAgent
// stopped passing the flag.
//
// Neutralize check: drop `reasoning: pub.reasoning` from RunAgent's
// emitter construction and the opted-in leg fails; the default leg stays green,
// which is why both legs are here.
func TestAGUIBackendEmitsReasoningEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		want    int
	}{
		{"default off", false, 0},
		{"opted in", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &aguiScriptedModel{script: func(int, *model.LLMRequest) *model.LLMResponse {
				return &model.LLMResponse{Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{Text: "the tag does not exist in the registry", Thought: true},
						{Text: "the image tag is wrong"},
					},
				}}
			}}
			h := newTurnHarness(t, m)
			b := &aguiBackend{turnDeps: h.deps(), bundle: &workload.Bundle{
				Name: "w",
				AGUI: workload.AGUI{Expose: true, EmitReasoning: tc.enabled},
			}}
			emit, got := collectEmit()
			res, err := b.RunAgent(context.Background(), agui.RunInput{
				ThreadID: "t1", RunID: "r1", Text: "why is it crashlooping?",
			}, emit)
			if err != nil {
				t.Fatalf("RunAgent: %v", err)
			}
			if res.Text != "the image tag is wrong" {
				t.Fatalf("result = %q, want the answer and never the thinking", res.Text)
			}
			var phases int
			for _, f := range *got {
				if _, ok := f.(agui.ReasoningStart); ok {
					phases++
				}
			}
			if phases != tc.want {
				t.Fatalf("reasoning phases = %d, want %d; frames: %v", phases, tc.want, aguiFrameTypes(*got))
			}
		})
	}
}
