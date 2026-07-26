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
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"

	"github.com/go-steer/mast/pkg/federation"
)

// toolRunner is the runtime surface of an ADK function tool (the
// concrete functiontool type is unexported).
type toolRunner interface {
	Run(ctx adkagent.Context, args any) (map[string]any, error)
}

// stubToolContext adapts a plain context.Context into an
// adkagent.Context for driving tools in tests.
type stubToolContext struct {
	adkagent.ContextMock
	ctx context.Context
}

func (s *stubToolContext) Deadline() (time.Time, bool) { return s.ctx.Deadline() }
func (s *stubToolContext) Done() <-chan struct{}       { return s.ctx.Done() }
func (s *stubToolContext) Err() error                  { return s.ctx.Err() }
func (s *stubToolContext) Value(key any) any           { return s.ctx.Value(key) }

// TestFederationRoundTripThroughInvokeRemoteAgent is fork-design
// **Phase 1 exit criterion 9**: "Federation round-trip (client side): a
// sample workload's planner invokes
// invoke_remote_agent("a2a://sample-external", ...) against a stub A2A
// v0.3 server via the A2A client adapter; result surfaces to the
// planner as tool output." The planner side is exercised at the tool
// boundary — Run with LLM-shaped map args is exactly what the ADK
// runner feeds a function tool when the model emits the call.
func TestFederationRoundTripThroughInvokeRemoteAgent(t *testing.T) {
	s := newStubA2AServer(t)
	s.installTaskFixture("task-e2e") // task completes on the 2nd tasks/get poll

	adapter, err := NewAdapter([]AgentConfig{s.config("sample-external")}, WithPollInterval(2*time.Millisecond))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	tl, err := federation.NewInvokeRemoteAgentTool(federation.NewRegistry(adapter))
	if err != nil {
		t.Fatalf("NewInvokeRemoteAgentTool: %v", err)
	}

	out, err := tl.(toolRunner).Run(&stubToolContext{ctx: context.Background()}, map[string]any{
		"reference": "a2a://sample-external/triage",
		"inputs":    map[string]any{"pod": "default/web-1", "symptom": "ImagePullBackOff"},
	})
	if err != nil {
		t.Fatalf("invoke_remote_agent: %v", err)
	}
	if out["state"] != string(TaskStateCompleted) || out["task_id"] != "task-e2e" {
		t.Errorf("tool output = %v", out)
	}
	if data, ok := out["output"].(map[string]any); !ok || data["root_cause"] != "bad image tag" {
		t.Errorf("tool output.output = %v", out["output"])
	}
	if out["text"] != "rolled back" {
		t.Errorf("tool output.text = %v", out["text"])
	}

	// The remote saw the planner's inputs as a data part.
	sends := s.calls(methodMessageSend)
	if len(sends) != 1 {
		t.Fatalf("message/send calls = %d, want 1", len(sends))
	}
	var params messageSendParams
	if err := json.Unmarshal(sends[0].Params, &params); err != nil {
		t.Fatalf("unmarshal send params: %v", err)
	}
	if params.Message.Parts[0].Data["symptom"] != "ImagePullBackOff" {
		t.Errorf("remote saw inputs %v", params.Message.Parts[0].Data)
	}
}

func TestAdapterUnknownAgentIsDispatchTimeError(t *testing.T) {
	adapter, err := NewAdapter(nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	ref, err := federation.ParseReference("a2a://nobody/skill")
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	// Frozen contract: unknown agent is a dispatch-time failure from
	// Invoke itself, not a Handle.Wait outcome.
	_, err = adapter.Invoke(context.Background(), ref, nil, federation.InvokeOptions{})
	if !errors.Is(err, federation.ErrUnknownAgent) {
		t.Fatalf("err = %v, want federation.ErrUnknownAgent", err)
	}
}

func TestAdapterExecutionErrorSurfacesFromWait(t *testing.T) {
	s := newStubA2AServer(t)
	s.handlers[methodMessageSend] = func(json.RawMessage) (any, *RPCError) {
		return Task{Kind: "task", ID: "t", ContextID: "c", Status: TaskStatus{State: TaskStateRejected}}, nil
	}
	adapter, err := NewAdapter([]AgentConfig{s.config("sample-external")})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	ref, _ := federation.ParseReference("a2a://sample-external/triage")
	h, err := adapter.Invoke(context.Background(), ref, nil, federation.InvokeOptions{})
	if err != nil {
		t.Fatalf("Invoke returned dispatch error for an execution failure: %v", err)
	}
	if _, err := h.Wait(context.Background()); !errors.Is(err, federation.ErrRemoteFailed) {
		t.Fatalf("Wait err = %v, want federation.ErrRemoteFailed", err)
	}
}

func TestAdapterDuplicateNameRejected(t *testing.T) {
	cfgs := []AgentConfig{
		{Name: "dup", Endpoint: "https://a.example/a2a", Filename: "a.yaml"},
		{Name: "dup", Endpoint: "https://b.example/a2a", Filename: "b.yaml"},
	}
	if _, err := NewAdapter(cfgs); err == nil {
		t.Fatal("NewAdapter accepted duplicate agent names")
	}
}
