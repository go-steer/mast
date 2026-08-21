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

	"github.com/go-steer/mast/pkg/monitor"
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
//
// # W4.4: reading the classification without owning it
//
// The third point is the one W4.4 had to hold onto while making the
// classification usable. A cycle that only carries opaque tool output
// cannot decide whether anything changed, and "notify only on change"
// (W4.5) is a decision mast has to make on its own at 3am.
//
// The seam that satisfies both is `monitor.transitions_from`: the
// bundle names WHICH collected result is the classification, mast
// parses it for shape, and every judgement inside it stays the
// producer's. What mast gains is the ability to say "this cycle
// classified four things and one of them is new"; what mast still
// cannot say is whether a given finding should have been called new.
// See pkg/monitor — the checks there are all "is this a whole answer",
// never "is this the right answer".

// monitorCollector runs a workload's declared collection calls at the
// top of each scheduled fire.
type monitorCollector struct {
	calls   []workload.MonitorCollect
	run     func(ctx adkagent.Context, name string, args map[string]any) (map[string]any, error)
	logger  *slog.Logger
	appName string
	userID  string

	// transitionsKey is the collect key whose result is parsed as the
	// run-to-run classification, or "" for a workload that collects raw
	// facts and lets the model read them (v0.5 W4.4).
	transitionsKey string

	// now is the injection point for the duration in the log line, the
	// same seam scheduledTrigger uses and for the same reason.
	now func() time.Time
}

func newMonitorCollector(logger *slog.Logger, mon workload.Monitor, run func(adkagent.Context, string, map[string]any) (map[string]any, error), appName, userID string) *monitorCollector {
	return &monitorCollector{
		calls:          mon.Collect,
		run:            run,
		logger:         logger,
		appName:        appName,
		userID:         userID,
		transitionsKey: mon.TransitionsKey(),
		now:            func() time.Time { return time.Now().UTC() },
	}
}

// cycleFacts is everything one fire gathered before the model was woken.
//
// Two fields rather than one map because they are read by different
// things: Collected is for the model, which gets it in the envelope and
// reasons over it, and Transitions is for mast, which has to decide
// whether this cycle is worth waking anyone about (W4.5) without
// understanding a word of it.
type cycleFacts struct {
	// Collected is the raw results keyed by the bundle's `as:` names.
	// The classification's own raw result is NOT here — see Transitions.
	Collected map[string]any

	// Transitions is the parsed classification, or nil if the workload
	// named none. Nil and empty are different answers: nil is "this
	// workload does not classify", empty is "it classified, and nothing
	// changed".
	Transitions *monitor.Set
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
// A MALFORMED CLASSIFICATION ABORTS IT TOO, for the same reason and
// with more force. The whole point of naming a transitions source is
// that mast reads it; a truncated or unparseable answer read leniently
// becomes an empty transition set, and an empty transition set is the
// wire for "all quiet". W4.5 will decline to notify on exactly that. So
// the parse is strict and its failure ends the cycle — see pkg/monitor
// for what "malformed" is allowed to mean, which is never "a class mast
// has not heard of".
func (c *monitorCollector) collect(ctx context.Context, sessionID string) (cycleFacts, error) {
	if !c.enabled() {
		return cycleFacts{}, nil
	}
	start := c.clock()
	cctx := newCollectContext(ctx, c.appName, c.userID, sessionID)
	out := make(map[string]any, len(c.calls))
	names := make([]string, 0, len(c.calls))
	facts := cycleFacts{}
	for _, call := range c.calls {
		if err := ctx.Err(); err != nil {
			return cycleFacts{}, fmt.Errorf("collection of %q: %w", call.Key(), err)
		}
		result, err := c.run(cctx, call.Tool, call.Args)
		if err != nil {
			return cycleFacts{}, err
		}
		if key := call.Key(); key != "" && key == c.transitionsKey {
			set, err := monitor.ParseResult(result)
			if err != nil {
				return cycleFacts{}, fmt.Errorf("monitor.transitions_from names %q, collected from %q: %w", key, call.Tool, err)
			}
			// Filed under Transitions and NOT also under Collected. The
			// model reads one spelling of this fact — the parsed one —
			// because a wake-up carrying both the records and the text
			// they were read from invites the model to reconcile two
			// copies of the same answer, and to reach for the raw text
			// the moment the two look different.
			facts.Transitions = &set
			names = append(names, key)
			continue
		}
		out[call.Key()] = result
		names = append(names, call.Key())
	}
	if len(out) > 0 {
		facts.Collected = out
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
		if facts.Transitions != nil {
			// The tally is built from the classes that turned up, not
			// from a list mast keeps — so a class lookout ships after
			// this line was written appears in it, correctly counted,
			// with no change here. `scanned` is on the line because a
			// cycle with nothing changed and nothing scanned is a
			// broken monitor, and the two are indistinguishable
			// without it.
			c.logger.Info("monitoring cycle classified what changed",
				"session", sessionID, "source", c.transitionsKey,
				"scanned", facts.Transitions.Scanned,
				"transitions", len(facts.Transitions.Transitions),
				"classes", facts.Transitions.Classes())
		}
	}
	return facts, nil
}
