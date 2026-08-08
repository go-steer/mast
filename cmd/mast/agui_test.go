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
	"errors"
	"testing"

	"google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/agui"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/workload"
)

// newAGUIBackendRunner builds an aguiBackend backed by a real turn stack
// (runner + transcript store + tracker + locks + meters over an in-memory
// session service), so RunAgent can drive turns end-to-end through runTurnPre.
// workloadName is "(test)" to match the harness's primed obs.
func newAGUIBackendRunner(t *testing.T, m model.LLM) *aguiBackend {
	t.Helper()
	h := newTurnHarness(t, m)
	return &aguiBackend{
		store:        h.store,
		obs:          h.obs,
		tracker:      h.tracker,
		logger:       discardLogger(),
		workloadName: "(test)",
		r:            h.runner,
		meters:       h.meters,
		wds:          h.wds,
		turnLocks:    h.locks,
	}
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
	sid := b.sessionIDFor("t1", "r1")
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
	if got := perThread.sessionIDFor("t1", "r1"); got != "agui-thread-t1" {
		t.Fatalf("per_thread session = %q, want agui-thread-t1", got)
	}
	if !isAGUISessionID("agui-thread-t1") {
		t.Fatal("agui-thread-t1 not recognized as an owned session id")
	}

	perRun := &aguiBackend{bundle: &workload.Bundle{AGUI: workload.AGUI{SessionModel: workload.AGUISessionPerRun}}}
	if got := perRun.sessionIDFor("t1", "r1"); got != "agui-run-r1" {
		t.Fatalf("per_run session = %q, want agui-run-r1", got)
	}

	// An absent correlation id mints a fresh owned session rather than
	// colliding id-less callers onto one shared session.
	minted := perThread.sessionIDFor("", "")
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

// TestAGUIOutcomeVocabulary pins the observability outcome constants against
// the fixed wire strings the pkg/agui server records through
// observability.Registry.AGUIRun. pkg/agui's outcome constants are unexported;
// they must equal these literals or a scrape sees an unprimed series.
func TestAGUIOutcomeVocabulary(t *testing.T) {
	pairs := map[string]string{
		observability.AGUIRunSuccess:  "success",
		observability.AGUIRunError:    "error",
		observability.AGUIRunAborted:  "aborted",
		observability.AGUIRunRejected: "rejected",
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
