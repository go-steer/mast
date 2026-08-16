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

// Tests for the v0.4 W4.1 scheduled trigger: the cadence arithmetic
// (anchored, skip-not-catch-up, bounded non-accumulating jitter), the
// durable anchor that survives a restart, and the fire path's identity
// and drain behaviour.
//
// The clock is injected throughout. A cadence test that slept would
// either take an interval per assertion or assert nothing, and neither
// is a test of arithmetic.
package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/transcript"
)

// anchorTime is a fixed, boring instant. UTC and on a second boundary
// so a failure message reads as arithmetic rather than as a timestamp.
var anchorTime = time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)

// TestNextTickIsAnchoredNotReset: every fire lands on anchor +
// k×interval for the smallest k in the future. This is the property a
// restart must not break — a daemon that came back at 03:47 and
// scheduled "an hour from now" would silently re-phase an hourly
// cadence to :47 past.
func TestNextTickIsAnchoredNotReset(t *testing.T) {
	const interval = time.Hour
	for _, tc := range []struct {
		name  string
		after time.Time
		want  time.Time
	}{
		{"at the anchor, the first tick is one interval out", anchorTime, anchorTime.Add(time.Hour)},
		{"mid-interval", anchorTime.Add(17 * time.Minute), anchorTime.Add(time.Hour)},
		{"exactly on a tick, the NEXT one", anchorTime.Add(time.Hour), anchorTime.Add(2 * time.Hour)},
		{"a hair before a tick", anchorTime.Add(time.Hour - time.Nanosecond), anchorTime.Add(time.Hour)},
		{"three days later, still on the same phase", anchorTime.Add(72*time.Hour + 47*time.Minute), anchorTime.Add(73 * time.Hour)},
		{"before the anchor: the anchor itself", anchorTime.Add(-5 * time.Minute), anchorTime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextTick(anchorTime, interval, tc.after); !got.Equal(tc.want) {
				t.Errorf("nextTick(after=%v) = %v, want %v", tc.after, got, tc.want)
			}
		})
	}
}

// TestTicksSkippedCounts: how many cadence points passed with nobody to
// run them, and which one was the last of them. The count is what the
// operator is told; the instant is what stops them being reported twice.
func TestTicksSkippedCounts(t *testing.T) {
	const interval = time.Hour
	for _, tc := range []struct {
		name    string
		last    time.Time
		now     time.Time
		want    int
		through time.Time
	}{
		{"nothing missed", anchorTime, anchorTime.Add(30 * time.Minute), 0, anchorTime},
		{"one, exactly on the tick", anchorTime, anchorTime.Add(time.Hour), 1, anchorTime.Add(time.Hour)},
		{"a weekend down", anchorTime, anchorTime.Add(50 * time.Hour), 50, anchorTime.Add(50 * time.Hour)},
		{"partial intervals do not count", anchorTime, anchorTime.Add(3*time.Hour + 59*time.Minute), 3, anchorTime.Add(3 * time.Hour)},
		{"a clock that moved backwards misses nothing", anchorTime, anchorTime.Add(-time.Hour), 0, anchorTime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, through := ticksSkipped(tc.last, interval, tc.now)
			if n != tc.want || !through.Equal(tc.through) {
				t.Errorf("ticksSkipped(last=%v, now=%v) = %d, %v; want %d, %v",
					tc.last, tc.now, n, through, tc.want, tc.through)
			}
		})
	}
}

// TestScheduledCadenceSurvivesARestartWithoutCatchingUp is the W4.1
// claim in one test: a daemon that was down across several ticks fires
// NONE of them, reports what it dropped, and resumes on the original
// phase.
func TestScheduledCadenceSurvivesARestartWithoutCatchingUp(t *testing.T) {
	const interval = time.Hour
	store := transcript.NewStore(adksession.InMemoryService(), appName)
	obs := observability.New()
	ctx := context.Background()

	// First process: anchors, then fires one tick.
	first, _ := newTestTrigger(t, store, obs, interval, 0)
	first.now = clockAt(anchorTime)
	if err := first.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first.fireDue(ctx, anchorTime.Add(interval))

	// Second process, three and a half intervals later. Whatever it
	// does, it must not run the ticks it was absent for.
	second, fired := newTestTrigger(t, store, obs, interval, 0)
	second.now = clockAt(anchorTime.Add(3*interval + 30*time.Minute))
	if err := second.seed(ctx); err != nil {
		t.Fatalf("seed after restart: %v", err)
	}
	if !second.anchor.Equal(anchorTime) {
		t.Fatalf("anchor after restart = %v, want the persisted %v — the cadence re-phased", second.anchor, anchorTime)
	}

	p := second.plan(second.now())
	if p.Skipped != 2 {
		t.Errorf("skipped = %d, want the 2 ticks that came due while the daemon was down", p.Skipped)
	}
	if want := anchorTime.Add(2 * interval); !p.SkippedFrom.Equal(want) {
		t.Errorf("skipped from = %v, want %v", p.SkippedFrom, want)
	}
	if want := anchorTime.Add(3 * interval); !p.SkippedThrough.Equal(want) {
		t.Errorf("skipped through = %v, want %v", p.SkippedThrough, want)
	}
	if want := anchorTime.Add(4 * interval); !p.Tick.Equal(want) {
		t.Errorf("next tick = %v, want %v — the phase the anchor sets, not an interval from boot", p.Tick, want)
	}
	if got := len(*fired); got != 0 {
		t.Errorf("the restarted process fired %d times while planning; a missed tick is skipped, not caught up", got)
	}

	// Planning again reports nothing: the skipped ticks were coalesced
	// away, so a busy loop cannot re-report them.
	if again := second.plan(second.now()); again.Skipped != 0 {
		t.Errorf("second plan reported %d skipped ticks again", again.Skipped)
	}
}

// TestScheduledMissedTicksAreCounted: the ticks that did not run are
// the only evidence a scheduled workload has stopped working, so each
// one lands on the metric the operator can alert on.
func TestScheduledMissedTicksAreCounted(t *testing.T) {
	const interval = time.Hour
	store := transcript.NewStore(adksession.InMemoryService(), appName)
	obs := observability.New()
	ctx := context.Background()

	tr, fired := newTestTrigger(t, store, obs, interval, 0)
	tr.now = clockAt(anchorTime)
	if err := tr.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Run the loop once against a clock five intervals on, then stop it
	// before the fire it plans: the assertion is about the ticks it
	// declined, not the one it would run next.
	tr.now = clockAt(anchorTime.Add(5 * interval))
	tr.stop()
	tr.run(ctx)

	if got := len(*fired); got != 0 {
		t.Errorf("fires = %d, want 0 — stop must not run the tick it had planned", got)
	}
	if got := scrapeCounter(t, obs, `mast_scheduled_fires_total{outcome="missed",workload="uat-sched"}`); got != "5" {
		t.Errorf("missed ticks counted = %s, want 5", got)
	}
}

// TestScheduledJitterIsBoundedAndDoesNotAccumulate: the offset delays a
// fire, it never moves the cadence. Ten consecutive plans with the
// jitter pinned at its maximum still land every tick exactly on the
// lattice — the drift this rules out is a "nightly" job that walks an
// hour later each week.
func TestScheduledJitterIsBoundedAndDoesNotAccumulate(t *testing.T) {
	const (
		interval = time.Hour
		jitter   = 5 * time.Minute
	)
	store := transcript.NewStore(adksession.InMemoryService(), appName)
	tr, _ := newTestTrigger(t, store, observability.New(), interval, jitter)
	tr.now = clockAt(anchorTime)
	tr.jitterFrac = func() float64 { return 0.999 } // as late as jitter allows
	if err := tr.seed(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for k := 1; k <= 10; k++ {
		want := anchorTime.Add(time.Duration(k) * interval)
		p := tr.plan(tr.now())
		if !p.Tick.Equal(want) {
			t.Fatalf("tick %d = %v, want %v", k, p.Tick, want)
		}
		if p.FireAt.Before(p.Tick) || !p.FireAt.Before(p.Tick.Add(jitter)) {
			t.Fatalf("fire %d at %v, want it inside [%v, %v)", k, p.FireAt, p.Tick, p.Tick.Add(jitter))
		}
		// The next iteration's clock is the last fire's LATE instant —
		// the case where drift would creep in if the anchor moved with
		// the fire.
		tr.last = p.Tick
		tr.now = clockAt(p.FireAt)
	}
}

// TestScheduledJitterZeroFiresOnTheTick: an operator who declared an
// exact cadence gets one. (The UAT leans on this: a jittered fire time
// would make "the restarted process fired on the original cadence"
// unassertable to the second.)
func TestScheduledJitterZeroFiresOnTheTick(t *testing.T) {
	store := transcript.NewStore(adksession.InMemoryService(), appName)
	tr, _ := newTestTrigger(t, store, observability.New(), time.Hour, 0)
	tr.now = clockAt(anchorTime)
	tr.jitterFrac = func() float64 { t.Fatal("jitterFrac consulted for a zero jitter"); return 0 }
	if err := tr.seed(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := tr.plan(tr.now())
	if !p.FireAt.Equal(p.Tick) {
		t.Errorf("fire at %v, want it on the tick %v", p.FireAt, p.Tick)
	}
}

// TestScheduledFireSpendsTheTickWhateverHappens: a failed run is not
// retried and does not stall the cadence. Retrying the failed run would
// be catch-up behaviour on the least suitable occasion for it; the next
// tick is the retry, against a fresher world.
func TestScheduledFireSpendsTheTickWhateverHappens(t *testing.T) {
	const interval = time.Hour
	store := transcript.NewStore(adksession.InMemoryService(), appName)
	obs := observability.New()
	ctx := context.Background()

	tr, fired := newTestTrigger(t, store, obs, interval, 0)
	tr.now = clockAt(anchorTime)
	tr.fire = func(_ context.Context, tick time.Time) error {
		*fired = append(*fired, tick)
		return errors.New("the model was unreachable")
	}
	if err := tr.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tick := anchorTime.Add(interval)
	tr.fireDue(ctx, tick)

	if len(*fired) != 1 {
		t.Fatalf("fires = %d, want 1", len(*fired))
	}
	rec := store.Schedule(ctx, defaultUserID, tr.workload)
	if rec == nil {
		t.Fatal("no schedule record after a failed fire")
	}
	if !rec.LastTick.Equal(tick) {
		t.Errorf("last_tick = %v, want the spent tick %v — a failed run must not be re-run", rec.LastTick, tick)
	}
	if got := scrapeCounter(t, obs, `mast_scheduled_fires_total{outcome="error",workload="uat-sched"}`); got != "1" {
		t.Errorf("error fires counted = %s, want 1", got)
	}
	// The cadence is untouched: the next tick is the next lattice point,
	// not a retry of this one.
	if p := tr.plan(tick.Add(time.Minute)); !p.Tick.Equal(tick.Add(interval)) {
		t.Errorf("next tick after a failure = %v, want %v", p.Tick, tick.Add(interval))
	}
}

// TestScheduledFireRefusedWhileDraining: the same contract every other
// turn-launcher honors — no new turns once a drain has begun. The tick
// is not persisted, so the next boot counts it as missed, which is what
// happened.
func TestScheduledFireRefusedWhileDraining(t *testing.T) {
	store := transcript.NewStore(adksession.InMemoryService(), appName)
	obs := observability.New()
	ctx := context.Background()

	tr, fired := newTestTrigger(t, store, obs, time.Hour, 0)
	tr.now = clockAt(anchorTime)
	if err := tr.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tr.tracker.beginDrain(ctx)

	tr.fireDue(ctx, anchorTime.Add(time.Hour))

	if len(*fired) != 0 {
		t.Errorf("the scheduler started a turn during a drain (%d fires)", len(*fired))
	}
	if got := scrapeCounter(t, obs, `mast_scheduled_fires_total{outcome="skipped",workload="uat-sched"}`); got != "1" {
		t.Errorf("skipped fires counted = %s, want 1", got)
	}
}

// TestScheduledRunIdentifiesItselfAsTheScheduler: a run nobody asked
// for says so. The contrast with the caller-less context is the point —
// without the stamp, a scheduled turn is indistinguishable from any
// other internal one, and an approval attributed to it would read like
// a person.
func TestScheduledRunIdentifiesItselfAsTheScheduler(t *testing.T) {
	ctx := context.Background()
	if got := approverFromContext(ctx); got == schedulerIdentity {
		t.Fatalf("a bare context already reports %q; the stamp proves nothing", got)
	}
	if got := approverFromContext(schedulerContext(ctx)); got != schedulerIdentity {
		t.Errorf("approver on a scheduled fire = %q, want %q", got, schedulerIdentity)
	}
}

// TestScheduledSessionIDIsLegibleAndNotReserved: a fire's session must
// be findable in `mast sessions list` and must never collide with the
// reserved ops-row namespace.
func TestScheduledSessionIDIsLegibleAndNotReserved(t *testing.T) {
	sid := scheduledSessionID("gke-triage", anchorTime.Add(90*time.Minute))
	if want := "scheduled-gke-triage-20260816T043000Z"; sid != want {
		t.Errorf("session ID = %q, want %q", sid, want)
	}
	if transcript.IsReservedSessionID(sid) {
		t.Errorf("session ID %q lands in the reserved ops-row namespace", sid)
	}
	// Two ticks are never two names for one session.
	if a, b := scheduledSessionID("wl", anchorTime), scheduledSessionID("wl", anchorTime.Add(time.Second)); a == b {
		t.Errorf("two ticks share the session ID %q", a)
	}
}

// TestScheduledAnchorIsPersistedBeforeTheFirstFire: a daemon redeployed
// more often than its cadence fires would otherwise re-anchor forever
// and never fire at all.
func TestScheduledAnchorIsPersistedBeforeTheFirstFire(t *testing.T) {
	store := transcript.NewStore(adksession.InMemoryService(), appName)
	ctx := context.Background()

	tr, _ := newTestTrigger(t, store, observability.New(), 24*time.Hour, 0)
	tr.now = clockAt(anchorTime)
	if err := tr.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := store.Schedule(ctx, defaultUserID, tr.workload)
	if rec == nil {
		t.Fatal("no schedule record after seeding; the anchor was not persisted")
	}
	if !rec.Anchor.Equal(anchorTime) {
		t.Errorf("persisted anchor = %v, want %v", rec.Anchor, anchorTime)
	}
	if rec.Fires != 0 {
		t.Errorf("fires = %d before anything fired", rec.Fires)
	}

	// A restart an hour later keeps the anchor rather than taking its
	// own boot time.
	next, _ := newTestTrigger(t, store, observability.New(), 24*time.Hour, 0)
	next.now = clockAt(anchorTime.Add(time.Hour))
	if err := next.seed(ctx); err != nil {
		t.Fatalf("seed after restart: %v", err)
	}
	if !next.anchor.Equal(anchorTime) {
		t.Errorf("anchor after restart = %v, want %v", next.anchor, anchorTime)
	}
}

// TestScheduledFireCountAccumulatesAcrossProcesses: the record answers
// "how often has this schedule run", not "how often has this process".
func TestScheduledFireCountAccumulatesAcrossProcesses(t *testing.T) {
	const interval = time.Hour
	store := transcript.NewStore(adksession.InMemoryService(), appName)
	ctx := context.Background()

	first, _ := newTestTrigger(t, store, observability.New(), interval, 0)
	first.now = clockAt(anchorTime)
	if err := first.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first.fireDue(ctx, anchorTime.Add(interval))
	first.fireDue(ctx, anchorTime.Add(2*interval))

	second, _ := newTestTrigger(t, store, observability.New(), interval, 0)
	second.now = clockAt(anchorTime.Add(3 * interval))
	if err := second.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	second.fireDue(ctx, anchorTime.Add(3*interval))

	rec := store.Schedule(ctx, defaultUserID, "uat-sched")
	if rec == nil {
		t.Fatal("no schedule record")
	}
	if rec.Fires != 3 {
		t.Errorf("fires = %d, want 3 across both processes", rec.Fires)
	}
}

// ---- helpers --------------------------------------------------------

// newTestTrigger builds a trigger over a recording fire callback,
// returning the slice the callback appends its ticks to.
func newTestTrigger(t *testing.T, store *transcript.Store, obs *observability.Registry, interval, jitter time.Duration) (*scheduledTrigger, *[]time.Time) {
	t.Helper()
	const workloadName = "uat-sched"
	fired := &[]time.Time{}
	tracker := newTurnTracker(store, discardLogger(), obs, workloadName)
	tr := newScheduledTrigger(store, discardLogger(), obs, tracker, workloadName, defaultUserID, interval, jitter,
		func(_ context.Context, tick time.Time) error {
			*fired = append(*fired, tick)
			return nil
		})
	return tr, fired
}

// clockAt freezes the injected clock at one instant.
func clockAt(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// scrapeCounter reads one sample's value out of the registry's own
// /metrics output — the same surface Prometheus reads, so the test
// cannot pass on a counter the exporter does not publish.
func scrapeCounter(t *testing.T, obs *observability.Registry, sample string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	obs.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, sample+" ") {
			return strings.TrimSpace(strings.TrimPrefix(line, sample))
		}
	}
	t.Fatalf("no sample %q in:\n%s", sample, rec.Body.String())
	return ""
}
