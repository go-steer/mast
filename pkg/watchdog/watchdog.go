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

// Originally derived from go-steer/core-agent@b8dd225e9ae7fdeb3ff23772cc5be25eed34b818

// Package watchdog implements the out-of-band behavioral observer
// from docs/model-selection-design.md (issue #123, PR 2 of 2).
//
// The watchdog catches sessions going off the rails — repeated
// identical tool calls, tools without intervening assistant text,
// progress stalls — via pure heuristics on per-turn telemetry. No
// LLM calls. Pairs with #119's per-tier compaction (context signal)
// and #145's cost ceiling (dollar signal); this is the *behavioral*
// signal layer.
//
// Current scope:
//
//   - Watchdog interface + Alert / Signal / Telemetry types.
//   - DefaultWatchdog implementing three detectors: repeated identical
//     tool calls (the read_file loop pattern from #144 + family);
//     alternating cycles (a → b → a → b) with path-canonicalized
//     argument comparison, which are the two evasions the v1 detector
//     documented and could not catch (#649, cycle.go + canonical.go);
//     and a tool-failure streak read off tool *outcomes* rather than
//     calls (#639, failure.go). The shape is built so adding signals
//     is mechanical — each signal is a small struct with an
//     Observe/Check pair, the DefaultWatchdog just fans observations
//     across them.
//   - Tool *outcome* observation via the optional ToolResultObserver
//     extension — see failure.go. Kept optional rather than folded
//     into Watchdog so a third-party implementation doesn't break to
//     gain one signal.
//   - "Warn" mode only: alerts are logged by the bridge's caller and
//     surfaced on the attach guardrail endpoint. No interactive
//     prompt, no auto-escalation, no model swap, and no halt — a
//     tripped signal never stops a turn.
//
// Future scope (deferred — see design doc §"Piece 2"):
//
//   - Additional signals: tools-without-text, files-not-touched,
//     context-growth-rate, cost-burn-rate.
//   - "Feedback" mode: route the observation into the model's own next
//     turn, which is the only party that can stop making the call.
//     core-agent shipped this as its #159; mast's port is sequenced
//     behind these detectors (see docs/sibling-sync.md).
//   - "Enforce" mode: a Critical alert halts the agent until an
//     operator resets, mirroring the budget kill switch. Until it
//     lands, every signal here is Warn — severity that nothing acts on
//     is a label, and a Critical that halts nothing reads as a bug.
//   - "Prompt" mode: pause turn, ask operator y/n via the existing
//     permissions prompter, resume on either path.
//   - "Auto" mode: escalate to a frontier model without operator
//     interaction.
//   - SSE event surface for alerts.
//
// The interface is designed so consumers can plug in their own
// implementation — same composability pattern as Compactor /
// Checkpointer. Default ships sensible for most operators; specific
// deployments override.

package watchdog

import (
	"fmt"
	"strings"
	"sync"
)

// Severity classifies the urgency of an alert. Warn is the operator-
// visible-but-not-action-blocking level v1 emits; the others are
// reserved for future modes that pause or escalate automatically.
type Severity string

const (
	SeverityWarn     Severity = "warn"
	SeverityCritical Severity = "critical"
)

// Alert is what a triggered signal returns. Fields are operator-
// facing — the agent's wiring logs Reason verbatim. Signal is the
// stable string ID the rest of the system can dispatch on (future
// "auto" mode picks behavior per signal).
type Alert struct {
	Signal   string
	Severity Severity
	Reason   string
}

// ToolCall is the per-tool-call observation the watchdog needs.
// Name is the canonical tool name (e.g. "read_file",
// "mcp.gke.list_clusters"). Args is the JSON-serialized argument
// blob, passed through as the caller produced it: the detectors
// canonicalize path-shaped values themselves (#649, canonical.go)
// rather than requiring every caller to agree on a normal form, and a
// third-party Watchdog implementation still sees the raw args.
type ToolCall struct {
	Name string
	Args string
}

// Watchdog observes per-turn telemetry and returns any alerts that
// triggered during observation. Implementations must be safe for
// concurrent use — the agent calls Observe* methods from the
// streaming event handler and Check from the post-turn hook;
// concurrency is bounded but real.
//
// The interface is intentionally narrow for v1: just tool-call
// observation + alert reporting. Richer telemetry (turn timing,
// per-turn cost delta, files-touched diff) can be added as
// additional Observe* methods as new signals need them.
type Watchdog interface {
	// ObserveToolCall records one tool invocation. Called by the
	// agent's event-tap as tool calls stream by; safe to call from
	// any goroutine.
	ObserveToolCall(ToolCall)

	// Check returns alerts triggered since the last Check call and
	// resets the per-call alert buffer. Returns nil when no signal
	// has tripped. Typically called from the agent's post-turn
	// hook; an alert returned here is "for the turn just ended."
	Check() []Alert

	// Reset clears all accumulated state. Called when the agent
	// resets (e.g. via a hypothetical /clear that clears history)
	// so signals don't carry across a logical session boundary.
	Reset()
}

// DefaultWatchdog is the package-default implementation. Fans
// observations across the configured signals; Check collects
// alerts from each.
type DefaultWatchdog struct {
	mu      sync.Mutex
	signals []Signal
	alerts  []Alert
}

// Signal is the per-detector interface inside DefaultWatchdog. Each
// signal owns its own state and decides when to emit an alert.
// Implementations must be safe to call serially from
// DefaultWatchdog (which holds a mutex across observations); they
// do NOT need to be concurrency-safe themselves.
//
// Adding a new signal: implement Signal, append to NewDefaultWatchdog's
// signal list (or to a constructor variant). No changes to
// DefaultWatchdog itself.
type Signal interface {
	// Name returns the stable signal ID used in Alert.Signal.
	Name() string

	// ObserveToolCall updates the signal's internal state with one
	// tool invocation. Returning a non-nil Alert means the signal
	// tripped on this observation; DefaultWatchdog appends it to
	// the pending-alerts buffer.
	ObserveToolCall(ToolCall) *Alert

	// Reset clears the signal's state. Called from
	// DefaultWatchdog.Reset.
	Reset()
}

// NewDefaultWatchdog returns a DefaultWatchdog wired with the
// default signal set:
//
//   - RepeatedToolCall (threshold 5): 5 consecutive calls to the same
//     tool with path-canonicalized-identical args.
//   - AlternatingCycle (period ≤ 4, 3 laps): the same short sequence
//     of calls repeated three times — the a → b → a → b shape the
//     repeat detector structurally cannot see (#649).
//   - ToolFailureStreak (3 in a row): every call erroring with none
//     succeeding in between, i.e. an agent with no verified evidence
//     about anything (#639).
//
// All three are Warn, because warn is mast's only posture: nothing
// here halts a turn. Operators wanting different thresholds, or a
// subset, construct DefaultWatchdog directly with a custom signal
// list — the cycle detector is the one most likely to be dropped, on
// a workload whose normal shape is a polling loop.
func NewDefaultWatchdog() *DefaultWatchdog {
	return &DefaultWatchdog{
		signals: []Signal{
			NewRepeatedToolCallSignal(5),
			NewAlternatingCycleSignal(DefaultCycleMaxPeriod, DefaultCycleRepeats),
			NewToolFailureStreakSignal(DefaultFailureStreak),
		},
	}
}

// ObserveToolCall fans the observation across every wired signal.
func (w *DefaultWatchdog) ObserveToolCall(tc ToolCall) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range w.signals {
		if alert := s.ObserveToolCall(tc); alert != nil {
			w.alerts = append(w.alerts, *alert)
		}
	}
}

// Check returns any alerts that accumulated since the last Check
// and resets the buffer. Returns nil (not an empty slice) when no
// alerts are pending — lets the caller skip work cheaply.
func (w *DefaultWatchdog) Check() []Alert {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.alerts) == 0 {
		return nil
	}
	out := w.alerts
	w.alerts = nil
	return out
}

// Reset clears alerts + every signal's state. Called on logical
// session boundaries (e.g. operator-initiated /clear).
func (w *DefaultWatchdog) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.alerts = nil
	for _, s := range w.signals {
		s.Reset()
	}
}

// RepeatedToolCallSignal trips when the same (name, args) tool call
// appears Threshold times consecutively. Catches the read_file loop
// pattern from issue #144 and similar runaway-tool-call shapes.
//
// "Consecutive" is the key word: a → b → a → b → a doesn't trip
// (no run of identical calls), but a → a → a → a → a does. This
// matches operator intuition ("the agent is stuck on the same
// thing") without flagging legitimate patterns like
// alternating-tool exploration loops.
//
// Args comparison is path-canonicalized (#649, see canonical.go),
// not literal-string as in v1: "main.go", "./main.go" and
// "/workspace/main.go" are one call, because an agent re-reading one
// file under three spellings is as stuck as one re-reading it under
// the same spelling. Everything else still compares exactly — a
// detector that generalizes too eagerly flags legitimate work.
type RepeatedToolCallSignal struct {
	Threshold int

	lastCall  ToolCall
	runLength int
	tripped   bool // emit one alert per run, not one per observation past threshold
}

// NewRepeatedToolCallSignal constructs a signal with the given
// threshold. Threshold must be ≥ 2 (a "repeated call" requires at
// least two of the same in a row); values < 2 are clamped to 2 to
// avoid the degenerate case where every tool call trips the signal.
func NewRepeatedToolCallSignal(threshold int) *RepeatedToolCallSignal {
	if threshold < 2 {
		threshold = 2
	}
	return &RepeatedToolCallSignal{Threshold: threshold}
}

// Name implements Signal.
func (s *RepeatedToolCallSignal) Name() string { return "repeated-tool-call" }

// ObserveToolCall implements Signal. Tracks the running count of
// consecutive identical calls; emits an alert when count reaches
// Threshold. Returns nil on subsequent observations within the
// same run (already-tripped guard) so we don't re-emit on every
// extra call — operators want one notice per stuck pattern, not
// one per tool call past the threshold.
func (s *RepeatedToolCallSignal) ObserveToolCall(tc ToolCall) *Alert {
	if s.matches(tc) {
		s.runLength++
	} else {
		s.lastCall = tc
		s.runLength = 1
		s.tripped = false
	}
	if s.runLength >= s.Threshold && !s.tripped {
		s.tripped = true
		return &Alert{
			Signal:   s.Name(),
			Severity: SeverityWarn,
			Reason: fmt.Sprintf(
				"agent has called %s with identical args %d times in a row — possible tool loop. Args: %s. If the agent is stuck, POST /sessions/{id}/interrupt on the attach surface. The workload's budget ceiling is the hard backstop.",
				tc.Name, s.runLength, truncate(tc.Args, 200),
			),
		}
	}
	return nil
}

// Reset implements Signal.
func (s *RepeatedToolCallSignal) Reset() {
	s.lastCall = ToolCall{}
	s.runLength = 0
	s.tripped = false
}

// matches reports whether tc is the same call as the run's lastCall,
// comparing args through argsEquivalent so path spellings of one file
// don't split a run. Returns false when there is no run in flight
// (runLength == 0).
func (s *RepeatedToolCallSignal) matches(tc ToolCall) bool {
	if s.runLength == 0 {
		return false
	}
	return s.lastCall.Name == tc.Name && argsEquivalent(s.lastCall.Args, tc.Args)
}

// truncate caps s at maxLen, replacing the middle with "…" so the
// shape stays recognizable. Used to keep Alert.Reason bounded — a
// 10KB JSON blob in the alert text isn't useful and inflates log
// volume.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	const ellipsis = "…" // 3 bytes UTF-8
	if maxLen <= len(ellipsis) {
		return ellipsis[:maxLen]
	}
	half := (maxLen - len(ellipsis)) / 2
	return s[:half] + ellipsis + s[len(s)-half:]
}

// String implements fmt.Stringer for Alert so log lines stay
// uniform. Format: "[severity] signal: reason".
func (a Alert) String() string {
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(string(a.Severity))
	b.WriteString("] ")
	b.WriteString(a.Signal)
	b.WriteString(": ")
	b.WriteString(a.Reason)
	return b.String()
}
