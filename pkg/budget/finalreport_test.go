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

// The three bounds on the final-report grant, one test each, plus the
// two properties that make it safe to hand to a concurrent roster.
package budget

import (
	"sync"
	"testing"
)

func TestNoFinalReportUnlessTheWorkloadAskedForOne(t *testing.T) {
	m := New(Config{Limits: Limits{MaxTurns: 1}, Scopes: map[string]Limits{"sp": {MaxTurns: 1}}})
	if err := m.Observe(authoredEvent("sp", 10)); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if m.AllowFinalReport("sp") {
		t.Error("an unconfigured meter granted a final report; the overshoot is opt-in " +
			"because a cap that can be exceeded by one call is not what every operator declared")
	}
}

func TestTheFinalReportIsGrantedOncePerAgent(t *testing.T) {
	m := New(Config{
		Limits:      Limits{MaxTurns: 10},
		Scopes:      map[string]Limits{"sp": {MaxTurns: 1}, "other": {MaxTurns: 1}},
		FinalReport: true,
	})
	for _, a := range []string{"sp", "other"} {
		if err := m.Observe(authoredEvent(a, 10)); err != nil {
			t.Fatalf("%s first call: %v", a, err)
		}
	}

	if !m.AllowFinalReport("sp") {
		t.Fatal("a specialist with spend on the record was refused its one report call")
	}
	if m.AllowFinalReport("sp") {
		t.Error("the grant was handed out twice. Once is the bound that keeps it out of the " +
			"invalid-report retry loop: a re-takeable grant feeds that loop a model call at a time")
	}
	// The latch is per author, not a single session-wide token — a
	// roster where one specialist is stopped must not silently spend
	// the next one's grant.
	if !m.AllowFinalReport("other") {
		t.Error("one specialist's grant consumed another's")
	}
	if got := m.FinalReportsTaken(); got != 2 {
		t.Errorf("FinalReportsTaken() = %d, want 2: the overshoot has to be reportable, "+
			"not arrive unannounced in a total", got)
	}
}

func TestNoFinalReportForAnAgentThatSpentNothing(t *testing.T) {
	// A session already at its ceiling, so the specialist's very first
	// call is refused. This is the case W10.3 settled: nothing was
	// looked at, so there is nothing to salvage and a report would have
	// to be invented.
	m := New(Config{
		Limits:      Limits{MaxTurns: 1},
		Scopes:      map[string]Limits{"sp": {MaxTurns: 5}},
		FinalReport: true,
	})
	if err := m.Observe(authoredEvent("coordinator", 10)); err != nil {
		t.Fatalf("coordinator call: %v", err)
	}
	if err := m.Allow("sp"); err == nil {
		t.Fatal("the session cap should already preclude the specialist's first call")
	}
	if m.AllowFinalReport("sp") {
		t.Error("an agent that has not made a call was granted one to report with; " +
			"there is no partial finding to salvage from an agent that never ran")
	}
	if got := m.FinalReportsTaken(); got != 0 {
		t.Errorf("FinalReportsTaken() = %d, want 0: a refused ask must not latch", got)
	}
}

// An unscoped author is metered into the session totals only, so the
// session's own spend is the only record it has. Reading zero there
// would deny the grant to every agent in a workload that declares no
// per-specialist ceilings, which is most of them.
func TestAnUnscopedAgentIsJudgedOnTheSessionsSpend(t *testing.T) {
	m := New(Config{Limits: Limits{MaxTurns: 1}, FinalReport: true})
	if m.AllowFinalReport("sp") {
		t.Fatal("granted before any call was made")
	}
	if err := m.Observe(authoredEvent("sp", 10)); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !m.AllowFinalReport("sp") {
		t.Error("an unscoped agent that has spent a call was refused its report")
	}
}

func TestANilMeterGrantsNothing(t *testing.T) {
	var m *Meter
	if m.AllowFinalReport("sp") {
		t.Error("a nil meter granted a final report; nil means unmetered, and an " +
			"unmetered call is never refused in the first place")
	}
	if got := m.FinalReportsTaken(); got != 0 {
		t.Errorf("FinalReportsTaken() on a nil meter = %d, want 0", got)
	}
}

// A fan-out roster asks from every branch at once, and the once-per-
// agent bound is only worth anything if it holds under that. Run with
// -race.
func TestConcurrentAsksHandOutExactlyOneGrant(t *testing.T) {
	m := New(Config{Limits: Limits{MaxTurns: 100}, FinalReport: true})
	if err := m.Observe(authoredEvent("sp", 10)); err != nil {
		t.Fatalf("first call: %v", err)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.AllowFinalReport("sp") {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 1 {
		t.Errorf("%d concurrent asks were granted, want exactly 1", granted)
	}
}
