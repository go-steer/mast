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

// Ported from go-steer/core-agent@6813f6d (DominantToolCallSignal),
// adapted to mast's signal set and its feedback-by-default posture.

package watchdog

import "fmt"

// Default tuning for DominantToolCallSignal. Exported so an operator
// wiring the signal can see what they are deviating from.
const (
	// DefaultDominantWindow is how many recent calls are considered.
	// Twelve is the smallest window that holds three laps of the
	// longest cycle the cycle detector recognizes, so the two
	// detectors reason about the same span of history.
	DefaultDominantWindow = 12
	// DefaultDominantThreshold is how many of that window one call
	// must account for. Eight of twelve is two thirds: high enough
	// that ordinary work — read, list, read, grep, read — does not
	// reach it, low enough that a loop with an interloper every third
	// call does.
	DefaultDominantThreshold = 8
)

// DominantToolCallSignal trips when one tool call accounts for most of
// the recent history, wherever the other calls fall — the shape between
// mast's two existing loop detectors (#227).
//
//	a a a b a a a c a a a
//
// The consecutive-repeat detector's run is reset to zero by every
// interloper, so it sees three runs of three and nothing to alert on.
// The cycle detector sees a near-uniform window with no repeating
// block and hands it back. Nothing trips until the interleaves happen
// to stop long enough for a clean run of five — and the delay is the
// whole cost: upstream measured 22 byte-identical calls over 2m20s on a
// live GKE run before the repeat detector caught a loop that was
// visibly degenerate by the fourth call. Most of that session's spend
// went into the gap.
//
// Density does not care where the interleaves fall, so it reaches the
// same verdict inside the first full window. Keys are canonicalized
// (canonicalArgs) the way the cycle detector's are: counting
// occurrences means keying a map, and the repeat detector's pairwise
// path-suffix relation is not transitive.
//
// # One behavior, one alert
//
// DefaultWatchdog fans each observation across every wired signal and
// appends every alert, so three overlapping detectors on one loop is
// three alerts — and under mast's default `feedback` posture that is
// three paragraphs of steering for one behavior. The dominant-call
// signal therefore stands down where another detector already owns the
// shape, structurally rather than by tuning:
//
//   - DeferToRepeatRun: the dominant call has reached a consecutive run
//     of this length inside the window. That is the repeat detector's
//     alert to raise.
//   - DeferToCyclePeriod: the window is a clean repetition of a block
//     of 2..DeferToCyclePeriod distinct calls. That is the cycle
//     detector's.
//
// Both are explicit fields and zero disables either, so an embedder
// wiring this signal on its own — without the other two — gets it
// undeferred and sees every dominant loop.
//
// # Not in the default set
//
// NewDefaultWatchdog does not wire this. mast's default posture is
// `feedback`, so a third Critical detector changes what every
// unattended workload is told about itself, and the cycle detector's
// docstring already records that a polling workload is the known false
// positive — a poll with any variation in it is precisely a dominant
// call with interleaves. Defaulting it is a posture decision, and it
// belongs with the open watchdog-governance question rather than with
// the port.
type DominantToolCallSignal struct {
	// Window is how many recent observations are considered.
	Window int
	// Threshold is how many of those Window observations one call must
	// account for.
	Threshold int
	// DeferToRepeatRun is the consecutive-run length at which
	// RepeatedToolCallSignal owns the shape. Zero disables the
	// deferral.
	DeferToRepeatRun int
	// DeferToCyclePeriod is the longest block length at which
	// AlternatingCycleSignal owns the shape. Zero disables the
	// deferral.
	DeferToCyclePeriod int

	history []string // canonical "name\x00args" keys, oldest first
	names   []string // tool names, parallel to history, for the alert text
	tripped string   // key already alerted on; "" when clear
}

// NewDominantToolCallSignal constructs the density detector deferring
// to mast's default repeat threshold and cycle period, which is what a
// caller adding it to the default signal set wants. window below 4 is
// clamped to 4 (a window that small is a repeat), threshold below 2 to
// 2, and a threshold above the window to the window (a threshold that
// can never be reached is a disabled signal wearing a detector's name).
func NewDominantToolCallSignal(window, threshold int) *DominantToolCallSignal {
	if window < 4 {
		window = 4
	}
	if threshold < 2 {
		threshold = 2
	}
	if threshold > window {
		threshold = window
	}
	return &DominantToolCallSignal{
		Window:             window,
		Threshold:          threshold,
		DeferToRepeatRun:   DefaultRepeatThreshold,
		DeferToCyclePeriod: DefaultCycleMaxPeriod,
	}
}

// Name implements Signal.
func (s *DominantToolCallSignal) Name() string { return "dominant-tool-call" }

// Reset implements Signal.
func (s *DominantToolCallSignal) Reset() {
	s.history = nil
	s.names = nil
	s.tripped = ""
}

// ObserveToolCall implements Signal. Appends the call to a bounded
// window and reports the call dominating it, unless another detector
// owns the shape.
func (s *DominantToolCallSignal) ObserveToolCall(tc ToolCall) *Alert {
	s.history = append(s.history, tc.Name+"\x00"+canonicalArgs(tc.Args))
	s.names = append(s.names, tc.Name)
	if len(s.history) > s.Window {
		s.history = s.history[len(s.history)-s.Window:]
		s.names = s.names[len(s.names)-s.Window:]
	}
	if len(s.history) < s.Window {
		return nil // a partial window has no density to report
	}

	key, name, count := s.dominant()
	if count < s.Threshold {
		// The window diversified: re-arm so a later loop alerts again.
		s.tripped = ""
		return nil
	}
	if s.deferred(key) {
		return nil
	}
	if s.tripped == key {
		return nil // one alert per behavior, not one per call past it
	}
	s.tripped = key

	return &Alert{
		Signal: s.Name(),
		// Critical for the same reason the other two loop detectors
		// are: this is a runaway, and it is the one that shows up
		// earliest — the argument for letting a posture stop it is
		// strongest here.
		Severity: SeverityCritical,
		Reason: fmt.Sprintf(
			"%d of the agent's last %d tool calls were the same call (%s) with identical args, interleaved with others — a tool loop neither the consecutive-repeat nor the cycle detector can see. If the agent is stuck, POST /sessions/{id}/interrupt on the attach surface. The workload's budget ceiling is the hard backstop.",
			count, s.Window, name,
		),
		Guidance: fmt.Sprintf(
			"%d of your last %d tool calls were %s with the same arguments every time. Interleaving other calls between them does not make the answer change — nothing in the inputs moved, so nothing in the results did. Act on what that call already told you, change your approach, or report what is blocking you.",
			count, s.Window, name,
		),
	}
}

// dominant returns the most frequent key in the window, its tool name,
// and its count. Ties go to whichever call appears first, so the result
// does not depend on map iteration order.
func (s *DominantToolCallSignal) dominant() (string, string, int) {
	counts := make(map[string]int, len(s.history))
	for _, k := range s.history {
		counts[k]++
	}
	var (
		bestKey  string
		bestName string
		best     int
	)
	for i, k := range s.history {
		if counts[k] > best {
			bestKey, bestName, best = k, s.names[i], counts[k]
		}
	}
	return bestKey, bestName, best
}

// deferred reports whether another detector already owns this shape.
func (s *DominantToolCallSignal) deferred(key string) bool {
	if s.DeferToRepeatRun > 0 && s.longestRun(key) >= s.DeferToRepeatRun {
		return true // the repeat detector's alert to raise
	}
	if s.DeferToCyclePeriod > 0 {
		for p := 2; p <= s.DeferToCyclePeriod; p++ {
			if len(s.history)%p != 0 || len(s.history)/p < 2 {
				continue
			}
			// uniform blocks are the repeat detector's, not the cycle
			// detector's — it skips them for exactly that reason, so
			// deferring on one would leave the shape unowned.
			if blockRepeats(s.history, p) && !uniform(s.history[:p]) {
				return true // the cycle detector's alert to raise
			}
		}
	}
	return false
}

// longestRun returns the longest consecutive run of key in the window.
func (s *DominantToolCallSignal) longestRun(key string) int {
	best, run := 0, 0
	for _, k := range s.history {
		if k != key {
			run = 0
			continue
		}
		run++
		if run > best {
			best = run
		}
	}
	return best
}
