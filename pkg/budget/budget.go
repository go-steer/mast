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

// Package budget meters model usage against workload budget ceilings.
//
// Spike-2 probe for the "where does cost accounting come from" question
// (docs/orchestration-design.md budget composition): ADK v2 carries
// genai UsageMetadata on every model event (session.Event embeds
// model.LLMResponse), so a meter over the runner's event stream sees
// token counts per call with no ADK patching. What ADK does NOT provide
// is pricing or enforcement — both are mast-side. This package is the
// minimal mast-side shape: per-session cumulative token/cost meter,
// checked as events stream; the caller aborts the run when Observe
// reports the ceiling is crossed.
//
// Known limitation (finding, not TODO): metering at the event stream
// is enforcement-after-the-call — a single runaway call is only caught
// once its usage event lands. Pre-call gating needs a model-layer
// interceptor (wrap model.LLM) or the v2.1.0 TaskRunner seam for tool
// fan-out; both compose with this meter rather than replacing it.
package budget

import (
	"errors"
	"fmt"
	"sync"

	"google.golang.org/adk/v2/session"
)

// ErrExceeded is returned by Observe once the session's cumulative
// usage crosses a ceiling. Callers should abort the run.
var ErrExceeded = errors.New("budget exceeded")

// Limits are the ceilings for one session. Zero values mean unlimited.
type Limits struct {
	MaxCostUSD float64
	MaxTokens  int64

	// MaxTurns caps the number of model calls in the session.
	//
	// Vocabulary: mast counts one "turn" per model call — the same
	// unit as the meter's calls counter (one streamed event carrying
	// UsageMetadata). This matches docs/orchestration-design.md's
	// "budget.max_turns remains mast-side turn counting (ADK has no
	// turn cap)": a Task specialist that loops through five model
	// calls before finish_task has spent five turns, not one.
	MaxTurns int

	RatePer1K float64 // flat USD per 1K total tokens (spike pricing model)
}

// Meter accumulates usage for one session.
type Meter struct {
	mu     sync.Mutex
	limits Limits
	tokens int64
	cost   float64
	calls  int
}

// NewMeter constructs a Meter with the given limits.
func NewMeter(limits Limits) *Meter {
	return &Meter{limits: limits}
}

// Observe folds one event's usage into the meter and reports whether a
// ceiling has been crossed. Events without UsageMetadata (function
// responses, control events) are free.
func (m *Meter) Observe(ev *session.Event) error {
	if ev == nil || ev.UsageMetadata == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.tokens += int64(ev.UsageMetadata.TotalTokenCount)
	m.cost = float64(m.tokens) / 1000 * m.limits.RatePer1K

	if m.limits.MaxTurns > 0 && m.calls > m.limits.MaxTurns {
		return fmt.Errorf("%w: %d model calls (turns) > cap %d", ErrExceeded, m.calls, m.limits.MaxTurns)
	}
	if m.limits.MaxTokens > 0 && m.tokens > m.limits.MaxTokens {
		return fmt.Errorf("%w: %d tokens > cap %d", ErrExceeded, m.tokens, m.limits.MaxTokens)
	}
	if m.limits.MaxCostUSD > 0 && m.cost > m.limits.MaxCostUSD {
		return fmt.Errorf("%w: $%.4f > cap $%.4f (%d tokens over %d calls)", ErrExceeded, m.cost, m.limits.MaxCostUSD, m.tokens, m.calls)
	}
	return nil
}

// Snapshot returns the cumulative usage so far.
func (m *Meter) Snapshot() (tokens int64, costUSD float64, calls int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens, m.cost, m.calls
}
