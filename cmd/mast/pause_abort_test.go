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

// Tests for the v0.2 pause/abort daemon machinery (issue #42): the
// runTurnPre chokepoint (terminal abort + gate pause refuse EVERY turn
// kind), the register-before-check / mark-then-sweep cancel handshake,
// the planned stop's pause-and-mark pass, and the timed-pause
// scheduler's requeue/drop semantics.
package main

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/watchdog"
)

// blockableModel yields one text response, optionally parking until
// its context is cancelled first (for in-flight-cancel tests).
type blockableModel struct {
	started chan struct{} // closed when a generation begins (if non-nil)
	block   bool
}

func (m *blockableModel) Name() string { return "blockable" }

func (m *blockableModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.started != nil {
			select {
			case <-m.started:
			default:
				close(m.started)
			}
		}
		if m.block {
			<-ctx.Done()
			yield(nil, ctx.Err())
			return
		}
		resp := &model.LLMResponse{
			Content: genai.NewContentFromText("done", genai.RoleModel),
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2,
			},
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}
		yield(resp, nil)
	}
}

// errModel fails generation outright, so a turn driven over it returns a
// runner error (not an ErrConflict chokepoint refusal) — the real path
// behind classifyTurn's default → "failed" projection.
type errModel struct{}

func (errModel) Name() string { return "errmodel" }

func (errModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, errors.New("model exploded"))
	}
}

// turnHarness wires the real runTurn stack — runner, transcript store,
// tracker, turn locks, meters — over an in-memory session service.
type turnHarness struct {
	svc     adksession.Service
	store   *transcript.Store
	runner  *runner.Runner
	tracker *turnTracker
	locks   *sessionTurnLocks
	meters  *meterPool
	wds     *watchdogPool
	obs     *observability.Registry
}

func newTurnHarness(t *testing.T, m model.LLM) *turnHarness {
	t.Helper()
	return newTurnHarnessOpts(t, m, watchdog.ModeWarn)
}

// newTurnHarnessOpts is the same stack with the watchdog posture (and
// any tools the fake model calls) spelled out.
func newTurnHarnessOpts(t *testing.T, m model.LLM, mode watchdog.Mode, tools ...tool.Tool) *turnHarness {
	t.Helper()
	h := &turnHarness{
		svc:    adksession.InMemoryService(),
		locks:  newSessionTurnLocks(),
		meters: newMeterPool(nil, nil, "", "test-model"),
		wds:    newWatchdogPool(mode),
		obs:    observability.New(),
	}
	h.obs.Prime("(test)")
	h.store = transcript.NewStore(h.svc, appName)
	h.tracker = newTurnTracker(h.store, discardLogger(), h.obs, "(test)")
	root, err := llmagent.New(llmagent.Config{
		Name:        "pause_abort_agent",
		Description: "chokepoint test agent",
		Model:       m,
		Tools:       tools,
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}
	h.runner, err = runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    h.svc,
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return h
}

func (h *turnHarness) turn(ctx context.Context, sid string) error {
	msg := genai.NewContentFromText("hello", genai.RoleUser)
	return runTurn(ctx, h.runner, discardLogger(), h.store, h.meters, h.wds, h.obs, h.tracker, h.locks, "(test)", sid, msg, "test")
}

func (h *turnHarness) eventCount(t *testing.T, sid string) int {
	t.Helper()
	d, err := h.store.Get(context.Background(), "", sid)
	if err != nil {
		t.Fatalf("Get(%q): %v", sid, err)
	}
	return d.EventCount
}

// TestChokepointRefusesAbortedSession pins the terminal-abort contract
// the v0.1 surface lacked: only /resume refused aborted sessions —
// inject/attach turns ran happily on them. Every turn kind now refuses
// at the chokepoint with ErrConflict, and no events are appended.
func TestChokepointRefusesAbortedSession(t *testing.T) {
	h := newTurnHarness(t, &blockableModel{})
	ctx := context.Background()

	if err := h.turn(ctx, "s-abort"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	before := h.eventCount(t, "s-abort")
	if err := h.store.Abort(ctx, "", "s-abort", "operator cancelled"); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	err := h.turn(ctx, "s-abort")
	if !errors.Is(err, inject.ErrConflict) {
		t.Fatalf("turn on aborted session: err = %v, want ErrConflict (session_aborted)", err)
	}
	if got := h.eventCount(t, "s-abort"); got != before {
		t.Errorf("aborted session gained events: %d -> %d (the refused turn ran)", before, got)
	}
}

// TestChokepointRefusesGatePausedSession: plane-B gate pause refuses
// every subsequent turn until the token resumes it.
func TestChokepointRefusesGatePausedSession(t *testing.T) {
	h := newTurnHarness(t, &blockableModel{})
	ctx := context.Background()

	if err := h.turn(ctx, "s-gate"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	handle, _, err := h.store.PauseGate(ctx, "", "s-gate", transcript.PauseSpec{
		Reason: transcript.ReasonMaintenanceWindow, Message: "deploy window",
	})
	if err != nil {
		t.Fatalf("PauseGate: %v", err)
	}

	if err := h.turn(ctx, "s-gate"); !errors.Is(err, inject.ErrConflict) {
		t.Fatalf("turn on gate-paused session: err = %v, want ErrConflict (session_paused)", err)
	}

	// Consuming the token IS the resume: the gate clears, turns run.
	if _, err := h.store.ConsumeToken(ctx, handle.Token, "test resume"); err != nil {
		t.Fatalf("ConsumeToken: %v", err)
	}
	if err := h.turn(ctx, "s-gate"); err != nil {
		t.Fatalf("turn after gate resume: %v", err)
	}
}

// TestAbortCancelsInFlightTurn pins the mark-then-sweep half of the
// cancel handshake: an abort against a session mid-turn cancels that
// turn instead of letting it run to completion (v0.1 abort was a
// marker only).
func TestAbortCancelsInFlightTurn(t *testing.T) {
	m := &blockableModel{started: make(chan struct{}), block: true}
	h := newTurnHarness(t, m)
	ctx := context.Background()

	turnErr := make(chan error, 1)
	go func() { turnErr <- h.turn(ctx, "s-cancel") }()

	select {
	case <-m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("model generation never started")
	}

	// The abort-handler sequence: durable marker first, then sweep.
	if err := h.store.Abort(ctx, "", "s-cancel", "kill it"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !h.tracker.cancelSession("s-cancel") {
		t.Fatal("cancelSession found no registered in-flight turn")
	}

	select {
	case <-turnErr:
		// The turn unwound. Its error shape is the runner's business —
		// ADK may even end the stream silently on cancel (verified
		// substrate fact); what matters is that it returned promptly.
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight turn did not unwind after abort cancel")
	}

	// And the session is terminal for the next turn.
	if err := h.turn(ctx, "s-cancel"); !errors.Is(err, inject.ErrConflict) {
		t.Fatalf("turn after abort: err = %v, want ErrConflict", err)
	}
}

// TestPlannedStopPauseAndMark pins issue #42's --pause-sessions: the
// drain's mark pass gate-pauses each session it marks (pause travels
// with the mark, under writeMu), the marker reason classifies the stop
// as operator-initiated, and the session projects paused — outranking
// interrupted — so #41's boot pass (candidates: interrupted) skips it.
func TestPlannedStopPauseAndMark(t *testing.T) {
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-stop")
	store := transcript.NewStore(svc, appName)
	obs := observability.New()
	obs.Prime("(test)")
	tr := newTurnTracker(store, discardLogger(), obs, "(test)")

	tr.begin("s-stop")
	tr.planStop("operator stop: deploy freeze", true)
	tr.beginDrain(context.Background())

	d, err := store.Get(context.Background(), "", "s-stop")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d.State != transcript.StatePaused {
		t.Fatalf("state after planned-stop mark = %q, want paused (outranks interrupted)", d.State)
	}
	// The pause that traveled with the mark is counted with the
	// planned-stop source (#50), not the operator-request source.
	assertMetric(t, obs, `mast_gate_pauses_total{source="planned_stop",workload="(test)"} 1`)
	assertMetric(t, obs, `mast_gate_pauses_total{source="operator",workload="(test)"} 0`)
	if d.PauseReason != string(transcript.ReasonOperator) {
		t.Errorf("pause reason = %q, want operator", d.PauseReason)
	}
	if d.InterruptReason != "" {
		t.Errorf("interrupt reason leaked into paused projection: %q", d.InterruptReason)
	}

	// The turn finishing inside the window clears the interruption
	// marker but NOT the gate pause — the operator asked for a pause.
	tr.end("s-stop")
	d, err = store.Get(context.Background(), "", "s-stop")
	if err != nil {
		t.Fatalf("Get after end: %v", err)
	}
	if d.State != transcript.StatePaused {
		t.Errorf("state after clean finish = %q, want still paused", d.State)
	}
}

// TestPlannedStopClassifiesMarker: without --pause-sessions the mark
// is an ordinary interruption marker, but its reason carries the
// operator-stop classification instead of "daemon shutdown".
func TestPlannedStopClassifiesMarker(t *testing.T) {
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-classify")
	store := transcript.NewStore(svc, appName)
	tr := newTurnTracker(store, discardLogger(), observability.New(), "(test)")

	tr.begin("s-classify")
	tr.planStop("operator stop", false)
	tr.beginDrain(context.Background())
	tr.freeze() // drain expired; the turn is a survivor

	d, err := store.Get(context.Background(), "", "s-classify")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d.State != transcript.StateInterrupted {
		t.Fatalf("state = %q, want interrupted", d.State)
	}
	if d.InterruptReason != "operator stop" {
		t.Errorf("marker reason = %q, want the operator-stop classification", d.InterruptReason)
	}
}

// TestSchedulerFireConsumeRequeue covers the scheduler's three
// outcomes: consumed/vanished tokens drop silently, fire errors
// requeue with backoff (never silently lost), successes clear.
func TestSchedulerFireConsumeRequeue(t *testing.T) {
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-timer")
	store := transcript.NewStore(svc, appName)
	ctx := context.Background()

	handle, _, err := store.PauseGate(ctx, "", "s-timer", transcript.PauseSpec{
		Reason:   transcript.ReasonRateLimitBackoff,
		ResumeAt: time.Now().UTC().Add(-time.Second), // already due
	})
	if err != nil {
		t.Fatalf("PauseGate: %v", err)
	}

	var fires, failures atomic.Int32
	failFirst := true
	sched := newPauseScheduler(store, discardLogger(), func(_ context.Context, rec *transcript.PauseRecord) error {
		if failFirst {
			failFirst = false
			failures.Add(1)
			return errors.New("chokepoint refused")
		}
		fires.Add(1)
		_, cerr := store.ConsumeToken(ctx, rec.Token, "timer")
		return cerr
	})

	// First fire fails → requeued with backoff, not lost.
	sched.fireDue(ctx, handle.Token)
	if failures.Load() != 1 {
		t.Fatalf("failures = %d, want 1", failures.Load())
	}
	sched.mu.Lock()
	_, requeued := sched.entries[handle.Token]
	sched.mu.Unlock()
	if !requeued {
		t.Fatal("failed fire was not requeued")
	}

	// Second fire succeeds and consumes.
	sched.fireDue(ctx, handle.Token)
	if fires.Load() != 1 {
		t.Fatalf("fires = %d, want 1", fires.Load())
	}
	if g := store.GatePause(ctx, "", "s-timer"); g != nil {
		t.Errorf("gate still active after timed resume: %+v", g)
	}

	// Third fire on the consumed token: silent no-op (operator-won-race
	// semantics).
	sched.fireDue(ctx, handle.Token)
	if fires.Load() != 1 || failures.Load() != 1 {
		t.Errorf("consumed-token fire ran the callback: fires=%d failures=%d", fires.Load(), failures.Load())
	}
}

// TestSchedulerExpiredTimedPauseFiresOnce is the adversarial-gate
// regression: a timed gate pause whose resume_at outlives its token TTL
// must fire ONCE and end — not requeue forever against an expired token.
// The fire callback mirrors the daemon's gate path (ConsumeScheduled).
func TestSchedulerExpiredTimedPauseFiresOnce(t *testing.T) {
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-exp-timer")
	store := transcript.NewStore(svc, appName)
	ctx := context.Background()

	// resume_at already due, token already expired (TTL can only shorten,
	// so a long resume_at is the only way to outlive the token).
	handle, _, err := store.PauseGate(ctx, "", "s-exp-timer", transcript.PauseSpec{
		Reason:   transcript.ReasonMaintenanceWindow,
		ResumeAt: time.Now().UTC().Add(-time.Second),
		TokenTTL: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("PauseGate: %v", err)
	}
	time.Sleep(2 * time.Millisecond)

	var fires atomic.Int32
	sched := newPauseScheduler(store, discardLogger(), func(fireCtx context.Context, rec *transcript.PauseRecord) error {
		fires.Add(1)
		_, cerr := store.ConsumeScheduled(fireCtx, rec.Token, "timer")
		if errors.Is(cerr, transcript.ErrAlreadyResumed) {
			return nil
		}
		return cerr
	})

	sched.fireDue(ctx, handle.Token)

	if fires.Load() != 1 {
		t.Fatalf("fires = %d, want exactly 1", fires.Load())
	}
	if g := store.GatePause(ctx, "", "s-exp-timer"); g != nil {
		t.Errorf("gate still active after expired timed resume: %+v", g)
	}
	// The entry must NOT be requeued — the pause ended, no livelock.
	sched.mu.Lock()
	_, requeued := sched.entries[handle.Token]
	sched.mu.Unlock()
	if requeued {
		t.Error("expired timed pause was requeued — the livelock the fix removes")
	}
}

// seedSession creates a primary session with one event so store.Get
// and the pause machinery have a real row to work with.
func seedSession(t *testing.T, svc adksession.Service, sid string) {
	t.Helper()
	ctx := context.Background()
	resp, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: appName, UserID: defaultUserID, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("create %q: %v", sid, err)
	}
	ev := adksession.NewEvent(ctx, "inv-seed")
	ev.Author = "user"
	ev.Content = genai.NewContentFromText("seed", genai.RoleUser)
	if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatalf("append seed event: %v", err)
	}
}

var _ = slog.Default // keep slog import if discardLogger moves
