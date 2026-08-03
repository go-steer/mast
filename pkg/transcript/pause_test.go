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

package transcript

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	adksession "google.golang.org/adk/v2/session"
)

// appendTo appends events to an already-seeded session through a
// fresh handle.
func appendTo(t *testing.T, svc adksession.Service, userID, sessionID string, events ...*adksession.Event) {
	t.Helper()
	ctx := context.Background()
	resp, err := svc.Get(ctx, &adksession.GetRequest{AppName: testApp, UserID: userID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("get session %q: %v", sessionID, err)
	}
	base := time.Now().Add(time.Minute)
	for i, ev := range events {
		ev.Timestamp = base.Add(time.Duration(i+1) * time.Second)
		if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatalf("append event %d to %q: %v", i, sessionID, err)
		}
	}
}

// parkEvent builds a long-running tool park: a model-response event
// whose FunctionCall's ID is stamped into LongRunningToolIDs and never
// answered — the wire shape ADK produces for request_operator_input /
// pause_session (one pause primitive, second spelling).
func parkEvent(author, tool, callID, message string) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-1")
	ev.Author = author
	ev.LongRunningToolIDs = []string{callID}
	fc := &genai.FunctionCall{ID: callID, Name: tool, Args: map[string]any{"message": message}}
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
	return ev
}

// TestLongRunningParkProjectsPaused is the regression test for the
// v0.1 gap the v0.2 pause/abort design's adversarial gate found (H1):
// scanPending derived paused exclusively from RequestedInput events,
// so a long-running park projected idle — invisible to operators and
// an auto-resume candidate. Verified to FAIL on the pre-fix
// scanPending (state comes back "idle" there).
func TestLongRunningParkProjectsPaused(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			store := NewStore(svc, testApp)
			seed(t, svc, "u1", "s-park",
				textEvent("user", "do the thing"),
				parkEvent("planner", "request_operator_input", "call-park-1", "need a decision"),
			)

			d, err := store.Get(context.Background(), "", "s-park")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if d.State != StatePaused {
				t.Fatalf("long-running park: state = %q, want %q", d.State, StatePaused)
			}
			if len(d.Pending) != 1 {
				t.Fatalf("pending = %+v, want exactly the park", d.Pending)
			}
			p := d.Pending[0]
			if p.InterruptID != "call-park-1" || !p.LongRunning || p.ToolName != "request_operator_input" || p.Message != "need a decision" {
				t.Errorf("pending park = %+v, want ID/LongRunning/ToolName/Message populated", p)
			}

			// The resume wire shape resolves it like any interrupt.
			appendTo(t, svc, "u1", "s-park", resolutionEvent("call-park-1"))
			d, err = store.Get(context.Background(), "", "s-park")
			if err != nil {
				t.Fatalf("Get after resolve: %v", err)
			}
			if d.State != StateIdle || len(d.Pending) != 0 {
				t.Errorf("after resolution: state=%q pending=%d, want idle/0", d.State, len(d.Pending))
			}
		})
	}
}

// TestParkOutranksInterruptedMarker pins the ladder consequence of the
// same gap: a park cut short by drain expiry used to project
// interrupted (a #41 auto-resume candidate holding an invisible,
// deliberate pause). Paused must trump interrupted.
func TestParkOutranksInterruptedMarker(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			store := NewStore(svc, testApp)
			seed(t, svc, "u1", "s-park-int",
				parkEvent("planner", "pause_session", "call-park-2", "cooling down"),
			)
			if err := store.MarkInterrupted(context.Background(), "u1", "s-park-int", "daemon shutdown"); err != nil {
				t.Fatalf("MarkInterrupted: %v", err)
			}
			d, err := store.Get(context.Background(), "", "s-park-int")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if d.State != StatePaused {
				t.Errorf("park + interrupted marker: state = %q, want %q (paused trumps interrupted)", d.State, StatePaused)
			}
		})
	}
}

func TestGatePauseLifecycle(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := NewStore(svc, testApp)
			seed(t, svc, "u1", "s-gate", textEvent("user", "hi"))

			h, err := store.PauseGate(ctx, "", "s-gate", PauseSpec{Reason: ReasonMaintenanceWindow, Message: "deploy at 2200"})
			if err != nil {
				t.Fatalf("PauseGate: %v", err)
			}
			if !strings.HasPrefix(h.Token, "mrt_") || h.SessionID != "s-gate" {
				t.Fatalf("handle = %+v, want mrt_ token for s-gate", h)
			}
			if h.ExpiresAt.Before(time.Now().Add(6 * 24 * time.Hour)) {
				t.Errorf("default TTL: expires %s, want ~7d out", h.ExpiresAt)
			}

			d, err := store.Get(ctx, "", "s-gate")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if d.State != StatePaused || d.PauseReason != string(ReasonMaintenanceWindow) || d.GatePause == nil {
				t.Fatalf("gate-paused projection = state %q reason %q gate %v", d.State, d.PauseReason, d.GatePause)
			}
			if g := store.GatePause(ctx, "", "s-gate"); !g.Active() || g.Token != h.Token {
				t.Fatalf("GatePause = %+v, want the active record", g)
			}

			// Second pause updates in place — same token, new reason.
			h2, err := store.PauseGate(ctx, "", "s-gate", PauseSpec{Reason: ReasonOperator, Message: "hold for review"})
			if err != nil {
				t.Fatalf("PauseGate update: %v", err)
			}
			if h2.Token != h.Token {
				t.Errorf("update minted a new token (%s vs %s), want in-place update", h2.Token, h.Token)
			}
			if d, _ := store.Get(ctx, "", "s-gate"); d.PauseReason != string(ReasonOperator) {
				t.Errorf("updated reason = %q, want operator", d.PauseReason)
			}

			// Consume = resume: the gate clears, the session is idle again.
			rec, err := store.ConsumeToken(ctx, h.Token, "operator resume --token")
			if err != nil {
				t.Fatalf("ConsumeToken: %v", err)
			}
			if rec.Plane != PlaneGate || rec.ConsumedAt.IsZero() {
				t.Fatalf("consumed record = %+v", rec)
			}
			if d, _ := store.Get(ctx, "", "s-gate"); d.State != StateIdle {
				t.Errorf("after consume: state = %q, want idle", d.State)
			}
			if g := store.GatePause(ctx, "", "s-gate"); g != nil {
				t.Errorf("GatePause after consume = %+v, want nil", g)
			}

			// Replay gets the structured no-op.
			if _, err := store.ConsumeToken(ctx, h.Token, "replay"); !errors.Is(err, ErrAlreadyResumed) {
				t.Errorf("second consume err = %v, want ErrAlreadyResumed", err)
			}
		})
	}
}

func TestTokenExpiryLeavesPauseIntact(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := NewStore(svc, testApp)
			seed(t, svc, "u1", "s-exp", textEvent("user", "hi"))

			h, err := store.PauseGate(ctx, "", "s-exp", PauseSpec{Reason: ReasonOther, Message: "x", TokenTTL: time.Nanosecond})
			if err != nil {
				t.Fatalf("PauseGate: %v", err)
			}
			time.Sleep(2 * time.Millisecond)

			if _, err := store.ConsumeToken(ctx, h.Token, "late"); !errors.Is(err, ErrTokenExpired) {
				t.Fatalf("expired consume err = %v, want ErrTokenExpired", err)
			}
			// The pause remains — expiry kills the token, not the pause.
			if d, _ := store.Get(ctx, "", "s-exp"); d.State != StatePaused {
				t.Errorf("after expired consume: state = %q, want still paused", d.State)
			}

			// extend-token is the recovery: audited lengthening, then resume.
			if _, err := store.ExtendToken(ctx, h.Token, time.Hour); err != nil {
				t.Fatalf("ExtendToken: %v", err)
			}
			if _, err := store.ConsumeToken(ctx, h.Token, "after extend"); err != nil {
				t.Fatalf("consume after extend: %v", err)
			}
			if d, _ := store.Get(ctx, "", "s-exp"); d.State != StateIdle {
				t.Errorf("after extend+consume: state = %q, want idle", d.State)
			}
		})
	}
}

// TestConsumeScheduledIgnoresExpiry pins the adversarial-gate fix: the
// timed-pause scheduler's consume honors a scheduled resume even when
// the operator-facing token has expired. Without it, a resume_at set
// beyond the token's TTL (the only way to schedule a pause longer than
// the cap, which mint can only shorten) livelocks the scheduler firing
// forever against an expired token. ConsumeToken (operator) still
// refuses; ConsumeScheduled (timer) consumes and ends the pause.
func TestConsumeScheduledIgnoresExpiry(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := NewStore(svc, testApp)
			seed(t, svc, "u1", "s-sched", textEvent("user", "hi"))

			h, err := store.PauseGate(ctx, "", "s-sched", PauseSpec{
				Reason:   ReasonMaintenanceWindow,
				Message:  "resume_at outlives the token",
				TokenTTL: time.Nanosecond, // expired essentially at once
			})
			if err != nil {
				t.Fatalf("PauseGate: %v", err)
			}
			time.Sleep(2 * time.Millisecond)

			// The operator path refuses (expiry guards stale possession)...
			if _, err := store.ConsumeToken(ctx, h.Token, "operator"); !errors.Is(err, ErrTokenExpired) {
				t.Fatalf("operator consume err = %v, want ErrTokenExpired", err)
			}
			if d, _ := store.Get(ctx, "", "s-sched"); d.State != StatePaused {
				t.Fatalf("pause did not survive operator refusal: state = %q", d.State)
			}

			// ...but the scheduler's own commitment fires through.
			if _, err := store.ConsumeScheduled(ctx, h.Token, "timer"); err != nil {
				t.Fatalf("ConsumeScheduled on expired token: %v", err)
			}
			if d, _ := store.Get(ctx, "", "s-sched"); d.State != StateIdle {
				t.Errorf("after scheduled consume: state = %q, want idle", d.State)
			}
			// Replay is still a benign no-op, not a second resume.
			if _, err := store.ConsumeScheduled(ctx, h.Token, "timer"); !errors.Is(err, ErrAlreadyResumed) {
				t.Errorf("scheduled replay err = %v, want ErrAlreadyResumed", err)
			}
		})
	}
}

func TestTokenTTLCannotLengthenAtMint(t *testing.T) {
	svc := adksession.InMemoryService()
	store := NewStore(svc, testApp)
	seed(t, svc, "u1", "s-ttl", textEvent("user", "hi"))
	_, err := store.PauseGate(context.Background(), "", "s-ttl",
		PauseSpec{Reason: ReasonOperator, TokenTTL: DefaultTokenTTL + time.Hour})
	if err == nil || !strings.Contains(err.Error(), "extend-token") {
		t.Fatalf("mint with TTL > default: err = %v, want refusal pointing at extend-token", err)
	}
}

func TestPauseGateRefusesAbortedAndUnknownReason(t *testing.T) {
	svc := adksession.InMemoryService()
	ctx := context.Background()
	store := NewStore(svc, testApp)
	seed(t, svc, "u1", "s-ab", textEvent("user", "hi"))

	if _, err := store.PauseGate(ctx, "", "s-ab", PauseSpec{Reason: "coffee_break"}); err == nil {
		t.Fatal("unknown reason accepted, want refusal")
	}
	if err := store.Abort(ctx, "", "s-ab", "done"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := store.PauseGate(ctx, "", "s-ab", PauseSpec{Reason: ReasonOperator}); !errors.Is(err, ErrAlreadyAborted) {
		t.Fatalf("pause aborted session: err = %v, want ErrAlreadyAborted", err)
	}
}

func TestInterruptPauseScanAndScope(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := NewStore(svc, testApp)
			resumeAt := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
			seed(t, svc, "u1", "s-intr",
				parkEvent("planner", "pause_session", "call-int-1", "quota backoff"),
			)

			h, err := store.PauseInterrupt(ctx, "u1", "s-intr", "call-int-1",
				PauseSpec{Reason: ReasonRateLimitBackoff, Message: "quota backoff", ResumeAt: resumeAt})
			if err != nil {
				t.Fatalf("PauseInterrupt: %v", err)
			}

			// The boot scan sees it, with the timer metadata intact.
			active, err := store.ScanPauses(ctx)
			if err != nil {
				t.Fatalf("ScanPauses: %v", err)
			}
			if len(active) != 1 {
				t.Fatalf("ScanPauses = %d records, want 1", len(active))
			}
			rec := active[0]
			if rec.Plane != PlaneInterrupt || rec.InterruptID != "call-int-1" ||
				rec.SessionID != "s-intr" || !rec.ResumeAt.Equal(resumeAt) {
				t.Fatalf("scanned record = %+v", rec)
			}

			// FindToken resolves it; a store under another app scope must not.
			if got, err := store.FindToken(ctx, h.Token); err != nil || got.Token != h.Token {
				t.Fatalf("FindToken = %+v, %v", got, err)
			}
			other := NewStore(svc, "other-app")
			if _, err := other.ConsumeToken(ctx, h.Token, "cross-scope"); !errors.Is(err, ErrTokenNotFound) {
				t.Fatalf("cross-scope consume err = %v, want ErrTokenNotFound", err)
			}

			// Consume drops it from the active scan.
			if _, err := store.ConsumeToken(ctx, h.Token, "timer"); err != nil {
				t.Fatalf("ConsumeToken: %v", err)
			}
			if active, _ := store.ScanPauses(ctx); len(active) != 0 {
				t.Errorf("ScanPauses after consume = %d, want 0", len(active))
			}
		})
	}
}

// TestAbortPurgesPauses pins the design's "abort purges" contract: an
// aborted session holds no resumable tokens and no live timers, and a
// purged token reads as not-found (not already-resumed).
func TestAbortPurgesPauses(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := NewStore(svc, testApp)
			seed(t, svc, "u1", "s-purge",
				parkEvent("planner", "pause_session", "call-p-1", "hold"),
			)
			gh, err := store.PauseGate(ctx, "", "s-purge", PauseSpec{Reason: ReasonOperator})
			if err != nil {
				t.Fatalf("PauseGate: %v", err)
			}
			ih, err := store.PauseInterrupt(ctx, "u1", "s-purge", "call-p-1", PauseSpec{Reason: ReasonAmbiguity})
			if err != nil {
				t.Fatalf("PauseInterrupt: %v", err)
			}

			if err := store.Abort(ctx, "", "s-purge", "operator cancelled"); err != nil {
				t.Fatalf("Abort: %v", err)
			}
			d, err := store.Get(ctx, "", "s-purge")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if d.State != StateAborted {
				t.Fatalf("state = %q, want aborted (abort outranks paused)", d.State)
			}
			for _, tok := range []string{gh.Token, ih.Token} {
				if _, err := store.FindToken(ctx, tok); !errors.Is(err, ErrTokenNotFound) {
					t.Errorf("token %s after abort: err = %v, want ErrTokenNotFound", tok, err)
				}
			}
			if active, _ := store.ScanPauses(ctx); len(active) != 0 {
				t.Errorf("ScanPauses after abort = %d, want 0", len(active))
			}
		})
	}
}
