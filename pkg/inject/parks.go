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

// The park read surface (#314). Everything else on this server acts;
// this is the one route that answers.
//
// The write gate's claim is that an approval binds to a typed change
// set rather than to a verb. An out-of-process operator surface could
// already answer a park — POST /resume, its verdict vocabulary pinned
// as wire literals since #248 — and had no way to find out what it was
// answering. That reduces every chat or web approval to "approve the
// thing, whatever it is": uninformed consent with an audit trail, which
// is worse than the CLI it replaces because it looks like the opposite.
//
// The types below are a deliberately narrow projection, not
// transcript.Detail re-exported. What they carry is the park: the
// pending interrupt, the call it gates, the change set that call
// belongs to, and the reverts the gate captured for calls that already
// ran. What they deliberately do NOT carry:
//
//   - model output of any kind — reasoning, narration, the assistant
//     text around the call;
//   - tool results, including the result of the read that produced a
//     capture (only its digest and the declared field names travel;
//     the values are cluster state and can be anything);
//   - any event the write gate did not park on.
//
// Widening that is a decision someone makes against these declarations,
// not a diff that happens because Detail grew a field.

package inject

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Park kinds. These are the same two strings transcript.Detail.Awaiting
// reports, and they are duplicated rather than imported because
// pkg/inject declares a wire contract and pkg/transcript declares a
// projection: a Go rename in either one must not silently change the
// other's JSON. cmd/mast holds both and pins the correspondence.
const (
	// ParkKindApproval is a write-gate park: a mutating call waiting to
	// be approved, rejected or edited. Change is set.
	ParkKindApproval = "approval"
	// ParkKindInput is a question the agent asked —
	// request_operator_input, or an ADK RequestedInput. Change is nil.
	ParkKindInput = "input"
)

// ParksRequest asks for the parks on one session, or on every parked
// session when SessionID is empty.
type ParksRequest struct {
	SessionID string `json:"session_id,omitempty"`
}

// ParksResult is the body of GET /parks and GET /parks/{session}.
//
// The two routes differ in which sessions appear, not in what a session
// looks like:
//
//   - GET /parks lists every PARKED session, each in full. Sessions
//     with nothing to answer are omitted.
//   - GET /parks/{session} returns the named session whether it is
//     parked or not, with an empty park list when it is not.
//
// Always an object with a `sessions` array, including for the
// single-session route and including when nothing is parked. A bare
// array would make adding a field later a breaking change, and a 404
// for "this session exists and is not parked" would be a lie a poller
// has to special-case.
type ParksResult struct {
	Sessions []SessionParks `json:"sessions"`
}

// SessionParks is one session's parks.
type SessionParks struct {
	SessionID string `json:"session_id"`
	// State is the session's projected state (paused, running, idle,
	// aborted, interrupted). A parked session is paused, but a paused
	// session is not necessarily parked — see Hold.
	State string `json:"state"`
	// Awaiting is what the session is waiting on overall:
	// ParkKindApproval when any park is an approval, ParkKindInput when
	// the open parks are only questions, empty when nothing is pending.
	// An approval outranks a question, the same ranking the attach
	// turn_state uses (#313).
	Awaiting string `json:"awaiting,omitempty"`
	// Parks are the open parks, oldest first.
	Parks []Park `json:"parks,omitempty"`
	// Hold is an operator-placed gate pause, if one is active. It is
	// reported beside the parks and never as one: nothing resolves by
	// answering a hold, so a client that renders it as a question puts
	// a prompt in front of somebody with nothing to say back.
	Hold *Hold `json:"hold,omitempty"`
	// Applied are calls this session already ran that the gate captured
	// a revert for, oldest first (#296). They are here because the
	// obvious operator affordance beside "approve this change" is "put
	// the last one back", and nothing outside the process could read
	// one.
	Applied []AppliedCall `json:"applied,omitempty"`
}

// Park is one open interrupt.
type Park struct {
	// InterruptID is the resume correlation key: POST /resume with this
	// and the session ID, or with a pause token that names it.
	InterruptID string `json:"interrupt_id"`
	// Kind is ParkKindApproval or ParkKindInput.
	Kind string `json:"kind"`
	// Question is the operator-facing one-liner the pausing node wrote.
	Question string `json:"question,omitempty"`
	// Author is the agent that raised it.
	Author string `json:"author,omitempty"`
	// RaisedAt is when the pausing event was recorded.
	RaisedAt time.Time `json:"raised_at"`
	// Change is the call this park gates. Set for ParkKindApproval and
	// nil otherwise — a question is not a change.
	Change *ProposedCall `json:"change,omitempty"`
}

// ProposedCall is a tool call waiting on a verdict.
type ProposedCall struct {
	// CallID is the function-call ID. It is the join key: every other
	// durable record of the same call — the decision, the applied edit,
	// the capture — is keyed by it.
	CallID string         `json:"call_id,omitempty"`
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args,omitempty"`
	// Key is the rendered one-line call the deny policy matches against
	// and the audit record stores.
	Key string `json:"key,omitempty"`
	// Policy is the gate posture that parked it.
	Policy string `json:"policy,omitempty"`
	// Agent is the specialist that proposed it.
	Agent string `json:"agent,omitempty"`
	// Stale, when set, is why an approval the operator already gave no
	// longer covers this call. Its presence is the difference between
	// "please approve this" and "you approved this and the ground
	// moved".
	Stale string `json:"stale,omitempty"`
	// VerdictFormat is the gate's own description of the answer it
	// accepts, carried so a client answering over raw HTTP can read the
	// shape of the reply in the question.
	VerdictFormat map[string]any `json:"verdict_format,omitempty"`
	// ChangeSet is the approved-as-a-unit set this call belongs to,
	// when it belongs to one. Present means `scope: change_set` is on
	// the table, and an operator can only make that trade if they are
	// shown what the rest of the set is.
	ChangeSet *ChangeSet `json:"change_set,omitempty"`
}

// ChangeSet is the set a parked call belongs to.
type ChangeSet struct {
	Specialist string `json:"specialist,omitempty"`
	Changes    []Call `json:"changes,omitempty"`
	// Grantable reports whether `scope: change_set` is admissible;
	// Ungrantable says why it is not.
	Grantable   bool   `json:"grantable"`
	Ungrantable string `json:"ungrantable,omitempty"`
	// TTLSeconds is how long approving the set authorizes its remaining
	// calls for.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// Preconditions names, per tool, the read each granted call is
	// re-checked against before it fires — and says where there is
	// none, because "approved for ten minutes" and "approved while the
	// Deployment still has 3 replicas" are different promises.
	Preconditions map[string]string `json:"preconditions,omitempty"`
}

// Call is a tool call named without a verdict attached to it — a member
// of a change set, or a revert.
type Call struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
}

// Hold is an active operator-placed gate pause.
type Hold struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Token is the pause handle a resume may be keyed by.
	Token     string    `json:"token,omitempty"`
	MintedAt  time.Time `json:"minted_at,omitzero"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// ResumeAt is set for a timed hold that releases itself.
	ResumeAt time.Time `json:"resume_at,omitzero"`
}

// AppliedCall is a mutating call that already ran and that the gate
// captured prior state for.
type AppliedCall struct {
	CallID string `json:"call_id,omitempty"`
	// Tool and Args are the call AS IT RAN. On an edited approval that
	// is the operator's arguments, not the model's.
	Tool       string         `json:"tool"`
	Args       map[string]any `json:"args,omitempty"`
	Key        string         `json:"key,omitempty"`
	CapturedAt time.Time      `json:"captured_at,omitzero"`
	// Read is the read that produced the capture, so a client can take
	// it again and compare. Digest hashes its whole result.
	Read   string `json:"read,omitempty"`
	Digest string `json:"digest,omitempty"`
	// PriorFields are the declared paths the capture narrowed to. The
	// VALUES are deliberately not here: they are cluster state, they
	// can be arbitrarily large, and this projection is read by relays.
	// Take the read again if you need them.
	PriorFields []string `json:"prior_fields,omitempty"`
	// Revert is the call that restores the captured state, already
	// checked against the revert tool's declared input schema. Nil when
	// the workload declared no inverse. mast never fires it: capture,
	// not restore.
	Revert *Call `json:"revert,omitempty"`
}

// ParksHandler answers a park read. Optional; when nil the /parks
// routes respond 404.
//
// It is a handler rather than a store because that is how every other
// route on this server is fed: pkg/inject owns the wire shape and
// cmd/mast owns where the answer comes from. Keeping it that way is
// what lets these declarations stay free of pkg/transcript and
// pkg/approval, which is the difference between a wire contract and a
// second Go spelling of an internal projection.
type ParksHandler func(ctx context.Context, req ParksRequest) (ParksResult, error)

// handleParks serves GET /parks and GET /parks/{session}.
//
// Authenticated exactly like /resume: a caller who may answer a park
// may read it, and a caller who may not read it has no business
// answering one either. The shared token is admitted, and so is a user
// from the table; nothing else is.
func (s *Server) handleParks(w http.ResponseWriter, r *http.Request) {
	if !s.attributedAuthOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.cfg.ParksHandler == nil {
		http.Error(w, "park reads not enabled: this daemon has no session store", http.StatusNotFound)
		return
	}
	req := ParksRequest{SessionID: r.PathValue("session")}

	out, err := s.cfg.ParksHandler(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		case errors.Is(err, ErrBadPayload):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case errors.Is(err, ErrUnavailable):
			w.Header().Set("Retry-After", "10")
			http.Error(w, "shutting down; retry against the replacement instance", http.StatusServiceUnavailable)
			return
		}
		s.logger.Error("park read failed", "session", req.SessionID, "error", err.Error())
		http.Error(w, "park read failed", http.StatusInternalServerError)
		return
	}
	if out.Sessions == nil {
		out.Sessions = []SessionParks{}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		// The status line is already out; all that is left is to say so
		// in the log rather than pretend the body was complete.
		s.logger.Error("park read: encoding response", "session", req.SessionID, "error", err.Error())
	}
}

// ErrNotFound, returned (or wrapped) by a ParksHandler, reports that
// the named session does not exist. Mapped to 404.
//
// A session that exists and is not parked is NOT this error: it answers
// 200 with an empty parks list, because "nothing to approve" is an
// answer and "no such session" is a different one, and a client that
// polls cannot tell them apart from a status code alone.
var ErrNotFound = errors.New("session not found")
