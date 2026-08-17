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

// Tool-outcome observation and the tool-failure-streak signal (#639).
//
// Every signal before this one reads tool *calls*. The failure mode in
// #639 is invisible from calls alone: during a GKE UAT an agent that
// could not reach its cluster at all reported the incident resolved —
// "everything is in tip-top shape" — with no tool call having verified
// anything. The calls looked normal. The results were the story.
//
// What this catches is deliberately narrow and objective: a run of
// tool calls that all came back as errors, with none succeeding in
// between. No prose is inspected. A detector that tried to recognize
// an over-confident *claim* would be a heuristic about English
// pretending to be a runtime guarantee.
//
// So this closes the evidence half, not the honesty half: it tells a
// model that has been failing every call that it has verified nothing,
// at the point where it is most likely to start narrating instead of
// reporting. It does not, and cannot, detect a confident conclusion
// drawn from tools that all succeeded and said nothing useful.
//
// mast is the deployment where this matters most and is heard least.
// An unattended workload's report is the whole product — nobody
// watched the tool calls scroll by — so a summary composed from
// nothing but errors is indistinguishable, downstream, from one
// composed from evidence.

package watchdog

import "fmt"

// DefaultFailureStreak is the number of consecutive failed tool calls
// that trips ToolFailureStreakSignal. Three is chosen to sit above
// ordinary exploration — a 404, a missing resource, one RBAC denial —
// and below the point where a model has usually stopped gathering
// evidence and started composing an answer.
const DefaultFailureStreak = 3

// ToolResult is the outcome half of a tool invocation. Error is the
// tool's error text, empty for success — the ADK convention is a
// reserved "error" key inside FunctionResponse.Response, and the
// session-event bridge flattens that to this field so the watchdog
// never has to know the provider's response shape.
type ToolResult struct {
	Name  string
	Error string
}

// Failed reports whether the call errored.
func (r ToolResult) Failed() bool { return r.Error != "" }

// ToolResultObserver is the optional half of Watchdog: an
// implementation that also wants to see tool *outcomes* implements it,
// and the bridge feeds results through a type assertion.
//
// Deliberately not folded into Watchdog. That interface is documented
// as a plug-in point ("consumers can plug in their own
// implementation"), so widening it would break every third-party
// watchdog at a minor version to add one signal — and a custom
// watchdog that only counts calls stays perfectly valid.
type ToolResultObserver interface {
	ObserveToolResult(ToolResult)
}

// SignalResultObserver is the same extension one level down, for
// signals inside DefaultWatchdog. A Signal that doesn't implement it
// simply never sees results.
type SignalResultObserver interface {
	ObserveToolResult(ToolResult) *Alert
}

// ObserveToolResult fans a tool outcome across every wired signal that
// implements SignalResultObserver. Implements ToolResultObserver.
func (w *DefaultWatchdog) ObserveToolResult(tr ToolResult) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range w.signals {
		ro, ok := s.(SignalResultObserver)
		if !ok {
			continue
		}
		if alert := ro.ObserveToolResult(tr); alert != nil {
			w.alerts = append(w.alerts, *alert)
		}
	}
}

// ToolFailureStreakSignal trips when Threshold tool calls in a row all
// return errors. Any successful call resets the run — the point is
// "this agent currently has no verified evidence," and one success is
// evidence.
//
// Severity is Warn, and stays Warn when mast grows a posture that acts
// on Critical. Halting a daemon three denials into a legitimate RBAC
// probe would make the backstop the outage. A failure streak is an
// evidence problem, so it goes where evidence problems belong — the
// operator log, and the session's guardrail surface. Runaway
// *behavior* is the loop detectors' business.
type ToolFailureStreakSignal struct {
	Threshold int

	streak  int
	names   []string
	lastErr string
	tripped bool // one alert per streak, not one per failure past it
}

// NewToolFailureStreakSignal constructs the signal. Threshold below 2
// is clamped to 2: a single failed call is an ordinary event, not a
// signal, and threshold 1 would alert on every one of them.
func NewToolFailureStreakSignal(threshold int) *ToolFailureStreakSignal {
	if threshold < 2 {
		threshold = 2
	}
	return &ToolFailureStreakSignal{Threshold: threshold}
}

// Name implements Signal.
func (s *ToolFailureStreakSignal) Name() string { return "tool-failure-streak" }

// ObserveToolCall implements Signal. Calls carry no outcome, so this
// signal ignores them; the interface requires the method because
// DefaultWatchdog fans every call across every signal.
func (s *ToolFailureStreakSignal) ObserveToolCall(ToolCall) *Alert { return nil }

// Reset implements Signal.
func (s *ToolFailureStreakSignal) Reset() {
	s.streak = 0
	s.names = nil
	s.lastErr = ""
	s.tripped = false
}

// ObserveToolResult implements SignalResultObserver.
func (s *ToolFailureStreakSignal) ObserveToolResult(tr ToolResult) *Alert {
	if !tr.Failed() {
		s.Reset()
		return nil
	}
	s.streak++
	s.lastErr = tr.Error
	// Bound the name list at the threshold: past that it's the same
	// story with more words, and the alert text is operator-facing
	// prose that ends up in logs.
	if len(s.names) < s.Threshold {
		s.names = append(s.names, tr.Name)
	}
	if s.streak < s.Threshold || s.tripped {
		return nil
	}
	s.tripped = true
	tools := distinctNames(s.names)
	return &Alert{
		Signal: s.Name(),
		// Warn, deliberately, now that ModeEnforce exists and halts on
		// Critical: stopping a daemon three denials into a legitimate
		// RBAC probe would make the backstop the outage. An evidence
		// gap needs a reader, not a kill switch. Runaway behavior is
		// what the loop detectors escalate.
		Severity: SeverityWarn,
		Reason: fmt.Sprintf(
			"%d tool calls in a row failed with no successful call in between (%s). The agent has no tool-verified evidence about the state it is being asked to report on; treat its next answer as unverified. Last error: %s",
			s.streak, tools, truncate(s.lastErr, 200),
		),
		// The one signal whose model-facing half is the more important
		// one. An operator reading "no verified evidence" already knows
		// to distrust the report; the model composing that report is the
		// party that can still go get the evidence — or say it could not.
		Guidance: fmt.Sprintf(
			"your last %d tool calls all failed (%s) with none succeeding in between, so you have not verified anything about the state you are working on. Do not describe that state as if you had. Fix the cause, try a different route to the evidence, or say plainly that you could not reach it. Last error: %s",
			s.streak, tools, truncate(s.lastErr, 200),
		),
	}
}

// distinctNames renders the streak's tool names for the alert text,
// collapsing repeats so "gke_get_pod, gke_get_pod, gke_get_pod" reads
// as one name — the repeated-tool-call signal is what covers
// repetition; this one is about outcomes.
func distinctNames(names []string) string {
	if len(names) == 0 {
		return "unknown tools"
	}
	out := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, n := range names {
		if n == "" {
			n = "unnamed tool"
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	s := out[0]
	for _, n := range out[1:] {
		s += ", " + n
	}
	return s
}
