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

// Originally derived from go-steer/core-agent@635a9eb75bc9b8c3cc6463e794893e887cfd1e0f:pkg/agent/watchdog.go

// Enforce mode: the posture that acts on a Critical alert.
//
// Upstream keeps this state on its Agent struct, behind
// WithWatchdogEnforce / ResetWatchdog / preflightWatchdog. mast has no
// Agent — its turns run through free functions over a runner event
// stream — so the state lives in an Enforcer the caller holds per
// session, and the halt itself is the caller's cancel. That split is
// the point: this package decides *whether* a session is halted and
// why; it never decides how a turn dies.

package watchdog

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Mode is the watchdog's posture: what the deployment does when a
// signal trips.
//
// Detection is identical in both. Every signal observes, tallies, and
// alerts the same way — the mode only decides the reaction, which is
// why a workload can be switched between them without changing what
// gets found.
type Mode string

const (
	// ModeWarn logs the alert and lets the turn run. mast's default,
	// because a false positive that stops an unattended incident
	// responder mid-triage is worse than one that annotates it.
	ModeWarn Mode = "warn"

	// ModeEnforce halts on a Critical alert: the turn in flight is
	// cancelled and every subsequent turn is refused until an operator
	// resets, the same contract the budget ceiling keeps.
	ModeEnforce Mode = "enforce"
)

// ParseMode validates an operator-supplied posture. An empty string is
// ModeWarn — an unset flag must not silently arm a kill switch.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeWarn:
		return ModeWarn, nil
	case ModeEnforce:
		return ModeEnforce, nil
	default:
		return "", fmt.Errorf("unknown watchdog mode %q (want %q or %q)", s, ModeWarn, ModeEnforce)
	}
}

// Enforces reports whether this mode halts on a Critical alert.
func (m Mode) Enforces() bool { return m == ModeEnforce }

// TrippedError is what a caller returns from a turn the watchdog
// halted, and what Preflight returns for every turn after it. A
// distinct type so a host can tell "an operator must reset this" apart
// from a failure worth retrying: retrying a watchdog trip re-drives the
// loop that caused it, which is the exact failure enforce mode exists
// to break.
type TrippedError struct {
	// Signal is the alert that halted the session.
	Signal string
	// Reason is the operator-facing text, alert included.
	Reason string
}

func (e *TrippedError) Error() string { return e.Reason }

// IsTripped reports whether err is a watchdog halt. Uses errors.As, so
// a caller may wrap it with turn context without losing the
// classification.
func IsTripped(err error) bool {
	var t *TrippedError
	return errors.As(err, &t)
}

// Enforcer holds one session's halt state.
//
// One per session, alongside the Watchdog itself: the signals count
// across turns, and so does the trip. A daemon-global enforcer would
// let one runaway session refuse turns for every other one.
//
// Safe for concurrent use. The alert path runs from the event tap
// while an attach handler can be reading Tripped or calling Reset.
type Enforcer struct {
	mu      sync.Mutex
	mode    Mode
	remedy  string
	tripped bool
	signal  string
	reason  string
}

// NewEnforcer returns an Enforcer in the given posture. The zero Mode
// is ModeWarn, so a caller that forgets to set one gets the harmless
// default rather than an armed kill switch.
//
// remedy is appended to the halt reason and answers the only question
// an operator reading it has: how do I clear this? It is the caller's
// to supply because the answer is host-specific — the daemon names its
// reset endpoint, a one-shot has none to name. Empty is fine.
func NewEnforcer(mode Mode, remedy string) *Enforcer {
	if mode == "" {
		mode = ModeWarn
	}
	return &Enforcer{mode: mode, remedy: remedy}
}

// Mode reports the configured posture.
func (e *Enforcer) Mode() Mode {
	if e == nil {
		return ModeWarn
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

// Observe records one alert and reports whether it halts the session.
//
// True exactly once per trip: the first Critical alert under
// ModeEnforce. Later alerts on an already-tripped session return false
// so the caller cancels a turn once rather than on every remaining
// event — the same idempotence upstream's maybeTripWatchdog keeps.
// Non-Critical alerts never halt, whatever the mode.
func (e *Enforcer) Observe(a Alert) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.mode.Enforces() || a.Severity != SeverityCritical || e.tripped {
		return false
	}
	e.tripped = true
	e.signal = a.Signal
	e.reason = strings.TrimSpace(fmt.Sprintf(
		"watchdog halted this session (%s): %s %s", a.Signal, a.Reason, e.remedy))
	return true
}

// Tripped reports whether the session is halted, and why. The reason
// is "" when it is not.
func (e *Enforcer) Tripped() (bool, string) {
	if e == nil {
		return false, ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tripped, e.reason
}

// Preflight returns a non-nil *TrippedError when the session is
// halted. Callers run it at the top of a turn, before any model call:
// the refusal has to be structural, or an auto-resume or a scheduled
// re-fire of the halted session re-drives the very loop that tripped
// it.
func (e *Enforcer) Preflight() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.tripped {
		return nil
	}
	return &TrippedError{Signal: e.signal, Reason: e.reason}
}

// Reset clears the halt. Safe when nothing tripped.
//
// It does not reset the Watchdog's signals — the caller owns both and
// resets them together, because clearing the trip while the signal
// still holds a completed run would re-halt on the next call.
func (e *Enforcer) Reset() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tripped = false
	e.signal = ""
	e.reason = ""
}
