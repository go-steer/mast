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
	"log/slog"
	"sync"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/observability"
)

// daemonSubRunObserver folds the events of a planner dispatch's private
// sub-runner into the two consumers the daemon's own turn loop feeds
// (#226): the session's budget meter and the metric registry.
//
// A planner runs each invoke_specialist dispatch on a runner of its
// own, and a private runner is a private event stream — through v0.4 a
// dispatched specialist's model calls reached no meter, no metric and
// no watchdog, which meant a workload with a $5 ceiling could spend
// past it as long as it spent through the planner's door. Coordinator,
// graph and fan-out dispatch never had the gap: those funnel sub-agent
// events up the root stream, where the turn loop already observes them.
//
// # What is wired here, and what is not
//
// The meter and the registry are process-scoped, so the observer holds
// them and picks the per-session meter by the OUTER session's ID. The
// watchdog is not: its enforcer is per-turn and reads a stream through
// watchdog.Tap, which has no seam a callback can join. A dispatched
// specialist can therefore still loop without being halted — the
// remaining half of #226, and the reason the issue stays open when this
// lands.
//
// # Late binding
//
// buildRoot runs before the meter pool and the registry exist (both
// need the bundle the root is built from), so the observer is
// constructed empty, handed to compose, and given its sinks by attach
// once they exist. Same shape as daemonPauseRecorder, and for the same
// reason. Until attach runs there is nothing to observe: no turn has
// started, so no dispatch can be in flight.
type daemonSubRunObserver struct {
	mu       sync.RWMutex
	meters   *meterPool
	obs      *observability.Registry
	workload string
	logger   *slog.Logger

	// unattributed fires once, for the sub-run event that arrives with
	// no outer session to bill. It should be unreachable — every
	// dispatch runs inside a turn, and a turn has a session — but the
	// alternative to noticing is metering a session named "", which
	// would look exactly like a workload that never spends.
	unattributed sync.Once
}

// attach gives the observer its sinks. Called once, after the meter
// pool and the registry are built.
func (o *daemonSubRunObserver) attach(meters *meterPool, obs *observability.Registry, workload string, logger *slog.Logger) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.meters, o.obs, o.workload, o.logger = meters, obs, workload, logger
}

// ObserveSubRun implements planner.SubRunObserver.
//
// The returned error stops the sub-run and hands the planner a labelled
// partial, so a specialist that crosses a ceiling costs the dispatch
// rather than the session. That is the finer-grained stop pkg/budget's
// package doc says the event-stream seam cannot give: here the run
// being stopped IS the specialist's.
//
// Metrics are recorded before the meter is consulted, because the call
// that crosses a ceiling still happened and still cost money — a token
// count that omits the last call is the one an operator reconciling a
// provider bill would trip over.
func (o *daemonSubRunObserver) ObserveSubRun(sessionID string, ev *session.Event) error {
	o.mu.RLock()
	meters, obs, wl, logger := o.meters, o.obs, o.workload, o.logger
	o.mu.RUnlock()
	if ev == nil {
		return nil
	}
	obs.Observe(ev, wl)
	if meters == nil {
		return nil
	}
	if sessionID == "" {
		o.unattributed.Do(func() {
			if logger != nil {
				logger.Error("planner sub-run event arrived with no session id; its spend is not metered",
					"workload", wl, "author", ev.Author)
			}
		})
		return nil
	}
	return meters.meter(sessionID).Observe(ev)
}
