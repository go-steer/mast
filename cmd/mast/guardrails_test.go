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
	"errors"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/watchdog"
)

// newGuardrailView builds the projection over a hand-sized meter pool,
// the same way newMeterPool sizes one from a bundle.
func newGuardrailView(lim budget.Limits, scopes map[string]budget.Limits) *guardrailView {
	return newGuardrailViewMode(watchdog.ModeWarn, lim, scopes)
}

// newGuardrailViewMode is the same, with the watchdog posture spelled
// out — the projection reads differently under enforce.
func newGuardrailViewMode(mode watchdog.Mode, lim budget.Limits, scopes map[string]budget.Limits) *guardrailView {
	return &guardrailView{
		meters: &meterPool{
			cfg:  budget.Config{Limits: lim, Scopes: scopes},
			byID: map[string]*budget.Meter{},
		},
		wds:    newWatchdogPool(mode),
		logger: discardLogger(),
	}
}

const gsid = "incident-abc"

// A session halted by `max_turns` has spent almost nothing. Reporting
// only dollars would show "$0.0061 spent" next to a session that
// refuses every prompt, and the operator would go looking for a
// different cause.
func TestGuardrailInfoReportsTheTurnCapThatHalted(t *testing.T) {
	g := newGuardrailView(budget.Limits{MaxTurns: 2, RatePer1K: 0.05}, nil)
	m := g.meters.meter(gsid)
	for i := 0; i < 3; i++ {
		_ = m.Observe(spend("coordinator", 40))
	}

	got := g.info(gsid)
	if !got.Halted || !got.CostCeiling.Tripped {
		t.Fatalf("info = %+v, want a halted session", got)
	}
	if got.CostCeiling.MaxTurns != 2 || got.CostCeiling.Turns != 3 {
		t.Errorf("turns = %d/%d, want 3/2", got.CostCeiling.Turns, got.CostCeiling.MaxTurns)
	}
	if !strings.Contains(got.CostCeiling.Reason, "model calls") {
		t.Errorf("reason = %q, want the turn cap named", got.CostCeiling.Reason)
	}
	// Nothing sits between the accumulator and the ceiling, so a reset
	// with no grant is a no-op that re-trips on the next event — the
	// client has to know that before it offers a bare reset button.
	if !got.CostCeiling.WouldRetrip {
		t.Error("would_retrip = false on a mast trip; a bare reset cannot clear it")
	}
	// Unconfigured dimensions read as unbounded, not as "0 allowed".
	if got.CostCeiling.MaxSessionUSD != 0 || got.CostCeiling.MaxTokens != 0 {
		t.Errorf("undeclared ceilings invented: %+v", got.CostCeiling)
	}
}

// Dollars are the wrong currency for a turn cap, and the reset has to
// say so before it spends anything — a 200 here would put the session
// straight back into the same wall on the next turn.
func TestResetRefusesAGrantThatWouldNotClearTheTrip(t *testing.T) {
	g := newGuardrailView(budget.Limits{MaxTurns: 2, MaxCostUSD: 10, RatePer1K: 0.05}, nil)
	m := g.meters.meter(gsid)
	for i := 0; i < 3; i++ {
		_ = m.Observe(spend("coordinator", 40))
	}

	resp, err := g.reset(gsid, attach.GuardrailResetRequest{
		Guardrail:           attach.GuardrailCostCeiling,
		AdditionalBudgetUSD: 50,
	})
	if !errors.Is(err, attach.ErrGuardrailRetrip) {
		t.Fatalf("err = %v, want ErrGuardrailRetrip", err)
	}
	// The refusal has to be actionable: the numbers say what to raise.
	if !strings.Contains(err.Error(), "model calls") {
		t.Errorf("error = %v, want the still-crossed dimension named", err)
	}
	if !resp.Guardrails.Halted {
		t.Error("refusal carries no state; the client must re-GET to render anything")
	}
	// And it has to be free to retry: a refused reset that had already
	// banked the $50 would silently drain the operator's headroom.
	if got := m.SessionLimits(); got.MaxCostUSD != 10 || got.MaxTurns != 2 {
		t.Errorf("ceilings moved on a refused reset: %+v", got)
	}
	if len(resp.Reset) != 0 {
		t.Errorf("reset = %v on a refusal", resp.Reset)
	}
}

// The happy path, and the one that matters: after the reset the next
// turn actually runs.
func TestResetWithEnoughRunwayUnwedgesTheSession(t *testing.T) {
	g := newGuardrailView(budget.Limits{MaxTurns: 2, RatePer1K: 0.05}, nil)
	m := g.meters.meter(gsid)
	for i := 0; i < 3; i++ {
		_ = m.Observe(spend("coordinator", 40))
	}

	resp, err := g.reset(gsid, attach.GuardrailResetRequest{AdditionalTurns: 10, Caller: "op@example.com"})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(resp.Reset) != 1 || resp.Reset[0] != attach.GuardrailCostCeiling {
		t.Errorf("reset = %v, want the cost ceiling cleared", resp.Reset)
	}
	if resp.TurnsAdded != 10 {
		t.Errorf("turns_added = %d, want 10", resp.TurnsAdded)
	}
	if resp.Guardrails.Halted {
		t.Errorf("post-reset state still halted: %+v", resp.Guardrails.CostCeiling)
	}
	if err := m.Observe(spend("coordinator", 40)); err != nil {
		t.Fatalf("the next turn still trips: %v", err)
	}
	// The spend is not rewritten — /usage and a post-incident review
	// have to keep seeing every call this session made.
	if _, _, calls := m.Snapshot(); calls != 4 {
		t.Errorf("calls = %d, want the accumulator untouched at 4", calls)
	}
}

// Granting into an unbounded dimension must not create a ceiling: a
// session with no turn cap that gets "+5 turns" would be capped at 5
// and halt four calls later — a reset that wedges the session it was
// called to unwedge.
func TestResetNeverImposesANewCeiling(t *testing.T) {
	g := newGuardrailView(budget.Limits{MaxCostUSD: 1, RatePer1K: 0.05}, nil)
	resp, err := g.reset(gsid, attach.GuardrailResetRequest{AdditionalTurns: 5, AdditionalBudgetUSD: 1})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if resp.TurnsAdded != 0 {
		t.Errorf("turns_added = %d, want 0 — the dimension was unlimited", resp.TurnsAdded)
	}
	if got := g.meters.meter(gsid).SessionLimits(); got.MaxTurns != 0 {
		t.Errorf("MaxTurns = %d, want it left unlimited", got.MaxTurns)
	}
	if resp.BudgetAddedUSD != 1 {
		t.Errorf("budget_added_usd = %v, want the bounded dimension raised", resp.BudgetAddedUSD)
	}
	// Nothing was tripped, and the message has to say so rather than
	// implying a recovery happened.
	if !strings.Contains(resp.Message, "nothing was tripped") {
		t.Errorf("message = %q", resp.Message)
	}
}

// A specialist's own ceiling stops the whole run, so an operator who
// only sees the session's numbers raises the wrong budget and watches
// the session wedge again on the next turn.
func TestScopeTripIsAttributedAndClearedSeparately(t *testing.T) {
	g := newGuardrailView(
		budget.Limits{MaxCostUSD: 100, RatePer1K: 0.05},
		map[string]budget.Limits{"OOMKilled": {MaxCostUSD: 0.25, RatePer1K: 0.05}},
	)
	m := g.meters.meter(gsid)
	_ = m.Observe(spend("OOMKilled", 10_000)) // $0.50 against a $0.25 cap

	got := g.info(gsid)
	if !got.Halted {
		t.Fatal("a crossed specialist ceiling halts the run; the projection says otherwise")
	}
	if len(got.CostCeiling.Scopes) != 1 || !got.CostCeiling.Scopes[0].Tripped {
		t.Fatalf("scopes = %+v, want OOMKilled reported over", got.CostCeiling.Scopes)
	}
	if !strings.Contains(got.CostCeiling.Reason, "OOMKilled") {
		t.Errorf("session reason = %q, want the specialist named", got.CostCeiling.Reason)
	}
	// The session has $99.50 of headroom; raising it buys nothing.
	if _, err := g.reset(gsid, attach.GuardrailResetRequest{AdditionalBudgetUSD: 50}); !errors.Is(err, attach.ErrGuardrailRetrip) {
		t.Fatalf("a session grant appeared to clear a specialist trip: %v", err)
	}
	resp, err := g.reset(gsid, attach.GuardrailResetRequest{Scope: "OOMKilled", AdditionalBudgetUSD: 0.5})
	if err != nil {
		t.Fatalf("scope reset: %v", err)
	}
	if resp.Guardrails.Halted {
		t.Errorf("still halted after raising the specialist's cap: %+v", resp.Guardrails.CostCeiling)
	}
	if !strings.Contains(resp.Message, "OOMKilled") {
		t.Errorf("message = %q, want it to say whose budget moved", resp.Message)
	}
}

// A typo'd specialist name has to read as "no such scope". Surfacing
// it as a retrip would send the operator off raising numbers on a
// scope that does not exist.
func TestResetRejectsAnUnknownScope(t *testing.T) {
	g := newGuardrailView(budget.Limits{}, map[string]budget.Limits{"OOMKilled": {MaxTurns: 3}})
	_, err := g.reset(gsid, attach.GuardrailResetRequest{Scope: "OOMKiled", AdditionalTurns: 3})
	if err == nil {
		t.Fatal("unknown scope accepted")
	}
	if errors.Is(err, attach.ErrGuardrailRetrip) {
		t.Errorf("unknown scope reported as a retrip: %v", err)
	}
	if !strings.Contains(err.Error(), "OOMKilled") {
		t.Errorf("error = %v, want the real scope names listed", err)
	}
	if l, _ := g.meters.meter(gsid).ScopeLimits("OOMKilled"); l.MaxTurns != 3 {
		t.Errorf("the real scope was modified: %+v", l)
	}
}

// Under the default posture mast's watchdog only logs. Reporting it as
// an armed guardrail without saying so would promise an enforcement
// that is not there.
func TestWatchdogProjectionIsHonestlyAdvisory(t *testing.T) {
	g := newGuardrailView(budget.Limits{}, nil)
	g.wds.note(gsid, watchdog.Alert{Signal: "repeated_tool_call", Reason: "kubectl get pods x6"})
	g.wds.note(gsid, watchdog.Alert{Signal: "repeated_tool_call", Reason: "kubectl describe x6"})

	got := g.info(gsid)
	if !got.Watchdog.Advisory || got.Watchdog.Mode != string(watchdog.ModeWarn) {
		t.Errorf("watchdog = %+v, want an advisory warn-mode posture", got.Watchdog)
	}
	if got.Watchdog.Tripped {
		t.Error("an advisory watchdog reported as tripped; nothing was halted")
	}
	// The alerts are the point: watchdog.Tap drains them to the log, so
	// without this residue a session that looped six times an hour ago
	// answers identically to a healthy one.
	if got.Watchdog.Alerts != 2 || !strings.Contains(got.Watchdog.Reason, "describe") {
		t.Errorf("alerts = %d reason = %q, want 2 and the latest", got.Watchdog.Alerts, got.Watchdog.Reason)
	}
	// An advisory watchdog never halts, so it must not set Halted.
	if got.Halted {
		t.Error("halted = true on alerts alone")
	}

	resp, err := g.reset(gsid, attach.GuardrailResetRequest{Guardrail: attach.GuardrailWatchdog})
	if err != nil {
		t.Fatalf("watchdog reset: %v", err)
	}
	if len(resp.Reset) != 1 || resp.Reset[0] != attach.GuardrailWatchdog {
		t.Errorf("reset = %v, want the watchdog cleared", resp.Reset)
	}
	if got := g.info(gsid); got.Watchdog.Alerts != 0 || got.Watchdog.Reason != "" {
		t.Errorf("watchdog after reset = %+v, want cleared", got.Watchdog)
	}
}

// Budget aimed at the watchdog is rejected, not dropped: the operator
// who believes they bought runway finds out on the next turn, which is
// the worst possible moment.
func TestResetRejectsBudgetOnTheWatchdog(t *testing.T) {
	g := newGuardrailView(budget.Limits{MaxCostUSD: 1}, nil)
	_, err := g.reset(gsid, attach.GuardrailResetRequest{
		Guardrail:           attach.GuardrailWatchdog,
		AdditionalBudgetUSD: 5,
	})
	if err == nil {
		t.Fatal("budget on a watchdog reset was accepted")
	}
	if got := g.meters.meter(gsid).SessionLimits(); got.MaxCostUSD != 1 {
		t.Errorf("ceiling moved anyway: %+v", got)
	}
}

// The read must not be the thing that decides where a grant lands: if
// GET minted a second meter, the reset would raise a ceiling nothing
// meters against and the session would stay wedged.
func TestReadAndResetShareOneMeter(t *testing.T) {
	g := newGuardrailView(budget.Limits{MaxTurns: 1, RatePer1K: 0.05}, nil)
	_ = g.info(gsid)
	if _, err := g.reset(gsid, attach.GuardrailResetRequest{AdditionalTurns: 5}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	m := g.meters.meter(gsid)
	if got := m.SessionLimits().MaxTurns; got != 6 {
		t.Fatalf("MaxTurns = %d, want the grant to have landed on the metered session", got)
	}
	if got := g.info(gsid).CostCeiling.MaxTurns; got != 6 {
		t.Errorf("GET reports MaxTurns = %d after the grant", got)
	}
}
