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

package watchdog

import (
	"fmt"
	"strings"
	"testing"
)

// TestTurnLoopNotesThenStops walks the #514 shape: one call repeated
// with identical arguments. Calls below the threshold pass silently,
// the threshold call and the ones after it carry the note with a
// countdown, and the call DefaultStopAfterNote later is refused.
func TestTurnLoopNotesThenStops(t *testing.T) {
	l := NewTurnLoop(DefaultRepeatThreshold, DefaultStopAfterNote)
	args := map[string]any{"scope": "data/pvc-data-postgres-0"}
	stopAt := DefaultRepeatThreshold + DefaultStopAfterNote
	for i := 1; i <= stopAt; i++ {
		id := fmt.Sprintf("call-%d", i)
		stop := l.Before("inv", id, "k8s_resource_spec", args)
		note := l.After("inv", id, map[string]any{"phase": "Pending"})
		switch {
		case i < DefaultRepeatThreshold:
			if stop != nil || note != "" {
				t.Fatalf("call %d: stop=%v note=%q, want neither below the threshold", i, stop, note)
			}
		case i < stopAt:
			if stop != nil {
				t.Fatalf("call %d stopped the turn early: %v", i, stop)
			}
			if note == "" {
				t.Fatalf("call %d carried no note — the model is never told inside the turn", i)
			}
			left := stopAt - i
			if !strings.Contains(note, stopWarning(left)) {
				t.Errorf("call %d note = %q, want the countdown %q", i, note, stopWarning(left))
			}
		default:
			if stop == nil {
				t.Fatalf("call %d was let through; the turn should end here", i)
			}
			if stop.Count != stopAt || stop.Tool != "k8s_resource_spec" {
				t.Errorf("stop = %+v, want Count %d on k8s_resource_spec", stop, stopAt)
			}
		}
	}
}

// TestTurnLoopResetsOnADifferentCall: a model that takes the note's
// advice gets a fresh run, not a turn that ends on its next repeat.
func TestTurnLoopResetsOnADifferentCall(t *testing.T) {
	l := NewTurnLoop(DefaultRepeatThreshold, DefaultStopAfterNote)
	same := map[string]any{"name": "a"}
	for i := 0; i < DefaultRepeatThreshold+DefaultStopAfterNote-1; i++ {
		if stop := l.Before("inv", "", "get", same); stop != nil {
			t.Fatalf("stopped at %d", i)
		}
		l.After("inv", "", nil)
	}
	if stop := l.Before("inv", "", "get", map[string]any{"name": "b"}); stop != nil {
		t.Fatalf("a different call stopped the turn: %v", stop)
	}
	for i := 0; i < DefaultRepeatThreshold-1; i++ {
		if stop := l.Before("inv", "", "get", same); stop != nil {
			t.Fatalf("the old run carried over: stopped at %d", i)
		}
	}
}

// TestTurnLoopLetsAPollerPoll is the false positive the result check
// exists for: the same call on purpose, watching something change. A
// run whose results differ is not a loop, however long it gets.
func TestTurnLoopLetsAPollerPoll(t *testing.T) {
	l := NewTurnLoop(DefaultRepeatThreshold, DefaultStopAfterNote)
	args := map[string]any{"name": "api"}
	for i := 0; i < 4*(DefaultRepeatThreshold+DefaultStopAfterNote); i++ {
		id := fmt.Sprintf("call-%d", i)
		if stop := l.Before("inv", id, "rollout_status", args); stop != nil {
			t.Fatalf("poll %d stopped the turn: %v", i, stop)
		}
		if note := l.After("inv", id, map[string]any{"ready": i}); note != "" {
			t.Fatalf("poll %d carried a note though its result changed: %q", i, note)
		}
	}
}

// TestTurnLoopIsPerTurn: two invocations never share a run, and a
// forgotten turn starts from zero.
func TestTurnLoopIsPerTurn(t *testing.T) {
	l := NewTurnLoop(2, 1)
	args := map[string]any{"x": 1}
	l.Before("a", "1", "t", args)
	if stop := l.Before("b", "2", "t", args); stop != nil {
		t.Fatalf("turn b inherited turn a's run: %v", stop)
	}
	l.Before("a", "3", "t", args) // threshold in a: noted
	if stop := l.Before("a", "4", "t", args); stop == nil {
		t.Fatal("turn a did not stop at threshold+stopAfter")
	}
	l.Forget("a")
	if stop := l.Before("a", "5", "t", args); stop != nil {
		t.Fatalf("a forgotten turn kept its run: %v", stop)
	}
	if stop := l.Before("", "6", "t", args); stop != nil {
		t.Fatal("an unattributable call was tracked")
	}
}

// TestLoopStopErrorKindMatchesAttach pins the bare string against the
// attach constant it stands in for. A literal here, not an import:
// see TrippedError's TurnErrorKind.
func TestLoopStopErrorKindMatchesAttach(t *testing.T) {
	if got := (&LoopStopError{}).TurnErrorKind(); got != "loop_stop" {
		t.Errorf("TurnErrorKind = %q, want loop_stop (attach.TurnErrorLoopStop)", got)
	}
}
