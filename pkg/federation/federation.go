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

// Package federation implements the v0.1 slice of
// docs/federation-design.md: the frozen Adapter interface, reference
// parsing for `<scheme>://<name>[/<skill>]` remote-agent references, a
// scheme-keyed adapter registry, and the planner-facing
// `invoke_remote_agent` tool.
//
// # The frozen Adapter shape, and why
//
// docs/fork-design.md P1.4 requires the interface be "frozen with an
// event/interrupt channel in mind — so v0.2's streaming/HITL
// propagation isn't a breaking change". The docs flagged signature
// churn as the risk, so the shape choice is load-bearing. Three
// candidate shapes were considered:
//
//  1. Invoke returns (*Result, error) directly. Rejected: it hard-wires
//     "the invocation is finished when Invoke returns". v0.2 streaming
//     must deliver events BEFORE the terminal result exists, so this
//     shape forces either a signature change or a semantics change
//     (Invoke returning a half-populated Result) — exactly the churn
//     we're freezing against.
//
//  2. Invoke takes a callback/event-sink field on InvokeOptions.
//     Rejected: adding the field later is non-breaking structurally,
//     but it inverts the data flow (push into caller-supplied sink vs.
//     pull from the invocation) and gives interrupts/HITL no natural
//     home — a remote `input-required` pause needs a bidirectional
//     handle, not a write-only sink.
//
//  3. Invoke returns a Handle (chosen). The Handle carries the whole
//     post-dispatch lifecycle: Wait for the terminal Result, Events for
//     intermediate updates, Cancel for remote cancellation. v0.1
//     adapters do all work synchronously inside Invoke (v0.1 blocks to
//     a bounded timeout, per docs/durable-execution-design.md phasing)
//     and return an already-resolved Handle; v0.2 adapters return a
//     live Handle whose Events stream and whose Wait blocks. Callers
//     written against Wait/Events today keep compiling AND keep their
//     semantics — only WHERE the waiting happens moves. Interrupt
//     propagation (v0.2 HITL) lands as a new Event type plus a response
//     method on a Handle extension interface, not as a signature edit.
//
// Contract split: Invoke returns a non-nil error only for dispatch-time
// failures (unresolvable reference, unknown agent, invalid options).
// Execution failures — transport errors, remote task failure, timeout —
// surface from Handle.Wait, so callers have exactly one place to handle
// them regardless of whether the adapter is synchronous or streaming.
//
// This shape freezes at v0.1. Additive evolution only: new fields on
// InvokeOptions/Result/Event structs, new Event types, extension
// interfaces on Handle.
package federation

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Sentinel errors, named per the docs/federation-design.md failure-mode
// table. Adapters wrap these so callers can errors.Is across protocols.
var (
	// ErrUnknownScheme — no adapter registered for the reference scheme.
	ErrUnknownScheme = errors.New("federation: unknown reference scheme")

	// ErrUnknownAgent — the scheme resolved but the named agent is not
	// configured (e.g. no matching .agents/a2a/*.yaml).
	ErrUnknownAgent = errors.New("federation: unknown remote agent")

	// ErrInvalidReference — the reference string does not parse against
	// the `<scheme>://<name>[/<skill>]` grammar.
	ErrInvalidReference = errors.New("federation: invalid reference")

	// ErrUnreachable — network partition, DNS failure, endpoint down.
	ErrUnreachable = errors.New("federation: remote agent unreachable")

	// ErrTimeout — the bounded v0.1 wait expired before the remote
	// reached a terminal state.
	ErrTimeout = errors.New("federation: remote invocation timed out")

	// ErrAuthFailed — the remote rejected our credentials (401/403) or
	// the configured credential source is unusable.
	ErrAuthFailed = errors.New("federation: remote authentication failed")

	// ErrProtocolMismatch — the remote does not speak a transport this
	// adapter supports (e.g. an agent card advertising only gRPC).
	ErrProtocolMismatch = errors.New("federation: protocol mismatch")

	// ErrRemoteFailed — the remote agent reached a non-success terminal
	// state (failed / rejected / canceled remotely).
	ErrRemoteFailed = errors.New("federation: remote invocation failed")
)

// Adapter is the protocol extension point from
// docs/federation-design.md. One adapter per reference scheme; the
// registry dispatches on Reference.Scheme. FROZEN at v0.1 — see the
// package documentation for the compatibility contract.
type Adapter interface {
	// Scheme returns the reference scheme this adapter serves ("a2a",
	// "mast", "http", ...). Must be non-empty, lowercase, and stable.
	Scheme() string

	// Invoke dispatches inputs to the remote agent identified by ref.
	// It returns an error only for dispatch-time failures (resolution,
	// validation); execution errors surface from Handle.Wait. v0.1
	// adapters MAY complete the entire remote interaction inside Invoke
	// (bounded by ctx and opts.Timeout) and return a resolved Handle.
	Invoke(ctx context.Context, ref Reference, inputs map[string]any, opts InvokeOptions) (Handle, error)
}

// InvokeOptions carries per-invocation knobs. A zero value is valid.
// Growth is additive-only (new fields, never changed ones).
type InvokeOptions struct {
	// Timeout bounds the whole invocation (dispatch through terminal
	// state). Zero means the adapter's / remote-agent config's default.
	Timeout time.Duration
}

// Handle is the post-dispatch lifecycle of one remote invocation.
type Handle interface {
	// Wait blocks until the invocation reaches a terminal state and
	// returns the Result, or the terminal error. Wait is idempotent:
	// subsequent calls return the same outcome. ctx bounds the wait
	// only; canceling it does not by itself cancel the remote task
	// (use Cancel).
	Wait(ctx context.Context) (*Result, error)

	// Events returns the intermediate-event stream. v0.1 adapters
	// return an already-closed channel (no events precede the terminal
	// state in synchronous mode); v0.2 streaming adapters deliver task
	// updates here. The channel is closed when the invocation reaches a
	// terminal state. Never returns nil.
	Events() <-chan Event

	// Cancel requests cancellation of the remote work. Idempotent;
	// best-effort. After a successful Cancel, Wait returns an error
	// wrapping ErrRemoteFailed (or the remote's terminal outcome if it
	// won the race).
	Cancel(ctx context.Context) error
}

// Event is one intermediate update from a remote invocation. v0.1
// defines the envelope only — no adapter produces events yet. v0.2
// (streaming, HITL propagation) adds Type values; consumers must ignore
// Types they do not recognize.
type Event struct {
	// Type discriminates the event ("status", "artifact",
	// "input-required", ...). Namespaced growth, additive-only.
	Type string

	// Data is the type-specific payload.
	Data map[string]any
}

// Result is the terminal outcome of a remote invocation, protocol
// agnostic. Growth is additive-only.
type Result struct {
	// State is the terminal state as reported by the remote, normalized
	// to the remote protocol's vocabulary (A2A: "completed"; a direct
	// message reply also reports "completed").
	State string

	// RemoteID identifies the remote unit of work when the protocol has
	// one (A2A task ID). Empty for direct request/response replies.
	RemoteID string

	// Text is the concatenated human-readable output (A2A text parts).
	Text string

	// Output is the structured output (A2A data parts, merged in
	// arrival order — later keys win).
	Output map[string]any

	// Raw is the protocol-level terminal payload, for debugging and for
	// callers that need fields Result does not model.
	Raw json.RawMessage
}

// resolvedHandle is the shared Handle implementation for synchronous
// (v0.1) adapters: the outcome is known before the Handle exists.
type resolvedHandle struct {
	res    *Result
	err    error
	events chan Event
}

// NewResolvedHandle wraps an already-terminal outcome in a Handle. It
// is the intended return path for v0.1 synchronous adapters (and for
// tests). Exactly one of res/err is meaningful; err wins when non-nil.
func NewResolvedHandle(res *Result, err error) Handle {
	h := &resolvedHandle{res: res, err: err, events: make(chan Event)}
	close(h.events) // terminal already: no intermediate events, ever
	return h
}

func (h *resolvedHandle) Wait(context.Context) (*Result, error) {
	if h.err != nil {
		return nil, h.err
	}
	return h.res, nil
}

func (h *resolvedHandle) Events() <-chan Event { return h.events }

// Cancel on a resolved handle is a no-op: the work is already terminal.
func (h *resolvedHandle) Cancel(context.Context) error { return nil }
