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
// # Scopes: per-specialist ceilings under the session's
//
// A workload budget bounds the session; a specialist's own budget
// bounds that specialist. Config.Scopes composes the two by attributing
// each usage event to the agent that authored it — session.Event.Author
// is the agent's name on every dispatch shape mast builds (a
// coordinator's sub-agent tool, a workflow-graph node, a planner's
// invoke_specialist), which is what makes one seam enough. A scope
// carries its own ceilings and, when the specialist declares a `model:`
// override, its own price, so a cheap analyst's tokens are not billed
// at the synthesizer's rate.
//
// Composition is tightest-cap-wins by construction rather than by
// arithmetic: every event is checked against its scope and against the
// session, and whichever ceiling is crossed first stops the run. A
// scope's ceiling is reported ahead of the session's on the event that
// crosses both, because the specialist is the more specific fact and
// the workload's cap would have been crossed on a later call anyway.
//
// # Known limitations (findings, not TODOs)
//
// Metering at the event stream is enforcement-after-the-call — a single
// runaway call is only caught once its usage event lands. Pre-call
// gating needs a model-layer interceptor (wrap model.LLM) or ADK's
// BeforeModel plugin callback; both compose with this meter rather than
// replacing it.
//
// A crossed scope ceiling stops the session, not just the specialist,
// because the event stream is outside the specialist's own run and the
// only lever there is the run context. Stopping one specialist and
// handing the coordinator a refusal it can route around is the better
// shape, and it needs the pre-call seam above.
package budget

import (
	"errors"
	"fmt"
	"sync"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/pricing"
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

	// Catalog prices each model call exactly, from the model the event
	// says it was billed against.
	//
	// Optional, and strictly better than RatePer1K where a caller can
	// supply it. The flat rate exists because this meter originally saw
	// only UsageMetadata.TotalTokenCount, so internal/compose derives it
	// as the plain average of a model's input and output rates — an
	// approximation that "overcharges input-heavy sessions and
	// undercharges output-heavy ones". Both halves of that premise have
	// since stopped being true: the event carries the input/output split
	// and the cache-read subset, and it carries ModelVersion, so the call
	// can be priced against the same pkg/pricing catalog everything else
	// uses. The error is not small on a real agent — an input-heavy,
	// cache-warm session measured here ran 5.9x over its flat-rate
	// figure, and a cost ceiling that wrong is a ceiling that fires on
	// the wrong sessions.
	//
	// Unknown models fall through to RatePer1K, so a catalog miss never
	// silently drops a session's cost to zero. Unpriced counts them.
	//
	// On a scope, nil means "inherit the session's catalog", matching
	// RatePer1K's rule below. A per-scope catalog is unusual — a rate is
	// a property of the model, and the model is on the event — but the
	// inherit rule costs nothing and keeps the two price knobs behaving
	// alike.
	Catalog *pricing.Catalog

	// RatePer1K is the flat USD price per 1K total tokens (spike
	// pricing model), and the fallback for a call Catalog cannot price.
	//
	// On a scope, zero means "inherit the session's rate" — the right
	// default for a specialist that declares no model of its own, and
	// the reason an un-tiered roster prices exactly as it did before
	// scopes existed.
	RatePer1K float64
}

// Config is the full meter shape: the session's ceilings plus the
// per-agent scopes composed under them.
type Config struct {
	// Limits are the session-wide ceilings (the workload budget).
	Limits Limits

	// Scopes are per-agent ceilings and prices, keyed by the agent name
	// that authors the event — for a specialist, its spec name. An
	// agent with no scope is metered into the session totals only.
	Scopes map[string]Limits
}

// Meter accumulates usage for one session, and for each scoped agent
// within it.
type Meter struct {
	mu     sync.Mutex
	limits Limits
	scopes map[string]Limits
	total  usage
	spent  map[string]*usage

	// unpriced counts calls a configured Catalog could not price. It is
	// session-wide rather than per-scope: it exists to label one cost
	// figure as a mix of two pricing models, and every figure this meter
	// reports is drawn from the same stream of calls.
	unpriced int
}

// usage is one accumulator: a session's or a scope's.
type usage struct {
	tokens int64
	cost   float64
	calls  int
}

// NewMeter constructs a Meter with the given session limits and no
// per-agent scopes.
func NewMeter(limits Limits) *Meter {
	return New(Config{Limits: limits})
}

// New constructs a Meter from a full config.
func New(cfg Config) *Meter {
	m := &Meter{limits: cfg.Limits}
	if len(cfg.Scopes) > 0 {
		m.scopes = make(map[string]Limits, len(cfg.Scopes))
		m.spent = make(map[string]*usage, len(cfg.Scopes))
		for name, l := range cfg.Scopes {
			m.scopes[name] = l
			m.spent[name] = &usage{}
		}
	}
	return m
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

	tokens := int64(ev.UsageMetadata.TotalTokenCount)
	rate := m.limits.RatePer1K
	cat := m.limits.Catalog
	scope, scoped := m.scopes[ev.Author]
	if scoped {
		if scope.RatePer1K > 0 {
			rate = scope.RatePer1K
		}
		if scope.Catalog != nil {
			cat = scope.Catalog
		}
	}
	// Cost accrues per event rather than being recomputed from the
	// running token total: with per-scope rates and per-model catalog
	// pricing the session total is a sum of differently-priced calls,
	// not one multiplication.
	spend := m.priceOf(ev, cat, rate)

	m.total.add(tokens, spend)
	if scoped {
		u := m.spent[ev.Author]
		u.add(tokens, spend)
		if err := check(scope, u); err != nil {
			return fmt.Errorf("%w: specialist %q: %s", ErrExceeded, ev.Author, err)
		}
	}
	if err := check(m.limits, &m.total); err != nil {
		return fmt.Errorf("%w: %s", ErrExceeded, err)
	}
	return nil
}

func (u *usage) add(tokens int64, cost float64) {
	u.calls++
	u.tokens += tokens
	u.cost += cost
}

// check reports the first ceiling in l that u has crossed, as the
// detail half of an ErrExceeded message.
func check(l Limits, u *usage) error {
	if l.MaxTurns > 0 && u.calls > l.MaxTurns {
		return fmt.Errorf("%d model calls (turns) > cap %d", u.calls, l.MaxTurns)
	}
	if l.MaxTokens > 0 && u.tokens > l.MaxTokens {
		return fmt.Errorf("%d tokens > cap %d", u.tokens, l.MaxTokens)
	}
	if l.MaxCostUSD > 0 && u.cost > l.MaxCostUSD {
		return fmt.Errorf("$%.4f > cap $%.4f (%d tokens over %d calls)", u.cost, l.MaxCostUSD, u.tokens, u.calls)
	}
	return nil
}

// priceOf costs one model call, against cat where it can and the flat
// rate where it cannot. Caller holds m.mu.
//
// Cached input is billed at the catalog's cache-read rate, which is
// typically a tenth of fresh input; on a cache-warm agent that subset is
// the majority of the prompt, so folding it in at the input rate is the
// single largest source of error in a flat-rate figure.
func (m *Meter) priceOf(ev *session.Event, cat *pricing.Catalog, rate float64) float64 {
	u := ev.UsageMetadata
	if cat != nil {
		if r, ok := cat.Lookup(ev.ModelVersion); ok && !r.IsZero() {
			// Clamped, not trusted: a provider that over-reports the
			// cached counter would otherwise produce negative uncached
			// tokens, and CostUSDWithCache would bill them at the input
			// rate as a credit — a ceiling that gets *further* away the
			// more the provider miscounts. core-agent's usage tracker
			// guards the same quirk the same way.
			cached := int(u.CachedContentTokenCount)
			if prompt := int(u.PromptTokenCount); cached > prompt {
				cached = prompt
			}
			return r.CostUSDWithCache(int(u.PromptTokenCount)-cached, cached, int(u.CandidatesTokenCount))
		}
		m.unpriced++
	}
	return float64(u.TotalTokenCount) / 1000 * rate
}

// Snapshot returns the session's cumulative usage so far.
func (m *Meter) Snapshot() (tokens int64, costUSD float64, calls int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total.tokens, m.total.cost, m.total.calls
}

// ScopeSnapshot returns one scoped agent's cumulative usage. ok is
// false for an agent the meter carries no scope for — which is not the
// same as an agent that has spent nothing.
func (m *Meter) ScopeSnapshot(name string) (tokens int64, costUSD float64, calls int, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.spent[name]
	if !ok {
		return 0, 0, 0, false
	}
	return u.tokens, u.cost, u.calls, true
}

// Unpriced reports how many calls a configured Catalog could not price and
// that fell back to RatePer1K. Non-zero means the cost figure is a mix of
// two pricing models and should be read as approximate — a caller that
// displays cost should surface it rather than let a stale catalog quietly
// downgrade an exact number.
func (m *Meter) Unpriced() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unpriced
}
