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
//
// Originally derived from go-steer/core-agent@b8dd225e9ae7fdeb3ff23772cc5be25eed34b818

// Session-event bridge for the behavioral watchdog (derived from
// core-agent's pkg/agent/watchdog.go). The watchdog itself is
// concern-free of the runner's internals; this file is the bridge
// that extracts tool-call observations from session events as they
// stream and drains alerts after the turn. In mast the bridge is a
// set of free functions — the lean core taps a runner event stream
// directly instead of routing through a core-agent-style Agent
// struct. See watchdog.go for the package docstring and the failure
// modes / v1 scoping.

package watchdog

import (
	"encoding/json"
	"iter"

	"google.golang.org/adk/v2/session"
)

// ObserveEvent walks ev's content parts and feeds any function-call
// parts to w. Args are JSON-serialized so the watchdog's literal-
// string-compare detector has stable input — Go's map iteration
// order would otherwise make logically-identical calls compare
// unequal.
//
// seen is the per-turn dedup set (#363): ADK's streaming aggregator
// can re-emit the same FunctionCall part across more than one event
// (an intermediate aggregate plus the final — the same duplication
// runner/events.go dedups for display). Calls carrying an ID dedup
// on it (a re-emitted part keeps its ID; a legitimate parallel call
// with identical args gets a fresh one); ID-less calls fall back to
// name+args, which also collapses same-args parallel calls within
// ONE turn — acceptable, since the watchdog's runaway signal is
// repetition ACROSS turns and the set resets each turn. Callers
// create one seen map per turn and pass it to every ObserveEvent
// call within that turn.
//
// Best-effort: if a part's args don't JSON-marshal cleanly we
// fall back to a recognizable placeholder; the alternative would
// be skipping the observation entirely, which silently weakens
// the signal. Better to compare on the placeholder than miss
// observations.
func ObserveEvent(w Watchdog, ev *session.Event, seen map[string]struct{}) {
	if w == nil || ev == nil || ev.Content == nil {
		return
	}
	for _, p := range ev.Content.Parts {
		if p == nil || p.FunctionCall == nil {
			continue
		}
		args := serializeArgs(p.FunctionCall.Args)
		key := p.FunctionCall.ID
		if key == "" {
			key = p.FunctionCall.Name + "\x00" + args
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		w.ObserveToolCall(ToolCall{
			Name: p.FunctionCall.Name,
			Args: args,
		})
	}
}

// Tap wraps one turn's runner event stream with watchdog
// observation. It creates its own per-turn dedup set (#363), feeds
// every FunctionCall part to w as events pass through, and — after
// the stream ends — drains w.Check() into onAlert, matching
// core-agent's drainWatchdogAlerts semantics: Check is called
// unconditionally (so alerts don't leak into the next turn) and the
// alerts are discarded when onAlert is nil. Tap does NOT call
// w.Reset() — per-turn Reset would wipe the signals' run state, and
// cross-turn repetition is exactly what the watchdog exists to
// count. Reset stays a caller decision at logical session
// boundaries.
//
// The drain is deferred, so it runs even when the consumer stops
// consuming early — same guarantee core-agent's wrapWithCleanup
// gives its post-turn hooks. Wrap each turn's stream with a fresh
// Tap; reusing one across turns would defeat the per-turn scoping
// of the dedup set.
func Tap(events iter.Seq2[*session.Event, error], w Watchdog, onAlert func(Alert)) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		defer drainAlerts(w, onAlert)
		// Per-turn dedup for watchdog observations (#363): scoped to
		// this turn — cross-turn repeats are exactly the signal the
		// watchdog exists to count.
		seen := map[string]struct{}{}
		for ev, err := range events {
			if w != nil && ev != nil {
				ObserveEvent(w, ev, seen)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// drainAlerts is the post-turn drain. Pulls any alerts the watchdog
// accumulated during the just-ended turn and dispatches them to
// onAlert. No-op when no watchdog is wired; when onAlert is nil the
// alerts are still pulled (so they don't leak into the next turn)
// but silently discarded.
func drainAlerts(w Watchdog, onAlert func(Alert)) {
	if w == nil {
		return
	}
	alerts := w.Check()
	if onAlert == nil {
		return
	}
	for _, alert := range alerts {
		onAlert(alert)
	}
}

// serializeArgs produces a stable JSON serialization of args.
// Sorted map keys come for free with encoding/json on
// map[string]any (it sorts alphabetically). On marshal failure,
// returns a placeholder rather than skipping the observation —
// the watchdog needs *some* comparable string per call.
func serializeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "<unmarshalable-args>"
	}
	return string(b)
}
