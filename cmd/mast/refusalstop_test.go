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
	"testing"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/watchdog"
)

// The gate reaches the stop through the context, so what the daemon
// installs has to satisfy the interface the gate looks for. A compile
// error here is the wiring breaking.
var _ approval.TurnStop = (*refusalStop)(nil)

func TestRefusalStopCancelsTheTurnAndKeepsTheReason(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &refusalStop{}
	s.arm(cancel)
	if s.why() != nil {
		t.Fatalf("a stop nobody pulled reports %v, want nil", s.why())
	}

	want := &approval.RefusalLoopError{Tool: "scale_deployment", Key: "k", Count: 3}
	s.StopTurn(want)

	<-ctx.Done() // the run is stopped, and as far as it knows, cancelled
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
	// Which is exactly why the reason has to survive separately: the
	// run's own error says nothing about a refusal.
	if got := s.why(); got != error(want) {
		t.Fatalf("why() = %v, want the reason it was stopped with", got)
	}
}

// A second suppression can land before the cancellation propagates, and
// it carries a higher count. The operator must read the number that was
// logged, not a later one.
func TestRefusalStopKeepsTheFirstReason(t *testing.T) {
	t.Parallel()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &refusalStop{}
	s.arm(cancel)
	first := &approval.RefusalLoopError{Tool: "scale_deployment", Key: "k", Count: 3}
	s.StopTurn(first)
	s.StopTurn(&approval.RefusalLoopError{Tool: "scale_deployment", Key: "k", Count: 4})

	if got := s.why(); got != error(first) {
		t.Fatalf("why() = %v, want the first reason", got)
	}
}

// Nothing in mast should reach a stop before its run exists, but the
// stop must not panic if something does: recording a reason it cannot
// act on is the right behaviour when there is no run to stop.
func TestRefusalStopWithoutARunRecordsAnyway(t *testing.T) {
	t.Parallel()
	s := &refusalStop{}
	want := &approval.RefusalLoopError{Tool: "t", Key: "k", Count: 3}
	s.StopTurn(want)
	if got := s.why(); got != error(want) {
		t.Fatalf("why() = %v, want the reason", got)
	}
}

// The #449 correction, and the one that is easy to get backwards: the
// gate's scrub clears the watchdog's evidence and must never clear its
// verdict. An arm that could un-halt a session would let a looping
// agent overrule the operator by looping harder.
func TestForgetToolRunClearsSignalsAndNotTheHalt(t *testing.T) {
	t.Parallel()
	const sid = "s1"
	wp := newWatchdogPool(watchdog.ModeEnforce)

	// Trip the session the way a real loop does: the same call over and
	// over until the repeated-call signal fires, then feed the alert to
	// the enforcer.
	wd := wp.watchdog(sid)
	for i := 0; i < watchdog.DefaultRepeatThreshold; i++ {
		wd.ObserveToolCall(watchdog.ToolCall{Name: "scale_deployment", Args: `{"replicas":10}`})
	}
	alerts := wd.Check()
	if len(alerts) == 0 {
		t.Fatalf("%d identical calls raised no alert; the signal this test depends on did not fire",
			watchdog.DefaultRepeatThreshold)
	}
	if !wp.enforcer(sid).Observe(alerts[0]) {
		t.Fatal("enforce mode did not trip on a Critical alert")
	}
	if halted, _ := wp.halted(sid); !halted {
		t.Fatal("session is not halted after a trip; nothing below would mean anything")
	}

	wp.forgetToolRun(sid)

	if halted, reason := wp.halted(sid); !halted {
		t.Fatalf("forgetToolRun cleared the halt (reason now %q) — the gate must not be able to un-halt a session", reason)
	}
	if err := wp.preflight(sid); err == nil {
		t.Fatal("the session accepts turns again after forgetToolRun; the halt was scrubbed along with the evidence")
	}

	// And the evidence really is gone: the run length restarts, so the
	// next identical call is the first in a row rather than the sixth.
	wd.ObserveToolCall(watchdog.ToolCall{Name: "scale_deployment", Args: `{"replicas":10}`})
	if got := wd.Check(); len(got) != 0 {
		t.Fatalf("one call after the scrub raised %d alert(s), want 0 — the run lengths were not cleared", len(got))
	}
}

// The other direction, for contrast: an operator's own reset clears all
// of it. The two methods differ by one line and the difference is the
// whole point, so both are pinned.
func TestResetClearsTheHaltForgetToolRunLeaves(t *testing.T) {
	t.Parallel()
	const sid = "s1"
	wp := newWatchdogPool(watchdog.ModeEnforce)
	wd := wp.watchdog(sid)
	for i := 0; i < watchdog.DefaultRepeatThreshold; i++ {
		wd.ObserveToolCall(watchdog.ToolCall{Name: "scale_deployment", Args: `{"replicas":10}`})
	}
	alerts := wd.Check()
	if len(alerts) == 0 {
		t.Fatal("no alert to trip on")
	}
	wp.enforcer(sid).Observe(alerts[0])

	wp.reset(sid)

	if halted, _ := wp.halted(sid); halted {
		t.Fatal("an operator reset left the session halted")
	}
	if err := wp.preflight(sid); err != nil {
		t.Fatalf("preflight after reset = %v, want nil", err)
	}
}
