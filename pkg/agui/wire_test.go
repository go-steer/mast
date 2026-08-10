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
			{InterruptID: "int-1", Status: ResumeStatusResolved, Payload: json.RawMessage(`{"ok":true}`)},
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
	if len(got.Resume) != 1 || got.Resume[0].Status != ResumeStatusResolved {
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

// TestEventTypeStringValues pins the literal on-the-wire string for every
// EventType discriminant. TestEventTypeDiscriminators only proves each
// constructor stamps its own EventType constant — it would stay green if a
// const's VALUE were renamed (e.g. EventRunStarted = "run.started"), since it
// compares against the same constant. AG-UI fixes these SCREAMING_SNAKE_CASE
// strings and a consumer dispatches on the exact text, so assert the literals
// directly: a value drift is an interop break that must fail here.
func TestEventTypeStringValues(t *testing.T) {
	cases := []struct {
		got  EventType
		want string
	}{
		{EventRunStarted, "RUN_STARTED"},
		{EventRunFinished, "RUN_FINISHED"},
		{EventRunError, "RUN_ERROR"},
		{EventStepStarted, "STEP_STARTED"},
		{EventStepFinished, "STEP_FINISHED"},
		{EventTextMessageStart, "TEXT_MESSAGE_START"},
		{EventTextMessageContent, "TEXT_MESSAGE_CONTENT"},
		{EventTextMessageEnd, "TEXT_MESSAGE_END"},
		{EventToolCallStart, "TOOL_CALL_START"},
		{EventToolCallArgs, "TOOL_CALL_ARGS"},
		{EventToolCallEnd, "TOOL_CALL_END"},
		{EventToolCallResult, "TOOL_CALL_RESULT"},
		{EventStateSnapshot, "STATE_SNAPSHOT"},
		{EventStateDelta, "STATE_DELTA"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("EventType value = %q, want %q", c.got, c.want)
		}
	}
}

// TestRunErrorCodeStringValues pins the RUN_ERROR code vocabulary literals. A
// consumer branches its retry/resume UX on these exact strings, so a rename
// (even one that keeps the Go code compiling) is a wire break.
func TestRunErrorCodeStringValues(t *testing.T) {
	cases := []struct {
		got  RunErrorCode
		want string
	}{
		{RunErrorAborted, "aborted"},
		{RunErrorInternal, "internal"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("RunErrorCode value = %q, want %q", c.got, c.want)
		}
	}
}

// TestEventWireKeys pins the exact JSON key set every event constructor emits.
// A consumer reads these camelCase names verbatim, so a Go-idiomatic tag rename
// (messageId→message_id, threadId→threadID) would round-trip cleanly within Go
// yet silently break every non-Go client. Marshal each constructor and assert
// the full key set — both a missing expected key and an unexpected extra key
// fail — so any tag drift surfaces here rather than at a client.
func TestEventWireKeys(t *testing.T) {
	cases := []struct {
		name  string
		event any
		keys  []string
	}{
		{"run-started", NewRunStarted("t", "r"), []string{"type", "threadId", "runId"}},
		{"run-finished", NewRunFinished("t", "r", json.RawMessage(`"x"`)), []string{"type", "threadId", "runId", "result", "outcome"}},
		{"run-error", NewRunError("boom", RunErrorInternal), []string{"type", "message", "code"}},
		{"text-start", NewTextMessageStart("m1"), []string{"type", "messageId", "role"}},
		{"text-content", NewTextMessageContent("m1", "hi"), []string{"type", "messageId", "delta"}},
		{"text-end", NewTextMessageEnd("m1"), []string{"type", "messageId"}},
		{"tool-start", NewToolCallStart("c1", "search", "m1"), []string{"type", "toolCallId", "toolCallName", "parentMessageId"}},
		{"tool-args", NewToolCallArgs("c1", `{"q":"x"}`), []string{"type", "toolCallId", "delta"}},
		{"tool-end", NewToolCallEnd("c1"), []string{"type", "toolCallId"}},
		{"tool-result", NewToolCallResult("c1", "42"), []string{"type", "toolCallId", "content"}},
		{"state-snapshot", NewStateSnapshot(json.RawMessage(`{}`)), []string{"type", "snapshot"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.event)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			want := make(map[string]bool, len(c.keys))
			for _, k := range c.keys {
				want[k] = true
				if _, ok := m[k]; !ok {
					t.Errorf("%s: missing key %q (payload: %s)", c.name, k, b)
				}
			}
			for k := range m {
				if !want[k] {
					t.Errorf("%s: unexpected key %q (payload: %s)", c.name, k, b)
				}
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

// TestResumeStatusStringValues pins the ResumeStatus vocabulary. A client sets
// these exact strings on a ResumeEntry; a rename would silently change how the
// daemon synthesizes a cancelled answer, so assert the literals directly.
func TestResumeStatusStringValues(t *testing.T) {
	if string(ResumeStatusResolved) != "resolved" {
		t.Errorf("ResumeStatusResolved = %q, want resolved", ResumeStatusResolved)
	}
	if string(ResumeStatusCancelled) != "cancelled" {
		t.Errorf("ResumeStatusCancelled = %q, want cancelled", ResumeStatusCancelled)
	}
}

// TestNewRunFinishedSuccessOutcome pins that a completed run carries an explicit
// success outcome alongside its result — a consumer that dispatches on
// outcome.type must see "success", not a bare resultful RunFinished.
func TestNewRunFinishedSuccessOutcome(t *testing.T) {
	rf := NewRunFinished("t", "r", json.RawMessage(`"done"`))
	if rf.Outcome == nil || rf.Outcome.Type != RunOutcomeSuccess {
		t.Fatalf("outcome = %+v, want type success", rf.Outcome)
	}
	if len(rf.Outcome.Interrupts) != 0 {
		t.Errorf("success outcome carried interrupts: %+v", rf.Outcome.Interrupts)
	}
	if string(rf.Result) != `"done"` {
		t.Errorf("result = %s, want \"done\"", rf.Result)
	}
}

// TestNewRunFinishedInterruptOutcome pins the HITL terminal frame: outcome type
// interrupt, the open interrupts listed, and NO result (a paused run has produced
// no answer). The interrupt round-trips through JSON with its wire keys intact.
func TestNewRunFinishedInterruptOutcome(t *testing.T) {
	its := []Interrupt{{ID: "int-1", Message: "approve?", ResponseSchema: json.RawMessage(`{"type":"object"}`)}}
	rf := NewRunFinishedInterrupt("t", "r", its)
	if rf.Outcome == nil || rf.Outcome.Type != RunOutcomeInterrupt {
		t.Fatalf("outcome = %+v, want type interrupt", rf.Outcome)
	}
	if rf.Result != nil {
		t.Errorf("interrupt outcome carried a result: %s", rf.Result)
	}
	b, err := json.Marshal(rf)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got RunFinished
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Outcome == nil || len(got.Outcome.Interrupts) != 1 {
		t.Fatalf("round-trip lost interrupts: %s", b)
	}
	gi := got.Outcome.Interrupts[0]
	if gi.ID != "int-1" || gi.Message != "approve?" || string(gi.ResponseSchema) != `{"type":"object"}` {
		t.Fatalf("interrupt round-trip mangled fields: %+v", gi)
	}
	// A resultless interrupt frame must not serialize a "result" key.
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal map: %v", err)
	}
	if _, ok := m["result"]; ok {
		t.Errorf("interrupt frame serialized a result key: %s", b)
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
