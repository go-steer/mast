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

package a2a

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// fakeBackend is a table-driven Backend for the server tests: GetTask and
// CancelTask return canned snapshots (or errors) keyed by task id.
type fakeBackend struct {
	mu            sync.Mutex
	tasks         map[string]TaskInfo // task id → snapshot GetTask returns
	getErr        map[string]error    // task id → error GetTask returns instead
	canceled      []string            // task ids passed to CancelTask, in order
	submitted     []SubmitParams      // params passed to SubmitMessage, in order
	submitErr     error               // when set, SubmitMessage returns it
	emitThenErr   error               // when set, StreamMessage emits the initial Task then returns it
	submitResult  TaskInfo            // when State != "", the snapshot SubmitMessage returns
	lastSubmitCtx context.Context     // ctx of the most recent SubmitMessage (trace assertions)
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{tasks: map[string]TaskInfo{}, getErr: map[string]error{}}
}

func (b *fakeBackend) GetTask(_ context.Context, id string) (TaskInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err, ok := b.getErr[id]; ok {
		return TaskInfo{}, err
	}
	info, ok := b.tasks[id]
	if !ok {
		return TaskInfo{}, ErrTaskNotFound
	}
	return info, nil
}

func (b *fakeBackend) CancelTask(_ context.Context, id, _ string) (TaskInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err, ok := b.getErr[id]; ok {
		return TaskInfo{}, err
	}
	info, ok := b.tasks[id]
	if !ok {
		return TaskInfo{}, ErrTaskNotFound
	}
	b.canceled = append(b.canceled, id)
	info.State = TaskStateCanceled
	b.tasks[id] = info
	return info, nil
}

func (b *fakeBackend) SubmitMessage(ctx context.Context, p SubmitParams) (string, TaskInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastSubmitCtx = ctx
	b.submitted = append(b.submitted, p)
	if b.submitErr != nil {
		return "", TaskInfo{}, b.submitErr
	}
	id := p.TaskID
	if id == "" {
		id = "task-" + strconv.Itoa(len(b.submitted))
	}
	info := b.submitResult
	if info.State == "" {
		info = TaskInfo{WorkloadName: "triage", State: TaskStateCompleted, Output: "echo:" + p.Text}
	}
	b.tasks[id] = info
	return id, info, nil
}

// StreamMessage mirrors SubmitMessage but drives the emit callback: the
// initial Task snapshot, then one interim working status-update, matching
// the daemon backend's stream shape. submitErr (when set) returns before
// any emit, so the server's pre-stream error path stays testable.
func (b *fakeBackend) StreamMessage(ctx context.Context, p SubmitParams, emit func(any)) (string, TaskInfo, error) {
	b.mu.Lock()
	b.lastSubmitCtx = ctx
	b.submitted = append(b.submitted, p)
	submitErr := b.submitErr
	emitThenErr := b.emitThenErr
	result := b.submitResult
	b.mu.Unlock()
	if submitErr != nil {
		return "", TaskInfo{}, submitErr
	}
	id := p.TaskID
	if id == "" {
		id = "task-" + strconv.Itoa(len(b.submitted))
	}
	info := result
	if info.State == "" {
		info = TaskInfo{WorkloadName: "triage", State: TaskStateCompleted, Output: "echo:" + p.Text}
	}
	emit(&Task{Kind: "task", ID: id, ContextID: info.ContextID, Status: TaskStatus{State: TaskStateWorking}})
	if emitThenErr != nil {
		// The drain-after-initial-Task race: the id/context are carried on the
		// return so the server can build a correlatable terminal frame.
		return id, TaskInfo{WorkloadName: info.WorkloadName, ContextID: info.ContextID}, emitThenErr
	}
	emit(&TaskStatusUpdateEvent{Kind: "status-update", TaskID: id, ContextID: info.ContextID, Status: TaskStatus{State: TaskStateWorking}, Final: false})
	b.mu.Lock()
	b.tasks[id] = info
	b.mu.Unlock()
	return id, info, nil
}

// recordingMetric captures A2ATask calls.
type recordingMetric struct {
	mu   sync.Mutex
	seen []string // "workload/outcome"
}

func (m *recordingMetric) A2ATask(workload, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, workload+"/"+outcome)
}

// testServer spins up an httptest server over a New(cfg) Server. cfg's
// Backend/Skills default to a single "triage" workload if unset.
func testServer(t *testing.T, cfg Config) (*httptest.Server, *fakeBackend) {
	t.Helper()
	be, _ := cfg.Backend.(*fakeBackend)
	if be == nil {
		be = newFakeBackend()
		cfg.Backend = be
	}
	if cfg.Skills == nil {
		cfg.Skills = []ExposedSkill{{WorkloadName: "triage", SkillName: "triage", Description: "triage GKE alerts"}}
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, be
}

// rpcCall POSTs a JSON-RPC request and returns the decoded response.
func rpcCall(t *testing.T, ts *httptest.Server, token, body string) (int, rpcResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/a2a", strings.NewReader(body))
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
	raw, _ := io.ReadAll(resp.Body)
	var out rpcResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode response %q: %v", raw, err)
		}
	}
	return resp.StatusCode, out
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with nil Backend: want error, got nil")
	}
	// Duplicate workload is rejected.
	_, err := New(Config{
		Backend: newFakeBackend(),
		Skills: []ExposedSkill{
			{WorkloadName: "w", SkillName: "a"},
			{WorkloadName: "w", SkillName: "b"},
		},
	})
	if err == nil {
		t.Fatal("New with duplicate workload: want error, got nil")
	}
	// Missing skill name is rejected.
	if _, err := New(Config{Backend: newFakeBackend(), Skills: []ExposedSkill{{WorkloadName: "w"}}}); err == nil {
		t.Fatal("New with missing SkillName: want error, got nil")
	}
}

// TestNewRefusesUnauthenticatedNonLoopback pins the #376-style guard: an
// explicitly non-loopback Listen without a validator must refuse to
// construct, because tasks/cancel is destructive. Loopback binds and any
// authenticated bind are allowed; an empty Listen (embedded/test default)
// is not gated.
func TestNewRefusesUnauthenticatedNonLoopback(t *testing.T) {
	v, _ := NewStaticBearerValidator(map[string]*Principal{"tok": {Subject: "s"}})
	cases := []struct {
		name      string
		listen    string
		validator TokenValidator
		wantErr   bool
	}{
		{"wildcard port unauth", ":7780", nil, true},
		{"all-interfaces unauth", "0.0.0.0:7780", nil, true},
		{"ipv6 wildcard unauth", "[::]:7780", nil, true},
		{"loopback unauth ok", "127.0.0.1:7780", nil, false},
		{"localhost unauth ok", "localhost:7780", nil, false},
		{"ipv6 loopback unauth ok", "[::1]:7780", nil, false},
		{"non-loopback with auth ok", "0.0.0.0:7780", v, false},
		{"empty listen ok (defaulted)", "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{
				Backend:   newFakeBackend(),
				Listen:    tc.listen,
				Validator: tc.validator,
				Skills:    []ExposedSkill{{WorkloadName: "triage", SkillName: "triage"}},
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

func TestStaticBearerValidator(t *testing.T) {
	p := &Principal{Subject: "svc", Scopes: []string{"triage:invoke"}}
	v, err := NewStaticBearerValidator(map[string]*Principal{"secret": p})
	if err != nil {
		t.Fatalf("NewStaticBearerValidator: %v", err)
	}
	got, err := v.Validate(context.Background(), "secret")
	if err != nil {
		t.Fatalf("Validate(good): %v", err)
	}
	if got.Subject != "svc" {
		t.Fatalf("Validate(good).Subject = %q, want svc", got.Subject)
	}
	if _, err := v.Validate(context.Background(), "wrong"); err != ErrInvalidToken {
		t.Fatalf("Validate(bad) = %v, want ErrInvalidToken", err)
	}
	// Construction rejects an empty map / empty token / nil principal.
	if _, err := NewStaticBearerValidator(nil); err == nil {
		t.Fatal("empty map: want error")
	}
	if _, err := NewStaticBearerValidator(map[string]*Principal{"": p}); err == nil {
		t.Fatal("empty token: want error")
	}
	if _, err := NewStaticBearerValidator(map[string]*Principal{"t": nil}); err == nil {
		t.Fatal("nil principal: want error")
	}
}

func TestAggregatedCard(t *testing.T) {
	ts, _ := testServer(t, Config{
		CardName:        "mast",
		CardDescription: "unattended triage",
		CardVersion:     "0.2.0",
		Skills: []ExposedSkill{
			{WorkloadName: "beta", SkillName: "beta-skill", Description: "b"},
			{WorkloadName: "alpha", SkillName: "alpha-skill", Description: "a"},
		},
	})
	resp, err := ts.Client().Get(ts.URL + WellKnownCardPath)
	if err != nil {
		t.Fatalf("GET card: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("card status = %d, want 200", resp.StatusCode)
	}
	var card AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if card.Name != "mast" || card.Version != "0.2.0" {
		t.Fatalf("card name/version = %q/%q", card.Name, card.Version)
	}
	if card.ProtocolVersion != ProtocolVersion || card.PreferredTransport != TransportJSONRPC {
		t.Fatalf("card protocol/transport = %q/%q", card.ProtocolVersion, card.PreferredTransport)
	}
	if !strings.HasSuffix(card.URL, "/a2a") {
		t.Fatalf("card url = %q, want .../a2a", card.URL)
	}
	if len(card.Skills) != 2 {
		t.Fatalf("card skills = %d, want 2", len(card.Skills))
	}
	// Skills are sorted by id for a deterministic card.
	if card.Skills[0].ID != "alpha-skill" || card.Skills[1].ID != "beta-skill" {
		t.Fatalf("skills not sorted: %q, %q", card.Skills[0].ID, card.Skills[1].ID)
	}
	// No auth configured → no security advertised.
	if card.SecuritySchemes != nil || card.Security != nil {
		t.Fatal("unauthenticated card must not advertise security")
	}
	// Card carries no machine-readable I/O schema (spec AgentSkill has none).
	raw, _ := json.Marshal(card)
	if strings.Contains(string(raw), "inputSchema") || strings.Contains(string(raw), "outputSchema") {
		t.Fatalf("card leaked a schema field: %s", raw)
	}
}

func TestCardAdvertisesSecurityWhenAuthOn(t *testing.T) {
	v, _ := NewStaticBearerValidator(map[string]*Principal{"tok": {Subject: "s"}})
	ts, _ := testServer(t, Config{Validator: v})
	resp, err := ts.Client().Get(ts.URL + WellKnownCardPath)
	if err != nil {
		t.Fatalf("GET card: %v", err)
	}
	defer resp.Body.Close()
	var card AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if card.SecuritySchemes == nil || card.SecuritySchemes.Bearer == nil {
		t.Fatal("authenticated card must advertise a bearer security scheme")
	}
	if card.SecuritySchemes.Bearer.Scheme != "Bearer" {
		t.Fatalf("scheme = %q, want Bearer", card.SecuritySchemes.Bearer.Scheme)
	}
	if len(card.Security) != 1 {
		t.Fatalf("card.Security = %v, want one requirement", card.Security)
	}
}

func TestPerWorkloadCard(t *testing.T) {
	ts, _ := testServer(t, Config{
		Skills: []ExposedSkill{{WorkloadName: "triage", SkillName: "triage-skill", Description: "d"}},
	})
	// Known workload, with and without the tolerated .json suffix.
	for _, path := range []string{"/.well-known/agent-card/triage", "/.well-known/agent-card/triage.json"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
		var card AgentCard
		if err := json.Unmarshal(body, &card); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if len(card.Skills) != 1 || card.Skills[0].ID != "triage-skill" {
			t.Fatalf("%s skills = %v", path, card.Skills)
		}
	}
	// Unknown workload → 404.
	resp, err := ts.Client().Get(ts.URL + "/.well-known/agent-card/nope")
	if err != nil {
		t.Fatalf("GET unknown: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown workload card status = %d, want 404", resp.StatusCode)
	}
}

func TestRPCFramingErrors(t *testing.T) {
	ts, _ := testServer(t, Config{})
	cases := []struct {
		name string
		body string
		code int
	}{
		{"malformed json", "{not json", errCodeParse},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"tasks/get"}`, errCodeInvalidRequest},
		{"missing method", `{"jsonrpc":"2.0","id":1}`, errCodeInvalidRequest},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"frobnicate"}`, errCodeMethodNotFound},
		{"message/send empty params", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{}}`, errCodeInvalidParams},
		{"message/stream empty params", `{"jsonrpc":"2.0","id":1,"method":"message/stream","params":{}}`, errCodeInvalidParams},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, resp := rpcCall(t, ts, "", tc.body)
			if status != http.StatusOK {
				t.Fatalf("HTTP status = %d, want 200 (JSON-RPC errors ride 200)", status)
			}
			if resp.Error == nil || resp.Error.Code != tc.code {
				t.Fatalf("error = %+v, want code %d", resp.Error, tc.code)
			}
		})
	}
}

func TestRPCEchoesID(t *testing.T) {
	ts, be := testServer(t, Config{})
	be.tasks["t1"] = TaskInfo{WorkloadName: "triage", State: TaskStateWorking}
	// String id must be echoed verbatim.
	_, resp := rpcCall(t, ts, "", `{"jsonrpc":"2.0","id":"abc","method":"tasks/get","params":{"id":"t1"}}`)
	if string(resp.ID) != `"abc"` {
		t.Fatalf("echoed id = %s, want \"abc\"", resp.ID)
	}
	if resp.JSONRPC != "2.0" {
		t.Fatalf("jsonrpc = %q, want 2.0", resp.JSONRPC)
	}
	// Numeric id echoed verbatim (not stringified).
	_, resp = rpcCall(t, ts, "", `{"jsonrpc":"2.0","id":7,"method":"tasks/get","params":{"id":"t1"}}`)
	if string(resp.ID) != `7` {
		t.Fatalf("echoed numeric id = %s, want 7", resp.ID)
	}
	// A parse error can't recover the request id, so the response id is
	// null — clients still get a well-formed envelope to correlate.
	_, resp = rpcCall(t, ts, "", `{not json`)
	if string(resp.ID) != `null` {
		t.Fatalf("parse-error id = %s, want null", resp.ID)
	}
}

func TestTasksGet(t *testing.T) {
	ts, be := testServer(t, Config{})
	be.tasks["t1"] = TaskInfo{WorkloadName: "triage", State: TaskStateInputRequired, StatusMessage: "awaiting approval"}

	// Success.
	_, resp := rpcCall(t, ts, "", `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"id":"t1"}}`)
	if resp.Error != nil {
		t.Fatalf("tasks/get error: %+v", resp.Error)
	}
	var task Task
	if err := json.Unmarshal(resp.Result, &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if task.Kind != "task" || task.ID != "t1" || task.Status.State != TaskStateInputRequired {
		t.Fatalf("task = %+v", task)
	}
	if task.Status.Message == nil || task.Status.Message.Parts[0].Text != "awaiting approval" {
		t.Fatalf("status message not surfaced: %+v", task.Status.Message)
	}

	// Missing id → invalid params.
	_, resp = rpcCall(t, ts, "", `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{}}`)
	if resp.Error == nil || resp.Error.Code != errCodeInvalidParams {
		t.Fatalf("missing id: error = %+v, want %d", resp.Error, errCodeInvalidParams)
	}

	// Unknown id → task not found.
	_, resp = rpcCall(t, ts, "", `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"id":"ghost"}}`)
	if resp.Error == nil || resp.Error.Code != errCodeTaskNotFound {
		t.Fatalf("unknown id: error = %+v, want %d", resp.Error, errCodeTaskNotFound)
	}
}

func TestTasksCancelIdempotentAndMetered(t *testing.T) {
	metric := &recordingMetric{}
	ts, be := testServer(t, Config{Metric: metric})
	be.tasks["t1"] = TaskInfo{WorkloadName: "triage", State: TaskStateWorking}

	_, resp := rpcCall(t, ts, "", `{"jsonrpc":"2.0","id":1,"method":"tasks/cancel","params":{"id":"t1"}}`)
	if resp.Error != nil {
		t.Fatalf("tasks/cancel error: %+v", resp.Error)
	}
	var task Task
	if err := json.Unmarshal(resp.Result, &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if task.Status.State != TaskStateCanceled {
		t.Fatalf("state = %q, want canceled", task.Status.State)
	}
	// Second cancel is idempotent — backend already reports canceled.
	_, resp = rpcCall(t, ts, "", `{"jsonrpc":"2.0","id":2,"method":"tasks/cancel","params":{"id":"t1"}}`)
	if resp.Error != nil {
		t.Fatalf("second cancel error: %+v", resp.Error)
	}
	// Metric fired once per cancel, tagged by workload + outcome.
	if len(metric.seen) != 2 || metric.seen[0] != "triage/canceled" {
		t.Fatalf("metric = %v, want two triage/canceled", metric.seen)
	}
	// Unknown id → not found.
	_, resp = rpcCall(t, ts, "", `{"jsonrpc":"2.0","id":3,"method":"tasks/cancel","params":{"id":"ghost"}}`)
	if resp.Error == nil || resp.Error.Code != errCodeTaskNotFound {
		t.Fatalf("cancel unknown: error = %+v", resp.Error)
	}
}

// sendBody is a message/send JSON-RPC body with the given text parts (and
// an optional continuation task id).
func sendBody(id, taskID, text string) string {
	tid := ""
	if taskID != "" {
		tid = `"taskId":"` + taskID + `",`
	}
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"message/send","params":{"message":{` +
		`"kind":"message","role":"user","messageId":"m1",` + tid +
		`"parts":[{"kind":"text","text":"` + text + `"}]}}}`
}

// TestMessageSendCompleted is the happy path: a fresh message runs the
// turn, and the backend's answer surfaces as a text artifact on the
// terminal task.
func TestMessageSendCompleted(t *testing.T) {
	ts, be := testServer(t, Config{})
	_, resp := rpcCall(t, ts, "", sendBody("1", "", "investigate pod"))
	if resp.Error != nil {
		t.Fatalf("message/send error: %+v", resp.Error)
	}
	var task Task
	if err := json.Unmarshal(resp.Result, &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if task.Kind != "task" || task.Status.State != TaskStateCompleted {
		t.Fatalf("task = %+v, want completed", task)
	}
	// The agent's answer surfaces as a text result artifact.
	if len(task.Artifacts) != 1 || len(task.Artifacts[0].Parts) != 1 ||
		task.Artifacts[0].Parts[0].Text != "echo:investigate pod" {
		t.Fatalf("result artifact = %+v, want echo:investigate pod", task.Artifacts)
	}
	// The backend received the joined text and no continuation id.
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.submitted) != 1 || be.submitted[0].Text != "investigate pod" {
		t.Fatalf("SubmitMessage params = %+v", be.submitted)
	}
	if be.submitted[0].TaskID != "" {
		t.Fatalf("fresh send carried a task id: %q", be.submitted[0].TaskID)
	}
}

// TestMessageSendContinuesExistingTask: a message carrying a task id
// resolves the owning workload via GetTask and threads the id into
// SubmitMessage (continuation, not a fresh task).
func TestMessageSendContinuesExistingTask(t *testing.T) {
	ts, be := testServer(t, Config{})
	be.tasks["task-x"] = TaskInfo{WorkloadName: "triage", State: TaskStateInputRequired}
	_, resp := rpcCall(t, ts, "", sendBody("1", "task-x", "approved"))
	if resp.Error != nil {
		t.Fatalf("message/send error: %+v", resp.Error)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.submitted) != 1 || be.submitted[0].TaskID != "task-x" {
		t.Fatalf("continuation SubmitMessage params = %+v, want TaskID task-x", be.submitted)
	}
}

// TestMessageSendUnknownContinuation: continuing an unknown task id fails
// the GetTask lookup with TaskNotFound and never reaches SubmitMessage.
func TestMessageSendUnknownContinuation(t *testing.T) {
	ts, be := testServer(t, Config{})
	_, resp := rpcCall(t, ts, "", sendBody("1", "ghost", "hi"))
	if resp.Error == nil || resp.Error.Code != errCodeTaskNotFound {
		t.Fatalf("unknown continuation: error = %+v, want %d", resp.Error, errCodeTaskNotFound)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.submitted) != 0 {
		t.Fatalf("unknown continuation reached the backend: %v", be.submitted)
	}
}

// stubLimiter is a table-driven RateLimiter for the server tests: it
// records every Allow call and returns a fixed verdict.
type stubLimiter struct {
	mu         sync.Mutex
	allow      bool
	retryAfter time.Duration
	calls      []RateLimitRequest
}

func (l *stubLimiter) Allow(_ context.Context, req RateLimitRequest) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, req)
	return l.allow, l.retryAfter
}

// TestMessageSendRateLimited: a refusing limiter turns message/send into
// the retryable -32000 error with an advisory Retry-After header, records
// a "rejected" outcome, and never reaches the backend. The fractional
// retryAfter (2.5s) also pins the ceil: Retry-After must round UP to 3, not
// floor to 2 — a floored hint invites a retry that is still rate limited.
func TestMessageSendRateLimited(t *testing.T) {
	lim := &stubLimiter{allow: false, retryAfter: 2500 * time.Millisecond}
	metric := &recordingMetric{}
	be := newFakeBackend()
	ts, _ := testServer(t, Config{Backend: be, Limiter: lim, Metric: metric})

	// Raw request so we can read the Retry-After header (rpcCall drops it).
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/a2a", strings.NewReader(sendBody("1", "", "investigate pod")))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer httpResp.Body.Close()
	if got := httpResp.Header.Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want 3", got)
	}
	raw, _ := io.ReadAll(httpResp.Body)
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if resp.Error == nil || resp.Error.Code != errCodeUnavailable {
		t.Fatalf("rate-limited send: error = %+v, want %d", resp.Error, errCodeUnavailable)
	}
	// Limiter was consulted for message/send against the resolved workload;
	// the backend was never driven.
	lim.mu.Lock()
	if len(lim.calls) != 1 || lim.calls[0].Method != methodMessageSend || lim.calls[0].Workload != "triage" {
		t.Fatalf("limiter calls = %+v, want one message/send for triage", lim.calls)
	}
	lim.mu.Unlock()
	be.mu.Lock()
	if len(be.submitted) != 0 {
		t.Fatalf("rate-limited send reached the backend: %v", be.submitted)
	}
	be.mu.Unlock()
	metric.mu.Lock()
	defer metric.mu.Unlock()
	if len(metric.seen) != 1 || metric.seen[0] != "triage/"+string(TaskStateRejected) {
		t.Fatalf("metric = %v, want one triage/rejected", metric.seen)
	}
}

// TestMessageSendRateLimiterAdmits: an admitting limiter lets the turn run
// and is keyed by the authenticated caller.
func TestMessageSendRateLimiterAdmits(t *testing.T) {
	v, _ := NewStaticBearerValidator(map[string]*Principal{
		"tok": {Subject: "alice", Tenant: "acme"},
	})
	lim := &stubLimiter{allow: true}
	ts, be := testServer(t, Config{Validator: v, Limiter: lim})
	_, resp := rpcCall(t, ts, "tok", sendBody("1", "", "investigate pod"))
	if resp.Error != nil {
		t.Fatalf("admitted send: error = %+v", resp.Error)
	}
	lim.mu.Lock()
	defer lim.mu.Unlock()
	if len(lim.calls) != 1 || lim.calls[0].Subject != "alice" || lim.calls[0].Tenant != "acme" {
		t.Fatalf("limiter call = %+v, want subject alice / tenant acme", lim.calls)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.submitted) != 1 {
		t.Fatalf("admitted send did not reach the backend: %v", be.submitted)
	}
}

// TestControlVerbsNotRateLimited: tasks/get and tasks/cancel are never
// gated, so an operator can always read or cancel a task even under a
// limiter that refuses everything.
func TestControlVerbsNotRateLimited(t *testing.T) {
	lim := &stubLimiter{allow: false, retryAfter: time.Second}
	be := newFakeBackend()
	be.tasks["t1"] = TaskInfo{WorkloadName: "triage", State: TaskStateWorking}
	ts, _ := testServer(t, Config{Backend: be, Limiter: lim})

	_, get := rpcCall(t, ts, "", `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"id":"t1"}}`)
	if get.Error != nil {
		t.Fatalf("tasks/get under refusing limiter: error = %+v", get.Error)
	}
	_, cancel := rpcCall(t, ts, "", `{"jsonrpc":"2.0","id":2,"method":"tasks/cancel","params":{"id":"t1"}}`)
	if cancel.Error != nil {
		t.Fatalf("tasks/cancel under refusing limiter: error = %+v", cancel.Error)
	}
	lim.mu.Lock()
	defer lim.mu.Unlock()
	if len(lim.calls) != 0 {
		t.Fatalf("control verbs consulted the limiter: %+v", lim.calls)
	}
}

// TestMessageSendRequiresText: a data-only message (no text parts) is
// rejected before the backend runs (Stage B is text-only).
func TestMessageSendRequiresText(t *testing.T) {
	ts, be := testServer(t, Config{})
	body := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{` +
		`"kind":"message","role":"user","messageId":"m1",` +
		`"parts":[{"kind":"data","data":{"pod":"web-1"}}]}}}`
	_, resp := rpcCall(t, ts, "", body)
	if resp.Error == nil || resp.Error.Code != errCodeInvalidParams {
		t.Fatalf("data-only send: error = %+v, want %d", resp.Error, errCodeInvalidParams)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.submitted) != 0 {
		t.Fatalf("data-only send reached the backend: %v", be.submitted)
	}
}

// TestMessageSendScopeForbidden: an authenticated-but-underscoped caller
// cannot invoke a fresh send, and the backend is never reached.
func TestMessageSendScopeForbidden(t *testing.T) {
	v, _ := NewStaticBearerValidator(map[string]*Principal{
		"weak": {Subject: "svc", Scopes: []string{"other:scope"}},
	})
	ts, be := testServer(t, Config{
		Validator: v,
		Skills:    []ExposedSkill{{WorkloadName: "triage", SkillName: "triage", Scopes: []string{"triage:invoke"}}},
	})
	status, _ := rpcCall(t, ts, "weak", sendBody("1", "", "hi"))
	if status != http.StatusForbidden {
		t.Fatalf("underscoped send: status = %d, want 403", status)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.submitted) != 0 {
		t.Fatalf("underscoped send reached the backend: %v", be.submitted)
	}
}

// TestMessageSendBackendError: a backend fault (not TaskNotFound) maps to
// the JSON-RPC internal error.
func TestMessageSendBackendError(t *testing.T) {
	ts, be := testServer(t, Config{})
	be.submitErr = errors.New("runner exploded")
	_, resp := rpcCall(t, ts, "", sendBody("1", "", "hi"))
	if resp.Error == nil || resp.Error.Code != errCodeInternal {
		t.Fatalf("backend error: error = %+v, want %d", resp.Error, errCodeInternal)
	}
}

// TestMessageSendMultiSkill: a fresh message/send on an endpoint exposing
// more than one skill has no first-class skill selector to route on, so it
// is refused as unsupported (-32004) rather than run against an arbitrary
// workload. The backend is never reached.
func TestMessageSendMultiSkill(t *testing.T) {
	ts, be := testServer(t, Config{
		Skills: []ExposedSkill{
			{WorkloadName: "triage", SkillName: "triage"},
			{WorkloadName: "deploy", SkillName: "deploy"},
		},
	})
	_, resp := rpcCall(t, ts, "", sendBody("1", "", "hi"))
	if resp.Error == nil || resp.Error.Code != errCodeUnsupportedOp {
		t.Fatalf("multi-skill fresh send: error = %+v, want %d", resp.Error, errCodeUnsupportedOp)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.submitted) != 0 {
		t.Fatalf("multi-skill send reached the backend: %v", be.submitted)
	}
}

// TestMessageSendUnavailable: a draining/transiently-unavailable backend
// maps to a retryable server-error code (-32000), not the internal-fault
// code (-32603) that would mislead a caller into not retrying.
func TestMessageSendUnavailable(t *testing.T) {
	ts, be := testServer(t, Config{})
	be.submitErr = fmt.Errorf("draining: %w", ErrUnavailable)
	_, resp := rpcCall(t, ts, "", sendBody("1", "", "hi"))
	if resp.Error == nil || resp.Error.Code != errCodeUnavailable {
		t.Fatalf("draining send: error = %+v, want %d", resp.Error, errCodeUnavailable)
	}
}

// TestMessageSendProjectsContextID: the backend's contextId (server-minted
// when the caller omits one) reaches the returned Task on the wire, so a
// client can group follow-up messages under it.
func TestMessageSendProjectsContextID(t *testing.T) {
	ts, be := testServer(t, Config{})
	be.submitResult = TaskInfo{WorkloadName: "triage", State: TaskStateCompleted, ContextID: "ctx-abc123"}
	_, resp := rpcCall(t, ts, "", sendBody("1", "", "hi"))
	if resp.Error != nil {
		t.Fatalf("message/send error: %+v", resp.Error)
	}
	var task Task
	if err := json.Unmarshal(resp.Result, &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if task.ContextID != "ctx-abc123" {
		t.Fatalf("task.contextId = %q, want ctx-abc123 (server-assigned grouping id)", task.ContextID)
	}
}

// TestServerExtractsTraceContext proves the endpoint adopts a caller's
// W3C trace context (traceparent) into the ctx the backend runs under, so
// the turn's spans parent under the caller's span. Neutralize check: have
// handleRPC pass r.Context() (not the extracted ctx) to the handlers and
// the propagated span context vanishes.
func TestServerExtractsTraceContext(t *testing.T) {
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	ts, be := testServer(t, Config{})
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const spanID = "00f067aa0ba902b7"

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/a2a", strings.NewReader(sendBody("1", "", "hi")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	be.mu.Lock()
	defer be.mu.Unlock()
	if be.lastSubmitCtx == nil {
		t.Fatal("SubmitMessage never ran")
	}
	sc := trace.SpanContextFromContext(be.lastSubmitCtx)
	if !sc.IsValid() {
		t.Fatal("no span context reached the backend ctx (traceparent not extracted)")
	}
	if got := sc.TraceID().String(); got != traceID {
		t.Fatalf("propagated trace id = %q, want %q", got, traceID)
	}
}

// streamBody is a message/stream JSON-RPC body with the given text (and an
// optional continuation task id).
func streamBody(id, taskID, text string) string {
	tid := ""
	if taskID != "" {
		tid = `"taskId":"` + taskID + `",`
	}
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"message/stream","params":{"message":{` +
		`"kind":"message","role":"user","messageId":"m1",` + tid +
		`"parts":[{"kind":"text","text":"` + text + `"}]}}}`
}

// streamCall POSTs a message/stream request and returns the HTTP status,
// the response Content-Type, and the decoded JSON-RPC responses. For an SSE
// response it decodes one per `data:` frame, in order; for a pre-stream
// error (application/json) it decodes the single response as one element.
func streamCall(t *testing.T, ts *httptest.Server, token, body string) (int, string, []rpcResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/a2a", strings.NewReader(body))
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
	var out []rpcResponse
	// Transport-level failures (401/403) ride plain-text HTTP bodies, not
	// JSON-RPC; the caller asserts on status, so leave frames empty.
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, ct, nil
	}
	if strings.HasPrefix(ct, "text/event-stream") {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok {
				continue // blank separator line or a non-data field
			}
			var r rpcResponse
			if err := json.Unmarshal([]byte(data), &r); err != nil {
				t.Fatalf("decode SSE frame %q: %v", data, err)
			}
			out = append(out, r)
		}
		if err := scanner.Err(); err != nil {
			t.Fatalf("scan SSE: %v", err)
		}
	} else {
		raw, _ := io.ReadAll(resp.Body)
		var r rpcResponse
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		out = append(out, r)
	}
	return resp.StatusCode, ct, out
}

// frameKind decodes the "kind" discriminator of a streamed result.
func frameKind(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var k struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		t.Fatalf("decode frame kind %q: %v", raw, err)
	}
	return k.Kind
}

// TestMessageStreamHappyPath: a fresh message/stream returns an SSE stream
// whose frames are, in order, the initial Task, at least one interim
// working status-update, the result artifact, and a final status-update
// (final=true) carrying the terminal state. The task outcome is metered
// once.
func TestMessageStreamHappyPath(t *testing.T) {
	metric := &recordingMetric{}
	ts, _ := testServer(t, Config{Metric: metric})
	status, ct, frames := streamCall(t, ts, "", streamBody("1", "", "investigate pod"))
	if status != http.StatusOK {
		t.Fatalf("HTTP status = %d, want 200", status)
	}
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if len(frames) < 4 {
		t.Fatalf("frames = %d, want >= 4 (task, status-update, artifact-update, final status-update)", len(frames))
	}
	// Every frame is a well-formed JSON-RPC success echoing the request id.
	for i, f := range frames {
		if f.Error != nil {
			t.Fatalf("frame %d carried an error: %+v", i, f.Error)
		}
		if string(f.ID) != "1" {
			t.Fatalf("frame %d id = %s, want 1", i, f.ID)
		}
	}
	if k := frameKind(t, frames[0].Result); k != "task" {
		t.Fatalf("first frame kind = %q, want task", k)
	}
	// An interim working status-update precedes the terminal bookend.
	if k := frameKind(t, frames[1].Result); k != "status-update" {
		t.Fatalf("second frame kind = %q, want status-update", k)
	}
	// The result artifact carries the agent's answer.
	var gotArtifact bool
	for _, f := range frames {
		if frameKind(t, f.Result) != "artifact-update" {
			continue
		}
		var au TaskArtifactUpdateEvent
		if err := json.Unmarshal(f.Result, &au); err != nil {
			t.Fatalf("decode artifact-update: %v", err)
		}
		if !au.LastChunk {
			t.Error("artifact-update lastChunk = false, want true (whole-artifact emit)")
		}
		if len(au.Artifact.Parts) == 0 || au.Artifact.Parts[0].Text != "echo:investigate pod" {
			t.Fatalf("artifact parts = %+v, want the agent answer", au.Artifact.Parts)
		}
		gotArtifact = true
	}
	if !gotArtifact {
		t.Fatal("no artifact-update frame; the agent answer was not streamed as an artifact")
	}
	// The last frame is the terminal status-update: final=true, completed.
	last := frames[len(frames)-1]
	if k := frameKind(t, last.Result); k != "status-update" {
		t.Fatalf("last frame kind = %q, want status-update", k)
	}
	var final TaskStatusUpdateEvent
	if err := json.Unmarshal(last.Result, &final); err != nil {
		t.Fatalf("decode final status-update: %v", err)
	}
	if !final.Final {
		t.Error("last status-update final = false, want true")
	}
	if final.Status.State != TaskStateCompleted {
		t.Fatalf("final state = %q, want completed", final.Status.State)
	}
	// Exactly one metered outcome, tagged completed.
	metric.mu.Lock()
	defer metric.mu.Unlock()
	if len(metric.seen) != 1 || metric.seen[0] != "triage/"+string(TaskStateCompleted) {
		t.Fatalf("metric = %v, want one triage/completed", metric.seen)
	}
}

// TestMessageStreamOnlyOneFinal pins the stream-termination contract:
// exactly one status-update carries final=true, and it is the last frame —
// interim progress updates must not prematurely close the stream.
func TestMessageStreamOnlyOneFinal(t *testing.T) {
	ts, _ := testServer(t, Config{})
	_, _, frames := streamCall(t, ts, "", streamBody("1", "", "hi"))
	finals := 0
	lastFinal := -1
	for i, f := range frames {
		if frameKind(t, f.Result) != "status-update" {
			continue
		}
		var su TaskStatusUpdateEvent
		if err := json.Unmarshal(f.Result, &su); err != nil {
			t.Fatalf("decode status-update: %v", err)
		}
		if su.Final {
			finals++
			lastFinal = i
		}
	}
	if finals != 1 {
		t.Fatalf("final=true count = %d, want exactly 1", finals)
	}
	if lastFinal != len(frames)-1 {
		t.Fatalf("final=true at frame %d, want the last frame %d", lastFinal, len(frames)-1)
	}
}

// TestMessageStreamRateLimited: a refusing limiter turns message/stream
// into a normal JSON-RPC -32000 error (NOT an SSE stream) with an advisory
// Retry-After, records a "rejected" outcome, and never reaches the backend
// — the refusal is decided before the SSE upgrade.
func TestMessageStreamRateLimited(t *testing.T) {
	lim := &stubLimiter{allow: false, retryAfter: 2500 * time.Millisecond}
	metric := &recordingMetric{}
	be := newFakeBackend()
	ts, _ := testServer(t, Config{Backend: be, Limiter: lim, Metric: metric})

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/a2a", strings.NewReader(streamBody("1", "", "hi")))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer httpResp.Body.Close()
	if ct := httpResp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("rate-limited stream opened an SSE stream (Content-Type %q); refusal must precede the upgrade", ct)
	}
	if got := httpResp.Header.Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want 3", got)
	}
	raw, _ := io.ReadAll(httpResp.Body)
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if resp.Error == nil || resp.Error.Code != errCodeUnavailable {
		t.Fatalf("rate-limited stream: error = %+v, want %d", resp.Error, errCodeUnavailable)
	}
	lim.mu.Lock()
	if len(lim.calls) != 1 || lim.calls[0].Method != methodMessageStream || lim.calls[0].Workload != "triage" {
		t.Fatalf("limiter calls = %+v, want one message/stream for triage", lim.calls)
	}
	lim.mu.Unlock()
	be.mu.Lock()
	if len(be.submitted) != 0 {
		t.Fatalf("rate-limited stream reached the backend: %v", be.submitted)
	}
	be.mu.Unlock()
	metric.mu.Lock()
	defer metric.mu.Unlock()
	if len(metric.seen) != 1 || metric.seen[0] != "triage/"+string(TaskStateRejected) {
		t.Fatalf("metric = %v, want one triage/rejected", metric.seen)
	}
}

// TestMessageStreamRequiresText: a data-only message is rejected before the
// backend runs, as a normal JSON-RPC error (streaming is text-only).
func TestMessageStreamRequiresText(t *testing.T) {
	ts, be := testServer(t, Config{})
	body := `{"jsonrpc":"2.0","id":1,"method":"message/stream","params":{"message":{` +
		`"kind":"message","role":"user","messageId":"m1",` +
		`"parts":[{"kind":"data","data":{"pod":"web-1"}}]}}}`
	_, ct, frames := streamCall(t, ts, "", body)
	if strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("text-less stream opened an SSE stream (Content-Type %q)", ct)
	}
	if len(frames) != 1 || frames[0].Error == nil || frames[0].Error.Code != errCodeInvalidParams {
		t.Fatalf("data-only stream: frames = %+v, want one invalid-params error", frames)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.submitted) != 0 {
		t.Fatalf("data-only stream reached the backend: %v", be.submitted)
	}
}

// TestMessageStreamTaskNotFound: a continuation targeting an unknown task
// is a pre-stream JSON-RPC not-found error, not an opened SSE stream.
func TestMessageStreamTaskNotFound(t *testing.T) {
	ts, _ := testServer(t, Config{})
	_, ct, frames := streamCall(t, ts, "", streamBody("1", "ghost", "hi"))
	if strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("unknown-continuation stream opened an SSE stream (Content-Type %q)", ct)
	}
	if len(frames) != 1 || frames[0].Error == nil || frames[0].Error.Code != errCodeTaskNotFound {
		t.Fatalf("unknown continuation: frames = %+v, want one task-not-found error", frames)
	}
}

// TestMessageStreamScopeForbidden: an authenticated-but-underscoped caller
// cannot open a stream, and the backend is never reached (403 before the
// SSE upgrade).
func TestMessageStreamScopeForbidden(t *testing.T) {
	v, _ := NewStaticBearerValidator(map[string]*Principal{
		"weak": {Subject: "svc", Scopes: []string{"other:scope"}},
	})
	ts, be := testServer(t, Config{
		Validator: v,
		Skills:    []ExposedSkill{{WorkloadName: "triage", SkillName: "triage", Scopes: []string{"triage:invoke"}}},
	})
	status, _, _ := streamCall(t, ts, "weak", streamBody("1", "", "hi"))
	if status != http.StatusForbidden {
		t.Fatalf("underscoped stream: status = %d, want 403", status)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.submitted) != 0 {
		t.Fatalf("underscoped stream reached the backend: %v", be.submitted)
	}
}

// TestMessageStreamUnavailableBeforeEmit: a backend that refuses before any
// emit (drain) yields a normal retryable JSON-RPC error (-32000), not a
// half-open SSE stream.
func TestMessageStreamUnavailableBeforeEmit(t *testing.T) {
	ts, be := testServer(t, Config{})
	be.submitErr = fmt.Errorf("draining: %w", ErrUnavailable)
	_, ct, frames := streamCall(t, ts, "", streamBody("1", "", "hi"))
	if strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("pre-emit failure opened an SSE stream (Content-Type %q)", ct)
	}
	if len(frames) != 1 || frames[0].Error == nil || frames[0].Error.Code != errCodeUnavailable {
		t.Fatalf("pre-emit drain: frames = %+v, want one unavailable error", frames)
	}
}

// TestMessageStreamMidStreamError: a backend that fails AFTER the initial
// Task frame (the drain-after-initial-Task race) must close the already-open
// SSE stream with exactly one terminal failed status-update — final:true, and
// carrying the minted taskId/contextId so the client can correlate the close
// with the task it opened. Neutralize check: drop the id/context from the
// backend's error return (as the pre-fix code did) and the taskId assertion
// fails on an empty id.
func TestMessageStreamMidStreamError(t *testing.T) {
	ts, be := testServer(t, Config{})
	// State set so the backend's default-fill does not overwrite ContextID.
	be.submitResult = TaskInfo{WorkloadName: "triage", State: TaskStateWorking, ContextID: "ctx-mid"}
	be.emitThenErr = fmt.Errorf("draining mid-turn: %w", ErrUnavailable)

	_, ct, frames := streamCall(t, ts, "", streamBody("1", "", "hi"))
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("mid-stream failure did not open an SSE stream (Content-Type %q)", ct)
	}
	if len(frames) != 2 {
		t.Fatalf("mid-stream failure: got %d frames, want 2 (initial task + terminal status-update)", len(frames))
	}
	// Initial frame: the working Task snapshot with the minted id.
	if k := frameKind(t, frames[0].Result); k != "task" {
		t.Fatalf("frame[0] kind = %q, want task", k)
	}
	var initial Task
	if err := json.Unmarshal(frames[0].Result, &initial); err != nil {
		t.Fatalf("decode initial task: %v", err)
	}
	// Terminal frame: exactly one final:true failed status-update, correlatable
	// by the same taskId/contextId (the regression this test pins).
	if k := frameKind(t, frames[1].Result); k != "status-update" {
		t.Fatalf("frame[1] kind = %q, want status-update", k)
	}
	var term TaskStatusUpdateEvent
	if err := json.Unmarshal(frames[1].Result, &term); err != nil {
		t.Fatalf("decode terminal frame: %v", err)
	}
	if !term.Final {
		t.Fatal("terminal frame final = false, want true (stream must end on the failure)")
	}
	if term.Status.State != TaskStateFailed {
		t.Fatalf("terminal state = %q, want failed", term.Status.State)
	}
	if term.TaskID == "" || term.TaskID != initial.ID {
		t.Fatalf("terminal taskId = %q, want the initial frame's %q (correlatable)", term.TaskID, initial.ID)
	}
	if term.ContextID != "ctx-mid" {
		t.Fatalf("terminal contextId = %q, want ctx-mid", term.ContextID)
	}
}

// TestMessageStreamSharesSendRateLimitBucket: message/send and message/stream
// draw from the same (caller, workload) bucket, so exhausting one refuses the
// other. Neutralize check: add Method to the limiter's bucket key and the
// second (stream) call is admitted, failing the -32000 assertion.
func TestMessageStreamSharesSendRateLimitBucket(t *testing.T) {
	lim, err := NewTokenBucketLimiter(1, 1) // 1 token: the first turn-driving call spends it
	if err != nil {
		t.Fatalf("NewTokenBucketLimiter: %v", err)
	}
	ts, be := testServer(t, Config{Limiter: lim})

	// First message/send consumes the only token.
	st, resp := rpcCall(t, ts, "", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"parts":[{"kind":"text","text":"hi"}]}}}`)
	if st != http.StatusOK || resp.Error != nil {
		t.Fatalf("first send: status %d, resp %+v, want 200 ok", st, resp)
	}
	// message/stream from the same caller now finds the shared bucket empty.
	_, ct, frames := streamCall(t, ts, "", streamBody("2", "", "hi"))
	if strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("rate-limited stream opened an SSE stream (Content-Type %q)", ct)
	}
	if len(frames) != 1 || frames[0].Error == nil || frames[0].Error.Code != errCodeUnavailable {
		t.Fatalf("shared-bucket stream: frames = %+v, want one -32000 (bucket drained by send)", frames)
	}
	if len(be.submitted) != 1 {
		t.Fatalf("rate-limited stream reached the backend: submitted = %d, want 1 (send only)", len(be.submitted))
	}
}

// TestMessageStreamCardAdvertisesStreaming: the aggregated card advertises
// the streaming capability so a client knows message/stream is served.
func TestMessageStreamCardAdvertisesStreaming(t *testing.T) {
	ts, _ := testServer(t, Config{})
	resp, err := ts.Client().Get(ts.URL + WellKnownCardPath)
	if err != nil {
		t.Fatalf("GET card: %v", err)
	}
	defer resp.Body.Close()
	var card AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if !card.Capabilities.Streaming {
		t.Fatal("card capabilities.streaming = false, want true (message/stream is served)")
	}
}

func TestAuthRequired(t *testing.T) {
	v, _ := NewStaticBearerValidator(map[string]*Principal{
		"good": {Subject: "svc", Scopes: []string{"triage:invoke"}},
	})
	ts, be := testServer(t, Config{
		Validator: v,
		Skills:    []ExposedSkill{{WorkloadName: "triage", SkillName: "triage", Scopes: []string{"triage:invoke"}}},
	})
	be.tasks["t1"] = TaskInfo{WorkloadName: "triage", State: TaskStateWorking}
	body := `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"id":"t1"}}`

	// No bearer → 401.
	status, _ := rpcCall(t, ts, "", body)
	if status != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", status)
	}
	// Bad bearer → 401.
	status, _ = rpcCall(t, ts, "nope", body)
	if status != http.StatusUnauthorized {
		t.Fatalf("bad token: status = %d, want 401", status)
	}
	// Good bearer with the required scope → 200.
	status, resp := rpcCall(t, ts, "good", body)
	if status != http.StatusOK || resp.Error != nil {
		t.Fatalf("good token: status = %d, error = %+v", status, resp.Error)
	}
}

func TestScopeForbidden(t *testing.T) {
	v, _ := NewStaticBearerValidator(map[string]*Principal{
		// Authenticated, but WITHOUT the triage:invoke scope.
		"weak": {Subject: "svc", Scopes: []string{"other:scope"}},
	})
	ts, be := testServer(t, Config{
		Validator: v,
		Skills:    []ExposedSkill{{WorkloadName: "triage", SkillName: "triage", Scopes: []string{"triage:invoke"}}},
	})
	be.tasks["t1"] = TaskInfo{WorkloadName: "triage", State: TaskStateWorking}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/a2a",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"id":"t1"}}`))
	req.Header.Set("Authorization", "Bearer weak")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("underscoped caller: status = %d, want 403", resp.StatusCode)
	}
}

// TestScopeForbiddenOnCancel proves the scope gate covers the destructive
// verb too: an authenticated-but-underscoped caller cannot cancel a task,
// and the backend's CancelTask is never reached.
func TestScopeForbiddenOnCancel(t *testing.T) {
	v, _ := NewStaticBearerValidator(map[string]*Principal{
		"weak": {Subject: "svc", Scopes: []string{"other:scope"}},
	})
	ts, be := testServer(t, Config{
		Validator: v,
		Skills:    []ExposedSkill{{WorkloadName: "triage", SkillName: "triage", Scopes: []string{"triage:invoke"}}},
	})
	be.tasks["t1"] = TaskInfo{WorkloadName: "triage", State: TaskStateWorking}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/a2a",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tasks/cancel","params":{"id":"t1"}}`))
	req.Header.Set("Authorization", "Bearer weak")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("underscoped cancel: status = %d, want 403", resp.StatusCode)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.canceled) != 0 {
		t.Fatalf("underscoped cancel reached the backend: canceled = %v", be.canceled)
	}
}

// TestUnexposedWorkloadForbidden pins authorizeTask's defensive branch: a
// task whose resolved workload is not exposed by this server is refused
// (403) rather than leaked under some other skill's scopes — even for a
// caller that would satisfy the exposed skill's scopes.
func TestUnexposedWorkloadForbidden(t *testing.T) {
	v, _ := NewStaticBearerValidator(map[string]*Principal{
		"good": {Subject: "svc", Scopes: []string{"triage:invoke"}},
	})
	ts, be := testServer(t, Config{
		Validator: v,
		Skills:    []ExposedSkill{{WorkloadName: "triage", SkillName: "triage", Scopes: []string{"triage:invoke"}}},
	})
	// Backend resolves the task to a workload the server does not expose.
	be.tasks["t1"] = TaskInfo{WorkloadName: "ghostwork", State: TaskStateWorking}

	status, _ := rpcCall(t, ts, "good", `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"id":"t1"}}`)
	if status != http.StatusForbidden {
		t.Fatalf("unexposed workload: status = %d, want 403", status)
	}
}

func TestVersionHeader(t *testing.T) {
	ts, _ := testServer(t, Config{})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/a2a",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"id":"x"}}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get(VersionHeader); got != ProtocolVersion {
		t.Fatalf("%s = %q, want %q", VersionHeader, got, ProtocolVersion)
	}
}
