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

package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/auth"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/permissions"
)

// The end-to-end claim #364 makes: a question raised by the write gate
// is readable and answerable at the address an attach client dials,
// with no second copy of the approval logic behind it.
//
// The store here is the real one, seeded through the same helpers
// parks_test.go uses, because what this file has to get right is a
// mapping between two vocabularies and a stand-in store would only
// assert the mapping against itself.

// resumeSpy records what RespondPrompt sent down the resume path.
type resumeSpy struct {
	mu   sync.Mutex
	got  []inject.ResumeRequest
	fail error
}

func (r *resumeSpy) resume(_ context.Context, req inject.ResumeRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.got = append(r.got, req)
	return nil
}

func (r *resumeSpy) calls() []inject.ResumeRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]inject.ResumeRequest(nil), r.got...)
}

// permsFixture wires a source over a store holding the given events.
func permsFixture(t *testing.T, sid string, events ...*adksession.Event) (*parkPerms, *resumeSpy) {
	t.Helper()
	store, svc := parkStore(t)
	seedParkSession(t, svc, sid, events...)
	spy := &resumeSpy{}
	src := newParkPerms(store, sid, spy.resume, nil)
	if src == nil {
		t.Fatal("newParkPerms returned nil for a wired daemon")
	}
	p, ok := src.(*parkPerms)
	if !ok {
		t.Fatalf("newParkPerms returned %T", src)
	}
	p.poll = 10 * time.Millisecond
	return p, spy
}

// TestNewParkPermsNilWhenItCannotServe pins the honesty half: a daemon
// missing either half of the mechanism reports no capability rather
// than a route that fails on use, which is the defect #364 was filed
// about.
func TestNewParkPermsNilWhenItCannotServe(t *testing.T) {
	t.Parallel()
	store, _ := parkStore(t)
	spy := &resumeSpy{}

	if got := newParkPerms(nil, "s1", spy.resume, nil); got != nil {
		t.Errorf("no session store: newParkPerms = %#v, want nil (it cannot see parks)", got)
	}
	if got := newParkPerms(store, "s1", nil, nil); got != nil {
		t.Errorf("no resume path: newParkPerms = %#v, want nil (it cannot resolve parks)", got)
	}
	if got := newParkPerms(store, "s1", spy.resume, nil); got == nil {
		t.Error("store and resume both wired: newParkPerms = nil, want a source")
	}
}

// TestPermFrameCarriesTheChangeAndOnlyApprovals pins both halves of the
// projection: what an approval frame says, and what never becomes one.
func TestPermFrameCarriesTheChangeAndOnlyApprovals(t *testing.T) {
	t.Parallel()
	store, svc := parkStore(t)
	seedParkSession(t, svc, "s-frames",
		gatePark("call-gate", scaleRequest()),
		questionPark("call-ask", "which namespace?"),
	)
	d, err := store.Get(context.Background(), "", "s-frames")
	if err != nil {
		t.Fatalf("read session: %v", err)
	}

	frames := map[string]attach.PromptFrame{}
	var kinds []string
	for _, park := range projectParks(d).Parks {
		f, ok := permFrame(park)
		if !ok {
			continue
		}
		kinds = append(kinds, park.Kind)
		frames[f.ID] = f
	}

	// An input park is a question the agent asked in words, and the only
	// answer this wire carries is a permissions decision. Publishing one
	// would put a prompt in front of an operator whose buttons cannot
	// answer it.
	if _, present := frames["call-ask"]; present {
		t.Error("an operator-input park reached the perms stream; it has no answerable decision")
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %v (kinds %v), want only the approval park", frames, kinds)
	}

	got := frames["call-gate"]
	// The literal, not the constant (#248): Kind is what switchboard's
	// client reads to decide which buttons to offer, and its rule for
	// this value is the only one that matches mast's write gate — deny
	// and allow-once, nothing wider. A kind it does not recognise gets
	// the full six, four of which this daemon answers 400. A Go rename
	// would keep a constant-to-constant assertion green while breaking
	// a client in a repo the compiler cannot see.
	if got.Kind != "control_plane_write" {
		t.Errorf("Kind = %q, want control_plane_write — the only kind whose client-side button set is deny + allow-once", got.Kind)
	}
	if got.ToolName != "scale_deployment" {
		t.Errorf("ToolName = %q, want the parked tool", got.ToolName)
	}
	// The rendered call, not the generic hint: it is what the policy
	// matched and what the audit row will say, so it is the text the
	// operator should be approving.
	if got.Detail != scaleRequest().Key {
		t.Errorf("Detail = %q, want the rendered call %q", got.Detail, scaleRequest().Key)
	}
	// The proposing specialist, not the event author, when the gate
	// recorded one.
	if got.Source != "remediator" {
		t.Errorf("Source = %q, want the proposing specialist", got.Source)
	}
	if got.At.IsZero() {
		t.Error("At is zero; the frame should carry when the park was raised")
	}
}

// A confirmation with no gate payload is still an approval — a tool
// calling RequestConfirmation itself. It reaches the stream with the
// question the projection rendered, because dropping it would hide a
// session that is waiting on somebody.
func TestPermFrameKeepsAPayloadlessConfirmation(t *testing.T) {
	t.Parallel()
	got, ok := permFrame(inject.Park{
		InterruptID: "call-1",
		Kind:        inject.ParkKindApproval,
		Question:    "Approve this?",
		Author:      "planner",
		RaisedAt:    time.Now(),
	})
	if !ok {
		t.Fatal("a payloadless confirmation was dropped; it is still a question somebody has to answer")
	}
	if got.Detail != "Approve this?" || got.Source != "planner" {
		t.Errorf("frame = %+v, want the projection's question and author", got)
	}
	if got.ToolName != "" {
		t.Errorf("ToolName = %q, want empty — there is no recorded call", got.ToolName)
	}
}

// TestVerdictForDecision pins the vocabulary map, including the four
// refusals. They are not this file's policy: RecordMutationVerdict
// admits exactly allow-once and deny for a mutating call (W2.3), and
// refusing at the door means the client learns its button set is wrong
// from a 400 instead of from a turn that quietly did less than the
// button promised.
func TestVerdictForDecision(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in   permissions.Decision
		want approval.Verdict
		err  bool
	}{
		"deny": {
			in:   permissions.DecisionDeny,
			want: approval.Verdict{Verdict: approval.OutcomeReject},
		},
		"allow-once": {
			in:   permissions.DecisionAllowOnce,
			want: approval.Verdict{Verdict: approval.OutcomeApprove, Scope: approval.ScopeOnce},
		},
		"allow-session":      {in: permissions.DecisionAllowSession, err: true},
		"allow-session-verb": {in: permissions.DecisionAllowSessionVerb, err: true},
		"allow-session-tool": {in: permissions.DecisionAllowSessionTool, err: true},
		"allow-always":       {in: permissions.DecisionAllowAlways, err: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := verdictForDecision(tc.in)
			if tc.err {
				if !errors.Is(err, attach.ErrDecisionNotAdmissible) {
					t.Fatalf("err = %v, want ErrDecisionNotAdmissible (a 400, so the client can fix its buttons)", err)
				}
				// The message has to name the decision, or the client
				// operator cannot tell which button is wrong.
				if !strings.Contains(err.Error(), decisionWire(tc.in)) {
					t.Errorf("err = %q, want it to name %q", err, decisionWire(tc.in))
				}
				return
			}
			if err != nil {
				t.Fatalf("verdictForDecision(%v) err = %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("verdict = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestParkPermsSeedsAnAlreadyOpenPark is the durability claim on the
// read side. A park is a row, not a signal: a subscriber attaching to a
// session that parked before it connected — including after a daemon
// restart, where no in-process event survives — must still see the
// question. switchboard's client relies on exactly this ("no seq, no
// since, no replay window").
func TestParkPermsSeedsAnAlreadyOpenPark(t *testing.T) {
	t.Parallel()
	p, _ := permsFixture(t, "s-seed", gatePark("call-gate", scaleRequest()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frames, cleanup := p.SubscribePrompts(ctx)
	defer cleanup()

	select {
	case f := <-frames:
		if f.ID != "call-gate" {
			t.Errorf("frame ID = %q, want the parked interrupt", f.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no frame for a park that was already open when the subscriber arrived")
	}

	// And exactly once while it stays open: the stream re-reads the same
	// durable state every tick, so a naive implementation would re-ask
	// the same question every second.
	select {
	case f := <-frames:
		t.Errorf("the same open park was published twice: %+v", f)
	case <-time.After(100 * time.Millisecond):
	}
}

// A failed read skips the whole tick, pruning included. Pruning against
// a read that did not happen would forget a park that is still open and
// re-send it on the next successful tick — a duplicate question in a
// chat thread, caused by the database being briefly slow.
func TestParkPermsTickDoesNotPruneOnAFailedRead(t *testing.T) {
	t.Parallel()
	p, _ := permsFixture(t, "s-prune", gatePark("call-gate", scaleRequest()))

	out := make(chan attach.PromptFrame, 4)
	sent := map[string]bool{}
	p.tick(context.Background(), out, sent)
	if !sent["call-gate"] {
		t.Fatalf("first tick did not publish the open park (sent = %v)", sent)
	}

	// Same source, unreadable session: a read that fails, not a park
	// that closed.
	p.sid = "s-does-not-exist"
	p.tick(context.Background(), out, sent)
	if !sent["call-gate"] {
		t.Error("a failed read pruned the sent-set; the next good tick would re-ask an answered question")
	}
}

// TestParkPermsRespondTakesTheResumePath is the claim that there is no
// second copy of the approval logic: an answer posted to
// /perms/respond goes down the same closure POST /resume runs, carrying
// the write gate's own verdict shape.
func TestParkPermsRespondTakesTheResumePath(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		decision permissions.Decision
		want     approval.Verdict
	}{
		"allow-once": {
			decision: permissions.DecisionAllowOnce,
			want:     approval.Verdict{Verdict: approval.OutcomeApprove, Scope: approval.ScopeOnce},
		},
		"deny": {
			decision: permissions.DecisionDeny,
			want:     approval.Verdict{Verdict: approval.OutcomeReject},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sid := "s-respond-" + name
			p, spy := permsFixture(t, sid, gatePark("call-gate", scaleRequest()))

			if _, err := p.RespondPrompt(context.Background(), "call-gate", tc.decision); err != nil {
				t.Fatalf("RespondPrompt: %v", err)
			}
			got := spy.calls()
			if len(got) != 1 {
				t.Fatalf("resume called %d times, want 1", len(got))
			}
			if got[0].SessionID != sid || got[0].InterruptID != "call-gate" {
				t.Errorf("resume request = %+v, want the parked session and interrupt", got[0])
			}
			if !reflect.DeepEqual(got[0].Response, tc.want) {
				t.Errorf("resume response = %#v, want %#v", got[0].Response, tc.want)
			}
		})
	}
}

// The answer is attributed to the verified caller, and the identity the
// route echoes is the one the record was stamped with — the same
// approverFromContext the resume path runs, on the same context (#194).
// A client that reports "approved by X" in a thread is quoting the audit
// record, not guessing from who held the bearer token.
func TestParkPermsRespondEchoesTheRecordedApprover(t *testing.T) {
	t.Parallel()
	p, _ := permsFixture(t, "s-approver", gatePark("call-gate", scaleRequest()))

	ctx := auth.WithCaller(context.Background(), auth.Caller{Identity: "alice@example.com"})
	approver, err := p.RespondPrompt(ctx, "call-gate", permissions.DecisionAllowOnce)
	if err != nil {
		t.Fatalf("RespondPrompt: %v", err)
	}
	if approver != "alice@example.com" {
		t.Errorf("approver = %q, want the authenticated caller", approver)
	}
	if want := approverFromContext(ctx); approver != want {
		t.Errorf("approver = %q, want %q — the same identity the verdict is stamped with", approver, want)
	}
}

// Two failures that must not look alike, and neither of which may run a
// turn: a prompt that is gone (404 — what a client sees when two
// operators race the same button) and a decision the write gate does
// not take (400 — the client's button set is wrong).
func TestParkPermsRespondRefusesWithoutResuming(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		id       string
		decision permissions.Decision
		want     error
	}{
		"an interrupt that is not parked": {
			id:       "call-nope",
			decision: permissions.DecisionAllowOnce,
			want:     attach.ErrPromptNotFound,
		},
		"an operator-input park, which this wire cannot answer": {
			id:       "call-ask",
			decision: permissions.DecisionAllowOnce,
			want:     attach.ErrPromptNotFound,
		},
		"a scope broader than the operator was shown": {
			id:       "call-gate",
			decision: permissions.DecisionAllowSession,
			want:     attach.ErrDecisionNotAdmissible,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, spy := permsFixture(t, "s-refuse",
				gatePark("call-gate", scaleRequest()),
				questionPark("call-ask", "which namespace?"),
			)
			_, err := p.RespondPrompt(context.Background(), tc.id, tc.decision)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got := spy.calls(); len(got) != 0 {
				t.Errorf("a refused answer still ran a resume turn: %+v", got)
			}
		})
	}
}

// A read that fails is not evidence the park is gone. The pre-check
// exists to turn a known-stale id into a clean 404; refusing a real
// approval because the lookup was slow is the worse error, and the
// resume path re-checks against ADK's own interrupt matching anyway.
func TestParkPermsRespondProceedsWhenTheLookupFails(t *testing.T) {
	t.Parallel()
	store, _ := parkStore(t)
	spy := &resumeSpy{}
	p := newParkPerms(store, "s-never-created", spy.resume, nil).(*parkPerms)

	if _, err := p.RespondPrompt(context.Background(), "call-gate", permissions.DecisionAllowOnce); err != nil {
		t.Fatalf("RespondPrompt: %v", err)
	}
	if got := spy.calls(); len(got) != 1 {
		t.Fatalf("resume called %d times, want 1 — an unreadable store must not veto an approval", len(got))
	}
}
