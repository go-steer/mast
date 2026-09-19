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
	"google.golang.org/genai"
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

	// Pricer prices each model call exactly, against the per-model
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
	// Unknown models fall through to RatePer1K, so a pricing miss never
	// silently drops a session's cost to zero. Unpriced counts them.
	//
	// On a scope, nil means "inherit the session's pricer", matching
	// Backend, Model and RatePer1K below.
	Pricer Pricer

	// Backend and Model are the (backend, model) pair the pricer is
	// keyed by, resolved by the caller. Ignored unless Pricer is set.
	//
	// This is what the field above used to say it owed, paid. A rate is
	// a property of the pair and not of the model — mast's bare table has
	// Claude rows that are first-party and Gemini rows that are Vertex's
	// — so the pricer is asked for the pair, whose backend half comes
	// from internal/compose.Backend and is not on the event. An empty
	// Backend asks for the bare id, which is right for an offline fake or
	// a backend that could not be resolved.
	//
	// Model is what that comment expected to read off ModelVersion, and
	// it has to be carried for a second reason the comment did not know:
	// the event frequently does not name the model at all. ADK's
	// streaming aggregator rebuilds the final response — the only one
	// carrying UsageMetadata — without ModelVersion
	// (internal/llminternal/stream_aggregator.go, adk/v2 v2.2.0), so on
	// a streaming Gemini run every priced call reaches the pricer with
	// an empty key. Its database session service has no column for the
	// field either, which is one of the reasons durable.go keeps a spend
	// ledger rather than replaying events.
	//
	// The event is still asked first, because a server echo names the
	// model that was actually billed and can be more specific than the
	// id the run was started with; an echo the pricer cannot resolve
	// falls through to Model rather than ending the search. Without this
	// pair a configured Pricer would miss on every Gemini call and
	// quietly serve the flat rate it was configured to replace.
	//
	// Model is the name internal/compose already resolved —
	// SpecModelName for a specialist, the run's model for the session.
	Backend string
	Model   string

	// RatePer1K is the flat USD price per 1K total tokens (spike
	// pricing model), and the fallback for a call Pricer cannot price.
	//
	// On a scope, zero means "inherit the session's rate" — the right
	// default for a specialist that declares no model of its own, and
	// the reason an un-tiered roster prices exactly as it did before
	// scopes existed.
	RatePer1K float64
}

// IsZero reports whether l declares nothing at all — no ceiling and no
// price. Scope composition uses it to drop a specialist that asked for
// neither.
//
// It exists because Limits stopped being safely comparable when Pricer
// became an interface: == on two Limits that both carry a pricer whose
// dynamic type is uncomparable — a func, a map-backed table, a mock with
// a recorded call log — panics at runtime. The two call sites this
// replaced compared against the zero Limits, which is the one shape that
// cannot panic (the dynamic types differ, so the comparison
// short-circuits), so nothing was broken. But the operator being usable
// only against one specific operand is not a property anyone should have
// to know, and "declares nothing" is what those call sites were asking
// anyway.
func (l Limits) IsZero() bool {
	return l.MaxCostUSD == 0 &&
		l.MaxTokens == 0 &&
		l.MaxTurns == 0 &&
		l.Pricer == nil &&
		l.Backend == "" &&
		l.Model == "" &&
		l.RatePer1K == 0
}

// Pricer prices one model call. ok is false when the call cannot be
// priced — an unknown (backend, model) pair, or a known one whose rates
// are all zero — and the meter then falls back to Limits.RatePer1K and
// counts the call in Unpriced.
//
// This is deliberately narrower than the rate table behind it. A pricer
// is asked for a number, never for rates: mast's own implementation
// wraps pkg/pricing, but that package owes a re-key from the backend
// name to the more general notion of a provider profile
// (docs/model-support-design.md M2), and a third backend must not be a
// breaking change to the meter. Naming the catalog here would have
// frozen the table's shape through pkg/budget, which is one of the six
// paths v1.0 covers.
type Pricer interface {
	PriceCall(backend, modelID string, c Call) (usd float64, ok bool)
}

// Call is one model call's billable token counts, already normalized
// out of the provider's usage record.
//
// A struct rather than three int parameters because the buckets are the
// part expected to grow: reasoning tokens billed apart from output, and
// cache-*write* tokens billed apart from cache reads, are both on
// model-support-design's list, and pkg/pricing's own Rates says 1h-TTL
// cache support "means adding a second rate here". Each of those arrives
// as a new field, which a pricer that does not know about it ignores.
type Call struct {
	// UncachedInputTokens is prompt tokens billed at the full input
	// rate — the prompt less whatever the provider served from cache.
	UncachedInputTokens int

	// CachedInputTokens is the prompt subset served from cache, billed
	// at the cache-read rate.
	CachedInputTokens int

	// CacheWriteTokens is the prompt subset that CREATED a cache entry
	// this call, billed at the cache-write rate — a premium over fresh
	// input (Anthropic's 5-minute TTL is 1.25x), not a discount. Zero
	// for a provider that has no such bucket: Gemini's explicit caches
	// bill storage per hour rather than per written token.
	//
	// The three input buckets are mutually exclusive and sum to the
	// prompt.
	CacheWriteTokens int

	// OutputTokens is everything the model generated, billed at the
	// output rate. On a reasoning model this includes thinking tokens.
	OutputTokens int
}

// DetailKey is the model.LLMResponse.CustomMetadata key a provider
// adapter attaches its usage sidecar under. The meter reads the value
// there when it implements Detailer and ignores it otherwise.
//
// The key is stable and namespaced because the sidecar rides on an
// ADK-owned map that anything in the process may write to.
const DetailKey = "mast.usage_detail"

// Detailer is the sidecar contract: what a provider adapter attaches
// under DetailKey so the meter can see buckets genai's usage metadata
// has no field for. mast's implementation is pkg/providers/usage.Detail.
//
// An interface rather than a named struct type because pkg/budget
// imports nothing else in this module — a provider package naming a
// budget type is the right direction for that dependency, and the
// reverse would drag an unsupported package into the v1.0 freeze
// through a supported one (#338).
//
// The method returns a struct for the same reason Call is one: the
// buckets are the part expected to grow, and a new field is additive
// where a new method is not.
type Detailer interface {
	UsageBuckets() Buckets
}

// Buckets is a provider's own statement of the token counts behind one
// call, for the counts genai's UsageMetadata cannot carry.
//
// Every count is a pointer because nil means the provider did not say,
// which is not the same as zero: an Anthropic turn that wrote no cache
// entry reports cache_creation_input_tokens = 0, while a provider that
// has no such concept reports nothing at all, and billing those two the
// same way is how an undercount reports success.
//
// A stated count wins over the genai projection of the same bucket.
// Absent fields fall back to the genai fields, so an event with no
// sidecar prices exactly as it did before this type existed.
type Buckets struct {
	// CacheReadTokens is the prompt subset served from cache —
	// Anthropic's cache_read_input_tokens, Gemini's
	// cachedContentTokenCount. Also reachable through
	// UsageMetadata.CachedContentTokenCount, so a nil here is not a
	// gap; the field exists so a provider whose genai projection is
	// lossy can correct it.
	CacheReadTokens *int64

	// CacheWriteTokens is the prompt subset that created a cache entry —
	// Anthropic's cache_creation_input_tokens. genai's usage metadata
	// has nowhere to carry this, which is why the sidecar exists: folded
	// into the uncached bucket it is billed at 1x instead of 1.25x and
	// every cache-warming turn is undercounted (#352).
	CacheWriteTokens *int64
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

	// FinalReport lets an agent that has already spent something buy one
	// model call past its ceiling, to write the report it was stopped
	// before finishing. Off by default: a cap that can be overshot by one
	// call is not what every operator declared. See finalreport.go for
	// what the grant is and the three bounds on it.
	FinalReport bool
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

	// finalReport is Config.FinalReport, and finalReportTaken latches
	// which authors have spent their one grant. See finalreport.go.
	finalReport      bool
	finalReportTaken map[string]bool
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
	m := &Meter{limits: cfg.Limits, onSpend: cfg.OnSpend, finalReport: cfg.FinalReport}
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
	s, err := m.fold(ev, flooredUsage(ev.UsageMetadata))
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
// both what it added and whether that crossed a ceiling. u is the
// event's usage metadata as flooredUsage read it — every count in this
// path comes from there and none from ev.UsageMetadata.
func (m *Meter) fold(ev *session.Event, u genai.GenerateContentResponseUsageMetadata) (Spend, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tokens := int64(u.TotalTokenCount)
	rate := m.limits.RatePer1K
	pricer := m.limits.Pricer
	backend, model := m.limits.Backend, m.limits.Model
	scope, scoped := m.scopes[ev.Author]
	if scoped {
		if scope.RatePer1K > 0 {
			rate = scope.RatePer1K
		}
		if scope.Pricer != nil {
			pricer = scope.Pricer
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
	// running token total: with per-scope rates and per-model exact
	// pricing the session total is a sum of differently-priced calls,
	// not one multiplication.
	c := callOf(ev, u)
	spend, ref, priced, unpriced := m.priceOf(c, u, pricer, backend, rate, ev.ModelVersion, model)
	s := Spend{
		Author:   ev.Author,
		Tokens:   tokens,
		CostUSD:  spend,
		Unpriced: unpriced,

		At:                       ev.Timestamp,
		Model:                    priced,
		Call:                     c,
		ThoughtsTokens:           int64(u.ThoughtsTokenCount),
		ToolUseTokens:            int64(u.ToolUsePromptTokenCount),
		CostUSDUncachedReference: ref,
	}

	m.total.add(tokens, spend)
	if scoped {
		su := m.spent[ev.Author]
		su.add(tokens, spend)
		if err := check(scope, su); err != nil {
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

// priceOf costs one model call, through the pricer where it can and at
// the flat rate where it cannot, and reports whether it had to fall
// back. Caller holds m.mu.
//
// It prices the call twice. ref is what the same call would have cost on
// a backend with no prompt cache at all, which is the only figure that
// answers "what is the cache buying me" and the one an operator has no
// other way to get (attach.UsageTotals.CostUSDUncachedReference, #356).
// The second pass is a second lookup in a rate table, not a second model
// call, and it has to happen here: by the time the Spend reaches a
// consumer the rates are gone.
//
// ref is not clamped to cost. A warming turn really does cost more than
// not caching — see Spend.CostUSDUncachedReference.
//
// priced is the id the price was resolved against, and it is the key any
// per-model report must group on. Grouping on a different id than the
// one that was billed — the sidecar's ServedModel echo, say — produces a
// breakdown whose rows do not add up to the total above them.
func (m *Meter) priceOf(c Call, u genai.GenerateContentResponseUsageMetadata, p Pricer, backend string, rate float64, ids ...string) (cost, ref float64, priced string, unpriced bool) {
	if p != nil {
		if usd, id, ok := priceFirst(p, backend, c, ids...); ok {
			ref := uncachedReference(c)
			if ref == c {
				// Nothing was served from cache and nothing was written to
				// one, so the counterfactual is the call itself. Skipping
				// the lookup is exact rather than an approximation, and it
				// keeps the no-cache case — every call on a backend without
				// prompt caching, and the first call on one with it — at
				// one table lookup.
				return usd, usd, id, false
			}
			// Same id, not the search again: the reference is this call
			// repriced, so asking a second time could answer from a
			// different row.
			refUSD, _, ok := priceFirst(p, backend, ref, id)
			if !ok {
				// Unreachable through mast's own pricer (the row that
				// answered above answers again), but a Pricer is an
				// interface. Reporting the cost is "no saving computable",
				// which is true; reporting zero would claim the cache cost
				// this call everything it spent.
				refUSD = usd
			}
			return usd, refUSD, id, false
		}
		m.unpriced++
		unpriced = true
	}
	// The flat rate has no notion of a cache, so the reference IS the
	// cost. Reporting a saving of zero would be a claim; reporting the
	// same number twice is the gap, and the renderer omits the delta
	// when it is not positive.
	flat := float64(u.TotalTokenCount) / 1000 * rate
	return flat, flat, firstNonEmpty(ids...), unpriced
}

// uncachedReference is c as it would have been billed by a backend that
// served nothing from cache and kept nothing for later: every prompt
// token fresh, output unchanged.
//
// Cache writes fold into fresh input rather than staying separate,
// because the counterfactual is a caller who never asked for the entry
// — those tokens would still have been sent, at the plain rate, without
// the premium. Leaving them in their own bucket would price the
// reference as "cached but paying to write", which is not a run anyone
// could have had.
func uncachedReference(c Call) Call {
	return Call{
		UncachedInputTokens: c.UncachedInputTokens + c.CachedInputTokens + c.CacheWriteTokens,
		OutputTokens:        c.OutputTokens,
	}
}

// priceFirst asks the pricer for each of ids in turn and returns the
// first price it gets, and the id that got it, skipping empty ones.
// Order is preference: callers pass the event's own ModelVersion before
// the configured name, because a server echo names what was actually
// billed — but an echo the pricer cannot resolve is no better than no
// echo, so a miss falls through rather than ending the search. See
// Limits.Model for why the second id is needed at all.
//
// Every id is tried on the same backend, including the echoed one: the
// echo names the model the backend billed for, never a different
// backend.
func priceFirst(p Pricer, backend string, c Call, ids ...string) (float64, string, bool) {
	for _, id := range ids {
		if id == "" {
			continue
		}
		if usd, ok := p.PriceCall(backend, id, c); ok {
			return usd, id, true
		}
	}
	return 0, "", false
}

// firstNonEmpty names the call for a report when no pricer resolved it —
// an unpriced call, or a meter running on the flat rate alone. It is the
// same preference order priceFirst walks, so the two never disagree
// about which id describes a call; what differs is only whether a rate
// was found for it.
func firstNonEmpty(ids ...string) string {
	for _, id := range ids {
		if id != "" {
			return id
		}
	}
	return ""
}

// callOf normalizes one event's usage into the billable buckets a
// Pricer is asked about. Reading the provider's counters is the meter's
// job and pricing them is the pricer's, which is the seam: everything
// below is an assertion about what the counters mean, and nothing below
// is an assertion about what they cost.
//
// Two sources, in that order of authority: the provider's own sidecar
// (Detailer, when the adapter attached one) and genai's usage metadata.
// A sidecar refines the split of a prompt whose total the genai record
// still owns — it never restates the total. u is that record as
// flooredUsage read it; the sidecar's own counts are clipped below.
//
// Cached input is separated because it bills at the cache-read rate,
// typically a tenth of fresh input; on a cache-warm agent that subset is
// the majority of the prompt, so folding it in at the input rate is the
// single largest source of error in a flat-rate figure. Cache writes are
// separated for the mirror-image reason: they bill at a premium, and
// folding them in undercounts every turn that warms a cache.
func callOf(ev *session.Event, u genai.GenerateContentResponseUsageMetadata) Call {
	b := bucketsOf(ev)

	prompt := int(u.PromptTokenCount)

	read := int(u.CachedContentTokenCount)
	if b.CacheReadTokens != nil {
		read = int(*b.CacheReadTokens)
	}
	var write int
	if b.CacheWriteTokens != nil {
		write = int(*b.CacheWriteTokens)
	}

	// Fitted, not trusted: a provider that over-reports an input bucket
	// would otherwise produce negative uncached tokens, billed at the
	// input rate as a credit — a ceiling that gets *further* away the
	// more the provider miscounts. core-agent's usage tracker guards the
	// cached counter the same way; every bucket that splits the prompt
	// needs the same guard, and it belongs here rather than in each
	// adapter, where the next provider would have to remember it.
	//
	// Reads are fitted first, so when the counters do not add up the
	// residual lands on the write bucket: reads are corroborated by a
	// genai field mast has always read, writes arrive only from the
	// sidecar. The error is attributed to the newer counter.
	read = fitBucket(read, prompt)
	write = fitBucket(write, prompt-read)

	// Thoughts are billed at the output rate and counted separately from
	// the candidates: Gemini reports promptTokenCount +
	// candidatesTokenCount + thoughtsTokenCount == totalTokenCount, so
	// leaving them out is a straight undercount of output. On a reasoning
	// model it is not a rounding error — a triage run measured here spent
	// 6,449 thinking tokens against 1,180 candidate tokens, so the omitted
	// term was 85% of billable output. The field is Gemini-only;
	// pkg/providers/anthropic never sets it, and Anthropic's own output
	// count already includes thinking.
	return Call{
		UncachedInputTokens: prompt - read - write,
		CachedInputTokens:   read,
		CacheWriteTokens:    write,
		OutputTokens:        int(u.CandidatesTokenCount) + int(u.ThoughtsTokenCount),
	}
}

// bucketsOf returns what the provider said about this call, or the zero
// Buckets — every field nil, "said nothing" — for an event carrying no
// sidecar, which is every event from an adapter that does not attach
// one and every event read back from storage (the sidecar is an
// in-process value, not part of the persisted event contract).
func bucketsOf(ev *session.Event) Buckets {
	d, _ := ev.CustomMetadata[DetailKey].(Detailer)
	if d == nil {
		return Buckets{}
	}
	return d.UsageBuckets()
}

// fitBucket clips one input bucket into the room the prompt has left.
// Negative is not a count, and a bucket bigger than the prompt it is a
// subset of is a miscount, not a credit.
func fitBucket(n, room int) int {
	if room < 0 {
		room = 0
	}
	switch {
	case n < 0:
		return 0
	case n > room:
		return room
	default:
		return n
	}
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
