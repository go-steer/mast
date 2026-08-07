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
		{"message/stream unsupported", `{"jsonrpc":"2.0","id":1,"method":"message/stream","params":{}}`, errCodeUnsupportedOp},
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
