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

// Originally derived from go-steer/core-agent@635a9eb75bc9b8c3cc6463e794893e887cfd1e0f:pkg/agent/watchdog_enforce_test.go

package watchdog

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func critical(signal string) Alert {
	return Alert{Signal: signal, Severity: SeverityCritical, Reason: "looping on " + signal + "."}
}

func warn(signal string) Alert {
	return Alert{Signal: signal, Severity: SeverityWarn, Reason: "advisory about " + signal + "."}
}

func TestParseMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeWarn, false},
		{"warn", ModeWarn, false},
		{"enforce", ModeEnforce, false},
		{"Enforce", "", true},
		{"halt", "", true},
		{"prompt", "", true}, // a deferred mode is not a valid one
	}
	for _, tc := range tests {
		got, err := ParseMode(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseMode(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// An unset posture must not arm a kill switch. This is the direction a
// default has to be wrong in.
func TestNewEnforcer_ZeroModeIsWarn(t *testing.T) {
	t.Parallel()
	e := NewEnforcer("", "")
	if e.Mode() != ModeWarn {
		t.Errorf("Mode = %q, want warn", e.Mode())
	}
	if e.Observe(critical("repeated-tool-call")) {
		t.Error("a zero-mode enforcer halted; the default must observe only")
	}
}

func TestEnforcer_WarnNeverHalts(t *testing.T) {
	t.Parallel()
	e := NewEnforcer(ModeWarn, "")
	for range 10 {
		if e.Observe(critical("repeated-tool-call")) {
			t.Fatal("warn mode halted on a Critical alert")
		}
	}
	if tripped, _ := e.Tripped(); tripped {
		t.Error("warn mode recorded a trip")
	}
	if err := e.Preflight(); err != nil {
		t.Errorf("warn mode refused a turn: %v", err)
	}
}

func TestEnforcer_HaltsOnCriticalAndRefusesTheNextTurn(t *testing.T) {
	t.Parallel()
	e := NewEnforcer(ModeEnforce, "Clear it with POST /sessions/s1/guardrails/reset.")

	if err := e.Preflight(); err != nil {
		t.Fatalf("healthy session refused a turn: %v", err)
	}
	if !e.Observe(critical("alternating-tool-cycle")) {
		t.Fatal("a Critical alert under enforce must halt")
	}

	tripped, reason := e.Tripped()
	if !tripped {
		t.Fatal("Tripped = false after a halt")
	}
	for _, want := range []string{"alternating-tool-cycle", "guardrails/reset"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason omits %q — an operator has to be told what stopped it and how to clear it: %q", want, reason)
		}
	}

	err := e.Preflight()
	if err == nil {
		t.Fatal("a halted session accepted the next turn — the refusal has to be structural, or auto-resume re-drives the loop")
	}
	if !IsTripped(err) {
		t.Errorf("IsTripped(%v) = false; a host would retry a watchdog halt", err)
	}
	var te *TrippedError
	if !errors.As(err, &te) || te.Signal != "alternating-tool-cycle" {
		t.Errorf("TrippedError = %+v, want the halting signal named", te)
	}
	if !IsTripped(fmt.Errorf("turn %q: %w", "t3", err)) {
		t.Error("IsTripped must see through a wrap; callers add turn context")
	}
	if IsTripped(errors.New("plain")) {
		t.Error("IsTripped matched an unrelated error")
	}
}

// A host with no reset surface — the one-shot path — supplies no
// remedy, and the reason must still read as a sentence rather than as
// one with a hole where the advice goes.
func TestEnforcer_EmptyRemedyLeavesNoDanglingText(t *testing.T) {
	t.Parallel()
	e := NewEnforcer(ModeEnforce, "")
	e.Observe(critical("repeated-tool-call"))
	_, reason := e.Tripped()
	if strings.TrimSpace(reason) != reason {
		t.Errorf("reason = %q, want no padding from the absent remedy", reason)
	}
	if !strings.Contains(reason, "looping on repeated-tool-call.") {
		t.Errorf("reason = %q, want the alert's own text", reason)
	}
}

// One cancel per trip. The alert path runs on every remaining event of
// an unwinding turn, and re-cancelling a turn already cancelled is
// pointless work.
func TestEnforcer_HaltsExactlyOnce(t *testing.T) {
	t.Parallel()
	e := NewEnforcer(ModeEnforce, "")
	halts := 0
	for range 20 {
		if e.Observe(critical("repeated-tool-call")) {
			halts++
		}
	}
	if halts != 1 {
		t.Errorf("halts = %d, want 1", halts)
	}
}

// The evidence-gap signal must not stop a run, whatever the posture.
func TestEnforcer_WarnAlertsNeverHaltUnderEnforce(t *testing.T) {
	t.Parallel()
	e := NewEnforcer(ModeEnforce, "")
	for range 10 {
		if e.Observe(warn("tool-failure-streak")) {
			t.Fatal("a Warn alert halted under enforce")
		}
	}
	if err := e.Preflight(); err != nil {
		t.Errorf("Preflight refused after Warn alerts only: %v", err)
	}
}

func TestEnforcer_ResetClearsTheHalt(t *testing.T) {
	t.Parallel()
	e := NewEnforcer(ModeEnforce, "")
	e.Observe(critical("repeated-tool-call"))
	e.Reset()

	if tripped, reason := e.Tripped(); tripped || reason != "" {
		t.Errorf("Tripped = (%v, %q) after Reset, want (false, \"\")", tripped, reason)
	}
	if err := e.Preflight(); err != nil {
		t.Errorf("Preflight refused after Reset: %v", err)
	}
	// And it re-arms: a fresh runaway halts again.
	if !e.Observe(critical("repeated-tool-call")) {
		t.Error("Reset disarmed the enforcer instead of clearing the trip")
	}
}

func TestEnforcer_ResetOnAHealthySessionIsANoOp(t *testing.T) {
	t.Parallel()
	e := NewEnforcer(ModeEnforce, "")
	e.Reset()
	if err := e.Preflight(); err != nil {
		t.Errorf("Preflight = %v after a no-op Reset", err)
	}
}

// A nil enforcer is the "no posture configured" case at the call
// sites, and it must read as warn rather than panic in the event tap.
func TestEnforcer_NilIsWarn(t *testing.T) {
	t.Parallel()
	var e *Enforcer
	if e.Mode() != ModeWarn {
		t.Errorf("nil Mode = %q, want warn", e.Mode())
	}
	if e.Observe(critical("x")) {
		t.Error("nil enforcer halted")
	}
	if tripped, _ := e.Tripped(); tripped {
		t.Error("nil enforcer reported a trip")
	}
	if err := e.Preflight(); err != nil {
		t.Errorf("nil Preflight = %v, want nil", err)
	}
	e.Reset()
}

// The event tap observes while an attach handler reads or resets.
func TestEnforcer_ConcurrentUse(t *testing.T) {
	t.Parallel()
	e := NewEnforcer(ModeEnforce, "")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 200 {
				switch i % 4 {
				case 0:
					e.Observe(critical("repeated-tool-call"))
				case 1:
					e.Tripped()
				case 2:
					_ = e.Preflight()
				default:
					e.Reset()
				}
			}
		}(i)
	}
	wg.Wait()
}
