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

package agent

import (
	"context"
	"reflect"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
)

// firstResponse drains the single-shot iterator the fakes return.
func firstResponse(t *testing.T, m model.LLM, req *model.LLMRequest) *model.LLMResponse {
	t.Helper()
	var got *model.LLMResponse
	for resp, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent error: %v", err)
		}
		got = resp
		break
	}
	if got == nil {
		t.Fatal("no response emitted")
	}
	return got
}

// firstFunctionCall returns the first FunctionCall part, or nil.
func firstFunctionCall(resp *model.LLMResponse) *genai.FunctionCall {
	if resp == nil || resp.Content == nil {
		return nil
	}
	for _, p := range resp.Content.Parts {
		if p != nil && p.FunctionCall != nil {
			return p.FunctionCall
		}
	}
	return nil
}

func userContent(text string) *genai.Content {
	return &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: text}}}
}

func toolSet(names ...string) map[string]any {
	m := make(map[string]any, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

// The coordinator turn (a sub-agent "task" tool offered, no finish_task)
// delegates by calling that tool once, threading the incident reason as
// the "request" arg.
func TestToolActor_CoordinatorDelegates(t *testing.T) {
	m := NewToolActorModel("test")
	req := &model.LLMRequest{
		Tools:    toolSet("uat-worker"),
		Contents: []*genai.Content{userContent(`{"reason":"ApplyChange"}`)},
	}
	fc := firstFunctionCall(firstResponse(t, m, req))
	if fc == nil || fc.Name != "uat-worker" {
		t.Fatalf("want delegation call to uat-worker, got %+v", fc)
	}
	if got := fc.Args["request"]; got != "ApplyChange" {
		t.Fatalf("want request arg %q, got %q", "ApplyChange", got)
	}
}

// Once the delegation tool has answered, the coordinator emits a final
// text (not another delegation) — the turn is done.
func TestToolActor_CoordinatorFinishesAfterWorker(t *testing.T) {
	m := NewToolActorModel("test")
	req := &model.LLMRequest{
		Tools: toolSet("uat-worker"),
		Contents: []*genai.Content{
			userContent(`{"reason":"ApplyChange"}`),
			{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "uat-worker"}}}},
		},
	}
	resp := firstResponse(t, m, req)
	if fc := firstFunctionCall(resp); fc != nil {
		t.Fatalf("want final text, got function call %q", fc.Name)
	}
	if resp.Content == nil || len(resp.Content.Parts) == 0 || resp.Content.Parts[0].Text == "" {
		t.Fatalf("want non-empty final text, got %+v", resp.Content)
	}
}

// The worker turn (finish_task offered) drives the reason-selected MCP
// tool. The reason arrives as plain delegation-request text, not the JSON
// envelope, so selection must match against that.
// The arguments are exactly what the fixture declares — no more (an
// undeclared property fails the MCP server's schema check) and no fewer
// (apply_change's replicas is required). apply_change's value is also
// what the write gate's edit legs measure against: the operator edits it
// to something else, and the fixture records which one ran.
func TestToolActor_WorkerDrivesSelectedTool(t *testing.T) {
	for _, tc := range []struct {
		name, request, wantTool string
		wantArgs                map[string]any
	}{
		{"apply", "ApplyChange", "apply_change", map[string]any{"replicas": 10}},
		{"read", "ReadStatus", "read_status", map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewToolActorModel("test")
			req := &model.LLMRequest{
				Tools:    toolSet("finish_task", "apply_change", "read_status", "transfer_to_agent"),
				Contents: []*genai.Content{userContent(tc.request)},
			}
			fc := firstFunctionCall(firstResponse(t, m, req))
			if fc == nil || fc.Name != tc.wantTool {
				t.Fatalf("want call to %q, got %+v", tc.wantTool, fc)
			}
			if !reflect.DeepEqual(fc.Args, tc.wantArgs) {
				t.Fatalf("args = %+v, want %+v", fc.Args, tc.wantArgs)
			}
			// The gate rewrites a parked call's arguments in place, so a
			// map shared with the package-level table would leak one
			// call's edit into the next.
			fc.Args["replicas"] = 99
			again := firstFunctionCall(firstResponse(t, m, req))
			if !reflect.DeepEqual(again.Args, tc.wantArgs) {
				t.Errorf("second call's args = %+v, want %+v — the fake hands out a shared map", again.Args, tc.wantArgs)
			}
		})
	}
}

// Once the selected tool has answered, the worker finishes rather than
// re-driving it — this is what lets a blocking tool be called exactly once.
func TestToolActor_WorkerFinishesAfterTool(t *testing.T) {
	m := NewToolActorModel("test")
	req := &model.LLMRequest{
		Tools: toolSet("finish_task", "apply_change", "read_status", "transfer_to_agent"),
		Contents: []*genai.Content{
			userContent("ApplyChange"),
			{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "apply_change"}}}},
			{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "apply_change"}}}},
		},
	}
	fc := firstFunctionCall(firstResponse(t, m, req))
	if fc == nil || fc.Name != "finish_task" {
		t.Fatalf("want finish_task after tool answered, got %+v", fc)
	}
}

// A worker turn whose reason selects no known tool finishes immediately
// rather than driving an arbitrary one.
func TestToolActor_WorkerFinishesWhenNoToolSelected(t *testing.T) {
	m := NewToolActorModel("test")
	req := &model.LLMRequest{
		Tools:    toolSet("finish_task", "apply_change", "read_status", "transfer_to_agent"),
		Contents: []*genai.Content{userContent("SomethingUnrelated")},
	}
	fc := firstFunctionCall(firstResponse(t, m, req))
	if fc == nil || fc.Name != "finish_task" {
		t.Fatalf("want finish_task when no tool selected, got %+v", fc)
	}
}
