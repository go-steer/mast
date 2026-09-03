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

// The daemon half of W10.2: a ceiling reached *during* a turn.
//
// budget_durable_test.go covers the door — a session that arrives already
// unable to make a call is refused with a 409. This covers the other
// case, where the turn starts with headroom and runs out mid-flight. It
// is the one that needed a decision, because a refusal is a synthesized
// answer: the stream ends cleanly, the session has an answer in it, and
// runTurn would return nil to a caller whose work did not happen.
//
// Note the root is built through mastagent.NewCoordinator rather than
// llmagent.New, which is the point rather than an incidental. The gate is
// installed by the constructor, so a harness that skips the constructor
// is testing an agent the daemon never builds.

package main

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/watchdog"
	"github.com/go-steer/mast/pkg/workload"
)

// cappedTurnHarness is newTurnHarnessOpts with two changes: a turn
// ceiling on the meter pool, and a root built the way buildRoot builds
// one.
func cappedTurnHarness(t *testing.T, m *loopingModel, maxTurns int) *turnHarness {
	t.Helper()
	b := &workload.Bundle{}
	b.Budget.MaxTurns = maxTurns

	h := &turnHarness{
		svc:    adksession.InMemoryService(),
		locks:  newSessionTurnLocks(),
		meters: newMeterPool(b, nil, "", "test-model"),
		wds:    newWatchdogPool(watchdog.ModeWarn),
		obs:    observability.New(),
	}
	h.obs.Prime("(test)")
	h.store = transcript.NewStore(h.svc, appName)
	h.tracker = newTurnTracker(h.store, discardLogger(), h.obs, "(test)")

	root, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name:        "precall_agent",
		Description: "pre-call ceiling test agent",
		Instruction: "answer",
		Model:       m,
		Tools:       []tool.Tool{pokeTool(t)},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	h.runner, err = runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    h.svc,
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return h
}

// A turn that reaches its ceiling mid-flight stops there and says so.
//
// loopingModel wants three calls to finish (two tool rounds then an
// answer). max_turns is 2, so the third is refused. The assertions are
// the two halves that could each be right on their own and wrong
// together: the provider was asked exactly twice, *and* the caller was
// told — a silent completion would satisfy the first and hide the
// second.
func TestATurnThatReachesItsCeilingMidFlightIsReported(t *testing.T) {
	m := &loopingModel{rounds: 2}
	h := cappedTurnHarness(t, m, 2)

	err := h.turn(context.Background(), "s-midturn")
	if err == nil {
		t.Fatal("a turn stopped by its turn cap returned nil; the caller cannot tell it from work that finished")
	}
	if !errors.Is(err, budget.ErrRefused) {
		t.Errorf("err = %v, want budget.ErrRefused", err)
	}
	if errors.Is(err, budget.ErrExceeded) {
		t.Errorf("err = %v, want it NOT to claim a ceiling was crossed: the call was never made", err)
	}
	if got := m.calls.Load(); got != 2 {
		t.Errorf("the provider was asked for %d calls under max_turns: 2, want exactly 2", got)
	}
	if _, _, calls := h.meters.meter("s-midturn").Snapshot(); calls != 2 {
		t.Errorf("the meter counted %d model calls, want 2", calls)
	}
	if !strings.Contains(err.Error(), "cap of 2") {
		t.Errorf("err = %v, want the reason to name the ceiling", err)
	}
}

// The metric has to move. mast_budget_trips_total is what budgets.md
// tells an operator to alert on, and a refusal that raised no trip would
// make the *better* enforcement look like a quieter deployment — the
// alert going silent exactly when the ceiling started working.
func TestARefusalCountsAsABudgetTrip(t *testing.T) {
	m := &loopingModel{rounds: 2}
	h := cappedTurnHarness(t, m, 2)

	if err := h.turn(context.Background(), "s-trip"); !errors.Is(err, budget.ErrRefused) {
		t.Fatalf("turn: %v, want budget.ErrRefused", err)
	}
	if got := tripCount(t, h.obs); got != 1 {
		t.Errorf("mast_budget_trips_total = %d after a refusal, want 1", got)
	}
}

// A turn with room to finish is not touched by any of this: it runs, it
// completes, it reports nil, and it raises no trip. Without this the
// pair above is satisfied by a gate that refuses everything.
func TestATurnWithHeadroomIsUnaffected(t *testing.T) {
	m := &loopingModel{rounds: 2}
	h := cappedTurnHarness(t, m, 10)

	if err := h.turn(context.Background(), "s-headroom"); err != nil {
		t.Fatalf("a turn under a max_turns: 10 ceiling returned %v, want nil", err)
	}
	if got := m.calls.Load(); got != 3 {
		t.Errorf("the provider was asked for %d calls, want the 3 this fixture needs to finish", got)
	}
	if got := tripCount(t, h.obs); got != 0 {
		t.Errorf("mast_budget_trips_total = %d on a turn that finished, want 0", got)
	}
}

// The second turn on a session the first one stopped never starts: the
// meter is per-session and carries the spend, so preflight refuses at the
// door with the 409 that names the reset endpoint. Retrying is the
// scheduler's default behaviour, so this is the path most sessions
// actually take.
func TestTheTurnAfterARefusalIsRefusedAtTheDoor(t *testing.T) {
	m := &loopingModel{rounds: 2}
	h := cappedTurnHarness(t, m, 2)
	ctx := context.Background()

	if err := h.turn(ctx, "s-again"); !errors.Is(err, budget.ErrRefused) {
		t.Fatalf("first turn: %v, want budget.ErrRefused", err)
	}
	before := m.calls.Load()

	err := h.turn(ctx, "s-again")
	if !errors.Is(err, inject.ErrConflict) {
		t.Errorf("second turn err = %v, want inject.ErrConflict (409)", err)
	}
	if got := m.calls.Load(); got != before {
		t.Errorf("the retried turn made %d more model calls, want 0", got-before)
	}
	if !strings.Contains(err.Error(), "guardrails/reset") {
		t.Errorf("refusal = %q, does not tell the operator how to clear it", err)
	}
}

// spinner is an agent that keeps asking for model calls whatever it is
// told — the shape of every retry loop that sits above the model and
// reacts to a bad answer by asking again. The write gate's change-set
// contract is the real one: it hands a specialist its refused report
// back and gives it another turn.
//
// Through v0.5 a loop like this was bounded by the thing it was burning.
// A refusal costs nothing, so it is bounded by nothing, and the pre-call
// gate turns "expensive and self-limiting" into "free and infinite"
// unless the turn driver stops the stream itself.
//
// hardCap is the test's own tourniquet: without it a regression here
// hangs the suite instead of failing it.
type spinner struct {
	name    string
	tokens  int32
	hardCap int

	mu       sync.Mutex
	asked    int
	refused  int
	hitCap   bool
	postGate int // calls asked for after the first refusal
}

func (s *spinner) run(ictx adkagent.InvocationContext) iter.Seq2[*adksession.Event, error] {
	return func(yield func(*adksession.Event, error) bool) {
		gate := mastagent.CallGateFrom(ictx)
		for i := 0; ; i++ {
			if i >= s.hardCap {
				s.mu.Lock()
				s.hitCap = true
				s.mu.Unlock()
				return
			}
			if ictx.Err() != nil {
				return
			}
			var err error
			if gate != nil {
				err = gate.Allow(s.name)
			}
			s.mu.Lock()
			s.asked++
			if s.refused > 0 {
				s.postGate++
			}
			if err != nil {
				s.refused++
			}
			refusedNow := err != nil
			s.mu.Unlock()

			ev := &adksession.Event{Author: s.name}
			// A refused call reports no usage, because it never happened.
			// That is the whole reason this loop does not stop on its own.
			if !refusedNow {
				ev.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: s.tokens}
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (s *spinner) stats() (asked, refused, postGate int, hitCap bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked, s.refused, s.postGate, s.hitCap
}

// A retry loop above a refused call does not spin.
//
// The assertion that matters is postGate: how many more calls the loop
// got to ask for after the ceiling first said no. One is the refusal the
// driver is reacting to; the rest would be free calls in an unbounded
// loop, and 200 of them is the suite noticing a hang instead of hanging.
func TestARefusedCallDoesNotLeaveARetryLoopSpinning(t *testing.T) {
	s := &spinner{name: "spinner", tokens: 100, hardCap: 200}
	root, err := adkagent.New(adkagent.Config{Name: "spinner", Description: "asks forever", Run: s.run})
	if err != nil {
		t.Fatalf("adkagent.New: %v", err)
	}

	b := &workload.Bundle{}
	b.Budget.MaxTurns = 2
	h := &turnHarness{
		svc:    adksession.InMemoryService(),
		locks:  newSessionTurnLocks(),
		meters: newMeterPool(b, nil, "", "test-model"),
		wds:    newWatchdogPool(watchdog.ModeWarn),
		obs:    observability.New(),
	}
	h.obs.Prime("(test)")
	h.store = transcript.NewStore(h.svc, appName)
	h.tracker = newTurnTracker(h.store, discardLogger(), h.obs, "(test)")
	h.runner, err = runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    h.svc,
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	turnErr := h.turn(context.Background(), "s-spin")

	asked, refused, postGate, hitCap := s.stats()
	if hitCap {
		t.Fatalf("the loop ran to its own %d-call tourniquet: the turn never stopped it (asked=%d refused=%d)", s.hardCap, asked, refused)
	}
	if !errors.Is(turnErr, budget.ErrRefused) {
		t.Errorf("turn err = %v, want budget.ErrRefused", turnErr)
	}
	if refused == 0 {
		t.Fatalf("the ceiling never refused anything after %d calls; the fixture is not measuring the gate", asked)
	}
	if postGate > 1 {
		t.Errorf("the loop asked for %d more calls after the first refusal, want at most 1: a refusal is free, so nothing else bounds this", postGate)
	}
}

// tripCount reads mast_budget_trips_total off an actual scrape, summed
// across labels — the test only cares that the counter moved, and a
// scrape is the artifact an operator's alert reads.
func tripCount(t *testing.T, obs *observability.Registry) int {
	t.Helper()
	rec := httptest.NewRecorder()
	obs.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: status %d, want 200", rec.Code)
	}
	var n int
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "mast_budget_trips_total{") {
			continue
		}
		fields := strings.Fields(line)
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		n += int(v)
	}
	return n
}
