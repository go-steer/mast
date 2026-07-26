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

// Wire types for the A2A v0.3 subset the synchronous client speaks:
// the agent card, JSON-RPC 2.0 envelopes, and the message/task objects
// exchanged by message/send, tasks/get, and tasks/cancel. Field names
// follow the spec's JSON (camelCase); the struct subset is deliberately
// minimal — unmodeled fields ride along in raw JSON where callers might
// need them (federation.Result.Raw).

import (
	"encoding/json"
	"fmt"
)

// WellKnownCardPath is the spec-defined agent-card discovery path.
const WellKnownCardPath = "/.well-known/agent-card.json"

// ProtocolVersion is the A2A spec line this client implements and
// advertises in the A2A-Version request header (docs/a2a-design.md
// resolved open question 1: pin to the v0.3 line, send the header,
// document tested-against versions per release).
const ProtocolVersion = "0.3"

// VersionHeader is the HTTP request header carrying ProtocolVersion.
const VersionHeader = "A2A-Version"

// AgentCard is the discovery-time contract served at
// /.well-known/agent-card.json. Note: spec AgentSkill has NO I/O
// schemas — only inputModes/outputModes media types. Structured I/O
// contracts are conveyed in skill descriptions or out of band
// (docs/a2a-design.md, protocol overview).
type AgentCard struct {
	Name                 string           `json:"name"`
	Description          string           `json:"description,omitempty"`
	URL                  string           `json:"url"`
	Version              string           `json:"version,omitempty"`
	ProtocolVersion      string           `json:"protocolVersion,omitempty"`
	PreferredTransport   string           `json:"preferredTransport,omitempty"`
	AdditionalInterfaces []AgentInterface `json:"additionalInterfaces,omitempty"`
	Capabilities         Capabilities     `json:"capabilities,omitempty"`
	DefaultInputModes    []string         `json:"defaultInputModes,omitempty"`
	DefaultOutputModes   []string         `json:"defaultOutputModes,omitempty"`
	Skills               []AgentSkill     `json:"skills,omitempty"`
}

// TransportJSONRPC is the card transport identifier for JSON-RPC 2.0
// over HTTP — the only transport this client speaks (docs/a2a-design.md
// endpoint layout: "JSON-RPC only at first"). An absent
// preferredTransport defaults to JSON-RPC per spec.
const TransportJSONRPC = "JSONRPC"

// AgentInterface is one (transport, url) alternative from the card.
type AgentInterface struct {
	Transport string `json:"transport"`
	URL       string `json:"url"`
}

// Capabilities is the card's capability declaration.
type Capabilities struct {
	Streaming         bool `json:"streaming,omitempty"`
	PushNotifications bool `json:"pushNotifications,omitempty"`
}

// AgentSkill is one named capability. Per the v0.3 spec it carries
// media types, not JSON Schemas.
type AgentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// Message is an A2A message (kind: "message").
type Message struct {
	Kind      string         `json:"kind"`
	MessageID string         `json:"messageId"`
	Role      string         `json:"role"`
	Parts     []Part         `json:"parts"`
	TaskID    string         `json:"taskId,omitempty"`
	ContextID string         `json:"contextId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Part is a message/artifact content part. Exactly one of Text / Data
// is populated for the kinds this client produces and consumes ("text",
// "data"); other kinds ("file") pass through with only Kind set.
type Part struct {
	Kind     string         `json:"kind"`
	Text     string         `json:"text,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Task is an A2A task (kind: "task").
type Task struct {
	Kind      string     `json:"kind"`
	ID        string     `json:"id"`
	ContextID string     `json:"contextId,omitempty"`
	Status    TaskStatus `json:"status"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

// TaskStatus is the task's current lifecycle position.
type TaskStatus struct {
	State   TaskState `json:"state"`
	Message *Message  `json:"message,omitempty"`
}

// Artifact is a task output artifact.
type Artifact struct {
	ArtifactID  string `json:"artifactId,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Parts       []Part `json:"parts,omitempty"`
}

// TaskState is the A2A v0.3 task lifecycle vocabulary.
type TaskState string

const (
	TaskStateSubmitted     TaskState = "submitted"
	TaskStateWorking       TaskState = "working"
	TaskStateInputRequired TaskState = "input-required"
	TaskStateAuthRequired  TaskState = "auth-required"
	TaskStateCompleted     TaskState = "completed"
	TaskStateFailed        TaskState = "failed"
	TaskStateCanceled      TaskState = "canceled"
	TaskStateRejected      TaskState = "rejected"
)

// Terminal reports whether the state ends the task lifecycle.
func (s TaskState) Terminal() bool {
	switch s {
	case TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected:
		return true
	}
	return false
}

// JSON-RPC 2.0 methods on the single A2A endpoint.
const (
	methodMessageSend = "message/send"
	methodTasksGet    = "tasks/get"
	methodTasksCancel = "tasks/cancel"
)

// rpcRequest is a JSON-RPC 2.0 request envelope.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response envelope.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object (A2A-specific codes included,
// e.g. -32001 TaskNotFound).
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("a2a: JSON-RPC error %d: %s", e.Code, e.Message)
}

// messageSendParams is the params object for message/send.
type messageSendParams struct {
	Message       *Message           `json:"message"`
	Configuration *messageSendConfig `json:"configuration,omitempty"`
}

// messageSendConfig is the client's send configuration. The v0.1
// client always requests blocking (it waits synchronously regardless;
// a server honoring the hint saves us the poll loop).
type messageSendConfig struct {
	Blocking            *bool    `json:"blocking,omitempty"`
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
	HistoryLength       *int     `json:"historyLength,omitempty"`
}

// taskQueryParams is the params object for tasks/get.
type taskQueryParams struct {
	ID            string `json:"id"`
	HistoryLength *int   `json:"historyLength,omitempty"`
}

// taskIDParams is the params object for tasks/cancel.
type taskIDParams struct {
	ID string `json:"id"`
}

// sendReply is the polymorphic result of message/send: a Message (kind
// "message", direct reply) or a Task (kind "task"). decodeSendReply
// dispatches on the "kind" discriminator.
type sendReply struct {
	Message *Message
	Task    *Task
	Raw     json.RawMessage
}

func decodeSendReply(raw json.RawMessage) (*sendReply, error) {
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("a2a: malformed message/send result: %w", err)
	}
	reply := &sendReply{Raw: raw}
	switch probe.Kind {
	case "message":
		reply.Message = &Message{}
		if err := json.Unmarshal(raw, reply.Message); err != nil {
			return nil, fmt.Errorf("a2a: malformed message reply: %w", err)
		}
	case "task":
		reply.Task = &Task{}
		if err := json.Unmarshal(raw, reply.Task); err != nil {
			return nil, fmt.Errorf("a2a: malformed task reply: %w", err)
		}
	default:
		return nil, fmt.Errorf("a2a: message/send result has unknown kind %q (want \"message\" or \"task\")", probe.Kind)
	}
	return reply, nil
}
