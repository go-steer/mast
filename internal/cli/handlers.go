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

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-steer/mast/internal/inject"
	"github.com/go-steer/mast/internal/observability"
	"github.com/go-steer/mast/internal/transcript"
)

// recordAbort performs a terminal abort's durable write and, on
// success, its metric (#50). Extracted from the /abort handler so the
// write-plus-count pairing is exercised directly by a test — the
// counter must fire only when the marker actually landed.
func recordAbort(ctx context.Context, store *transcript.Store, obs *observability.Registry, workload, sessionID, reason string) error {
	if err := store.Abort(ctx, "", sessionID, reason); err != nil {
		return err
	}
	obs.Abort(workload)
	return nil
}

// openGatePause records an operator gate pause and, when it opened a NEW
// pause, its operator-sourced metric (#50), returning the raw
// handle/error for the caller to map to HTTP status. The metric counts a
// distinct durable pause, so an in-place refresh of an already-active
// pause (same token) does not advance it.
func openGatePause(ctx context.Context, store *transcript.Store, obs *observability.Registry, workload, sessionID string, spec transcript.PauseSpec) (transcript.PauseHandle, error) {
	h, created, err := store.PauseGate(ctx, "", sessionID, spec)
	if err != nil {
		return transcript.PauseHandle{}, err
	}
	if created {
		// Only a newly-opened gate pause counts; a second /pause on an
		// already-paused session refreshes it in place (same token) and
		// is not a new pause (#50).
		obs.GatePause(workload, observability.GatePauseOperator)
	}
	return h, nil
}

// newTimedFireCallback builds the timed-pause scheduler's fire callback:
// it fires through the same operator doors (ConsumeScheduled for a gate
// pause, a resume for an interrupt pause) and emits exactly one
// mast_timed_pause_fires_total per fire, classified by disposition
// (#50). The error it returns still drives the scheduler's
// reschedule-on-error. Extracted so the real daemon callback — not a
// test twin — is what the metric test exercises.
func newTimedFireCallback(
	store *transcript.Store,
	tracker *turnTracker,
	obs *observability.Registry,
	workload string,
	resumeByInterrupt func(context.Context, inject.ResumeRequest) error,
	logger *slog.Logger,
) func(context.Context, *transcript.PauseRecord) error {
	return func(fireCtx context.Context, rec *transcript.PauseRecord) error {
		outcome, err := func() (string, error) {
			if tracker.isDraining() {
				return observability.TimedPauseSkipped, errors.New("daemon draining")
			}
			if rec.Plane == transcript.PlaneGate {
				// ConsumeScheduled, not ConsumeToken: the timer is the daemon's
				// own commitment and is not vetoed by the operator-facing token
				// TTL — a resume_at beyond the token's life would otherwise
				// livelock this fire against an expired token forever.
				_, err := store.ConsumeScheduled(fireCtx, rec.Token, "timer")
				if errors.Is(err, transcript.ErrAlreadyResumed) {
					return observability.TimedPauseSkipped, nil // an operator resumed earlier and won benignly
				}
				if err != nil {
					return observability.TimedPauseError, err
				}
				return observability.TimedPauseResumed, nil
			}
			req := inject.ResumeRequest{
				SessionID:   rec.SessionID,
				InterruptID: rec.InterruptID,
				Response:    map[string]any{"resumed_by": "timer", "resume_at": rec.ResumeAt.Format(time.RFC3339)},
			}
			rerr := resumeByInterrupt(fireCtx, req)
			cctx, cancel := context.WithTimeout(context.WithoutCancel(fireCtx), storeWriteTimeout)
			defer cancel()
			consumeIfAnswered(cctx, store, logger, rec, "timer")
			if rerr != nil {
				return observability.TimedPauseError, rerr
			}
			return observability.TimedPauseResumed, nil
		}()
		obs.TimedPauseFire(workload, outcome)
		return err
	}
}

// newResumeByToken builds the token-keyed resume path (v0.2 pause/abort
// design, "Resume tokens"): gate pause → consume IS the resume (no turn
// runs — nothing was parked); interrupt pause → the normal resume path,
// with consumption keyed on the durable append of the resume
// FunctionResponse.
//
// Top-level and dependency-injected, the same shape as
// newTimedFireCallback above, so the identity it records is testable
// without standing up a daemon — the point of #194 is an audit field,
// and an audit field nobody asserts on is a field that drifts.
func newResumeByToken(
	store *transcript.Store,
	logger *slog.Logger,
	resumeByInterrupt func(context.Context, inject.ResumeRequest) error,
) func(context.Context, inject.ResumeRequest) error {
	return func(reqCtx context.Context, req inject.ResumeRequest) error {
		// Who is spending this token. internal/inject resolves the caller onto
		// the request context before it reaches us (handleResume →
		// callerContext), so on this path `by` is a real identity — or
		// "shared-bearer-token" when the daemon runs without a user
		// table, which is honest about what one shared credential can
		// prove. Deliberately taken from the context and not from the
		// request body: an attribution a caller writes about itself is
		// worth nothing after an incident, the same reasoning that makes
		// attach's GuardrailResetRequest.Caller `json:"-"`.
		by := approverFromContext(reqCtx)
		rec, err := store.FindToken(reqCtx, req.Token)
		if err != nil {
			return fmt.Errorf("%v: %w", err, inject.ErrBadPayload)
		}
		if !rec.ConsumedAt.IsZero() {
			// already_resumed is a structured no-op, not an error: the
			// resume the token asked for has happened.
			logger.Info("resume token already consumed; no-op",
				"session", rec.SessionID, "consumed_at", rec.ConsumedAt.Format(time.RFC3339), "consumed_by", rec.ConsumedBy)
			return nil
		}
		if rec.Expired(time.Now().UTC()) {
			return fmt.Errorf("resume token expired %s (the pause remains; `mast sessions extend-token` is the recovery): %w",
				rec.ExpiresAt.Format(time.RFC3339), inject.ErrConflict)
		}
		if rec.Plane == transcript.PlaneGate {
			_, err := store.ConsumeToken(reqCtx, req.Token, by)
			if errors.Is(err, transcript.ErrAlreadyResumed) {
				return nil
			}
			return err
		}
		response := req.Response
		if response == nil {
			// The default answer the resumed turn sees. It used to say
			// "operator" — the same "a human did this and we cannot say
			// which" placeholder as the consumed-by literal, in a second
			// place, and this one is visible to the model.
			response = map[string]any{"resumed_by": by}
		}
		inner := inject.ResumeRequest{
			SessionID:   rec.SessionID,
			InterruptID: rec.InterruptID,
			Response:    response,
			AckEffects:  req.AckEffects,
		}
		rerr := resumeByInterrupt(reqCtx, inner)
		// Consumption keys on the durable append, not turn success: use
		// a fresh short-lived context so a request-scope cancellation
		// cannot strand an answered interrupt with a live token.
		//
		// WithoutCancel, not a bare Background: it keeps the request's
		// values, which is what carries the caller identity `by` was
		// read from into anything the consume path logs.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), storeWriteTimeout)
		defer cancel()
		consumeIfAnswered(cctx, store, logger, rec, by)
		return rerr
	}
}
