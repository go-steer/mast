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

// Package agui is mast's AG-UI server surface — the agent→user interop
// protocol (CopilotKit et al.): an HTTP POST of RunAgentInput that opens an
// SSE text/event-stream of typed run events, so a browser/app UI can render a
// mast workload's turn live. Like pkg/a2a it is runtime-free: the daemon
// supplies a Backend that drives each turn through the runTurnPre chokepoint,
// and this package only speaks the wire.
//
// Build-vs-buy: the wire vocabulary below is hand-rolled — the AG-UI Go SDK is
// deliberately NOT a dependency. This mirrors the A2A decision (#78) and keeps
// zero new external deps so the check-slim-deps CI gate stays green; it
// overrides docs/ag-ui-design.md's July "wrap the community Go SDK" guidance
// (recorded there). Only the subset mast actually emits/consumes is modeled;
// fields the server neither produces nor reads are omitted, and unmodeled
// input fields ride along in json.RawMessage where a backend might want them.

package agui

import "encoding/json"

// ProtocolVersion is the AG-UI protocol line this server implements. It is
// advertised in the discovery descriptor and pinned per release
// (docs/ag-ui-design.md).
const ProtocolVersion = "0.1"

// DiscoveryPath is the unauthenticated discovery endpoint listing the AG-UI
// workloads this daemon exposes (docs/ag-ui-design.md open question 8).
const DiscoveryPath = "/agui/agents.json"

// RunAgentInput is the request body a client POSTs to a workload's AG-UI
// endpoint to drive one turn. Field names follow the AG-UI spec JSON
// (camelCase). State/ForwardedProps ride along as raw JSON — the server echoes
// State back as the opening StateSnapshot and does not otherwise interpret it
// in Stage 1. Tools and Resume are parsed but unused in Stage 1 (client-tool
// acceptance and the HITL interrupt/resume lifecycle are follow-on stages);
// they are modeled so the vocabulary is complete and forward-compatible.
type RunAgentInput struct {
	ThreadID       string          `json:"threadId"`
	RunID          string          `json:"runId"`
	ParentRunID    *string         `json:"parentRunId,omitempty"`
	State          json.RawMessage `json:"state,omitempty"`
	Messages       []Message       `json:"messages,omitempty"`
	Tools          []Tool          `json:"tools,omitempty"`
	Context        []Context       `json:"context,omitempty"`
	ForwardedProps json.RawMessage `json:"forwardedProps,omitempty"`
	Resume         []ResumeEntry   `json:"resume,omitempty"`
}

// Message is one conversational message in a RunAgentInput. Role is the AG-UI
// role vocabulary ("user", "assistant", "system", "tool"); Content is the
// text body. Unmodeled per-role fields (tool calls on assistant messages,
// toolCallId on tool messages) are not consumed by the Stage 1 server.
type Message struct {
	ID      string `json:"id,omitempty"`
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	Name    string `json:"name,omitempty"`
}

// Tool is a client-declared tool offered to the agent for this run. Parsed but
// ignored in Stage 1 (accepting client tools is a follow-on); modeled so the
// input contract is complete.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Context is one supplementary context entry a client attaches to a run
// (docs/ag-ui-design.md RunAgentInput). Carried through opaquely in Stage 1.
type Context struct {
	Description string `json:"description,omitempty"`
	Value       string `json:"value,omitempty"`
}

// ResumeStatus is the disposition a client reports when resuming an
// interrupted run (docs/ag-ui-design.md Resume). Parsed but unused in Stage 1.
type ResumeStatus string

const (
	ResumeStatusAccepted ResumeStatus = "accepted"
	ResumeStatusRejected ResumeStatus = "rejected"
)

// ResumeEntry answers one prior interrupt when a client resumes a run. Parsed
// but unused in Stage 1 (the interrupt/resume lifecycle is deferred).
type ResumeEntry struct {
	InterruptID string          `json:"interruptId"`
	Status      ResumeStatus    `json:"status"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

// EventType is the discriminator on every AG-UI SSE event's "type" field.
type EventType string

// The AG-UI event vocabulary this server emits (docs/ag-ui-design.md emission
// map). Stage 1 ships the lifecycle, text-message triad, tool-call quartet,
// and state families; activity/reasoning/raw/custom families are deferred.
const (
	// Lifecycle.
	EventRunStarted   EventType = "RUN_STARTED"
	EventRunFinished  EventType = "RUN_FINISHED"
	EventRunError     EventType = "RUN_ERROR"
	EventStepStarted  EventType = "STEP_STARTED"
	EventStepFinished EventType = "STEP_FINISHED"

	// Assistant text (streamed as start → one-or-more content deltas → end).
	EventTextMessageStart   EventType = "TEXT_MESSAGE_START"
	EventTextMessageContent EventType = "TEXT_MESSAGE_CONTENT"
	EventTextMessageEnd     EventType = "TEXT_MESSAGE_END"

	// Tool calls (start → args deltas → end, then a result once available).
	EventToolCallStart  EventType = "TOOL_CALL_START"
	EventToolCallArgs   EventType = "TOOL_CALL_ARGS"
	EventToolCallEnd    EventType = "TOOL_CALL_END"
	EventToolCallResult EventType = "TOOL_CALL_RESULT"

	// Shared state.
	EventStateSnapshot EventType = "STATE_SNAPSHOT"
	EventStateDelta    EventType = "STATE_DELTA"
)

// RunErrorCode is the machine-readable code on a RUN_ERROR event. The
// vocabulary is deliberately small: aborted (operator/client cancellation),
// interrupt (the run paused for human input — an honest Stage 1 placeholder
// until the full HITL lifecycle lands), and internal (any server-side fault,
// with no detail leaked to the client).
type RunErrorCode string

const (
	RunErrorAborted   RunErrorCode = "aborted"
	RunErrorInterrupt RunErrorCode = "interrupt"
	RunErrorInternal  RunErrorCode = "internal"
)

// baseEvent is embedded in every AG-UI event: the "type" discriminator plus
// the optional envelope fields the spec allows on every frame. Timestamp is
// epoch milliseconds (a pointer so an unset timestamp is omitted rather than
// serialized as 0); RawEvent carries an untranslated upstream event when a
// producer has one (unused by this server, modeled for completeness).
type baseEvent struct {
	Type      EventType       `json:"type"`
	Timestamp *int64          `json:"timestamp,omitempty"`
	RawEvent  json.RawMessage `json:"rawEvent,omitempty"`
}

// RunStarted opens a run's event stream (docs/ag-ui-design.md: session start).
type RunStarted struct {
	baseEvent
	ThreadID string `json:"threadId"`
	RunID    string `json:"runId"`
}

// RunFinished is the terminal event for a run that completed. Result carries
// the workload's final answer (also present as the closing TextMessage triad;
// the duplication is inherent under message-granular streaming).
type RunFinished struct {
	baseEvent
	ThreadID string          `json:"threadId"`
	RunID    string          `json:"runId"`
	Result   json.RawMessage `json:"result,omitempty"`
}

// RunError is the terminal event for a run that did not complete normally:
// aborted, interrupted (paused for human input), or an internal fault. Message
// is a short human-readable summary; Code is the machine-readable disposition.
type RunError struct {
	baseEvent
	Message string       `json:"message"`
	Code    RunErrorCode `json:"code,omitempty"`
}

// StepStarted / StepFinished bracket a named step within a run (reserved for
// multi-step workloads; modeled so consumers can rely on the vocabulary).
type StepStarted struct {
	baseEvent
	StepName string `json:"stepName"`
}

type StepFinished struct {
	baseEvent
	StepName string `json:"stepName"`
}

// TextMessageStart / TextMessageContent / TextMessageEnd stream one assistant
// message. mast runs StreamingModeNone (message-granular), so a message is
// emitted as start + a single content frame carrying the whole text + end;
// the triad shape keeps token-level streaming a forward-compatible upgrade.
// Role is present only on the start frame ("assistant").
type TextMessageStart struct {
	baseEvent
	MessageID string `json:"messageId"`
	Role      string `json:"role"`
}

type TextMessageContent struct {
	baseEvent
	MessageID string `json:"messageId"`
	Delta     string `json:"delta"`
}

type TextMessageEnd struct {
	baseEvent
	MessageID string `json:"messageId"`
}

// ToolCallStart / ToolCallArgs / ToolCallEnd stream one tool invocation, and
// ToolCallResult reports its outcome once the runtime has run it. Args carries
// the JSON-encoded call arguments; Content carries the JSON-encoded response.
// ParentMessageID links the call to the assistant message that issued it.
type ToolCallStart struct {
	baseEvent
	ToolCallID      string `json:"toolCallId"`
	ToolCallName    string `json:"toolCallName"`
	ParentMessageID string `json:"parentMessageId,omitempty"`
}

type ToolCallArgs struct {
	baseEvent
	ToolCallID string `json:"toolCallId"`
	Delta      string `json:"delta"`
}

type ToolCallEnd struct {
	baseEvent
	ToolCallID string `json:"toolCallId"`
}

type ToolCallResult struct {
	baseEvent
	ToolCallID string `json:"toolCallId"`
	MessageID  string `json:"messageId,omitempty"`
	Content    string `json:"content"`
}

// StateSnapshot carries the full shared-state document; StateDelta carries an
// RFC-6902 JSON Patch against the last snapshot. Stage 1 emits one opening
// StateSnapshot (echoing the input State); per-key StateDelta emission is
// deferred (it needs a state-projection allowlist), but the type ships so the
// state vocabulary is complete.
type StateSnapshot struct {
	baseEvent
	Snapshot json.RawMessage `json:"snapshot"`
}

type StateDelta struct {
	baseEvent
	Delta json.RawMessage `json:"delta"`
}

// newBase stamps a base envelope for the given event type. Timestamp is left
// unset: the server does not have a monotonic wall clock seam wired in Stage 1
// and an absent timestamp is spec-legal; consumers order by receipt.
func newBase(t EventType) baseEvent {
	return baseEvent{Type: t}
}

// The constructors below build fully-formed events with the correct "type"
// discriminant already set, so callers cannot emit a frame with a mismatched
// or empty type. Each returns a concrete typed value; the server marshals it
// to a `data: <json>\n\n` SSE frame.

func NewRunStarted(threadID, runID string) RunStarted {
	return RunStarted{baseEvent: newBase(EventRunStarted), ThreadID: threadID, RunID: runID}
}

func NewRunFinished(threadID, runID string, result json.RawMessage) RunFinished {
	return RunFinished{baseEvent: newBase(EventRunFinished), ThreadID: threadID, RunID: runID, Result: result}
}

func NewRunError(msg string, code RunErrorCode) RunError {
	return RunError{baseEvent: newBase(EventRunError), Message: msg, Code: code}
}

func NewTextMessageStart(messageID string) TextMessageStart {
	return TextMessageStart{baseEvent: newBase(EventTextMessageStart), MessageID: messageID, Role: "assistant"}
}

func NewTextMessageContent(messageID, delta string) TextMessageContent {
	return TextMessageContent{baseEvent: newBase(EventTextMessageContent), MessageID: messageID, Delta: delta}
}

func NewTextMessageEnd(messageID string) TextMessageEnd {
	return TextMessageEnd{baseEvent: newBase(EventTextMessageEnd), MessageID: messageID}
}

func NewToolCallStart(toolCallID, name, parentMessageID string) ToolCallStart {
	return ToolCallStart{baseEvent: newBase(EventToolCallStart), ToolCallID: toolCallID, ToolCallName: name, ParentMessageID: parentMessageID}
}

func NewToolCallArgs(toolCallID, delta string) ToolCallArgs {
	return ToolCallArgs{baseEvent: newBase(EventToolCallArgs), ToolCallID: toolCallID, Delta: delta}
}

func NewToolCallEnd(toolCallID string) ToolCallEnd {
	return ToolCallEnd{baseEvent: newBase(EventToolCallEnd), ToolCallID: toolCallID}
}

func NewToolCallResult(toolCallID, content string) ToolCallResult {
	return ToolCallResult{baseEvent: newBase(EventToolCallResult), ToolCallID: toolCallID, Content: content}
}

func NewStateSnapshot(snapshot json.RawMessage) StateSnapshot {
	return StateSnapshot{baseEvent: newBase(EventStateSnapshot), Snapshot: snapshot}
}
