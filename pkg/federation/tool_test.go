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

package federation

import (
	"context"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
)

// toolRunner is the runtime surface of an ADK function tool. The
// concrete functiontool type is unexported; tests reach Run through
// this assertion, same as the ADK runner does.
type toolRunner interface {
	Run(ctx adkagent.Context, args any) (map[string]any, error)
}

// stubToolContext adapts a plain context.Context into an adkagent.Context
// for driving tools in tests, via the ADK's ContextMock embedding hook.
type stubToolContext struct {
	adkagent.ContextMock
	ctx context.Context
}

func newStubToolContext(ctx context.Context) *stubToolContext {
	return &stubToolContext{ctx: ctx}
}

func (s *stubToolContext) Deadline() (time.Time, bool) { return s.ctx.Deadline() }
func (s *stubToolContext) Done() <-chan struct{}       { return s.ctx.Done() }
func (s *stubToolContext) Err() error                  { return s.ctx.Err() }
func (s *stubToolContext) Value(key any) any           { return s.ctx.Value(key) }

func TestInvokeRemoteAgentTool(t *testing.T) {
	fa := &fakeAdapter{scheme: "a2a", res: &Result{
		State:    "completed",
		RemoteID: "task-1",
		Text:     "done",
		Output:   map[string]any{"root_cause": "oom"},
	}}
	tl, err := NewInvokeRemoteAgentTool(NewRegistry(fa))
	if err != nil {
		t.Fatalf("NewInvokeRemoteAgentTool: %v", err)
	}
	if tl.Name() != "invoke_remote_agent" {
		t.Errorf("Name() = %q, want invoke_remote_agent", tl.Name())
	}

	runner, ok := tl.(toolRunner)
	if !ok {
		t.Fatalf("tool %T does not expose Run", tl)
	}
	out, err := runner.Run(newStubToolContext(context.Background()), map[string]any{
		"reference": "a2a://sample-external/triage",
		"inputs":    map[string]any{"pod": "default/web-1"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out["state"] != "completed" || out["task_id"] != "task-1" || out["text"] != "done" {
		t.Errorf("tool output = %v", out)
	}
	if outMap, ok := out["output"].(map[string]any); !ok || outMap["root_cause"] != "oom" {
		t.Errorf("tool output.output = %v", out["output"])
	}
	if fa.lastRef.Skill != "triage" {
		t.Errorf("adapter saw skill %q, want triage", fa.lastRef.Skill)
	}
	if fa.lastIn["pod"] != "default/web-1" {
		t.Errorf("adapter saw inputs %v", fa.lastIn)
	}
}

func TestInvokeRemoteAgentToolUnknownScheme(t *testing.T) {
	tl, err := NewInvokeRemoteAgentTool(NewRegistry(&fakeAdapter{scheme: "a2a"}))
	if err != nil {
		t.Fatalf("NewInvokeRemoteAgentTool: %v", err)
	}
	_, err = tl.(toolRunner).Run(newStubToolContext(context.Background()), map[string]any{
		"reference": "grpc://svc/method",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown reference scheme") {
		t.Fatalf("err = %v, want unknown-scheme error surfaced to the planner", err)
	}
}
