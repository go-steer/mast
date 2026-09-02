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

package budget

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The attribution is what W10.3 acts on, so it has to hold for both
// enforcement errors and for neither of the things it is not.
func TestScopeAttributesAnEnforcementErrorToItsSpecialist(t *testing.T) {
	m := New(Config{
		Limits: Limits{MaxTurns: 100, MaxTokens: 1_000_000},
		Scopes: map[string]Limits{"sp": {MaxTurns: 1, MaxTokens: 5}},
	})

	// Crossed: the fold reports a specialist's own ceiling.
	crossed := m.Observe(authoredEvent("sp", 50))
	if !errors.Is(crossed, ErrExceeded) {
		t.Fatalf("Observe = %v, want ErrExceeded", crossed)
	}
	if got, ok := Scope(crossed); !ok || got != "sp" {
		t.Errorf("Scope(crossed) = %q, %v; want \"sp\", true", got, ok)
	}

	// Refused: the gate reports the same ceiling one call earlier.
	refused := m.Allow("sp")
	if !errors.Is(refused, ErrRefused) {
		t.Fatalf("Allow = %v, want ErrRefused", refused)
	}
	if got, ok := Scope(refused); !ok || got != "sp" {
		t.Errorf("Scope(refused) = %q, %v; want \"sp\", true", got, ok)
	}
}

// The session's own ceiling is the absence of an attribution rather than
// an attribution to "". A caller branching on ok would otherwise route
// around a ceiling with nothing on the other side of it.
func TestScopeReportsFalseForAWorkloadCeiling(t *testing.T) {
	m := NewMeter(Limits{MaxTurns: 1})
	if err := m.Observe(usageEvent(10)); err != nil {
		t.Fatalf("first call: %v", err)
	}

	refused := m.Allow("some-agent")
	if !errors.Is(refused, ErrRefused) {
		t.Fatalf("Allow = %v, want ErrRefused", refused)
	}
	if got, ok := Scope(refused); ok {
		t.Errorf("Scope(session refusal) = %q, true; want \"\", false", got)
	}

	crossed := m.Observe(usageEvent(10))
	if !errors.Is(crossed, ErrExceeded) {
		t.Fatalf("Observe = %v, want ErrExceeded", crossed)
	}
	if got, ok := Scope(crossed); ok {
		t.Errorf("Scope(session crossing) = %q, true; want \"\", false", got)
	}
	if _, ok := Scope(errors.New("something else")); ok {
		t.Error("Scope claimed an error that is not ours")
	}
	if _, ok := Scope(nil); ok {
		t.Error("Scope claimed a nil error")
	}
}

// The wrapper is invisible to everything that read these errors before
// it existed. Both are load-bearing outside this package: turn drivers
// match the sentinel with errors.Is, and pkg/attach's classifier matches
// the message prefix (a weak contract kept on purpose — #135/#208).
func TestScopingAnErrorChangesNeitherSentinelNorText(t *testing.T) {
	inner := fmt.Errorf("%w: specialist %q: over", ErrRefused, "sp")
	wrapped := scopedTo("sp", inner)

	if !errors.Is(wrapped, ErrRefused) {
		t.Error("the scoped error no longer unwraps to ErrRefused; every driver matching the sentinel stops seeing it")
	}
	if wrapped.Error() != inner.Error() {
		t.Errorf("message changed: %q, was %q", wrapped, inner)
	}
	if !strings.HasPrefix(wrapped.Error(), "budget refused") {
		t.Errorf("message %q no longer carries the prefix pkg/attach classifies on", wrapped)
	}
	if got := scopedTo("", inner); got != inner {
		t.Errorf("scopedTo(\"\") wrapped anyway: %v", got)
	}
	if got := scopedTo("sp", nil); got != nil {
		t.Errorf("scopedTo wrapped a nil error: %v", got)
	}
}

// Refusals is the reporting count and SessionRefusals is the stopping
// one, and the split is the whole of W10.3 at the meter: a driver that
// stopped on the first would lose the session to a specialist, and one
// that reported only the second would let a workload quietly shed half
// its roster in silence.
func TestSessionRefusalsCountOnlyTheWorkloadsOwn(t *testing.T) {
	m := New(Config{
		Limits: Limits{MaxTurns: 3},
		Scopes: map[string]Limits{"sp": {MaxTurns: 1}},
	})
	if err := m.Observe(authoredEvent("sp", 10)); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Two refusals for the specialist; the session still has runway.
	for i := 0; i < 2; i++ {
		if err := m.Allow("sp"); !errors.Is(err, ErrRefused) {
			t.Fatalf("Allow(sp) #%d = %v, want ErrRefused", i+1, err)
		}
	}
	if n, _ := m.Refusals(); n != 2 {
		t.Errorf("Refusals = %d, want 2", n)
	}
	if n, first := m.SessionRefusals(); n != 0 {
		t.Errorf("SessionRefusals = %d (%v), want 0: the workload has turns left", n, first)
	}
	if len(m.PrecludedSession()) != 0 {
		t.Errorf("PrecludedSession = %+v, want empty", m.PrecludedSession())
	}

	// Spend the session's cap, and now the workload itself is out.
	for i := 0; i < 2; i++ {
		if err := m.Observe(usageEvent(10)); err != nil {
			t.Fatalf("session call %d: %v", i+1, err)
		}
	}
	if err := m.Allow("other"); !errors.Is(err, ErrRefused) {
		t.Fatalf("Allow(other) = %v, want ErrRefused", err)
	}
	n, first := m.SessionRefusals()
	if n != 1 {
		t.Errorf("SessionRefusals = %d, want 1", n)
	}
	if _, scoped := Scope(first); scoped {
		t.Errorf("the first session refusal is attributed to a specialist: %v", first)
	}
	if all, _ := m.Refusals(); all != 3 {
		t.Errorf("Refusals = %d, want 3 — every refusal still counts for reporting", all)
	}
	if len(m.PrecludedSession()) == 0 {
		t.Error("PrecludedSession is empty on a workload that cannot make another call")
	}
	// And it reports the session's alone even now that both are out.
	for _, tr := range m.PrecludedSession() {
		if tr.Scope != "" {
			t.Errorf("PrecludedSession returned a specialist's ceiling: %+v", tr)
		}
	}
}

// The first reason is kept per counter, not per meter. A turn usually
// refuses a specialist before it refuses the workload, so a driver
// reading Refusals's first reason to explain why the turn *stopped*
// would name the ceiling that did not stop it.
func TestTheFirstSessionReasonIsNotTheFirstReason(t *testing.T) {
	m := New(Config{
		Limits: Limits{MaxTurns: 1},
		Scopes: map[string]Limits{"sp": {MaxTurns: 1}},
	})
	if err := m.Observe(authoredEvent("sp", 10)); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// The specialist is out on its own cap; so, now, is the session,
	// since that one call counted against both.
	if err := m.Allow("sp"); !errors.Is(err, ErrRefused) {
		t.Fatalf("Allow(sp) = %v, want ErrRefused", err)
	}
	if err := m.Allow("other"); !errors.Is(err, ErrRefused) {
		t.Fatalf("Allow(other) = %v, want ErrRefused", err)
	}

	_, first := m.Refusals()
	if got, _ := Scope(first); got != "sp" {
		t.Errorf("the first refusal is attributed to %q, want \"sp\"", got)
	}
	_, firstSession := m.SessionRefusals()
	if _, scoped := Scope(firstSession); scoped {
		t.Errorf("the first session refusal is a specialist's: %v", firstSession)
	}
	if firstSession == first {
		t.Error("the two counters kept the same reason; the stop would be explained by a ceiling that did not cause it")
	}
}

// PrecludedAfter is to TripsAfter what Precluded is to crossed, and a
// reset needs the earlier one: a grant that lands the target exactly on
// its new cap has crossed nothing and cleared nothing.
func TestPrecludedAfterProjectsAGrantOneCallEarlierThanTripsAfter(t *testing.T) {
	m := New(Config{Scopes: map[string]Limits{"sp": {MaxTurns: 2}}})
	for i := 0; i < 3; i++ {
		_ = m.Observe(authoredEvent("sp", 10))
	}

	// +1 turn puts the cap at 3 against 3 calls made: nothing is over,
	// and nothing more can be run either.
	exact := Limits{MaxTurns: 1}
	if got := m.TripsAfter("sp", exact); len(got) != 0 {
		t.Errorf("TripsAfter = %+v, want empty: 3 calls against a cap of 3 has crossed nothing", got)
	}
	after := m.PrecludedAfter("sp", exact)
	if len(after) == 0 {
		t.Fatal("PrecludedAfter is empty for a grant that buys no call; the operator would get a 200 and a refusal")
	}
	for _, tr := range after {
		if tr.Scope != "sp" || tr.Dimension != DimensionTurns {
			t.Errorf("unexpected trip %+v", tr)
		}
	}

	// +2 buys a call, so the grant clears.
	if got := m.PrecludedAfter("sp", Limits{MaxTurns: 2}); len(got) != 0 {
		t.Errorf("PrecludedAfter = %+v after a grant that buys a call, want empty", got)
	}
	// And none of it mutated anything.
	if l, _ := m.ScopeLimits("sp"); l.MaxTurns != 2 {
		t.Errorf("the projection moved the ceiling to %d, want it left at 2", l.MaxTurns)
	}
}

func TestANilMeterHasNoScopeState(t *testing.T) {
	var m *Meter
	if n, first := m.SessionRefusals(); n != 0 || first != nil {
		t.Errorf("a nil meter reported %d session refusals (%v)", n, first)
	}
	if tr := m.PrecludedSession(); tr != nil {
		t.Errorf("a nil meter reported session preclusions: %+v", tr)
	}
	if tr := m.PrecludedAll(); tr != nil {
		t.Errorf("a nil meter reported preclusions: %+v", tr)
	}
	if tr := m.PrecludedAfter("sp", Limits{MaxTurns: 1}); tr != nil {
		t.Errorf("a nil meter projected a grant: %+v", tr)
	}
}
