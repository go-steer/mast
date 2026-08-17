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

// Originally derived from go-steer/core-agent@b8dd225e9ae7fdeb3ff23772cc5be25eed34b818:pkg/agent/watchdog.go

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
	"fmt"
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
// Reports whether any observation actually landed, which is what
// gates Tap's in-turn drain: a signal cannot newly trip without one.
func ObserveEvent(w Watchdog, ev *session.Event, seen map[string]struct{}) bool {
	if w == nil || ev == nil || ev.Content == nil {
		return false
	}
	observed := false
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
		observed = true
		w.ObserveToolCall(ToolCall{
			Name: p.FunctionCall.Name,
			Args: args,
		})
	}
	return observed
}

// ObserveToolResults walks ev's content parts and feeds any
// function-response parts to w, when it implements the optional
// ToolResultObserver extension (#639). A watchdog that only counts
// calls is left alone.
//
// Success vs failure follows ADK's convention: a tool error is a
// reserved "error" key inside FunctionResponse.Response. Flattening it
// here means the watchdog never has to know a provider's response
// shape, and one place decides what "failed" means.
//
// Shares the per-turn dedup set with ObserveEvent, under a distinct
// key prefix — the same streaming aggregator that re-emits a
// FunctionCall part re-emits its FunctionResponse, and a
// double-counted failure would trip the streak signal at half its
// threshold. A response with no ID falls back to name+error, which
// collapses same-error parallel calls within one turn; that is the
// safe direction to be wrong in, since undercounting delays an
// advisory alert while overcounting fires it on work that was fine.
func ObserveToolResults(w Watchdog, ev *session.Event, seen map[string]struct{}) bool {
	if w == nil || ev == nil || ev.Content == nil {
		return false
	}
	obs, ok := w.(ToolResultObserver)
	if !ok {
		return false
	}
	observed := false
	for _, p := range ev.Content.Parts {
		if p == nil || p.FunctionResponse == nil {
			continue
		}
		errText := toolResponseError(p.FunctionResponse.Response)
		key := "result\x00" + p.FunctionResponse.ID
		if p.FunctionResponse.ID == "" {
			key = "result\x00" + p.FunctionResponse.Name + "\x00" + errText
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		observed = true
		obs.ObserveToolResult(ToolResult{
			Name:  p.FunctionResponse.Name,
			Error: errText,
		})
	}
	return observed
}

// toolResponseError extracts the tool error from an ADK function
// response, returning "" for a successful call.
//
// A non-string, non-error value under "error" still counts as a
// failure — a tool that returns a structured error object is failing,
// and treating an unrecognized shape as success would silently drop
// exactly the observations this signal exists to make.
func toolResponseError(resp map[string]any) string {
	v, ok := resp["error"]
	if !ok || v == nil {
		return ""
	}
	switch e := v.(type) {
	case string:
		if e == "" {
			return ""
		}
		return e
	case error:
		return e.Error()
	default:
		return fmt.Sprintf("%v", e)
	}
}

// Tap wraps one turn's runner event stream with watchdog
// observation. It creates its own per-turn dedup set (#363), feeds
// every FunctionCall part — and every FunctionResponse part, when w
// observes outcomes — to w as events pass through, and drains
// w.Check() into onAlert. Tap does NOT call w.Reset() — per-turn
// Reset would wipe the signals' run state, and cross-turn repetition
// is exactly what the watchdog exists to count. Reset stays a caller
// decision at logical session boundaries.
//
// Alerts drain twice over, and the first one is the one that matters.
//
//   - In-turn, as soon as an observation lands. A loop *inside* one
//     turn is the shape mast's tool-calling flow actually produces:
//     the model emits a dispatch or MCP call, the flow runs it and
//     calls the model again, all within a single Run. A turn-boundary
//     drain never fires on that at all while it is happening, so the
//     alert an operator needs during the incident arrives after it —
//     or, if the turn never ends, never. Gated on a fresh observation
//     because a signal cannot newly trip without one, and a turn emits
//     far more text than tool calls. This is also what lets an
//     enforcing caller cancel the turn in flight (see enforce.go).
//   - After the stream ends, unconditionally, so a signal that tripped
//     on the last observed event doesn't leak into the next turn. The
//     post-turn drain is deferred, so it runs even when the consumer
//     stops consuming early — the same guarantee core-agent's
//     wrapWithCleanup gives its post-turn hooks.
//
// Alerts are discarded when onAlert is nil, but still pulled. Wrap
// each turn's stream with a fresh Tap; reusing one across turns would
// defeat the per-turn scoping of the dedup set.
func Tap(events iter.Seq2[*session.Event, error], w Watchdog, onAlert func(Alert)) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		defer drainAlerts(w, onAlert)
		// Per-turn dedup for watchdog observations (#363): scoped to
		// this turn — cross-turn repeats are exactly the signal the
		// watchdog exists to count.
		seen := map[string]struct{}{}
		for ev, err := range events {
			if w != nil && ev != nil {
				observed := ObserveEvent(w, ev, seen)
				observed = ObserveToolResults(w, ev, seen) || observed
				if observed {
					drainAlerts(w, onAlert)
				}
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
