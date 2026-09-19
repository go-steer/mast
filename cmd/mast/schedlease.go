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
	"time"

	"gorm.io/gorm"

	"github.com/go-steer/mast/pkg/eventlog"
)

// Which work this lease governs, and why it is one lease and not three.
//
// Three things in serve start turns nobody asked for: the scheduled
// trigger (a cadence in the bundle), the timed-pause scheduler (a
// resume_at an operator set earlier), and the boot auto-resume scan (a
// session a prior shutdown cut short). Every one of them is driven by
// the daemon noticing the time or the store, not by a request arriving
// — so every one of them runs once per replica, and an operator who
// scales the StatefulSet to 2 gets each of them twice with nothing in
// any log to say so (#345, following #291: mast is a thing someone else
// installs).
//
// They are one lease because the operator's question is one question.
// "Is this replica the one that acts on its own?" has a single answer,
// and three leases would let a deployment be half-leader — the shape
// that produces the most confusing incident.
//
// WHAT THIS IS NOT. It is not leader election and it does not make mast
// multi-replica. A replica that loses the race at boot stays passive for
// its whole life: it serves inject, AG-UI and A2A normally, and it does
// not take over if the leader dies later. Kubernetes restores the leader
// instead — the lease goes stale 8s after the holder stops heartbeating,
// and whichever instance *boots* next takes it (see the retry window
// below). That reclaim is not identity-matched: acquireLock compares the
// row's heartbeat against the staleness window and steals it regardless
// of who held it, so a redeploy or a scale-up claims an abandoned lease
// as readily as a restart of the process that died. What never claims
// one is an instance already running — acquireSchedulingLease is called
// once, at startup, and nothing re-checks. That asymmetry, not the
// lease's expiry, is what "passive for its whole life" means above.
// A StatefulSet also deletes the highest ordinal first on scale-down, so
// the leader is also the last replica standing. Session-ownership
// handoff and a claim-based scheduler poll are designed in
// docs/deployment-design.md and are still not built; what ships here is
// the half that makes the trap loud instead of silent.

// schedulingLease is this daemon's answer to "do I act on my own?".
// The zero value does not occur — acquireSchedulingLease always returns
// a usable value, including when there is nothing to coordinate
// through.
type schedulingLease struct {
	// lease is nil when this replica is passive, and also when there is
	// no durable store to coordinate through (in which case holding is
	// assumed — see acquireSchedulingLease).
	lease *eventlog.InstanceLease
	// active is the decision. Read it; do not infer it from lease.
	active bool
}

// schedulingLeaseName is the lease key. Keyed on the workload rather
// than being a single global singleton: two daemons running *different*
// workloads against one store are outside the single-writer rule for
// other reasons, but they are not duplicating each other's schedules,
// and refusing that would be a claim this lease has no evidence for.
func schedulingLeaseName(workloadName string) string {
	if workloadName == "" {
		return "scheduling"
	}
	return "scheduling/" + workloadName
}

// The boot retry window, and why there is one at all.
//
// A daemon that exits cleanly releases its lease, so the contested case
// at boot is nearly always the other one: the previous process was
// SIGKILLed — OOM, node eviction, `kill -9` — and left a lease nothing
// released. Refusing on that would be refusing the dead process's own
// replacement, which then stays passive for life: an OOM kill would
// silently end scheduled work until somebody restarted the pod a second
// time. So a contested boot waits out the staleness window (plus a beat
// of slack) before concluding that the holder is alive.
//
// The cost lands on the genuine second replica, which spends this long
// at boot before it can say it is second. That is the right side to
// charge: it happens once per replica, while the crash case happens
// whenever a crash does.
//
// It is bounded, and that boundedness is the design. Retrying forever
// would be leader election with takeover, which needs session-ownership
// handoff to be real first (docs/deployment-design.md) — a passive
// replica that promoted itself the moment the leader stalled would be
// running turns against sessions the leader still believes it owns.
const (
	schedLeaseRetryFor   = eventlog.InstanceLeaseStaleAfter + 2*time.Second
	schedLeaseRetryEvery = 500 * time.Millisecond
)

// acquireSchedulingLease decides whether this replica drives
// unrequested work, and says so at a level matched to what the operator
// can do about it.
//
// Three outcomes, and the interesting one is the third:
//
//   - No durable store. There is nothing two replicas could coordinate
//     through, and a daemon without --session-db has already accepted
//     that nothing it writes survives; two of them are two unrelated
//     agents, not a fleet. Active, with a warning that names the flag.
//   - Lease taken by a live holder, still held after the retry window.
//     Passive, logged at ERROR with the holder id, because this is a
//     deployment mistake an operator can fix in one command and the
//     symptom it replaces (duplicate remediations) is one they would
//     otherwise debug from the far end.
//   - The store could not answer. **Active**, logged at ERROR. This
//     fails open deliberately: the harm it risks (duplicate work)
//     requires a second replica to actually exist, while failing closed
//     would silently stop all scheduled work in the single-replica
//     deployment that is almost everyone — turning a database hiccup at
//     boot into a workload that never fires again and says nothing.
//     Losing the coordination is the smaller loss than losing the
//     function being coordinated.
func acquireSchedulingLease(ctx context.Context, db *gorm.DB, workloadName string, logger *slog.Logger) *schedulingLease {
	return acquireSchedulingLeaseWithin(ctx, db, workloadName, logger, schedLeaseRetryFor, schedLeaseRetryEvery)
}

// acquireSchedulingLeaseWithin is acquireSchedulingLease with the retry
// window named, so a test can exercise the contested branches without
// spending the real window on each one.
func acquireSchedulingLeaseWithin(ctx context.Context, db *gorm.DB, workloadName string, logger *slog.Logger, retryFor, retryEvery time.Duration) *schedulingLease {
	name := schedulingLeaseName(workloadName)
	if db == nil {
		logger.Warn("scheduled work is not coordinated between instances (--session-db is empty); run exactly one mast for this workload",
			"workload", workloadName)
		return &schedulingLease{active: true}
	}

	deadline := time.Now().Add(retryFor)
	waited := false
	for {
		lease, err := eventlog.AcquireInstanceLease(ctx, db, name)
		switch {
		case err == nil:
			if waited {
				logger.Info("took the scheduling lease from an instance that stopped heartbeating; scheduled work resumes here",
					"workload", workloadName, "lease", name, "holder", lease.Holder())
			} else {
				logger.Info("this instance drives scheduled work for the workload",
					"workload", workloadName, "lease", name, "holder", lease.Holder())
			}
			return &schedulingLease{lease: lease, active: true}

		case errors.Is(err, eventlog.ErrSessionLocked):
			if remaining := time.Until(deadline); remaining > 0 && ctx.Err() == nil {
				if !waited {
					waited = true
					// INFO, not WARN: at this point the likeliest
					// explanation is that we are the restart of a process
					// that was killed, and waiting is the recovery.
					logger.Info("the scheduling lease is held; waiting in case its holder is gone",
						"workload", workloadName, "lease", name,
						"waiting_up_to", remaining.Round(time.Second).String(), "error", err.Error())
				}
				select {
				case <-ctx.Done():
				case <-time.After(retryEvery):
				}
				continue
			}
			logger.Error("another mast instance already drives scheduled work against this session store; this instance will serve requests but will not fire scheduled triggers, timed-pause resumes or boot auto-resume",
				"workload", workloadName, "lease", name, "error", err.Error())
			return &schedulingLease{}

		default:
			if ctx.Err() != nil {
				// Shutting down before we ever started. Not a fault, and
				// not a fail-open: there is no scheduled work to protect
				// in a process that is on its way out.
				return &schedulingLease{}
			}
			logger.Error("could not take the scheduling lease; proceeding as if this instance drives scheduled work, which duplicates it if another instance is running",
				"workload", workloadName, "lease", name, "error", err.Error())
			return &schedulingLease{active: true}
		}
	}
}

// drivesScheduledWork reports whether this replica should start the
// cadence-driven loops.
func (s *schedulingLease) drivesScheduledWork() bool { return s != nil && s.active }

// lost fires when the lease was taken away mid-life — the holder's
// heartbeat lapsed past the staleness window and another instance
// claimed it. Both instances now believe they drive the work, which is
// the split-brain the lease exists to prevent, so the caller stops.
// A lease that was never held (passive replica, or no durable store)
// returns nil, which blocks forever in a select: correct, since there
// is nothing to lose.
func (s *schedulingLease) lost() <-chan struct{} {
	if s == nil || s.lease == nil {
		return nil
	}
	return s.lease.Lost()
}

// release drops the lease so a successor does not have to wait out the
// staleness window. Idempotent and nil-safe.
func (s *schedulingLease) release() error {
	if s == nil || s.lease == nil {
		return nil
	}
	return s.lease.Release()
}
