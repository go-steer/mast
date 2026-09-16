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

// The attach server's /perms routes, answered out of the durable park
// (#364).
//
// Everything needed was already here and none of it was reachable from
// the address a client dials. GET /parks projects the park; POST
// /resume answers it; #313 made a parked session say
// awaiting_permission instead of idle; #314 and #368 made the change
// itself readable so an approval is informed. All of that is on the
// inject server, under mast's own route names. switchboard — and any
// other client built against core-agent — asks the attach server, at
// /sessions/<app>/<sid>/perms/stream and .../perms/respond, and got a
// 501.
//
// So this file is a translation, not a mechanism: attach's prompt wire
// contract on one side, the park on the other, and no third copy of the
// approval logic. A verdict posted here takes the same resume path a
// verdict posted to /resume takes, including the part where the
// approver is read off the authenticated request and never out of the
// body.
//
// # Why polling
//
// The source reads the session store on a ticker rather than
// subscribing to an event feed, and that is the durability showing
// through. A park is a row, not a signal. It outlives the turn that
// raised it, outlives the process, and is frequently already open when
// the first subscriber arrives — including after a restart, where
// there is no in-process event left to replay. A transition feed would
// be correct only for the subscriber that happened to be attached at
// the moment of the park.
//
// Reading the same durable state on every tick also makes the seed and
// the stream the same code, which is what switchboard's client
// documents relying on: "there is no seq, no since, no replay window",
// because subscribing shows you everything still outstanding.
//
// # Why the response is synchronous
//
// RespondPrompt runs the resume turn before it returns, so POST
// /perms/respond can take as long as the turn does. That is not an
// oversight: it is exactly what POST /resume already does, and the two
// answering the same question with different timing would be worse
// than either choice. A client should give the request the deadline it
// would give a turn, not the one it would give an acknowledgement.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/permissions"
	"github.com/go-steer/mast/pkg/transcript"
)

// promptKindWriteGate is the PromptFrame.Kind a write-gate park travels
// under.
//
// It is one of the ported five rather than a new value, and which one
// is a decision about the receiving client rather than about taxonomy.
// Kind is not display text — no client renders it to a reader; the tool
// and the rendered call are what a person sees. What Kind actually
// drives is the button set. switchboard's approval client derives the
// offered answers from it (pkg/approval/approval.go, Options), and its
// rule for control_plane_write is exactly mast's rule for a mutating
// call: deny and allow-once, nothing wider, because that gate "records
// ANY non-deny answer as allow-once and deliberately remembers
// nothing". Its rule for a kind it has never seen is the opposite —
// offer the full six, on the reasonable ground that the daemon's own
// vocabulary beats a guess. So a new string here would put four buttons
// in front of an operator that this daemon answers 400, which is the
// failure that client's own comment calls worse than an absent one.
//
// The stretch is admitted: core-agent raises this kind for writes to
// privilege-bearing files under .agents/, and mast raises it for a
// mutating tool call. The property the two share is the one the wire
// field conveys — elevated, nothing is remembered, only an explicit
// one-shot approval passes — and it is the only kind that conveys it.
// verdictForDecision is the backstop for a client that offers a broader
// answer regardless.
const promptKindWriteGate = "control_plane_write"

// parkPermsPollInterval is how often a subscriber's view of the store
// is refreshed. A park is answered by a human reading a chat message,
// so a second of latency is not the constraint; the constraint is that
// N attached subscribers must not turn into a read-per-subscriber-per-
// tick storm on a database the turn path also uses.
const parkPermsPollInterval = time.Second

// parkPermsReadTimeout bounds one projection read, on the same grounds
// as turnStateReadTimeout: a wedged database must cost the stream its
// next frame, not wedge the goroutine.
const parkPermsReadTimeout = 2 * time.Second

// parkPerms answers one session's /perms routes from its durable
// parks. Construct with newParkPerms; the zero value is not usable.
type parkPerms struct {
	store  *transcript.Store
	sid    string
	resume func(ctx context.Context, req inject.ResumeRequest) error
	logger *slog.Logger

	// poll is overridable for tests. Zero means parkPermsPollInterval.
	poll time.Duration
}

// newParkPerms returns the source for one session, or nil when the
// daemon cannot serve the routes.
//
// Nil rather than a source that errors, because the capability report
// reads this: a daemon with no session store cannot see parks and a
// daemon with no resume handler cannot resolve them, and in both cases
// the honest wire answer is perms_stream false and 501 — the same
// answer mast gave through v0.8. Advertising a surface that would fail
// on use is the defect #364 was filed about.
func newParkPerms(store *transcript.Store, sid string, resume func(context.Context, inject.ResumeRequest) error, logger *slog.Logger) attach.PermsSource {
	if store == nil || resume == nil {
		return nil
	}
	return &parkPerms{store: store, sid: sid, resume: resume, logger: logger}
}

// SubscribePrompts implements attach.PermsSource.
func (p *parkPerms) SubscribePrompts(ctx context.Context) (<-chan attach.PromptFrame, func()) {
	ctx, cancel := context.WithCancel(ctx)
	// Buffered by one tick's worth of plausible parks so a slow reader
	// cannot stall the ticker; the goroutine drops rather than blocks,
	// and a dropped frame is re-sent on the next tick because the
	// sent-set is only advanced on a successful send.
	out := make(chan attach.PromptFrame, 16)

	go func() {
		defer close(out)
		interval := p.poll
		if interval <= 0 {
			interval = parkPermsPollInterval
		}
		// Seed immediately: a subscriber attaching to an already-parked
		// session is the common case, not the edge one.
		sent := map[string]bool{}
		p.tick(ctx, out, sent)

		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.tick(ctx, out, sent)
			}
		}
	}()

	return out, cancel
}

// tick publishes the parks this subscriber has not seen and forgets the
// ones that have been answered.
//
// A failed read skips the whole tick, pruning included. Pruning against
// a read that did not happen would forget a park that is still open and
// re-send it on the next successful tick — a duplicate question in a
// chat thread, caused by the database being briefly slow.
func (p *parkPerms) tick(ctx context.Context, out chan<- attach.PromptFrame, sent map[string]bool) {
	frames, ok := p.pending(ctx)
	if !ok {
		return
	}
	open := make(map[string]bool, len(frames))
	for _, f := range frames {
		open[f.ID] = true
		if sent[f.ID] {
			continue
		}
		select {
		case out <- f:
			sent[f.ID] = true
		case <-ctx.Done():
			return
		default:
			// Reader is behind. Leave it out of sent so the next tick
			// tries again rather than dropping the question.
		}
	}
	for id := range sent {
		if !open[id] {
			delete(sent, id)
		}
	}
}

// pending projects the session's open approval parks. The bool reports
// whether the read succeeded, which the caller needs to tell "nothing
// is parked" from "I could not look".
func (p *parkPerms) pending(ctx context.Context) ([]attach.PromptFrame, bool) {
	ctx, cancel := context.WithTimeout(ctx, parkPermsReadTimeout)
	defer cancel()
	d, err := p.store.Get(ctx, "", p.sid)
	if err != nil {
		// ErrNotFound is not noise worth logging on a ticker: a session
		// registered with attach before its first turn has no row yet.
		if p.logger != nil && !errors.Is(err, transcript.ErrNotFound) {
			p.logger.Debug("perms stream: could not read session parks",
				"session", p.sid, "error", err.Error())
		}
		return nil, false
	}
	var out []attach.PromptFrame
	for _, park := range projectParks(d).Parks {
		if f, ok := permFrame(park); ok {
			out = append(out, f)
		}
	}
	return out, true
}

// permFrame renders one park as a prompt, or reports that it does not
// belong on this stream.
//
// Only approval parks do. An input park is a question the agent asked
// in words, and the only answer this wire carries is a permissions
// decision — so publishing one would put a prompt in front of an
// operator whose buttons cannot answer it, which is the same mistake
// pkg/inject's Hold is careful not to make. Those stay on /parks and
// POST /resume, where the answer has somewhere to go.
func permFrame(park inject.Park) (attach.PromptFrame, bool) {
	if park.Kind != inject.ParkKindApproval {
		return attach.PromptFrame{}, false
	}
	f := attach.PromptFrame{
		ID:     park.InterruptID,
		Kind:   promptKindWriteGate,
		Detail: park.Question,
		At:     park.RaisedAt,
		Source: park.Author,
	}
	if c := park.Change; c != nil {
		f.ToolName = c.Tool
		if c.Key != "" {
			// The rendered one-line call beats the park's generic
			// question: it is what the deny policy matched and what the
			// audit row will say, so it is the text an operator should
			// be approving.
			f.Detail = c.Key
		}
		if c.Agent != "" {
			f.Source = c.Agent
		}
	}
	return f, true
}

// RespondPrompt implements attach.PermsSource: one operator answer,
// delivered down the same resume path POST /resume uses.
func (p *parkPerms) RespondPrompt(ctx context.Context, id string, decision permissions.Decision) (string, error) {
	verdict, err := verdictForDecision(decision)
	if err != nil {
		return "", err
	}
	// Check the park exists before running anything. The resume path
	// would refuse an unknown interrupt too, but it would refuse it as
	// a 500-shaped failure after doing work, and "this prompt is gone"
	// is a 404 the client already knows how to handle — it is what it
	// sees when two operators race the same button.
	if err := p.mustBeOpenApproval(ctx, id); err != nil {
		return "", err
	}
	if err := p.resume(ctx, inject.ResumeRequest{
		SessionID:   p.sid,
		InterruptID: id,
		Response:    verdict,
	}); err != nil {
		return "", err
	}
	// The same call verdictFor makes when it stamps the durable record,
	// on the same context — so this is the identity the decision was
	// recorded against rather than a second guess at it. #194 put a
	// verified caller on the record; echoing it here is what lets a chat
	// thread say who approved instead of that somebody did.
	return approverFromContext(ctx), nil
}

// mustBeOpenApproval reports whether id names an approval park that is
// still open on this session.
//
// A read failure is deliberately NOT fatal here. The pre-check exists
// to turn a known-stale id into a clean 404; a database that cannot be
// read is not evidence that the park is gone, and refusing a real
// approval because the lookup was slow is the worse error. The resume
// path re-checks against ADK's own interrupt matching regardless, so
// letting it through loses nothing but the nicer status code.
func (p *parkPerms) mustBeOpenApproval(ctx context.Context, id string) error {
	frames, ok := p.pending(ctx)
	if !ok {
		return nil
	}
	for _, f := range frames {
		if f.ID == id {
			return nil
		}
	}
	return fmt.Errorf("%w: interrupt %q is not an open approval on session %s", attach.ErrPromptNotFound, id, p.sid)
}

// verdictForDecision maps the attach wire's decision vocabulary onto
// the write gate's.
//
// Two of the six map, and the other four are refused here rather than
// narrowed. That is not this file's policy — it is
// permissions.RecordMutationVerdict's, which admits exactly allow-once
// and deny for a mutating call and returns ErrGrantScopeRefused for
// anything broader, because "allow every call to this tool for the
// session" applied to a tool that patches cluster objects hands over
// the namespace for the session (W2.3). Refusing at the door rather
// than three layers in means the operator is told before the turn runs,
// and the client learns its button set is wrong from a 400 instead of
// from a turn that quietly did less than the button promised.
func verdictForDecision(d permissions.Decision) (approval.Verdict, error) {
	switch d {
	case permissions.DecisionDeny:
		return approval.Verdict{Verdict: approval.OutcomeReject}, nil
	case permissions.DecisionAllowOnce:
		return approval.Verdict{Verdict: approval.OutcomeApprove, Scope: approval.ScopeOnce}, nil
	}
	return approval.Verdict{}, fmt.Errorf(
		"%w: a mutating call takes only \"deny\" or \"allow-once\"; %q would grant more than the operator was shown",
		attach.ErrDecisionNotAdmissible, decisionWire(d))
}

// decisionWire names a decision for an error message. The inverse of
// attach.DecisionFromWire, kept here rather than exported from
// pkg/attach because its only consumer is that message.
func decisionWire(d permissions.Decision) string {
	switch d {
	case permissions.DecisionDeny:
		return "deny"
	case permissions.DecisionAllowOnce:
		return "allow-once"
	case permissions.DecisionAllowSession:
		return "allow-session"
	case permissions.DecisionAllowSessionVerb:
		return "allow-session-verb"
	case permissions.DecisionAllowSessionTool:
		return "allow-session-tool"
	case permissions.DecisionAllowAlways:
		return "allow-always"
	}
	return fmt.Sprintf("decision(%d)", int(d))
}
