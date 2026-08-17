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

// Originally derived from go-steer/core-agent@6510a65b54ead93b5f2c8c31f478443376203360

package watchdog

import (
	"fmt"
	"strings"
)

// AlternatingCycleSignal trips when the agent repeats the same short
// *sequence* of tool calls over and over — a → b → a → b → a → b (#649).
//
// This is the evasion the consecutive-repeat detector named in its own
// docstring and could not catch: "consecutive" means a run of one call,
// so any loop with a second call wedged in it reads as varied activity.
// The shape is not hypothetical. The live UAT that motivated upstream's
// enforce-mode backstop (#623) was an agent cycling list_agents →
// check_agent, and it survived both an operator "stop" and an
// interrupt.
//
// Detection is a period scan over the recent call history: for each
// period p in 2..MaxPeriod, the last p*Cycles observations trip the
// signal when they consist of Cycles byte-identical blocks of length p.
// Keys are canonicalized (canonicalArgs), so a cycle that alternates
// "./main.go" with "main.go" still reads as one call — but unlike the
// repeat detector this cannot use the pairwise path-suffix relation,
// which is not transitive and therefore cannot key a history buffer.
//
// Two deliberate limits on false positives, because this alert is
// meant to be actionable rather than ambient:
//
//   - A block whose entries are all the same call is skipped. It is a
//     plain repeat, already the other signal's job, and alerting twice
//     for one pattern doubles the noise.
//   - Cycles defaults to 3, i.e. six calls for the a→b→a→b→a→b shape.
//     Two passes through a sequence is normal work — read a file, list
//     pods, read another, list again. Three passes with byte-identical
//     arguments each time is not: nothing in the inputs changed, so
//     nothing in the results can have.
//
// A polling loop written as alternating tool calls is the known false
// positive, and it is a shape mast's own workloads produce — a
// scheduler-driven daemon watching a rollout settle looks exactly like
// this. An operator who wants the pattern anyway drops the signal by
// constructing DefaultWatchdog with their own signal list — which is
// the right answer for that workload, because the signal is Critical
// and the cost of the false positive therefore scales with the posture:
// one log line under warn, a paragraph of unwanted steering under
// feedback, a halted rollout watch under enforce.
type AlternatingCycleSignal struct {
	// MaxPeriod is the longest cycle considered. Periods start at 2 —
	// period 1 is a consecutive repeat, which RepeatedToolCallSignal
	// owns.
	MaxPeriod int
	// Cycles is how many consecutive repetitions of the block are
	// required before the signal trips.
	Cycles int

	history []string // canonical "name\x00args" keys, oldest first
	names   []string // tool names, parallel to history, for the alert text
	tripped string   // pattern fingerprint already alerted on; "" when clear
}

// Default tuning for AlternatingCycleSignal. Exported so an operator
// building a custom signal list can see what they are deviating from.
const (
	DefaultCycleMaxPeriod = 4
	DefaultCycleRepeats   = 3
)

// NewAlternatingCycleSignal constructs the cycle detector. maxPeriod
// below 2 is clamped to 2 (period 1 is the repeat detector's job) and
// cycles below 2 is clamped to 2 (a single pass through a sequence is
// not a cycle).
func NewAlternatingCycleSignal(maxPeriod, cycles int) *AlternatingCycleSignal {
	if maxPeriod < 2 {
		maxPeriod = 2
	}
	if cycles < 2 {
		cycles = 2
	}
	return &AlternatingCycleSignal{MaxPeriod: maxPeriod, Cycles: cycles}
}

// Name implements Signal.
func (s *AlternatingCycleSignal) Name() string { return "alternating-tool-cycle" }

// Reset implements Signal.
func (s *AlternatingCycleSignal) Reset() {
	s.history = nil
	s.names = nil
	s.tripped = ""
}

// ObserveToolCall implements Signal. Appends the call to a bounded
// history and reports the shortest cycle covering the tail.
func (s *AlternatingCycleSignal) ObserveToolCall(tc ToolCall) *Alert {
	s.history = append(s.history, tc.Name+"\x00"+canonicalArgs(tc.Args))
	s.names = append(s.names, tc.Name)
	if limit := s.MaxPeriod * s.Cycles; len(s.history) > limit {
		s.history = s.history[len(s.history)-limit:]
		s.names = s.names[len(s.names)-limit:]
	}

	period := s.detectCycle()
	if period == 0 {
		// The pattern broke: re-arm so a later cycle alerts again.
		s.tripped = ""
		return nil
	}
	block := s.history[len(s.history)-period:]
	// Fingerprint on the rotation-invariant form. The tail slides by one
	// call per observation, so a → b → a → b presents as "a,b" on one
	// lap and "b,a" on the next; keying on the raw tail would alert on
	// every single call of a loop that is obviously one pattern.
	fingerprint := canonicalRotation(block)
	if s.tripped == fingerprint {
		return nil // one alert per cycle, not one per lap
	}
	s.tripped = fingerprint

	seq := strings.Join(s.names[len(s.names)-period:], " → ")
	return &Alert{
		Signal: s.Name(),
		// Critical for the same reason the consecutive-repeat signal
		// is: this is a runaway. It is also the harder one to see from
		// a log, which is the argument for letting a posture stop it.
		Severity: SeverityCritical,
		Reason: fmt.Sprintf(
			"agent has repeated the same %d-call sequence (%s) %d times with identical args — possible tool loop that the consecutive-repeat detector cannot see. If the agent is stuck, POST /sessions/{id}/interrupt on the attach surface. The workload's budget ceiling is the hard backstop.",
			period, seq, s.Cycles,
		),
		Guidance: fmt.Sprintf(
			"you have run the same sequence of calls (%s) %d times with the same arguments each time. Nothing in the inputs changed, so nothing in the results did either. Break the cycle: act on what you already have, change your approach, or report what is blocking you.",
			seq, s.Cycles,
		),
	}
}

// detectCycle returns the shortest period in 2..MaxPeriod whose block
// repeats Cycles times at the tail of history, or 0 when there is no
// such cycle. Blocks made of a single repeated call are skipped —
// RepeatedToolCallSignal already covers those.
func (s *AlternatingCycleSignal) detectCycle() int {
	for p := 2; p <= s.MaxPeriod; p++ {
		need := p * s.Cycles
		if len(s.history) < need {
			continue
		}
		tail := s.history[len(s.history)-need:]
		if !blockRepeats(tail, p) || uniform(tail[:p]) {
			continue
		}
		return p
	}
	return 0
}

// blockRepeats reports whether tail is len(tail)/p copies of its first
// p entries.
func blockRepeats(tail []string, p int) bool {
	for i := p; i < len(tail); i++ {
		if tail[i] != tail[i-p] {
			return false
		}
	}
	return true
}

// canonicalRotation renders block in whichever rotation sorts first,
// so every rotation of one cycle produces the same fingerprint. Blocks
// are at most MaxPeriod entries, so the O(p²) form is cheaper than the
// bookkeeping to avoid it.
func canonicalRotation(block []string) string {
	best := ""
	for i := range block {
		rot := strings.Join(append(append([]string{}, block[i:]...), block[:i]...), "\x01")
		if best == "" || rot < best {
			best = rot
		}
	}
	return best
}

// uniform reports whether every entry in block is the same call.
func uniform(block []string) bool {
	for _, v := range block[1:] {
		if v != block[0] {
			return false
		}
	}
	return true
}
