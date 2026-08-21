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
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/runner"

	"github.com/go-steer/mast/pkg/auth"
	"github.com/go-steer/mast/pkg/monitor"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// The scheduled trigger (v0.4 W4.1): a workload that declares
// `edge_trigger.scheduled.interval` wakes itself on that cadence, with
// no inbound POST and no external cron. Until now the only way into a
// mast daemon was somebody (or something) calling it.
//
// Three decisions shape everything below.
//
// A MISSED TICK IS SKIPPED, NOT CAUGHT UP. If the daemon is down across
// three ticks it fires none of them and resumes on the anchored
// cadence, logging one line that says how many it dropped and over what
// window. A periodic run samples the CURRENT state of the world — the
// cluster, the bill, the queue depth — so a sample nobody took is owed
// to nobody; running it now produces the same answer the next tick will
// produce in a moment. The failure mode this avoids is worse than a
// missing sample: catch-up on boot means a crash-looping daemon spends
// money on a fresh backlog of model runs at every restart, and the
// crash is usually what the backlog is about.
//
// This deliberately differs from the timed-pause scheduler, which DOES
// catch up: pausesched.go's boot scan fires every expired timer
// immediately. That is not an inconsistency. A timed pause is a promise
// about one specific parked session — "come back to this at 09:00" —
// that nobody else will keep, and dropping it strands the session
// forever. A cadence makes no promise about any individual tick.
//
// THE CADENCE IS ANCHORED, NOT RESET. Fires land on anchor + k×interval
// for the smallest k in the future, with the anchor persisted
// (pkg/transcript/schedule.go). A restart resumes the original phase;
// it does not restart the clock. Otherwise a daemon that is redeployed
// twice a day would silently re-phase its nightly sweep to the middle
// of the afternoon.
//
// FIRES ARE JITTERED, THE ANCHOR IS NOT. Each fire is offset by a
// bounded random delay so N replicas started by one rollout do not all
// wake on the same second (see workload.ScheduledTrigger.Jitter). The
// offset applies to the fire only and is re-drawn each time, so it
// cannot accumulate into drift — tick k is still anchor + k×interval no
// matter how late tick k-1 ran.
//
// SINGLE-INSTANCE, like the timed-pause scheduler and for the same
// reason: mast assumes one daemon per session store (the single-writer
// rule). Two replicas of a scheduled workload each keep their own
// cadence and both fire. Multi-replica leader election is explicitly
// out of scope (docs/deployment-design.md).

// schedulerIdentity is the caller mast presents for a run nobody
// requested. It is deliberately in the namespaced "mast:" form no
// human identity can take, so a scheduled run is never mistaken for an
// operator's: a mutating call inside one still parks at the write gate
// for a real approver, and if that approval is ever attributed, the
// audit line reads mast:scheduler rather than a person who was asleep.
const schedulerIdentity = "mast:scheduler"

// schedulerContext stamps the scheduler's identity on a fire's context.
// Every turn-launching path sets its caller; this one has no request to
// take it from, so it says who it is explicitly rather than falling
// through to approverFromContext's "mast:internal" default, which
// cannot be told apart from any other caller-less internal turn.
func schedulerContext(ctx context.Context) context.Context {
	return auth.WithCaller(ctx, auth.Caller{Identity: schedulerIdentity})
}

// scheduledPayload is the envelope a scheduled fire opens its session
// with. It carries the tick the run is for, so a run can tell how stale
// its own trigger is and so two fires are never confusable in the
// transcript.
//
// Shaped like the inject envelope on purpose: the root agent sees a
// JSON object after a leading keyword, so a roster written for injects
// needs no new parsing to handle a scheduled wake-up.
//
// The bundle's prompt is deliberately NOT a field here. It is the
// workload author's instruction to the model, not data about the
// wake-up, and folding prose into a JSON string escapes whatever the
// author wrote — every quote, every brace — leaving the model to unquote
// its own instructions. It follows the envelope as its own paragraph
// instead.
type scheduledPayload struct {
	Kind     string `json:"kind"`
	Workload string `json:"workload"`
	Tick     string `json:"tick"`

	// Collected is what the workload's monitor.collect block gathered
	// before this turn started, keyed by the bundle's `as:` names (v0.5
	// W4.2). Absent on a workload that declares no monitor block.
	//
	// It rides in the envelope rather than arriving as a second message
	// because a cycle's facts and the tick they were sampled at are one
	// thing: a roster that reads the transitions has to be able to say
	// WHICH fire they belong to, and two messages can be interleaved by
	// a resume. The results are passed through exactly as the tools
	// returned them — mast does not summarize, re-key or reclassify
	// them, because what a transition means belongs to the tool that
	// classified it.
	Collected map[string]any `json:"collected,omitempty"`

	// Transitions is the run-to-run classification, parsed, when the
	// workload named a source for it with monitor.transitions_from
	// (v0.5 W4.4). Absent otherwise — including on a workload that
	// collects plenty and classifies nothing.
	//
	// Parsed rather than raw, and NOT repeated under Collected: the
	// records the model reads are the same records mast decided the
	// cycle's notification from, so an operator reading the transcript
	// and an operator reading the notification are looking at one
	// answer. The classes inside are the producer's own strings — see
	// pkg/monitor for why there is no vocabulary here to compare them
	// against.
	Transitions *monitor.Set `json:"transitions,omitempty"`
}

// scheduledSessionID names the session one tick's run owns.
//
// A fresh session per fire, not one long-lived session the cadence
// keeps appending to: a periodic run is a fresh sample of the world,
// and a session that accumulated every sweep since the daemon started
// would grow its own prompt without bound and carry last week's
// conclusions into today's. The tick is in the ID (to the second, and
// ticks are at least a second apart) so the sessions sort, and so a
// re-fire of one tick could only ever reuse its own row.
func scheduledSessionID(workload string, tick time.Time) string {
	return fmt.Sprintf("scheduled-%s-%s", workload, tick.UTC().Format("20060102T150405Z"))
}

// nextTick returns the first cadence point strictly after `after`:
// anchor + k×interval for the smallest such k. An `after` before the
// anchor yields the anchor itself, which is the only case where the
// returned tick is not one interval into the future — a schedule whose
// anchor is in the future has not started yet.
func nextTick(anchor time.Time, interval time.Duration, after time.Time) time.Time {
	if interval <= 0 {
		return time.Time{}
	}
	elapsed := after.Sub(anchor)
	if elapsed < 0 {
		return anchor
	}
	k := elapsed/interval + 1
	return anchor.Add(k * interval)
}

// ticksSkipped counts the cadence points in (last, now] and returns the
// latest of them: the ticks that came due with nobody to run them,
// because the daemon was down or because the previous run overran its
// own interval.
//
// `last` must itself be a cadence point (or the anchor), which is what
// makes this plain division rather than a search: the points after it
// are last + i×interval.
func ticksSkipped(last time.Time, interval time.Duration, now time.Time) (int, time.Time) {
	if interval <= 0 || last.IsZero() || !now.After(last) {
		return 0, last
	}
	n := int(now.Sub(last) / interval)
	if n <= 0 {
		return 0, last
	}
	return n, last.Add(time.Duration(n) * interval)
}

// tickPlan is one iteration of the cadence: the tick to fire, the
// jittered instant to fire it at, and the ticks coalesced away to get
// there.
type tickPlan struct {
	Tick   time.Time
	FireAt time.Time

	Skipped        int
	SkippedFrom    time.Time
	SkippedThrough time.Time
}

// scheduledTrigger drives one workload's cadence. One goroutine, one
// workload, one durable record.
type scheduledTrigger struct {
	workload string
	userID   string
	interval time.Duration
	jitter   time.Duration

	store   *transcript.Store
	logger  *slog.Logger
	obs     *observability.Registry
	tracker *turnTracker
	fire    func(ctx context.Context, tick time.Time) error

	// now and jitterFrac are the injection points that let the cadence
	// be tested at all: arithmetic that reads the wall clock can only be
	// tested with sleeps, and a unit test that sleeps for an interval is
	// a unit test that either takes minutes or proves nothing.
	// jitterFrac returns a value in [0,1).
	now        func() time.Time
	jitterFrac func() float64

	anchor time.Time
	last   time.Time

	stopOnce sync.Once
	stopping chan struct{}
}

func newScheduledTrigger(store *transcript.Store, logger *slog.Logger, obs *observability.Registry, tracker *turnTracker, workloadName, userID string, interval, jitter time.Duration, fire func(context.Context, time.Time) error) *scheduledTrigger {
	return &scheduledTrigger{
		workload:   workloadName,
		userID:     userID,
		interval:   interval,
		jitter:     jitter,
		store:      store,
		logger:     logger,
		obs:        obs,
		tracker:    tracker,
		fire:       fire,
		now:        func() time.Time { return time.Now().UTC() },
		jitterFrac: rand.Float64,
		stopping:   make(chan struct{}),
	}
}

// seed loads the persisted cadence, or anchors a new one and writes it
// down before the first fire.
//
// Writing at seed rather than at first fire is the part that is easy to
// get wrong: a daemon that only persisted an anchor once it had fired
// would re-anchor on every restart that happened inside the first
// interval, so a workload redeployed more often than it fires would
// never fire at all.
//
// A write failure is reported, not fatal. The cadence still runs on the
// in-memory anchor; what is lost is its survival of a restart, and
// refusing to schedule at all would trade a degraded trigger for no
// trigger.
func (t *scheduledTrigger) seed(ctx context.Context) error {
	if rec := t.store.Schedule(ctx, t.userID, t.workload); rec != nil {
		t.anchor = rec.Anchor.UTC()
		t.last = rec.LastTick.UTC()
		if t.last.IsZero() {
			// Anchored by an earlier boot that never got to fire. The
			// anchor is the last accounted-for point.
			t.last = t.anchor
		}
		t.logger.Info("scheduled trigger resumed its persisted cadence",
			"workload", t.workload, "anchor", t.anchor.Format(time.RFC3339Nano),
			"interval", t.interval.String(), "jitter", t.jitter.String(),
			"last_tick", t.last.Format(time.RFC3339Nano), "fires", rec.Fires)
		return nil
	}
	t.anchor = t.now().UTC()
	t.last = t.anchor
	t.logger.Info("scheduled trigger anchored",
		"workload", t.workload, "anchor", t.anchor.Format(time.RFC3339Nano),
		"interval", t.interval.String(), "jitter", t.jitter.String(),
		"next_tick", nextTick(t.anchor, t.interval, t.anchor).Format(time.RFC3339Nano))
	return t.persist(ctx, time.Time{}, 0)
}

// plan computes the next fire and folds in whatever the cadence missed
// since the last accounted-for tick. It advances `last` past the missed
// ticks — coalescing them — so they are reported once and never again.
func (t *scheduledTrigger) plan(now time.Time) tickPlan {
	var p tickPlan
	if n, through := ticksSkipped(t.last, t.interval, now); n > 0 {
		p.Skipped, p.SkippedFrom, p.SkippedThrough = n, t.last.Add(t.interval), through
		t.last = through
	}
	p.Tick = nextTick(t.anchor, t.interval, now)
	p.FireAt = p.Tick.Add(t.jitterOffset())
	return p
}

// jitterOffset draws this fire's offset in [0, jitter).
func (t *scheduledTrigger) jitterOffset() time.Duration {
	if t.jitter <= 0 {
		return 0
	}
	return time.Duration(t.jitterFrac() * float64(t.jitter))
}

// run is the cadence loop. It exits on ctx (the daemon's turn lifetime)
// or on stop; the caller owns the channel that says it has.
func (t *scheduledTrigger) run(ctx context.Context) {
	for {
		p := t.plan(t.now())
		if p.Skipped > 0 {
			for i := 0; i < p.Skipped; i++ {
				t.obs.ScheduledFire(t.workload, observability.ScheduledFireMissed)
			}
			// One line, whatever the count: the operator needs to know
			// the cadence lost time and how much, not to have their log
			// filled with one entry per tick a weekend of downtime cost.
			t.logger.Info("scheduled ticks skipped rather than caught up",
				"workload", t.workload, "ticks", p.Skipped,
				"from", p.SkippedFrom.Format(time.RFC3339Nano),
				"through", p.SkippedThrough.Format(time.RFC3339Nano),
				"next_tick", p.Tick.Format(time.RFC3339Nano))
		}
		delay := p.FireAt.Sub(t.now())
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-t.stopping:
			timer.Stop()
			return
		case <-timer.C:
		}
		t.fireDue(ctx, p.Tick)
	}
}

// stop asks the loop to exit without waiting for the current timer.
// Idempotent, so the drain path can call it whatever else has happened.
func (t *scheduledTrigger) stop() {
	t.stopOnce.Do(func() { close(t.stopping) })
}

// fireDue runs one tick and records that the tick is spent.
//
// The tick is consumed whatever the outcome — a failed run is not
// retried. Retrying would be the catch-up behaviour this scheduler
// rejects, on the least appropriate occasion for it: the run that just
// failed. The next tick is the retry, and it samples a fresher world.
func (t *scheduledTrigger) fireDue(ctx context.Context, tick time.Time) {
	t.last = tick
	log := t.logger.With("workload", t.workload, "tick", tick.Format(time.RFC3339Nano))

	// Same drain contract as every other turn-launcher (inject, resume,
	// attach, auto-resume, the timed-pause scheduler): stop launching
	// new turns the moment a drain begins. Nothing is persisted on this
	// path — the tick did not run, so letting the next boot count it as
	// missed is the truthful record, and a drain is no time to be
	// writing to the store.
	if ctx.Err() != nil || t.tracker.isDraining() {
		t.obs.ScheduledFire(t.workload, observability.ScheduledFireSkipped)
		log.Info("scheduled fire skipped; the daemon is draining")
		return
	}

	start := t.now()
	err := t.fire(ctx, tick)
	outcome := observability.ScheduledFireRan
	if err != nil {
		outcome = observability.ScheduledFireError
	}
	t.obs.ScheduledFire(t.workload, outcome)
	if err != nil {
		log.Error("scheduled fire failed; the cadence continues",
			"session", scheduledSessionID(t.workload, tick), "error", err.Error())
	} else {
		// The per-fire audit line. It names the session so an operator
		// can go read the run, and the tick so two logs from two
		// processes can be read as one cadence.
		log.Info("scheduled trigger fired",
			"session", scheduledSessionID(t.workload, tick),
			"caller", schedulerIdentity,
			"took", t.now().Sub(start).String())
	}

	// Persist AFTER the run, and on the error path too: the record's job
	// is to say which ticks are accounted for, not which ones succeeded.
	// A crash between the fire and this write costs at most a re-report
	// of one missed tick on the next boot, never a duplicate run — the
	// next boot's next tick is still in the future.
	if perr := t.persist(ctx, start, 1); perr != nil {
		log.Warn("scheduled trigger could not persist its cadence; a restart may re-phase it",
			"error", perr.Error())
	}
}

// persist writes the current cadence state, adding fires to the count
// carried in the existing record.
//
// The count is re-read rather than kept in memory so that a record
// written by a previous process is continued rather than reset — the
// operator reading it wants "this schedule has fired 412 times", not
// "this process has fired twice".
func (t *scheduledTrigger) persist(ctx context.Context, lastFire time.Time, fired int) error {
	rec := transcript.ScheduleRecord{
		Workload: t.workload,
		Interval: t.interval.String(),
		Anchor:   t.anchor,
		LastTick: t.last,
		LastFire: lastFire,
	}
	if prev := t.store.Schedule(ctx, t.userID, t.workload); prev != nil {
		rec.Fires = prev.Fires
		if lastFire.IsZero() {
			rec.LastFire = prev.LastFire
		}
	}
	rec.Fires += fired
	// Not on the caller's cancellable context: this write is bookkeeping
	// about a tick that already happened, and losing it because the run
	// it describes was cancelled is how a cadence re-phases itself.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeWriteTimeout)
	defer cancel()
	return t.store.SaveSchedule(wctx, t.userID, rec)
}

// newScheduledFireCallback builds what a tick actually does: collect on
// mast's own behalf if the workload declares a monitor block, then open
// a fresh session owned by the daemon's user, stamp the scheduler's
// identity on it, and drive one turn through the same chokepoint every
// other turn kind uses (runTurnPre) — no privileged side path, so the
// abort/gate-pause refusals, the turn lock, the budget meter, the
// watchdog, and the write gate all apply to a scheduled run exactly as
// they do to an injected one.
//
// The collection happens BEFORE runTurnPre, not inside it, and that
// ordering is the whole of W4.2: everything runTurnPre touches is about
// a model call, and the collection leg's claim is that it is not one.
// A collection failure therefore returns before a turn is launched, so
// the tick costs nothing and the failure is visible as an errored fire
// rather than as a model run that concluded nothing was wrong.
func newScheduledFireCallback(r *runner.Runner, logger *slog.Logger, store *transcript.Store, meters *meterPool, wds *watchdogPool, obs *observability.Registry, tracker *turnTracker, turnLocks *sessionTurnLocks, workloadName string, bundle *workload.Bundle, collector *monitorCollector, ensure func(string)) func(context.Context, time.Time) error {
	var prompt string
	if bundle != nil && bundle.EdgeTrigger.Scheduled != nil {
		prompt = bundle.EdgeTrigger.Scheduled.Prompt
	}
	return func(fireCtx context.Context, tick time.Time) error {
		sessionID := scheduledSessionID(workloadName, tick)
		if transcript.IsReservedSessionID(sessionID) {
			// Only reachable through a workload name ending in the
			// reserved ops-row suffix. Refusing here keeps the rule that
			// a marker row is never driven as a session true for the one
			// session ID mast composes for itself.
			return fmt.Errorf("scheduled session ID %q uses the reserved ops-row suffix; rename the workload", sessionID)
		}
		ctx := schedulerContext(fireCtx)
		// The same wallclock ceiling inject and resume turns run under
		// (#47). A periodic run is the easiest kind to leave wedged,
		// because nobody is waiting for its answer.
		if bundle != nil && bundle.Budget.MaxWallclockSeconds > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(bundle.Budget.MaxWallclockSeconds)*time.Second)
			defer cancel()
		}
		// The collection leg. It runs under the fire's context, so the
		// wallclock ceiling above bounds the whole cycle rather than
		// only the model's half of it — a wedged MCP server is exactly
		// the way an unattended cycle stops without anyone noticing.
		facts, err := collector.collect(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("monitoring cycle for tick %s: %w", tick.UTC().Format(time.RFC3339), err)
		}
		body, err := json.Marshal(scheduledPayload{
			Kind:        "scheduled",
			Workload:    workloadName,
			Tick:        tick.UTC().Format(time.RFC3339),
			Collected:   facts.Collected,
			Transitions: facts.Transitions,
		})
		if err != nil {
			return fmt.Errorf("marshal scheduled payload: %w", err)
		}
		text := fmt.Sprintf("SCHEDULED %s", string(body))
		if prompt != "" {
			text += "\n\n" + prompt
		}
		// Registered for attach before the turn, like inject: an
		// unattended run is the one an operator most wants to be able to
		// tail while it happens.
		ensure(sessionID)
		msg := genai.NewContentFromText(text, genai.RoleUser)
		return runTurnPre(ctx, r, logger, store, meters, wds, obs, tracker, turnLocks,
			workloadName, sessionID, msg, "scheduled:"+tick.UTC().Format(time.RFC3339), nil, nil)
	}
}
