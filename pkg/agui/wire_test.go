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
	"encoding/json"
	"testing"
)

// TestRunAgentInputRoundTrip pins that a RunAgentInput decodes from the AG-UI
// camelCase wire form and re-encodes to the same field names, including the
// raw-JSON passthrough fields (state/forwardedProps) and the parsed-but-unused
// Stage 1 fields (tools/resume).
func TestRunAgentInputRoundTrip(t *testing.T) {
	parent := "run-parent"
	in := RunAgentInput{
		ThreadID:    "thread-1",
		RunID:       "run-1",
		ParentRunID: &parent,
		State:       json.RawMessage(`{"count":1}`),
		Messages: []Message{
			{ID: "m1", Role: "user", Content: "hello"},
		},
		Tools: []Tool{
			{Name: "search", Description: "web search", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
		Context:        []Context{{Description: "tz", Value: "UTC"}},
		ForwardedProps: json.RawMessage(`{"ui":"chat"}`),
		Resume: []ResumeEntry{
			{InterruptID: "int-1", Status: ResumeStatusAccepted, Payload: json.RawMessage(`{"ok":true}`)},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got RunAgentInput
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ThreadID != in.ThreadID || got.RunID != in.RunID {
		t.Errorf("ids: got %q/%q, want %q/%q", got.ThreadID, got.RunID, in.ThreadID, in.RunID)
	}
	if got.ParentRunID == nil || *got.ParentRunID != parent {
		t.Errorf("parentRunId: got %v, want %q", got.ParentRunID, parent)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "hello" {
		t.Errorf("messages round-trip lost content: %+v", got.Messages)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "search" {
		t.Errorf("tools round-trip lost tool: %+v", got.Tools)
	}
	if len(got.Resume) != 1 || got.Resume[0].Status != ResumeStatusAccepted {
		t.Errorf("resume round-trip lost entry: %+v", got.Resume)
	}
	// Raw-JSON passthrough must survive verbatim (semantically).
	if string(got.State) != `{"count":1}` {
		t.Errorf("state: got %s, want {\"count\":1}", got.State)
	}
}

// TestRunAgentInputCamelCaseFields pins the exact wire field names a
// CopilotKit client sends — a Go-idiomatic rename (e.g. "threadID") would
// silently break interop, so assert the camelCase keys explicitly.
func TestRunAgentInputCamelCaseFields(t *testing.T) {
	raw := `{"threadId":"t","runId":"r","parentRunId":"p","state":{"a":1},"forwardedProps":{"b":2}}`
	var in RunAgentInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if in.ThreadID != "t" || in.RunID != "r" {
		t.Fatalf("camelCase decode failed: %+v", in)
	}
	if in.ParentRunID == nil || *in.ParentRunID != "p" {
		t.Fatalf("parentRunId decode failed: %+v", in.ParentRunID)
	}
	if string(in.State) != `{"a":1}` {
		t.Fatalf("state decode failed: %s", in.State)
	}
}

// TestEventTypeDiscriminators pins that every constructor stamps the correct
// "type" discriminant and that the type survives marshaling — a consumer
// dispatches solely on this field, so an empty or wrong type is a wire break.
func TestEventTypeDiscriminators(t *testing.T) {
	cases := []struct {
		name  string
		event any
		want  EventType
	}{
		{"run-started", NewRunStarted("t", "r"), EventRunStarted},
		{"run-finished", NewRunFinished("t", "r", json.RawMessage(`"done"`)), EventRunFinished},
		{"run-error", NewRunError("boom", RunErrorInternal), EventRunError},
		{"text-start", NewTextMessageStart("m1"), EventTextMessageStart},
		{"text-content", NewTextMessageContent("m1", "hi"), EventTextMessageContent},
		{"text-end", NewTextMessageEnd("m1"), EventTextMessageEnd},
		{"tool-start", NewToolCallStart("c1", "search", "m1"), EventToolCallStart},
		{"tool-args", NewToolCallArgs("c1", `{"q":"x"}`), EventToolCallArgs},
		{"tool-end", NewToolCallEnd("c1"), EventToolCallEnd},
		{"tool-result", NewToolCallResult("c1", "42"), EventToolCallResult},
		{"state-snapshot", NewStateSnapshot(json.RawMessage(`{}`)), EventStateSnapshot},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.event)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var probe struct {
				Type EventType `json:"type"`
			}
			if err := json.Unmarshal(b, &probe); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if probe.Type != c.want {
				t.Errorf("type = %q, want %q (payload: %s)", probe.Type, c.want, b)
			}
		})
	}
}

// TestTextMessageStartRole pins that the start frame declares the assistant
// role (the content/end frames deliberately omit it) — a consumer keys the
// rendered bubble off this.
func TestTextMessageStartRole(t *testing.T) {
	b, err := json.Marshal(NewTextMessageStart("m1"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got TextMessageStart
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Role != "assistant" {
		t.Errorf("role = %q, want assistant", got.Role)
	}
}

// TestRunFinishedOmitsEmptyResult pins that a resultless RunFinished does not
// serialize a null/empty "result" key (omitempty on a nil RawMessage), so a
// consumer sees the field's absence rather than a null it must special-case.
func TestRunFinishedOmitsEmptyResult(t *testing.T) {
	b, err := json.Marshal(NewRunFinished("t", "r", nil))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := m["result"]; ok {
		t.Errorf("empty result should be omitted, got %s", b)
	}
}

// TestBaseEventOmitsUnsetEnvelope pins that the optional envelope fields
// (timestamp, rawEvent) are omitted when unset — an absent timestamp is
// spec-legal and must not serialize as timestamp:0/rawEvent:null.
func TestBaseEventOmitsUnsetEnvelope(t *testing.T) {
	b, err := json.Marshal(NewRunStarted("t", "r"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := m["timestamp"]; ok {
		t.Errorf("unset timestamp should be omitted, got %s", b)
	}
	if _, ok := m["rawEvent"]; ok {
		t.Errorf("unset rawEvent should be omitted, got %s", b)
	}
}
