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
	"fmt"
	"log/slog"
	"time"

	adkagent "google.golang.org/adk/v2/agent"

	"github.com/go-steer/mast/pkg/workload"
)

// The collection leg (v0.5 W4.2): the part of a monitoring cycle that
// runs before the model is woken.
//
// A scheduled cycle used to be one thing — wake the model, hand it a
// tick, let it work out what to look at. That shape cannot do unattended
// monitoring, and the reason is specific rather than aesthetic. Knowing
// what CHANGED between two runs means asking something that keeps
// per-run state, and a tool that advances persisted state as a side
// effect of answering is a mutating tool. Under the shipped default
// hitl.on_mutation: require_approval, a model that held that tool would
// park the cycle for an operator on every fire. Nobody is awake to
// answer at 3am, which is the hour the whole feature is for.
//
// So the collection runs here instead, on mast's own behalf, and the
// model is handed the answer as its input. Three consequences follow,
// and all three are properties the release is claiming rather than
// implementation details:
//
//   - Nothing gates it, because no model asked for anything. That is
//     not a hole: internal/compose.CheckMonitorCollectSurface refuses
//     to start if a collect tool is reachable from any roster, so the
//     ungated door and the gated door never lead to the same tool.
//   - It costs zero model calls, by construction rather than by
//     measurement. A leg the model is not part of cannot spend a token
//     (scoreboard row 9).
//   - mast still learns nothing about the domain. It runs the calls the
//     bundle names and passes the results through; what a transition
//     means is the collected tool's business, not mast's (W4.4).

// monitorCollector runs a workload's declared collection calls at the
// top of each scheduled fire.
type monitorCollector struct {
	calls   []workload.MonitorCollect
	run     func(ctx adkagent.Context, name string, args map[string]any) (map[string]any, error)
	logger  *slog.Logger
	appName string
	userID  string

	// now is the injection point for the duration in the log line, the
	// same seam scheduledTrigger uses and for the same reason.
	now func() time.Time
}

func newMonitorCollector(logger *slog.Logger, mon workload.Monitor, run func(adkagent.Context, string, map[string]any) (map[string]any, error), appName, userID string) *monitorCollector {
	return &monitorCollector{
		calls:   mon.Collect,
		run:     run,
		logger:  logger,
		appName: appName,
		userID:  userID,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// enabled reports whether this workload collects at all. A nil collector
// is a workload with no monitor block, so callers do not have to check
// for both.
func (c *monitorCollector) enabled() bool {
	return c != nil && len(c.calls) > 0 && c.run != nil
}

func (c *monitorCollector) clock() time.Time {
	if c == nil || c.now == nil {
		return time.Now().UTC()
	}
	return c.now()
}

// collect runs every declared call and returns the results keyed by the
// bundle's `as:` names.
//
// SERIAL, IN DECLARATION ORDER. The shape this exists for is a scan
// followed by a diff of that scan against the last one, and running the
// two concurrently would classify against a run that had not finished.
// The bundle's order is the dependency order; there is no benefit in
// guessing otherwise for a leg that runs once every fifteen minutes.
//
// THE FIRST FAILURE ABORTS THE CYCLE. The alternative — collect what you
// can and wake the model with the rest — is the one that has to be
// argued against, because it sounds resilient. It is not: a cycle whose
// diff failed and whose scan succeeded would hand the model a snapshot
// with no transitions attached, and the honest reading of "no
// transitions" is "nothing changed". A monitor that reports calm because
// its collection broke is worse than one that reports nothing, because
// only the second is visibly broken. The fire fails, the error is logged
// against the tick, observability counts it as an errored fire, and the
// cadence continues to the next one — no model call is spent on a
// question mast could not gather the facts for.
//
// Notifying an operator that the MONITORING ITSELF is failing is a real
// gap and is W4.5's, where the egress client lives. Until then the
// signal is the errored-fire counter and the log line, which is what
// every other unattended failure in mast surfaces as.
func (c *monitorCollector) collect(ctx context.Context, sessionID string) (map[string]any, error) {
	if !c.enabled() {
		return nil, nil
	}
	start := c.clock()
	cctx := newCollectContext(ctx, c.appName, c.userID, sessionID)
	out := make(map[string]any, len(c.calls))
	names := make([]string, 0, len(c.calls))
	for _, call := range c.calls {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("collection of %q: %w", call.Key(), err)
		}
		result, err := c.run(cctx, call.Tool, call.Args)
		if err != nil {
			return nil, err
		}
		out[call.Key()] = result
		names = append(names, call.Key())
	}
	if c.logger != nil {
		// Deliberately does NOT claim "0 model calls". A log line mast
		// writes about itself is not evidence for the number the release
		// is claiming; the meter is, and it is read independently on two
		// surfaces. What this line is for is the operator's question —
		// did the cycle gather anything, from what, and how long did it
		// take before the model was woken.
		c.logger.Info("monitoring cycle collected before waking the model",
			"session", sessionID, "collected", names,
			"took", c.clock().Sub(start).String())
	}
	return out, nil
}
