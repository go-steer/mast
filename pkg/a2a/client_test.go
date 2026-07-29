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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/federation"
)

// stubA2AServer is an httptest-backed A2A v0.3 server: agent card at
// the well-known path plus the single JSON-RPC 2.0 endpoint with
// per-method handlers. It records every request for assertions.
type stubA2AServer struct {
	t *testing.T

	mu       sync.Mutex
	requests []stubRequest // JSON-RPC requests, in order
	cardGets int

	// handlers maps JSON-RPC method → handler returning (result, error).
	handlers map[string]func(params json.RawMessage) (any, *RPCError)

	// wantToken, when set, enforces `Authorization: Bearer <wantToken>`
	// on the JSON-RPC endpoint (401 otherwise).
	wantToken string

	srv  *httptest.Server
	card AgentCard
}

type stubRequest struct {
	Method  string
	Params  json.RawMessage
	Headers http.Header
}

func newStubA2AServer(t *testing.T) *stubA2AServer {
	t.Helper()
	s := &stubA2AServer{
		t:        t,
		handlers: map[string]func(json.RawMessage) (any, *RPCError){},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+WellKnownCardPath, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.cardGets++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(s.card); err != nil {
			t.Errorf("encode card: %v", err)
		}
	})
	mux.HandleFunc("POST /a2a", func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get(VersionHeader); v != ProtocolVersion {
			t.Errorf("JSON-RPC request missing/wrong %s header: %q", VersionHeader, v)
		}
		if s.wantToken != "" && r.Header.Get("Authorization") != "Bearer "+s.wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var rawReq struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rawReq); err != nil {
			t.Errorf("decode JSON-RPC request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if rawReq.JSONRPC != "2.0" {
			t.Errorf("jsonrpc = %q, want 2.0", rawReq.JSONRPC)
		}
		s.mu.Lock()
		s.requests = append(s.requests, stubRequest{Method: rawReq.Method, Params: rawReq.Params, Headers: r.Header.Clone()})
		handler := s.handlers[rawReq.Method]
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"jsonrpc": "2.0", "id": rawReq.ID}
		if handler == nil {
			resp["error"] = &RPCError{Code: -32601, Message: "method not found: " + rawReq.Method}
		} else if result, rpcErr := handler(rawReq.Params); rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode JSON-RPC response: %v", err)
		}
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	s.card = AgentCard{
		Name:            "sample-external",
		Description:     "stub A2A v0.3 agent",
		URL:             s.srv.URL + "/a2a",
		ProtocolVersion: "0.3.0",
		Skills: []AgentSkill{
			{ID: "triage", Name: "Triage", InputModes: []string{"application/json"}, OutputModes: []string{"application/json"}},
		},
	}
	return s
}

func (s *stubA2AServer) calls(method string) []stubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []stubRequest
	for _, r := range s.requests {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func (s *stubA2AServer) config(name string) AgentConfig {
	return AgentConfig{Name: name, AgentCardURL: s.srv.URL, TimeoutSeconds: 5}
}

func newTestClient(t *testing.T, cfg AgentConfig) *Client {
	t.Helper()
	c, err := NewClient(cfg, WithPollInterval(2*time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// directReplyHandler returns a message/send handler answering with a
// direct message (no task opened).
func directReplyHandler(text string, data map[string]any) func(json.RawMessage) (any, *RPCError) {
	return func(json.RawMessage) (any, *RPCError) {
		return Message{
			Kind:      "message",
			MessageID: "m-reply",
			Role:      "agent",
			Parts: []Part{
				{Kind: "text", Text: text},
				{Kind: "data", Data: data},
			},
		}, nil
	}
}

func TestSendDirectMessageReply(t *testing.T) {
	s := newStubA2AServer(t)
	s.handlers[methodMessageSend] = directReplyHandler("all clear", map[string]any{"verdict": "ok"})

	res, err := newTestClient(t, s.config("sample-external")).Send(context.Background(), "triage", map[string]any{"pod": "default/web-1"}, 0)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.State != "completed" || res.Text != "all clear" || res.Output["verdict"] != "ok" || res.RemoteID != "" {
		t.Errorf("Result = %+v", res)
	}

	sends := s.calls(methodMessageSend)
	if len(sends) != 1 {
		t.Fatalf("message/send calls = %d, want 1", len(sends))
	}
	var params messageSendParams
	if err := json.Unmarshal(sends[0].Params, &params); err != nil {
		t.Fatalf("unmarshal send params: %v", err)
	}
	if params.Message == nil || params.Message.Role != "user" || params.Message.MessageID == "" {
		t.Errorf("sent message = %+v", params.Message)
	}
	if params.Message.Metadata["skillId"] != "triage" {
		t.Errorf("skillId metadata = %v, want triage", params.Message.Metadata)
	}
	if len(params.Message.Parts) != 1 || params.Message.Parts[0].Kind != "data" || params.Message.Parts[0].Data["pod"] != "default/web-1" {
		t.Errorf("sent parts = %+v", params.Message.Parts)
	}
	if params.Configuration == nil || params.Configuration.Blocking == nil || !*params.Configuration.Blocking {
		t.Errorf("configuration = %+v, want blocking=true", params.Configuration)
	}
}

// taskFixture installs the second required fixture from fork-design
// exit criterion 9's test surface: message/send opens a task that
// completes on the SECOND tasks/get poll.
func (s *stubA2AServer) installTaskFixture(taskID string) {
	polls := 0
	s.handlers[methodMessageSend] = func(json.RawMessage) (any, *RPCError) {
		return Task{Kind: "task", ID: taskID, ContextID: "ctx-1", Status: TaskStatus{State: TaskStateSubmitted}}, nil
	}
	s.handlers[methodTasksGet] = func(params json.RawMessage) (any, *RPCError) {
		var q taskQueryParams
		if err := json.Unmarshal(params, &q); err != nil || q.ID != taskID {
			return nil, &RPCError{Code: -32001, Message: "task not found"}
		}
		polls++
		if polls < 2 {
			return Task{Kind: "task", ID: taskID, ContextID: "ctx-1", Status: TaskStatus{State: TaskStateWorking}}, nil
		}
		return Task{
			Kind: "task", ID: taskID, ContextID: "ctx-1",
			Status: TaskStatus{
				State:   TaskStateCompleted,
				Message: &Message{Kind: "message", MessageID: "m-done", Role: "agent", Parts: []Part{{Kind: "text", Text: "rolled back"}}},
			},
			Artifacts: []Artifact{{ArtifactID: "a-1", Parts: []Part{{Kind: "data", Data: map[string]any{"root_cause": "bad image tag"}}}}},
		}, nil
	}
}

func TestSendTaskReplyPollsToCompletion(t *testing.T) {
	s := newStubA2AServer(t)
	s.installTaskFixture("task-42")

	res, err := newTestClient(t, s.config("sample-external")).Send(context.Background(), "triage", map[string]any{"pod": "p"}, 0)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.State != string(TaskStateCompleted) || res.RemoteID != "task-42" {
		t.Errorf("Result = %+v", res)
	}
	if res.Output["root_cause"] != "bad image tag" {
		t.Errorf("Output = %v", res.Output)
	}
	if res.Text != "rolled back" {
		t.Errorf("Text = %q", res.Text)
	}
	if gets := len(s.calls(methodTasksGet)); gets != 2 {
		t.Errorf("tasks/get calls = %d, want 2 (completes on second poll)", gets)
	}
	if cancels := len(s.calls(methodTasksCancel)); cancels != 0 {
		t.Errorf("tasks/cancel calls = %d, want 0", cancels)
	}
}

func TestSendCancelsTaskOnContextCancellation(t *testing.T) {
	s := newStubA2AServer(t)
	s.handlers[methodMessageSend] = func(json.RawMessage) (any, *RPCError) {
		return Task{Kind: "task", ID: "task-stuck", ContextID: "c", Status: TaskStatus{State: TaskStateSubmitted}}, nil
	}
	s.handlers[methodTasksGet] = func(json.RawMessage) (any, *RPCError) {
		return Task{Kind: "task", ID: "task-stuck", ContextID: "c", Status: TaskStatus{State: TaskStateWorking}}, nil
	}
	s.handlers[methodTasksCancel] = func(json.RawMessage) (any, *RPCError) {
		return Task{Kind: "task", ID: "task-stuck", ContextID: "c", Status: TaskStatus{State: TaskStateCanceled}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := newTestClient(t, s.config("sample-external")).Send(ctx, "triage", nil, 0)
	if err == nil {
		t.Fatal("Send succeeded, want cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled in chain", err)
	}
	cancels := s.calls(methodTasksCancel)
	if len(cancels) != 1 {
		t.Fatalf("tasks/cancel calls = %d, want 1", len(cancels))
	}
	var p taskIDParams
	if err := json.Unmarshal(cancels[0].Params, &p); err != nil || p.ID != "task-stuck" {
		t.Errorf("tasks/cancel params = %s", cancels[0].Params)
	}
}

func TestSendBoundedTimeoutCancelsTask(t *testing.T) {
	s := newStubA2AServer(t)
	s.handlers[methodMessageSend] = func(json.RawMessage) (any, *RPCError) {
		return Task{Kind: "task", ID: "task-slow", ContextID: "c", Status: TaskStatus{State: TaskStateWorking}}, nil
	}
	s.handlers[methodTasksGet] = func(json.RawMessage) (any, *RPCError) {
		return Task{Kind: "task", ID: "task-slow", ContextID: "c", Status: TaskStatus{State: TaskStateWorking}}, nil
	}
	s.handlers[methodTasksCancel] = func(json.RawMessage) (any, *RPCError) {
		return Task{Kind: "task", ID: "task-slow", ContextID: "c", Status: TaskStatus{State: TaskStateCanceled}}, nil
	}

	// v0.1 blocks to a bounded timeout (durable-execution phasing) —
	// here 30ms, far below the fixture's forever-working task.
	_, err := newTestClient(t, s.config("sample-external")).Send(context.Background(), "triage", nil, 30*time.Millisecond)
	if !errors.Is(err, federation.ErrTimeout) {
		t.Fatalf("err = %v, want federation.ErrTimeout", err)
	}
	if cancels := len(s.calls(methodTasksCancel)); cancels != 1 {
		t.Errorf("tasks/cancel calls = %d, want 1", cancels)
	}
}

func TestSendFailedTask(t *testing.T) {
	s := newStubA2AServer(t)
	s.handlers[methodMessageSend] = func(json.RawMessage) (any, *RPCError) {
		return Task{
			Kind: "task", ID: "task-f", ContextID: "c",
			Status: TaskStatus{
				State:   TaskStateFailed,
				Message: &Message{Kind: "message", MessageID: "m", Role: "agent", Parts: []Part{{Kind: "text", Text: "backend exploded"}}},
			},
		}, nil
	}
	_, err := newTestClient(t, s.config("sample-external")).Send(context.Background(), "triage", nil, 0)
	if !errors.Is(err, federation.ErrRemoteFailed) {
		t.Fatalf("err = %v, want federation.ErrRemoteFailed", err)
	}
	if !strings.Contains(err.Error(), "backend exploded") {
		t.Errorf("err %q does not carry the remote failure detail", err)
	}
}

func TestSendInputRequiredIsRejectedInV01(t *testing.T) {
	// Remote HITL needs programmatic pause (v0.2); the v0.1 client
	// must cancel and say so rather than spin until timeout.
	s := newStubA2AServer(t)
	s.handlers[methodMessageSend] = func(json.RawMessage) (any, *RPCError) {
		return Task{Kind: "task", ID: "task-h", ContextID: "c", Status: TaskStatus{State: TaskStateInputRequired}}, nil
	}
	s.handlers[methodTasksCancel] = func(json.RawMessage) (any, *RPCError) {
		return Task{Kind: "task", ID: "task-h", ContextID: "c", Status: TaskStatus{State: TaskStateCanceled}}, nil
	}
	_, err := newTestClient(t, s.config("sample-external")).Send(context.Background(), "triage", nil, 0)
	if !errors.Is(err, federation.ErrRemoteFailed) || !strings.Contains(err.Error(), "input-required") {
		t.Fatalf("err = %v, want ErrRemoteFailed mentioning input-required", err)
	}
	if cancels := len(s.calls(methodTasksCancel)); cancels != 1 {
		t.Errorf("tasks/cancel calls = %d, want 1", cancels)
	}
}

func TestBearerAuthFromEnv(t *testing.T) {
	s := newStubA2AServer(t)
	s.wantToken = "sekrit"
	s.handlers[methodMessageSend] = directReplyHandler("ok", nil)

	cfg := s.config("sample-external")
	cfg.Auth = &AuthConfig{Type: AuthTypeBearer, TokenEnv: "TEST_A2A_TOKEN"}

	t.Setenv("TEST_A2A_TOKEN", "sekrit")
	if _, err := newTestClient(t, cfg).Send(context.Background(), "triage", nil, 0); err != nil {
		t.Fatalf("Send with valid token: %v", err)
	}

	// Wrong token → HTTP 401 → ErrAuthFailed.
	t.Setenv("TEST_A2A_TOKEN", "wrong")
	if _, err := newTestClient(t, cfg).Send(context.Background(), "triage", nil, 0); !errors.Is(err, federation.ErrAuthFailed) {
		t.Fatalf("err = %v, want federation.ErrAuthFailed on 401", err)
	}

	// Configured-but-unset env var fails before any request is sent.
	cfgUnset := cfg
	cfgUnset.Auth = &AuthConfig{Type: AuthTypeBearer, TokenEnv: "TEST_A2A_TOKEN_UNSET"}
	if _, err := newTestClient(t, cfgUnset).Send(context.Background(), "triage", nil, 0); !errors.Is(err, federation.ErrAuthFailed) {
		t.Fatalf("err = %v, want federation.ErrAuthFailed for unset token env", err)
	}
}

func TestAgentCardFetchedOnceAndCached(t *testing.T) {
	s := newStubA2AServer(t)
	s.handlers[methodMessageSend] = directReplyHandler("ok", nil)

	c := newTestClient(t, s.config("sample-external"))
	for i := 0; i < 3; i++ {
		if _, err := c.Send(context.Background(), "triage", nil, 0); err != nil {
			t.Fatalf("Send #%d: %v", i+1, err)
		}
	}
	s.mu.Lock()
	gets := s.cardGets
	s.mu.Unlock()
	if gets != 1 {
		t.Errorf("agent-card fetches = %d, want 1 (cached for process lifetime; refresh is v0.2)", gets)
	}
	card, err := c.Card(context.Background())
	if err != nil || card == nil || card.Name != "sample-external" {
		t.Errorf("Card() = (%+v, %v)", card, err)
	}
}

func TestSkillValidation(t *testing.T) {
	s := newStubA2AServer(t)
	s.handlers[methodMessageSend] = directReplyHandler("ok", nil)

	// Unknown skill vs. the fetched card.
	c := newTestClient(t, s.config("sample-external"))
	if _, err := c.Send(context.Background(), "nope", nil, 0); err == nil || !strings.Contains(err.Error(), "not present in agent card") {
		t.Errorf("unknown skill err = %v", err)
	}

	// Config allowlist narrows the card.
	cfg := s.config("sample-external")
	cfg.Skills = []string{"other-skill"}
	if _, err := newTestClient(t, cfg).Send(context.Background(), "triage", nil, 0); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("allowlist err = %v", err)
	}
}

func TestEndpointOnlyConfigSkipsCard(t *testing.T) {
	s := newStubA2AServer(t)
	s.handlers[methodMessageSend] = directReplyHandler("ok", nil)

	cfg := AgentConfig{Name: "pinned", Endpoint: s.srv.URL + "/a2a", TimeoutSeconds: 5}
	res, err := newTestClient(t, cfg).Send(context.Background(), "triage", nil, 0)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q", res.Text)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cardGets != 0 {
		t.Errorf("card fetches = %d, want 0 for endpoint-only config", s.cardGets)
	}
}

func TestCardWithoutJSONRPCTransportIsProtocolMismatch(t *testing.T) {
	s := newStubA2AServer(t)
	s.card.PreferredTransport = "GRPC"
	s.card.AdditionalInterfaces = []AgentInterface{{Transport: "HTTP+JSON", URL: s.srv.URL + "/rest"}}

	_, err := newTestClient(t, s.config("sample-external")).Send(context.Background(), "triage", nil, 0)
	if !errors.Is(err, federation.ErrProtocolMismatch) {
		t.Fatalf("err = %v, want federation.ErrProtocolMismatch", err)
	}
}

func TestUnreachableEndpoint(t *testing.T) {
	cfg := AgentConfig{Name: "gone", Endpoint: "http://127.0.0.1:1/a2a", TimeoutSeconds: 2}
	_, err := newTestClient(t, cfg).Send(context.Background(), "", nil, 0)
	if !errors.Is(err, federation.ErrUnreachable) {
		t.Fatalf("err = %v, want federation.ErrUnreachable", err)
	}
}
