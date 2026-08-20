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

// Originally derived from go-steer/core-agent@6510a65b54ead93b5f2c8c31f478443376203360:pkg/agent/watchdog.go

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
// The postures are a ladder, and each rung includes the one below it:
// warn logs, feedback also tells the model, enforce also stops it.
//
// Detection is identical in all three. Every signal observes, tallies,
// and alerts the same way — the mode only decides the reaction, which
// is why a workload can be switched between them without changing what
// gets found.
type Mode string

const (
	// ModeWarn logs the alert and lets the turn run. The bottom rung, and
	// what an empty value parses to — but not what a host picks when
	// nobody chose; see DefaultMode for why those are different
	// questions.
	ModeWarn Mode = "warn"

	// ModeFeedback warns, and additionally routes each alert's Guidance
	// into the session's next prompt. The party that can stop making the
	// looping call is the model making it, and under warn alone it is
	// the one party never told.
	ModeFeedback Mode = "feedback"

	// ModeEnforce feeds back, and additionally halts on a Critical
	// alert: the turn in flight is cancelled and every subsequent turn
	// is refused until an operator resets, the same contract the budget
	// ceiling keeps.
	//
	// Enforce implies feedback on purpose. Without it, the turn after a
	// reset starts with the model knowing nothing about why it was
	// stopped, which is a treadmill: loop, halt, reset, loop.
	ModeEnforce Mode = "enforce"
)

// DefaultMode is the rung a host picks when nothing declared one — no
// flag, no bundle field, no explicit argument.
//
// This is a different question from ParseMode's, and the two answers
// differ on purpose. ParseMode is handed a value and asked what it
// means; the safe reading of an empty value is the bottom rung, because
// a parse must never arm a kill switch out of nothing. A host choosing
// a default is instead asked what to do when nobody chose, and for mast
// the honest answer is not warn: every mast run is unattended, so a
// warning goes to a log nobody is tailing and warn is indistinguishable
// from off. Feedback routes the same observation to the one party
// present at the scene — the model — and its false-positive cost is a
// paragraph, not a halted workload. Hosts that want the halt say so.
const DefaultMode = ModeFeedback

// ParseMode validates an operator-supplied posture. An empty string is
// ModeWarn — an unset flag must not silently arm a kill switch.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeWarn:
		return ModeWarn, nil
	case ModeFeedback:
		return ModeFeedback, nil
	case ModeEnforce:
		return ModeEnforce, nil
	default:
		return "", fmt.Errorf("unknown watchdog mode %q (want %q, %q or %q)", s, ModeWarn, ModeFeedback, ModeEnforce)
	}
}

// Enforces reports whether this mode halts on a Critical alert.
func (m Mode) Enforces() bool { return m == ModeEnforce }

// Feeds reports whether this mode routes alert Guidance back into the
// model's next turn. True for enforce as well as feedback: the rungs
// accumulate.
func (m Mode) Feeds() bool { return m == ModeFeedback || m == ModeEnforce }

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

// TurnErrorKind declares how a host's operator surface should label
// this, implementing attach.SelfClassifyingError without importing it —
// this package stays stdlib-only, and the string is pinned against
// attach's constant in enforce_test.go.
//
// It exists because Error() is the wrong thing to classify a halt by:
// Reason is built from the offending tool's name and its model-supplied
// arguments, and a substring scan over that let the agent choose its
// own label (#208).
func (e *TrippedError) TurnErrorKind() string { return "watchdog_halt" }

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

// Adopt restores a halt this Enforcer did not observe — the trip a
// previous process recorded before it died — and reports whether it
// took effect.
//
// Signal and reason are carried over verbatim rather than
// reconstructed, so a restored halt says what the original said. An
// operator reading "watchdog halted this session (tool_failure_streak)"
// after a pod roll is reading the sentence the halt was written with,
// not a paraphrase of it.
//
// Two refusals, both deliberate:
//
// Adopt is a no-op unless the current mode enforces. The persisted trip
// is history; the mode is configuration, and configuration still wins.
// A deployment that has since been dialed back to feedback — or that
// restarted with a different bundle — must not inherit a halt it would
// no longer produce, or a posture change becomes unreachable: the
// process refuses turns because of a trip only enforce mode could have
// made, and only a turn could clear.
//
// Adopt is also a no-op on an already-tripped Enforcer, so a restore
// racing a live halt cannot overwrite the fresher reason with the
// stored one.
func (e *Enforcer) Adopt(signal, reason string) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.mode.Enforces() || e.tripped {
		return false
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		// A halt with no sentence is worse than no halt: the operator
		// gets a refusal that cannot explain itself. Synthesize the
		// least-bad one rather than latch silence.
		reason = strings.TrimSpace(fmt.Sprintf(
			"watchdog halted this session (%s) before a restart. %s", signal, e.remedy))
	}
	e.tripped = true
	e.signal = signal
	e.reason = reason
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
