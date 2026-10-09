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

// The in-turn half of feedback mode (#514).
//
// Feedback delivers an alert's Guidance on the session's next turn,
// which is the right place for a loop that spans turns and no place at
// all for one inside a single turn. A self-hosted Gemma 4 called one
// tool with identical arguments 441 times in one turn, until the server
// refused the context: the repeat signal tripped at five, the
// observation was queued for a next turn, and nothing reached the model
// or stopped the turn.
//
// TurnLoop closes that from the tool side, where the turn is still
// running. From the threshold-th identical call onward, each repeated
// call's result carries a note, so the model reads it before choosing
// its next call. If the model keeps repeating anyway,
// the call StopAfterNote repeats later is not made and the turn ends
// with a LoopStopError. Nothing latches: the next turn starts clean,
// with the observation in front of it like any other feedback.
//
// Turn scope is the point. The cross-turn watchdog keeps counting as it
// did; this counts per invocation and forgets when the turn ends, so a
// stop here is never a session halt. Halting the session is enforce
// mode's job, and it still does that at the first trip.

package watchdog

import (
	"fmt"
	"sync"
)

// DefaultStopAfterNote is how many more identical calls a model gets
// after the in-turn note before the turn ends. The note lands on the
// call that trips the repeat signal; the third repeat after it is the
// one that is refused.
//
// Not one, because a model mid-way through parallel calls can have
// issued the next repeat before it read the note. Not more, because
// identical arguments after an explicit "the result will not change" is
// as certain a loop as this package ever sees.
const DefaultStopAfterNote = 3

// LoopNoteKey is the field TurnLoop's note travels under in a tool
// result. Exported so hosts and transcript tooling find it without
// re-typing the literal.
const LoopNoteKey = "watchdog"

// LoopStopError is the reason a turn ended because the model kept
// making one call after being told, inside the turn, that it was
// looping.
//
// A turn error, not a session halt — compare TrippedError. It names the
// tool and the call so an operator reading a one-line turn failure
// knows which loop it was.
type LoopStopError struct {
	Tool  string
	Args  string
	Count int
}

func (e *LoopStopError) Error() string {
	return fmt.Sprintf(
		"turn ended: the model called %s with identical arguments %d times in a row and kept going after being told the result would not change (args=%s). "+
			"The last call was not made. Nothing is latched; the next turn starts clean.",
		e.Tool, e.Count, truncate(e.Args, 200))
}

// TurnErrorKind implements attach.SelfClassifyingError without
// importing it, as TrippedError does. Pinned against attach's constant
// in turnloop_test.go.
func (e *LoopStopError) TurnErrorKind() string { return "loop_stop" }

// TurnLoop tracks consecutive identical tool calls per turn, keyed by
// invocation ID, and decides when a result carries the in-turn note and
// when a call ends the turn.
//
// "Identical" is stricter here than in the repeat signal: same tool,
// path-equivalent arguments (argsEquivalent), AND the same result every
// time. The signal can afford to count calls alone, because under
// feedback its false positive costs a paragraph. This one ends a turn,
// and the false positive the watchdog's own docs name — a daemon
// polling a rollout with the same call on purpose — is exactly a run of
// identical calls whose results change. A result that changed breaks
// the run.
//
// One per runner: invocation IDs are unique across sessions, so it
// needs no session awareness. Safe for concurrent use — ADK can run a
// turn's parallel tool calls on separate goroutines.
type TurnLoop struct {
	threshold int
	stopAfter int

	mu    sync.Mutex
	turns map[string]*turnRun
}

type turnRun struct {
	last   ToolCall
	result string // digest of the run's result so far; "" until one lands
	length int
	// notes maps function-call ID to the note its result should carry,
	// set before the tool runs and consumed after.
	notes map[string]string
}

// NewTurnLoop returns a TurnLoop that notes from the threshold-th
// identical call and stops stopAfter calls later. Values below the
// floors (2 and 1) are raised to them.
func NewTurnLoop(threshold, stopAfter int) *TurnLoop {
	if threshold < 2 {
		threshold = 2
	}
	if stopAfter < 1 {
		stopAfter = 1
	}
	return &TurnLoop{threshold: threshold, stopAfter: stopAfter, turns: map[string]*turnRun{}}
}

// Before observes one call about to run. It returns a non-nil
// LoopStopError when the call should not run and the turn should end.
// Otherwise the call runs, and After both records its result and says
// whether the result carries a note.
//
// An empty invocation ID is a call this cannot attribute to a turn; it
// is let through untracked.
func (l *TurnLoop) Before(invocationID, callID, name string, args map[string]any) *LoopStopError {
	if invocationID == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.turns[invocationID]
	if t == nil {
		t = &turnRun{notes: map[string]string{}}
		l.turns[invocationID] = t
	}
	tc := ToolCall{Name: name, Args: serializeArgs(args)}
	if t.length > 0 && t.last.Name == tc.Name && argsEquivalent(t.last.Args, tc.Args) {
		t.length++
	} else {
		t.last, t.result, t.length = tc, "", 1
	}
	if t.length < l.threshold {
		return nil
	}
	left := l.threshold + l.stopAfter - t.length
	if left <= 0 {
		return &LoopStopError{Tool: name, Args: tc.Args, Count: t.length}
	}
	t.notes[callID] = loopNote(name, t.length, left)
	return nil
}

// After records the result of callID and returns the note that result
// should carry, or "" for none.
//
// A result that differs from the run's breaks the run: the call starts
// a new one of length 1, and a note already decided for it is dropped,
// because "the result will not change" has just been shown false.
func (l *TurnLoop) After(invocationID, callID string, result map[string]any) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.turns[invocationID]
	if t == nil {
		return ""
	}
	note := t.notes[callID]
	delete(t.notes, callID)
	digest := serializeArgs(result)
	switch {
	case t.result == "":
		t.result = digest
	case t.result != digest:
		t.result, t.length = digest, 1
		return ""
	}
	return note
}

// Forget drops a finished turn. Hygiene rather than correctness: an
// invocation ID never recurs, so a stale entry can only cost memory.
func (l *TurnLoop) Forget(invocationID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.turns, invocationID)
}

// loopNote is the model-facing text. It carries the same instruction
// as the repeat signal's Guidance and adds the one fact only the turn
// knows: how many more repeats end it.
func loopNote(name string, n, left int) string {
	return fmt.Sprintf(
		"Automated observation, not part of the tool result: you have called %s with the same arguments %d times in a row in this turn, and the result will not change. "+
			"Use what it already returned, try a different tool or different arguments, or report what is blocking you. "+
			"%s",
		name, n, stopWarning(left))
}

func stopWarning(left int) string {
	if left == 1 {
		return "Calling it again with the same arguments ends this turn."
	}
	return fmt.Sprintf("Calling it %d more times with the same arguments ends this turn.", left)
}
