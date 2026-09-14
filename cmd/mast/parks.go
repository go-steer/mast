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

// The park read (#314): projecting a transcript.Detail onto the wire
// types in pkg/inject.
//
// This file is the only place both vocabularies are in scope, and that
// is deliberate. pkg/inject declares a JSON contract and must not
// import pkg/transcript or pkg/approval; pkg/transcript is frozen at
// v1.0 and must not learn a wire string. So the translation lives in
// package main, the same layering #313 used for turn_state — and the
// same consequence applies: every field a client can see is copied
// across by a line in this file, so a field added to Detail cannot
// reach the wire by accident.
//
// What it copies, and what it does not, is the point. See parks.go in
// pkg/inject for the contract; the omissions are enforced here.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/transcript"
)

// parksReader answers GET /parks and GET /parks/{session} out of the
// daemon's session store.
type parksReader struct {
	store  *transcript.Store
	logger *slog.Logger
}

// parksHandler returns the inject hook, or nil when there is no store
// to read — a daemon without one answers 404 rather than 200 with an
// empty list, because "no parks" and "I cannot see parks" are different
// answers and only one of them means it is safe to stop looking.
func parksHandler(store *transcript.Store, logger *slog.Logger) inject.ParksHandler {
	if store == nil {
		return nil
	}
	r := parksReader{store: store, logger: logger}
	return r.read
}

func (p parksReader) read(ctx context.Context, req inject.ParksRequest) (inject.ParksResult, error) {
	if req.SessionID != "" {
		d, err := p.store.Get(ctx, "", req.SessionID)
		if err != nil {
			if errors.Is(err, transcript.ErrNotFound) {
				return inject.ParksResult{}, fmt.Errorf("session %q: %w", req.SessionID, inject.ErrNotFound)
			}
			return inject.ParksResult{}, fmt.Errorf("read session %q: %w", req.SessionID, err)
		}
		// Named explicitly, so answered unconditionally: a client
		// polling "is my session still parked" needs 200 with an empty
		// list, not a 404 it has to tell apart from a typo.
		return inject.ParksResult{Sessions: []inject.SessionParks{projectParks(d)}}, nil
	}
	return p.list(ctx)
}

// list returns every parked session.
//
// Only parked ones: an idle session has nothing for an operator to
// answer, and a listing that included them would make the interesting
// rows the minority of the response. The set is inherently
// human-scaled — every row is a question somebody has to answer — which
// is what makes it safe to return each one in full rather than making
// clients round-trip per session.
func (p parksReader) list(ctx context.Context) (inject.ParksResult, error) {
	summaries, err := p.store.List(ctx, "")
	if err != nil {
		return inject.ParksResult{}, fmt.Errorf("list sessions: %w", err)
	}
	out := make([]inject.SessionParks, 0, len(summaries))
	for _, s := range summaries {
		if s.State != transcript.StatePaused {
			continue
		}
		d, err := p.store.Get(ctx, s.UserID, s.ID)
		if err != nil {
			// A session that finished and was reaped between the list
			// and the read is not an error; anything else is. Returning
			// a short list for a real store failure would look like
			// "nothing to approve", which is the one wrong answer this
			// route can give.
			if errors.Is(err, transcript.ErrNotFound) {
				p.logf("park read: session vanished during listing", "session", s.ID)
				continue
			}
			return inject.ParksResult{}, fmt.Errorf("read session %q: %w", s.ID, err)
		}
		out = append(out, projectParks(d))
	}
	// Oldest question first. List orders by last event, which puts the
	// busiest session at the top; an approval queue wants the one that
	// has been waiting longest, because that is the one whose answer is
	// most overdue.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := oldestPark(out[i]), oldestPark(out[j])
		if !a.Equal(b) {
			return a.Before(b)
		}
		return out[i].SessionID < out[j].SessionID
	})
	return inject.ParksResult{Sessions: out}, nil
}

func (p parksReader) logf(msg string, args ...any) {
	if p.logger == nil {
		return
	}
	p.logger.Warn(msg, args...)
}

// oldestPark is the sort key: when a session's park was raised. A
// session paused by a hold alone has none, and sorts by its hold
// instead so it still lands in time order rather than at the front.
func oldestPark(s inject.SessionParks) time.Time {
	var t time.Time
	for _, park := range s.Parks {
		if t.IsZero() || park.RaisedAt.Before(t) {
			t = park.RaisedAt
		}
	}
	if t.IsZero() && s.Hold != nil {
		t = s.Hold.MintedAt
	}
	return t
}

// projectParks copies a transcript detail onto the wire types, field by
// deliberate field.
func projectParks(d *transcript.Detail) inject.SessionParks {
	out := inject.SessionParks{
		SessionID: d.ID,
		State:     d.State,
		Awaiting:  parkKind(d.Awaiting()),
	}
	for _, pending := range d.Pending {
		out.Parks = append(out.Parks, projectPark(pending))
	}
	if g := d.GatePause; g.Active() {
		out.Hold = &inject.Hold{
			Reason:    string(g.Reason),
			Message:   g.Message,
			Token:     g.Token,
			MintedAt:  g.MintedAt,
			ExpiresAt: g.ExpiresAt,
			ResumeAt:  g.ResumeAt,
		}
	}
	for _, c := range d.Captures {
		out.Applied = append(out.Applied, projectCapture(c))
	}
	// Deliberately not copied, and each for its own reason:
	//
	//   - d.EventCount, and the transcript it counts: model output and
	//     tool results, which is exactly what this projection exists to
	//     leave behind.
	//   - d.AppliedEdits: an audit record about a call that already ran
	//     and cannot be answered. `mast sessions show` and the decision
	//     export are where "who rewrote this" is asked, and putting it
	//     in an approval queue invites a client to render a completed
	//     decision as a pending one.
	//   - d.AbortReason / d.InterruptReason: State says which of those
	//     happened, and neither is a park.
	return out
}

// projectPark copies one pending interrupt.
//
// The discriminator is the parked tool's name, the same one
// transcript.Detail.Awaiting and isConfirmationPark use: ADK spells a
// write-gate park as a long-running call to
// toolconfirmation.FunctionCallName.
func projectPark(p transcript.PendingInput) inject.Park {
	out := inject.Park{
		InterruptID: p.InterruptID,
		Kind:        inject.ParkKindInput,
		Question:    p.Message,
		Author:      p.Author,
		RaisedAt:    p.RaisedAt,
	}
	if p.ToolName != toolconfirmation.FunctionCallName {
		// A question the agent asked. p.Message is the whole of it, and
		// p.Payload is whatever the pausing node attached — arbitrary,
		// unreviewed, and not going on this wire.
		return out
	}
	out.Kind = inject.ParkKindApproval
	if p.Payload == nil {
		// A confirmation raised by something other than mast's write
		// gate — a tool calling RequestConfirmation itself. Still a
		// park: report it with the question the projection rendered and
		// let the operator answer it in the CLI. Dropping it would hide
		// a session that is waiting on somebody, which is the failure
		// #313 just fixed on the attach side.
		return out
	}
	req, err := approval.DecodeRequest(p.Payload)
	if err != nil || req.Tool == "" {
		// Same, one step later: a payload that is not the gate's.
		// DecodeRequest accepts anything JSON-shaped, so the empty Tool
		// is the real check — a ProposedCall naming no tool is a blank
		// in the shape of a call, and a client would render it as one.
		return out
	}
	out.Change = &inject.ProposedCall{
		Tool:          req.Tool,
		Args:          req.Args,
		Key:           req.Key,
		Policy:        req.Policy,
		Agent:         req.Agent,
		Stale:         req.Stale,
		VerdictFormat: req.Verdict,
	}
	if set := req.ChangeSet; set != nil {
		cs := &inject.ChangeSet{
			Specialist:    set.Specialist,
			Grantable:     set.Grantable,
			Ungrantable:   set.Ungrantable,
			TTLSeconds:    set.TTLSeconds,
			Preconditions: set.Preconditions,
		}
		for _, c := range set.Changes {
			cs.Changes = append(cs.Changes, inject.Call{Tool: c.Tool, Args: c.Arguments})
		}
		out.Change.ChangeSet = cs
	}
	return out
}

// projectCapture copies one prior-state capture (#296).
//
// CaptureRecord.Prior — the captured values themselves — is the one
// field deliberately dropped. It is unbounded cluster state read from a
// live object, and this response goes to chat relays. What travels is
// enough to act on it: the field names the capture narrowed to, the
// digest that tells two captures of the same object apart, the read
// that produced it so a client can take it again, and the revert call
// itself, whose arguments already carry the values that matter.
func projectCapture(c approval.CaptureRecord) inject.AppliedCall {
	out := inject.AppliedCall{
		CallID:      c.FunctionCallID,
		Tool:        c.Tool,
		Args:        c.Arguments,
		Key:         c.Key,
		CapturedAt:  c.CapturedAt,
		Read:        c.Read,
		Digest:      c.Digest,
		PriorFields: c.PriorFields,
	}
	if c.Revert != nil {
		out.Revert = &inject.Call{Tool: c.Revert.Tool, Args: c.Revert.Arguments}
	}
	return out
}

// parkKind maps the transcript's awaiting vocabulary onto the wire's.
//
// The two vocabularies spell the same two things identically today, and
// this switch is what keeps that a fact rather than a dependency: a
// rename on either side lands here as a compile error or a test
// failure, not as a silently changed JSON value in a client repo the
// compiler cannot see (#248). Anything unrecognised answers "".
func parkKind(awaiting string) string {
	switch awaiting {
	case transcript.AwaitingApproval:
		return inject.ParkKindApproval
	case transcript.AwaitingInput:
		return inject.ParkKindInput
	}
	return ""
}
