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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeBackend is a table-driven Backend for the server tests: GetTask and
// CancelTask return canned snapshots (or errors) keyed by task id.
type fakeBackend struct {
	mu       sync.Mutex
	tasks    map[string]TaskInfo // task id → snapshot GetTask returns
	getErr   map[string]error    // task id → error GetTask returns instead
	canceled []string            // task ids passed to CancelTask, in order
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
		{"message/send unsupported", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{}}`, errCodeUnsupportedOp},
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
