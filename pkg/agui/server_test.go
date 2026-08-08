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

package agui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-steer/mast/pkg/serverauth"
)

// fakeBackend is a controllable Backend for the server tests. By default it
// emits the opening frames (RunStarted, StateSnapshot), one text triad echoing
// the input, and returns a success RunResult — the same shape the real daemon
// backend produces. Control fields divert it: beforeEmitErr returns before any
// emit (the pre-stream drain path); emitThenErr returns after the opening
// frames (the mid-stream failure path); result overrides the terminal outcome
// (abort / interrupt).
type fakeBackend struct {
	mu            sync.Mutex
	runs          []RunInput      // inputs seen, in order
	lastCtx       context.Context // ctx of the most recent RunAgent (trace assertions)
	beforeEmitErr error           // returned before any emit
	emitThenErr   error           // returned after the opening frames
	result        RunResult       // terminal outcome (zero → default success echo)
}

func newFakeBackend() *fakeBackend { return &fakeBackend{} }

func (b *fakeBackend) RunAgent(ctx context.Context, in RunInput, emit func(any)) (RunResult, error) {
	b.mu.Lock()
	b.lastCtx = ctx
	b.runs = append(b.runs, in)
	beforeEmitErr := b.beforeEmitErr
	emitThenErr := b.emitThenErr
	result := b.result
	b.mu.Unlock()

	if beforeEmitErr != nil {
		return RunResult{}, beforeEmitErr
	}
	// Opening frames (mirrors cmd/mast/agui.go): RunStarted then the shared
	// state snapshot echoing the input state.
	emit(NewRunStarted(in.ThreadID, in.RunID))
	snap := in.State
	if snap == nil {
		snap = json.RawMessage("{}")
	}
	emit(NewStateSnapshot(snap))
	if emitThenErr != nil {
		return RunResult{}, emitThenErr
	}
	// Interior: one assistant text message echoing the turn input.
	emit(NewTextMessageStart("m1"))
	emit(NewTextMessageContent("m1", "echo:"+in.Text))
	emit(NewTextMessageEnd("m1"))

	if result == (RunResult{}) {
		result = RunResult{Text: "echo:" + in.Text}
	}
	return result, nil
}

// recordingMetric captures AGUIRun calls as "workload/outcome".
type recordingMetric struct {
	mu   sync.Mutex
	seen []string
}

func (m *recordingMetric) AGUIRun(workload, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, workload+"/"+outcome)
}

// stubLimiter is a table-driven RateLimiter: records every Allow call and
// returns a fixed verdict.
type stubLimiter struct {
	mu         sync.Mutex
	allow      bool
	retryAfter time.Duration
	calls      []serverauth.RateLimitRequest
}

func (l *stubLimiter) Allow(_ context.Context, req serverauth.RateLimitRequest) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, req)
	return l.allow, l.retryAfter
}

const testEndpoint = "/agui/triage"

// testServer spins up an httptest server over a New(cfg) Server. cfg's Backend
// and Exposed default to a single "triage" workload if unset.
func testServer(t *testing.T, cfg Config) (*httptest.Server, *fakeBackend) {
	t.Helper()
	be, _ := cfg.Backend.(*fakeBackend)
	if be == nil {
		be = newFakeBackend()
		cfg.Backend = be
	}
	if cfg.Exposed == nil {
		cfg.Exposed = []ExposedWorkload{{WorkloadName: "triage", EndpointPath: testEndpoint, Description: "triage GKE alerts"}}
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, be
}

// runBody builds a RunAgentInput POST body with one user message.
func runBody(threadID, runID, text string) string {
	return fmt.Sprintf(`{"threadId":%q,"runId":%q,"messages":[{"role":"user","content":%q}]}`, threadID, runID, text)
}

// sseFrame is one decoded SSE event: its type discriminant and raw JSON.
type sseFrame struct {
	typ EventType
	raw json.RawMessage
}

// runCall POSTs a RunAgentInput and returns the HTTP status, Content-Type, and
// — for an SSE response — the decoded frames in order. For a pre-stream HTTP
// error, frames is nil (the caller asserts on status).
func runCall(t *testing.T, ts *httptest.Server, token, path, body string) (int, string, []sseFrame) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, ct, nil
	}
	var out []sseFrame
	if strings.HasPrefix(ct, "text/event-stream") {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok {
				continue
			}
			var probe struct {
				Type EventType `json:"type"`
			}
			if err := json.Unmarshal([]byte(data), &probe); err != nil {
				t.Fatalf("decode SSE frame %q: %v", data, err)
			}
			out = append(out, sseFrame{typ: probe.Type, raw: json.RawMessage(data)})
		}
		if err := scanner.Err(); err != nil {
			t.Fatalf("scan SSE: %v", err)
		}
	}
	return resp.StatusCode, ct, out
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with nil Backend: want error, got nil")
	}
	// Duplicate endpoint path is rejected.
	if _, err := New(Config{
		Backend: newFakeBackend(),
		Exposed: []ExposedWorkload{
			{WorkloadName: "a", EndpointPath: "/agui/x"},
			{WorkloadName: "b", EndpointPath: "/agui/x"},
		},
	}); err == nil {
		t.Fatal("New with duplicate endpoint path: want error, got nil")
	}
	// Missing endpoint path is rejected.
	if _, err := New(Config{Backend: newFakeBackend(), Exposed: []ExposedWorkload{{WorkloadName: "w"}}}); err == nil {
		t.Fatal("New with missing EndpointPath: want error, got nil")
	}
	// Path not starting with "/" is rejected.
	if _, err := New(Config{Backend: newFakeBackend(), Exposed: []ExposedWorkload{{WorkloadName: "w", EndpointPath: "agui/w"}}}); err == nil {
		t.Fatal("New with relative endpoint path: want error, got nil")
	}
	// A workload path colliding with the discovery path is rejected.
	if _, err := New(Config{Backend: newFakeBackend(), Exposed: []ExposedWorkload{{WorkloadName: "w", EndpointPath: DiscoveryPath}}}); err == nil {
		t.Fatal("New with discovery-path collision: want error, got nil")
	}
}

// TestNewRefusesUnauthenticatedNonLoopback pins the bind guard: an explicitly
// non-loopback Listen without a validator must refuse to construct, because a
// run drives a budgeted turn. Loopback binds and any authenticated bind are
// allowed; an empty Listen (embedded/test default) is not gated.
func TestNewRefusesUnauthenticatedNonLoopback(t *testing.T) {
	v, _ := serverauth.NewStaticBearerValidator(map[string]*serverauth.Principal{"tok": {Subject: "s"}})
	cases := []struct {
		name      string
		listen    string
		validator serverauth.TokenValidator
		wantErr   bool
	}{
		{"wildcard port unauth", ":7781", nil, true},
		{"all-interfaces unauth", "0.0.0.0:7781", nil, true},
		{"ipv6 wildcard unauth", "[::]:7781", nil, true},
		{"loopback unauth ok", "127.0.0.1:7781", nil, false},
		{"localhost unauth ok", "localhost:7781", nil, false},
		{"non-loopback with auth ok", "0.0.0.0:7781", v, false},
		{"empty listen ok (defaulted)", "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{
				Backend:   newFakeBackend(),
				Listen:    tc.listen,
				Validator: tc.validator,
				Exposed:   []ExposedWorkload{{WorkloadName: "triage", EndpointPath: testEndpoint}},
			})
			if tc.wantErr && err == nil {
				t.Fatalf("New(listen=%q, auth=%v): want refusal, got nil", tc.listen, tc.validator != nil)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("New(listen=%q, auth=%v): want ok, got %v", tc.listen, tc.validator != nil, err)
			}
		})
	}
}

// TestDiscovery pins the discovery document shape: a sorted array of workload
// descriptors, public (unauthenticated), advertising the endpoint, scopes, and
// whether auth is required.
func TestDiscovery(t *testing.T) {
	v, _ := serverauth.NewStaticBearerValidator(map[string]*serverauth.Principal{"tok": {Subject: "s"}})
	ts, _ := testServer(t, Config{
		Validator: v,
		Exposed: []ExposedWorkload{
			{WorkloadName: "beta", EndpointPath: "/agui/beta", Description: "b", Scopes: []string{"beta:invoke"}},
			{WorkloadName: "alpha", EndpointPath: "/agui/alpha", Description: "a"},
		},
	})
	resp, err := ts.Client().Get(ts.URL + DiscoveryPath)
	if err != nil {
		t.Fatalf("GET discovery: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("discovery status = %d, want 200", resp.StatusCode)
	}
	var out []AgentDescriptor
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode discovery: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("descriptors = %d, want 2", len(out))
	}
	// Sorted by endpoint for a deterministic document.
	if out[0].Endpoint != "/agui/alpha" || out[1].Endpoint != "/agui/beta" {
		t.Fatalf("not sorted by endpoint: %q, %q", out[0].Endpoint, out[1].Endpoint)
	}
	if out[0].Name != "alpha" || out[0].ProtocolVersion != ProtocolVersion {
		t.Fatalf("descriptor[0] = %+v", out[0])
	}
	// Auth required (a validator is configured); per-workload scopes surface.
	if !out[1].Auth.Required {
		t.Error("beta.auth.required = false, want true (validator configured)")
	}
	if len(out[1].Auth.Scopes) != 1 || out[1].Auth.Scopes[0] != "beta:invoke" {
		t.Fatalf("beta.auth.scopes = %v, want [beta:invoke]", out[1].Auth.Scopes)
	}
}

// TestRunHappyPath: a fresh run opens an SSE stream whose frames are, in
// order, RunStarted, a StateSnapshot, the assistant text triad, and exactly
// one terminal RunFinished carrying the final answer as result. The outcome is
// metered once as success.
func TestRunHappyPath(t *testing.T) {
	metric := &recordingMetric{}
	ts, be := testServer(t, Config{Metric: metric})
	status, ct, frames := runCall(t, ts, "", testEndpoint, runBody("thread-1", "run-1", "investigate pod"))
	if status != http.StatusOK {
		t.Fatalf("HTTP status = %d, want 200", status)
	}
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	// First frame is RunStarted.
	if frames[0].typ != EventRunStarted {
		t.Fatalf("first frame type = %q, want %q", frames[0].typ, EventRunStarted)
	}
	// Exactly one terminal event, and it is the last frame.
	terminals := 0
	lastTerminal := -1
	for i, f := range frames {
		if f.typ == EventRunFinished || f.typ == EventRunError {
			terminals++
			lastTerminal = i
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events = %d, want exactly 1", terminals)
	}
	if lastTerminal != len(frames)-1 {
		t.Fatalf("terminal event at frame %d, want the last frame %d", lastTerminal, len(frames)-1)
	}
	// The terminal RunFinished carries the agent's answer as result.
	last := frames[len(frames)-1]
	if last.typ != EventRunFinished {
		t.Fatalf("last frame type = %q, want %q", last.typ, EventRunFinished)
	}
	var fin RunFinished
	if err := json.Unmarshal(last.raw, &fin); err != nil {
		t.Fatalf("decode RunFinished: %v", err)
	}
	if fin.ThreadID != "thread-1" || fin.RunID != "run-1" {
		t.Fatalf("RunFinished ids = %q/%q, want thread-1/run-1", fin.ThreadID, fin.RunID)
	}
	var result string
	if err := json.Unmarshal(fin.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result != "echo:investigate pod" {
		t.Fatalf("result = %q, want echo:investigate pod", result)
	}
	// The backend received the turn text.
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.runs) != 1 || be.runs[0].Text != "investigate pod" {
		t.Fatalf("backend runs = %+v", be.runs)
	}
	metric.mu.Lock()
	defer metric.mu.Unlock()
	if len(metric.seen) != 1 || metric.seen[0] != "triage/success" {
		t.Fatalf("metric = %v, want one triage/success", metric.seen)
	}
}

// TestRunEmitsTextTriad pins the assistant-text translation: the stream
// carries a TEXT_MESSAGE_START / CONTENT / END triad with a shared messageId,
// the content delta carrying the answer.
func TestRunEmitsTextTriad(t *testing.T) {
	ts, _ := testServer(t, Config{})
	_, _, frames := runCall(t, ts, "", testEndpoint, runBody("t", "r", "hi"))
	var start, content, end int
	var contentDelta string
	for _, f := range frames {
		switch f.typ {
		case EventTextMessageStart:
			start++
		case EventTextMessageContent:
			content++
			var c TextMessageContent
			if err := json.Unmarshal(f.raw, &c); err != nil {
				t.Fatalf("decode content: %v", err)
			}
			contentDelta = c.Delta
		case EventTextMessageEnd:
			end++
		}
	}
	if start != 1 || content != 1 || end != 1 {
		t.Fatalf("text triad counts = start:%d content:%d end:%d, want 1/1/1", start, content, end)
	}
	if contentDelta != "echo:hi" {
		t.Fatalf("content delta = %q, want echo:hi", contentDelta)
	}
}

// TestRunAborted: a run the backend reports as aborted closes with a terminal
// RUN_ERROR carrying the aborted code — never a fabricated success.
func TestRunAborted(t *testing.T) {
	metric := &recordingMetric{}
	be := newFakeBackend()
	be.result = RunResult{Aborted: true}
	ts, _ := testServer(t, Config{Backend: be, Metric: metric})
	_, ct, frames := runCall(t, ts, "", testEndpoint, runBody("t", "r", "hi"))
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	last := frames[len(frames)-1]
	if last.typ != EventRunError {
		t.Fatalf("last frame type = %q, want %q", last.typ, EventRunError)
	}
	var re RunError
	if err := json.Unmarshal(last.raw, &re); err != nil {
		t.Fatalf("decode RunError: %v", err)
	}
	if re.Code != RunErrorAborted {
		t.Fatalf("RunError code = %q, want %q", re.Code, RunErrorAborted)
	}
	// No RunFinished may appear — an aborted turn must not report success.
	for _, f := range frames {
		if f.typ == EventRunFinished {
			t.Fatal("aborted run emitted a RUN_FINISHED (fabricated success)")
		}
	}
	metric.mu.Lock()
	defer metric.mu.Unlock()
	if len(metric.seen) != 1 || metric.seen[0] != "triage/aborted" {
		t.Fatalf("metric = %v, want one triage/aborted", metric.seen)
	}
}

// TestRunInterrupted: a run that paused for human input closes with a terminal
// RUN_ERROR carrying the interrupt code (the honest Stage 1 placeholder), never
// a success.
func TestRunInterrupted(t *testing.T) {
	be := newFakeBackend()
	be.result = RunResult{Interrupted: true}
	ts, _ := testServer(t, Config{Backend: be})
	_, _, frames := runCall(t, ts, "", testEndpoint, runBody("t", "r", "hi"))
	last := frames[len(frames)-1]
	if last.typ != EventRunError {
		t.Fatalf("last frame type = %q, want %q", last.typ, EventRunError)
	}
	var re RunError
	if err := json.Unmarshal(last.raw, &re); err != nil {
		t.Fatalf("decode RunError: %v", err)
	}
	if re.Code != RunErrorInterrupt {
		t.Fatalf("RunError code = %q, want %q", re.Code, RunErrorInterrupt)
	}
	for _, f := range frames {
		if f.typ == EventRunFinished {
			t.Fatal("interrupted run emitted a RUN_FINISHED (fabricated success)")
		}
	}
}

// TestRunMidStreamError: a backend that fails AFTER the opening frames must
// close the already-open SSE stream with a terminal RUN_ERROR{internal} (no
// server-side detail leaked), not a truncated stream.
func TestRunMidStreamError(t *testing.T) {
	metric := &recordingMetric{}
	be := newFakeBackend()
	be.emitThenErr = errors.New("runner exploded after opening")
	ts, _ := testServer(t, Config{Backend: be, Metric: metric})
	_, ct, frames := runCall(t, ts, "", testEndpoint, runBody("t", "r", "hi"))
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("mid-stream failure did not open an SSE stream (Content-Type %q)", ct)
	}
	if frames[0].typ != EventRunStarted {
		t.Fatalf("frame[0] type = %q, want %q", frames[0].typ, EventRunStarted)
	}
	last := frames[len(frames)-1]
	if last.typ != EventRunError {
		t.Fatalf("last frame type = %q, want %q", last.typ, EventRunError)
	}
	var re RunError
	if err := json.Unmarshal(last.raw, &re); err != nil {
		t.Fatalf("decode RunError: %v", err)
	}
	if re.Code != RunErrorInternal {
		t.Fatalf("RunError code = %q, want %q", re.Code, RunErrorInternal)
	}
	// The client-facing message must not leak the internal error text.
	if strings.Contains(re.Message, "runner exploded") {
		t.Fatalf("RunError leaked internal detail: %q", re.Message)
	}
	metric.mu.Lock()
	defer metric.mu.Unlock()
	if len(metric.seen) != 1 || metric.seen[0] != "triage/error" {
		t.Fatalf("metric = %v, want one triage/error", metric.seen)
	}
}

// TestRunUnavailableBeforeEmit: a backend that refuses before any emit (drain)
// yields a clean HTTP 503 — not a half-open SSE stream — so a client reads a
// retryable error, not a truncated stream. Metered as rejected.
func TestRunUnavailableBeforeEmit(t *testing.T) {
	metric := &recordingMetric{}
	be := newFakeBackend()
	be.beforeEmitErr = fmt.Errorf("draining: %w", ErrUnavailable)
	ts, _ := testServer(t, Config{Backend: be, Metric: metric})
	status, ct, _ := runCall(t, ts, "", testEndpoint, runBody("t", "r", "hi"))
	if strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("pre-emit drain opened an SSE stream (Content-Type %q)", ct)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("pre-emit drain: status = %d, want 503", status)
	}
	metric.mu.Lock()
	defer metric.mu.Unlock()
	if len(metric.seen) != 1 || metric.seen[0] != "triage/rejected" {
		t.Fatalf("metric = %v, want one triage/rejected", metric.seen)
	}
}

// TestRunInternalErrorBeforeEmit: a generic backend fault before any emit maps
// to HTTP 500 (not 503), so a caller does not treat a genuine fault as
// retryable.
func TestRunInternalErrorBeforeEmit(t *testing.T) {
	be := newFakeBackend()
	be.beforeEmitErr = errors.New("nil map dereference")
	ts, _ := testServer(t, Config{Backend: be})
	status, ct, _ := runCall(t, ts, "", testEndpoint, runBody("t", "r", "hi"))
	if strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("pre-emit fault opened an SSE stream (Content-Type %q)", ct)
	}
	if status != http.StatusInternalServerError {
		t.Fatalf("pre-emit fault: status = %d, want 500", status)
	}
}

// TestRunRequiresUserText: a request without a user message text is rejected
// with HTTP 400 before the backend runs (Stage 1 requires a text turn).
func TestRunRequiresUserText(t *testing.T) {
	ts, be := testServer(t, Config{})
	// No messages at all.
	status, _, _ := runCall(t, ts, "", testEndpoint, `{"threadId":"t","runId":"r"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("no messages: status = %d, want 400", status)
	}
	// A non-user message does not count.
	status, _, _ = runCall(t, ts, "", testEndpoint, `{"threadId":"t","runId":"r","messages":[{"role":"assistant","content":"hi"}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("assistant-only: status = %d, want 400", status)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.runs) != 0 {
		t.Fatalf("text-less request reached the backend: %v", be.runs)
	}
}

// TestRunMalformedBody: a body that is not valid JSON is rejected with HTTP 400.
func TestRunMalformedBody(t *testing.T) {
	ts, _ := testServer(t, Config{})
	status, _, _ := runCall(t, ts, "", testEndpoint, `{not json`)
	if status != http.StatusBadRequest {
		t.Fatalf("malformed body: status = %d, want 400", status)
	}
}

// TestRunBodyTooLarge: a POST body past the MaxBytesReader cap is refused 400
// before it is fully buffered, and never reaches the backend. Neutralize check:
// drop the MaxBytesReader wrap in handleRun and an oversized body decodes and
// dispatches instead of being refused.
func TestRunBodyTooLarge(t *testing.T) {
	ts, be := testServer(t, Config{})
	// A valid-shaped RunAgentInput whose single user message content exceeds the
	// 1 MiB cap; the decoder trips the reader limit before it finishes.
	huge := strings.Repeat("a", (2 << 20))
	body := `{"threadId":"t","runId":"r","messages":[{"role":"user","content":"` + huge + `"}]}`
	status, _, _ := runCall(t, ts, "", testEndpoint, body)
	if status != http.StatusBadRequest {
		t.Fatalf("oversized body: status = %d, want 400", status)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.runs) != 0 {
		t.Fatalf("oversized request reached the backend: %v", be.runs)
	}
}

// TestRunAuthRequired: with a validator, a run without a valid bearer is 401
// (before any dispatch), and a good bearer passes.
func TestRunAuthRequired(t *testing.T) {
	v, _ := serverauth.NewStaticBearerValidator(map[string]*serverauth.Principal{
		"good": {Subject: "svc"},
	})
	ts, be := testServer(t, Config{Validator: v})

	status, _, _ := runCall(t, ts, "", testEndpoint, runBody("t", "r", "hi"))
	if status != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", status)
	}
	status, _, _ = runCall(t, ts, "nope", testEndpoint, runBody("t", "r", "hi"))
	if status != http.StatusUnauthorized {
		t.Fatalf("bad token: status = %d, want 401", status)
	}
	status, ct, _ := runCall(t, ts, "good", testEndpoint, runBody("t", "r", "hi"))
	if status != http.StatusOK || !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("good token: status = %d, ct = %q, want 200 SSE", status, ct)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.runs) != 1 {
		t.Fatalf("good-token run did not reach the backend: %v", be.runs)
	}
}

// TestRunScopeForbidden: an authenticated-but-underscoped caller is refused
// 403 before the backend runs.
func TestRunScopeForbidden(t *testing.T) {
	v, _ := serverauth.NewStaticBearerValidator(map[string]*serverauth.Principal{
		"weak": {Subject: "svc", Scopes: []string{"other:scope"}},
	})
	ts, be := testServer(t, Config{
		Validator: v,
		Exposed:   []ExposedWorkload{{WorkloadName: "triage", EndpointPath: testEndpoint, Scopes: []string{"triage:invoke"}}},
	})
	status, _, _ := runCall(t, ts, "weak", testEndpoint, runBody("t", "r", "hi"))
	if status != http.StatusForbidden {
		t.Fatalf("underscoped run: status = %d, want 403", status)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.runs) != 0 {
		t.Fatalf("underscoped run reached the backend: %v", be.runs)
	}
}

// TestRunRateLimited: a refusing limiter turns a run into HTTP 429 with an
// advisory Retry-After (NOT an SSE stream), records a "rejected" outcome, and
// never reaches the backend — the refusal precedes the SSE upgrade. The
// fractional retryAfter (2.5s) pins the ceil: Retry-After rounds UP to 3.
func TestRunRateLimited(t *testing.T) {
	lim := &stubLimiter{allow: false, retryAfter: 2500 * time.Millisecond}
	metric := &recordingMetric{}
	be := newFakeBackend()
	ts, _ := testServer(t, Config{Backend: be, Limiter: lim, Metric: metric})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+testEndpoint, strings.NewReader(runBody("t", "r", "hi")))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal("rate-limited run opened an SSE stream; refusal must precede the upgrade")
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate-limited run: status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want 3", got)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	lim.mu.Lock()
	if len(lim.calls) != 1 || lim.calls[0].Method != methodRun || lim.calls[0].Workload != "triage" {
		t.Fatalf("limiter calls = %+v, want one agui/run for triage", lim.calls)
	}
	lim.mu.Unlock()
	be.mu.Lock()
	if len(be.runs) != 0 {
		t.Fatalf("rate-limited run reached the backend: %v", be.runs)
	}
	be.mu.Unlock()
	metric.mu.Lock()
	defer metric.mu.Unlock()
	if len(metric.seen) != 1 || metric.seen[0] != "triage/rejected" {
		t.Fatalf("metric = %v, want one triage/rejected", metric.seen)
	}
}

// TestRunRateLimiterAdmitsKeyedByCaller: an admitting limiter lets the turn run
// and is keyed by the authenticated caller.
func TestRunRateLimiterAdmitsKeyedByCaller(t *testing.T) {
	v, _ := serverauth.NewStaticBearerValidator(map[string]*serverauth.Principal{
		"tok": {Subject: "alice", Tenant: "acme"},
	})
	lim := &stubLimiter{allow: true}
	ts, be := testServer(t, Config{Validator: v, Limiter: lim})
	status, _, _ := runCall(t, ts, "tok", testEndpoint, runBody("t", "r", "hi"))
	if status != http.StatusOK {
		t.Fatalf("admitted run: status = %d, want 200", status)
	}
	lim.mu.Lock()
	defer lim.mu.Unlock()
	if len(lim.calls) != 1 || lim.calls[0].Subject != "alice" || lim.calls[0].Tenant != "acme" {
		t.Fatalf("limiter call = %+v, want subject alice / tenant acme", lim.calls)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.runs) != 1 {
		t.Fatalf("admitted run did not reach the backend: %v", be.runs)
	}
}

// TestRunExtractsTraceContext proves the endpoint adopts a caller's W3C trace
// context (traceparent) into the ctx the backend runs under, so the turn's
// spans parent under the caller's span. Neutralize check: pass r.Context()
// (not the extracted ctx) to the backend and the propagated span vanishes.
func TestRunExtractsTraceContext(t *testing.T) {
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	ts, be := testServer(t, Config{})
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const spanID = "00f067aa0ba902b7"

	req, _ := http.NewRequest(http.MethodPost, ts.URL+testEndpoint, strings.NewReader(runBody("t", "r", "hi")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	be.mu.Lock()
	defer be.mu.Unlock()
	if be.lastCtx == nil {
		t.Fatal("RunAgent never ran")
	}
	sc := trace.SpanContextFromContext(be.lastCtx)
	if !sc.IsValid() {
		t.Fatal("no span context reached the backend ctx (traceparent not extracted)")
	}
	if got := sc.TraceID().String(); got != traceID {
		t.Fatalf("propagated trace id = %q, want %q", got, traceID)
	}
}

// TestRunEchoesStateSnapshot: the client-supplied state is echoed back as the
// opening StateSnapshot the backend emits.
func TestRunEchoesStateSnapshot(t *testing.T) {
	ts, _ := testServer(t, Config{})
	body := `{"threadId":"t","runId":"r","state":{"count":7},"messages":[{"role":"user","content":"hi"}]}`
	_, _, frames := runCall(t, ts, "", testEndpoint, body)
	var snapFrame *sseFrame
	for i := range frames {
		if frames[i].typ == EventStateSnapshot {
			snapFrame = &frames[i]
			break
		}
	}
	if snapFrame == nil {
		t.Fatal("no STATE_SNAPSHOT frame")
	}
	var ss StateSnapshot
	if err := json.Unmarshal(snapFrame.raw, &ss); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if string(ss.Snapshot) != `{"count":7}` {
		t.Fatalf("snapshot = %s, want {\"count\":7}", ss.Snapshot)
	}
}

// TestOutcomeConstantLiterals pins this package's unexported run-outcome
// constants to their fixed string values. The daemon passes these through to
// observability.Registry.AGUIRun, which primes exactly these labels; if a value
// drifted here, a real scrape would carry an unprimed series while the metric
// registry's parallel pin (cmd/mast TestAGUIOutcomeVocabulary) stayed green.
// The two pins bracket the same literals from both sides so neither can move
// unnoticed.
func TestOutcomeConstantLiterals(t *testing.T) {
	pairs := map[string]string{
		outcomeSuccess:  "success",
		outcomeError:    "error",
		outcomeAborted:  "aborted",
		outcomeRejected: "rejected",
	}
	for got, want := range pairs {
		if got != want {
			t.Errorf("outcome constant = %q, want %q", got, want)
		}
	}
}
