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
	"log/slog"
	"sync"
	"time"

	"github.com/go-steer/mast/pkg/transcript"
)

// daemonPauseRecorder is the pause_session record sink for serve mode:
// the transcript store, plus a timer push into the scheduler for
// records minted with resume_at mid-serve (the boot scan only covers
// records that predate the process). The scheduler attaches after
// construction because it needs the runner, which needs the root,
// which needs this recorder.
type daemonPauseRecorder struct {
	store *transcript.Store

	mu    sync.Mutex
	sched *pauseScheduler
}

func (r *daemonPauseRecorder) attach(s *pauseScheduler) {
	r.mu.Lock()
	r.sched = s
	r.mu.Unlock()
}

func (r *daemonPauseRecorder) PauseInterrupt(ctx context.Context, userID, sessionID, interruptID string, spec transcript.PauseSpec) (transcript.PauseHandle, error) {
	h, err := r.store.PauseInterrupt(ctx, userID, sessionID, interruptID, spec)
	if err != nil || spec.ResumeAt.IsZero() {
		return h, err
	}
	r.mu.Lock()
	sched := r.sched
	r.mu.Unlock()
	if sched != nil {
		sched.push(h.Token, spec.ResumeAt)
	}
	return h, err
}

// schedRequeueDelay is the backoff for a timer fire the chokepoint (or
// a transient failure) refused — requeued, not lost (v0.2 pause/abort
// design, timed-pause section).
const schedRequeueDelay = time.Minute

// pauseScheduler is the single-instance timed-pause scheduler
// (docs/durable-execution-design.md, "Timed pause"): one goroutine
// over the pending resume_at set, seeded from the boot ops-row scan,
// pushed on mint. It holds tokens and fire times only — the record is
// re-fetched at fire time, so an operator resume that won the race, an
// extension, or an abort purge is always honored (a consumed or
// vanished token is a silent no-op).
//
// Firing goes through the fire callback, which routes through the same
// doors an operator would use (gate → consume; interrupt → the
// turn-locked, budget-wrapped resume path). A fire error requeues with
// backoff. Multi-replica coordination is v0.3; this scheduler assumes
// it is the only one over its DB, same as the daemon assumes the
// single-writer rule.
type pauseScheduler struct {
	store  *transcript.Store
	logger *slog.Logger
	fire   func(ctx context.Context, rec *transcript.PauseRecord) error

	mu      sync.Mutex
	entries map[string]time.Time // token → fire time
	wake    chan struct{}
}

func newPauseScheduler(store *transcript.Store, logger *slog.Logger, fire func(context.Context, *transcript.PauseRecord) error) *pauseScheduler {
	return &pauseScheduler{
		store:   store,
		logger:  logger,
		fire:    fire,
		entries: map[string]time.Time{},
		wake:    make(chan struct{}, 1),
	}
}

// push arms (or re-arms) a token's fire time and wakes the loop.
func (ps *pauseScheduler) push(token string, at time.Time) {
	if token == "" || at.IsZero() {
		return
	}
	ps.mu.Lock()
	ps.entries[token] = at
	ps.mu.Unlock()
	select {
	case ps.wake <- struct{}{}:
	default:
	}
}

// seed loads every active pause record with a resume_at from the store
// — the boot scan. Timers that expired while the daemon was down fire
// immediately.
func (ps *pauseScheduler) seed(ctx context.Context) error {
	records, err := ps.store.ScanPauses(ctx)
	if err != nil {
		return err
	}
	n := 0
	for _, rec := range records {
		if !rec.ResumeAt.IsZero() {
			ps.push(rec.Token, rec.ResumeAt)
			n++
		}
	}
	if n > 0 {
		ps.logger.Info("timed-pause scheduler seeded from boot scan", "timers", n)
	}
	return nil
}

// run is the scheduler loop. It exits with ctx (the daemon's turn
// lifetime).
func (ps *pauseScheduler) run(ctx context.Context) {
	for {
		token, at, ok := ps.earliest()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-ps.wake:
				continue
			}
		}
		delay := time.Until(at)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-ps.wake:
			timer.Stop()
			continue
		case <-timer.C:
			ps.fireDue(ctx, token)
		}
	}
}

func (ps *pauseScheduler) earliest() (string, time.Time, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	var (
		token string
		at    time.Time
		found bool
	)
	for tok, t := range ps.entries {
		if !found || t.Before(at) {
			token, at, found = tok, t, true
		}
	}
	return token, at, found
}

// fireDue pops the entry, re-fetches the record for freshness, and
// dispatches. Outcomes:
//   - token vanished (abort purge, never existed) or consumed (operator
//     resumed early): drop silently — the design's benign race.
//   - record fetch hit a transient store fault: requeue with backoff —
//     the entry was already popped, so a blip must not lose the timer.
//   - resume_at moved into the future (a re-pause updated it): re-arm.
//   - fire failed (chokepoint refusal — gate-paused or draining — or a
//     transient error): requeue with backoff and an audit log line,
//     never silently lost (adversarial gate finding M6).
func (ps *pauseScheduler) fireDue(ctx context.Context, token string) {
	ps.mu.Lock()
	delete(ps.entries, token)
	ps.mu.Unlock()

	rec, err := ps.store.FindToken(ctx, token)
	if errors.Is(err, transcript.ErrTokenNotFound) {
		return // genuinely gone (abort purge, never existed): benign drop.
	}
	if err != nil {
		// A transient store fault (a List blip, a lock timeout) is not a
		// vanished token — the entry is already popped, so requeue it
		// rather than losing the timer until the next boot scan.
		ps.logger.Warn("timed-pause record fetch failed; requeued",
			"retry_in", schedRequeueDelay.String(), "error", err.Error())
		ps.push(token, time.Now().Add(schedRequeueDelay))
		return
	}
	if !rec.Active() {
		return // consumed (operator resumed early): benign drop.
	}
	if now := time.Now().UTC(); rec.ResumeAt.After(now) {
		ps.push(token, rec.ResumeAt)
		return
	}
	if err := ps.fire(ctx, rec); err != nil {
		ps.logger.Warn("timed resume refused; requeued",
			"session", rec.SessionID, "plane", rec.Plane, "reason", string(rec.Reason),
			"retry_in", schedRequeueDelay.String(), "error", err.Error())
		ps.push(token, time.Now().Add(schedRequeueDelay))
		return
	}
	ps.logger.Info("timed resume fired",
		"session", rec.SessionID, "plane", rec.Plane, "reason", string(rec.Reason))
}
