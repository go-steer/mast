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
// is the agent's name on every dispatch shape mast builds, which is
// what makes one attribution rule enough for all of them.
//
// It does not follow that one seam is enough to SEE them. A
// coordinator's sub-agent tool and a workflow-graph node funnel their
// events up the root runner's stream, so observing that stream catches
// both. A planner's invoke_specialist does not: it runs the specialist
// on a private runner, whose events reach the host only through
// planner.SubRunObserver (#226). A host that meters must feed this
// package from both — the arithmetic is identical either way, and the
// Author on a sub-run event is still the specialist's name, so a
// declared ceiling binds on the planner's door exactly as it does on a
// coordinator's.
//
// A scope carries its own ceilings and, when the specialist declares a
// `model:` override, its own price, so a cheap analyst's tokens are not
// billed at the synthesizer's rate.
//
// Composition is tightest-cap-wins by construction rather than by
// arithmetic: every event is checked against its scope and against the
// session, and whichever ceiling is reached first stops what it holds.
// A scope's ceiling is reported ahead of the session's on the event
// that crosses both, because the specialist is the more specific fact
// and the workload's cap would have been crossed on a later call
// anyway.
//
// # Who a ceiling stops
//
// A ceiling holder is either the workload or one specialist, and since
// v0.6 the two have different consequences. Scope(err) reports which an
// enforcement error belonged to: a scoped one closes that specialist's
// path, and the turn drivers log it, count it, and route on; an
// unscoped one ends the turn. Through v0.5 both ended the turn, which
// was never a decision — Observe runs on the event stream, outside the
// specialist's own run, where the run context was the only lever
// available. Allow is the lever that was missing, and it is per-call
// and per-agent (see allow.go).
//
// The counting follows the same split. Refusals reports every refusal,
// for the metric and the log line; SessionRefusals reports only the
// workload's own, for the driver deciding whether to stop. Each keeps
// its own first reason on purpose: the first refusal of a turn is
// usually a specialist's, and a driver that stopped on the session's
// cap while quoting a specialist's reason would send an operator to
// raise the wrong ceiling.
//
// # Known limitations (findings, not TODOs)
//
// Metering at the event stream is enforcement-after-the-call — a single
// runaway call is only priced once its usage event lands. This meter
// stays the ledger; Allow is asked in front of it, and refuses only
// where the arithmetic is a proof rather than an estimate, so the two
// disagree by at most one call.
package budget

import (
	"errors"
	"fmt"
	"sync"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/pricing"
)

// ErrExceeded is returned by Observe once cumulative usage crosses a
// ceiling. Callers should abort the run — unless Scope reports the
// ceiling was one specialist's, in which case one path is closed and
// the run can continue without it.
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

	// Catalog prices each model call exactly, against the per-model
	// input / cache-read / output rates rather than one blended number.
	//
	// Optional, and strictly better than RatePer1K where a caller can
	// supply it. The flat rate exists because this meter originally saw
	// only UsageMetadata.TotalTokenCount, so internal/compose derives it
	// as the plain average of a model's input and output rates — an
	// approximation that "overcharges input-heavy sessions and
	// undercharges output-heavy ones". That premise stopped being true
	// once the event carried the input/output split and the cache-read
	// subset: the call can be priced against the same pkg/pricing catalog
	// everything else uses. The error is not small on a real agent — an
	// input-heavy, cache-warm session measured here ran 5.9x over its
	// flat-rate figure, and a cost ceiling that wrong is a ceiling that
	// fires on the wrong sessions.
	//
	// Unknown models fall through to RatePer1K, so a catalog miss never
	// silently drops a session's cost to zero. Unpriced counts them.
	//
	// On a scope, nil means "inherit the session's catalog", matching
	// Backend, Model and RatePer1K below.
	Catalog *pricing.Catalog

	// Backend and Model are the (backend, model) pair the catalog is
	// keyed by, resolved by the caller. Ignored unless Catalog is set.
	//
	// This is what the field above used to say it owed, paid. A rate is
	// a property of the pair and not of the model — the bare table's
	// Claude rows are first-party and its Gemini rows are Vertex's — so
	// pricing goes through pricing.LookupFor, whose backend half is
	// internal/compose.Backend and is not on the event. An empty Backend
	// is exactly the bare lookup, which is right for an offline fake or
	// a backend that could not be resolved.
	//
	// Model is what that comment expected to read off ModelVersion, and
	// it has to be carried for a second reason the comment did not know:
	// the event frequently does not name the model at all. ADK's
	// streaming aggregator rebuilds the final response — the only one
	// carrying UsageMetadata — without ModelVersion
	// (internal/llminternal/stream_aggregator.go, adk/v2 v2.2.0), so on
	// a streaming Gemini run every priced call reaches the catalog with
	// an empty key. Its database session service has no column for the
	// field either, which is one of the reasons durable.go keeps a spend
	// ledger rather than replaying events.
	//
	// The event is still asked first, because a server echo names the
	// model that was actually billed and can be more specific than the
	// id the run was started with; an echo the catalog cannot resolve
	// falls through to Model rather than ending the search. Without this
	// pair a configured Catalog would miss on every Gemini call and
	// quietly serve the flat rate it was configured to replace.
	//
	// Model is the name internal/compose already resolved —
	// SpecModelName for a specialist, the run's model for the session.
	Backend string
	Model   string

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

	// OnSpend, when set, is called once per priced call with what that
	// call added — the write half of the durability seam described in
	// durable.go. It runs on the caller's goroutine, outside the meter's
	// lock, after the fold and before Observe returns, including when
	// the fold reported a crossed ceiling: the call happened and the
	// money is spent whether or not it was the one that stopped the run.
	//
	// It must not call back into the meter (Snapshot and friends take
	// the same lock the fold just released, so a re-entrant caller would
	// read a different meter than the one it was told about, and a
	// re-entrant Observe would recurse). Keep it to handing the Spend
	// somewhere durable.
	OnSpend func(Spend)
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

	// restored latches once prior spend has been folded in, so a second
	// fold is refused rather than double-counted (see Restore).
	restored bool

	// refusals counts what Allow turned down, and firstRefusal keeps the
	// reason it gave the first time. A refusal produces a synthesized
	// answer rather than an error, so without this a turn stopped by its
	// ceiling is indistinguishable from one that finished (see Refusals).
	refusals     int
	firstRefusal error

	// The subset of those a specialist cannot be blamed for. Counted
	// separately rather than filtered on read because only the first
	// reason is kept, and the first refusal of a turn is often a
	// specialist's while the one that has to stop the turn is the
	// workload's (see SessionRefusals).
	sessionRefusals     int
	firstSessionRefusal error

	// onSpend is Config.OnSpend. Set at construction and never mutated,
	// so Observe reads it without the lock.
	onSpend func(Spend)
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
	m := &Meter{limits: cfg.Limits, onSpend: cfg.OnSpend}
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
	s, err := m.fold(ev)
	// Outside the lock, and unconditional: a call that crossed a ceiling
	// still cost what it cost, and a ledger that dropped exactly the
	// calls that tripped the guardrail would understate every session
	// this feature exists for.
	if m.onSpend != nil {
		m.onSpend(s)
	}
	return err
}

// fold is Observe's locked half: it accumulates the event and reports
// both what it added and whether that crossed a ceiling.
func (m *Meter) fold(ev *session.Event) (Spend, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tokens := int64(ev.UsageMetadata.TotalTokenCount)
	rate := m.limits.RatePer1K
	cat := m.limits.Catalog
	backend, model := m.limits.Backend, m.limits.Model
	scope, scoped := m.scopes[ev.Author]
	if scoped {
		if scope.RatePer1K > 0 {
			rate = scope.RatePer1K
		}
		if scope.Catalog != nil {
			cat = scope.Catalog
		}
		if scope.Model != "" {
			// The backend comes with the model and not on its own: an
			// inherited backend against an overridden model would key
			// the lookup to a pair that never ran. A scope that resolved
			// to no backend prices bare, same as the session would.
			backend, model = scope.Backend, scope.Model
		}
	}
	// Cost accrues per event rather than being recomputed from the
	// running token total: with per-scope rates and per-model catalog
	// pricing the session total is a sum of differently-priced calls,
	// not one multiplication.
	spend, unpriced := m.priceOf(ev, cat, backend, model, rate)
	s := Spend{Author: ev.Author, Tokens: tokens, CostUSD: spend, Unpriced: unpriced}

	m.total.add(tokens, spend)
	if scoped {
		u := m.spent[ev.Author]
		u.add(tokens, spend)
		if err := check(scope, u); err != nil {
			return s, scopedTo(ev.Author, fmt.Errorf("%w: specialist %q: %s", ErrExceeded, ev.Author, err))
		}
	}
	if err := check(m.limits, &m.total); err != nil {
		return s, fmt.Errorf("%w: %s", ErrExceeded, err)
	}
	return s, nil
}

func (u *usage) add(tokens int64, cost float64) {
	u.calls++
	u.tokens += tokens
	u.cost += cost
}

// check reports the first ceiling in l that u has crossed, as the
// detail half of an ErrExceeded message.
func check(l Limits, u *usage) error {
	if t := crossed("", l, u); len(t) > 0 {
		return errors.New(t[0].Reason)
	}
	return nil
}

// crossed lists every ceiling in l that u is already past, in the
// order check reports them. Enforcement and reporting read the same
// comparisons from here: a projection that decided "tripped" any other
// way could disagree with the meter that actually stops the run.
func crossed(scope string, l Limits, u *usage) []Trip {
	var out []Trip
	if l.MaxTurns > 0 && u.calls > l.MaxTurns {
		out = append(out, Trip{Scope: scope, Dimension: DimensionTurns,
			Reason: fmt.Sprintf("%d model calls (turns) > cap %d", u.calls, l.MaxTurns)})
	}
	if l.MaxTokens > 0 && u.tokens > l.MaxTokens {
		out = append(out, Trip{Scope: scope, Dimension: DimensionTokens,
			Reason: fmt.Sprintf("%d tokens > cap %d", u.tokens, l.MaxTokens)})
	}
	if l.MaxCostUSD > 0 && u.cost > l.MaxCostUSD {
		out = append(out, Trip{Scope: scope, Dimension: DimensionCostUSD,
			Reason: fmt.Sprintf("$%.4f > cap $%.4f (%d tokens over %d calls)", u.cost, l.MaxCostUSD, u.tokens, u.calls)})
	}
	return out
}

// priceOf costs one model call, against cat where it can and the flat
// rate where it cannot, and reports whether it had to fall back.
// Caller holds m.mu.
//
// Cached input is billed at the catalog's cache-read rate, which is
// typically a tenth of fresh input; on a cache-warm agent that subset is
// the majority of the prompt, so folding it in at the input rate is the
// single largest source of error in a flat-rate figure.
func (m *Meter) priceOf(ev *session.Event, cat *pricing.Catalog, backend, model string, rate float64) (cost float64, unpriced bool) {
	u := ev.UsageMetadata
	if cat != nil {
		if r, ok := lookupRates(cat, backend, ev.ModelVersion, model); ok {
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
			// Thoughts are billed at the output rate and counted
			// separately from the candidates: Gemini reports
			// promptTokenCount + candidatesTokenCount + thoughtsTokenCount
			// == totalTokenCount, so leaving them out is a straight
			// undercount of output. On a reasoning model it is not a
			// rounding error — a triage run measured here spent 6,449
			// thinking tokens against 1,180 candidate tokens, so the
			// omitted term was 85% of billable output. The field is
			// Gemini-only; pkg/providers/anthropic never sets it, and
			// Anthropic's own output count already includes thinking.
			out := int(u.CandidatesTokenCount) + int(u.ThoughtsTokenCount)
			return r.CostUSDWithCache(int(u.PromptTokenCount)-cached, cached, out), false
		}
		m.unpriced++
		unpriced = true
	}
	return float64(u.TotalTokenCount) / 1000 * rate, unpriced
}

// lookupRates returns the first of ids the catalog prices on backend,
// skipping empty ones. Order is preference: callers pass the event's own
// ModelVersion before the configured name, because a server echo names
// what was actually billed — but an echo the catalog cannot resolve is
// no better than no echo, so a miss falls through rather than ending
// the search. See Limits.Model for why the second id is needed at all.
//
// Every id is tried on the same backend, including the echoed one: the
// echo names the model the backend billed for, never a different
// backend. LookupFor already falls back from the qualified key to the
// bare one, so an empty backend and an unqualified model both still
// price.
func lookupRates(cat *pricing.Catalog, backend string, ids ...string) (pricing.Rates, bool) {
	for _, id := range ids {
		if id == "" {
			continue
		}
		if r, ok := cat.LookupFor(backend, id); ok && !r.IsZero() {
			return r, true
		}
	}
	return pricing.Rates{}, false
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
