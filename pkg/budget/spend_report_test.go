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

// The reporting half of Spend (#356): the breakdown a usage report needs
// is emitted by the meter that computed it, not re-derived downstream.
//
// These tests reuse the measured Claude turn from usage_detail_test.go —
// the same fixture the pricing assertions run on, deliberately, because
// the claim here is that the report and the invoice describe one call.

package budget

import (
	"math"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// hitEvent is the second measured turn: the same prompt served from the
// cache the first one warmed.
func hitEvent() *session.Event {
	return &session.Event{
		LLMResponse: model.LLMResponse{
			ModelVersion: "claude-sonnet-5",
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:        warmPrompt,
				CachedContentTokenCount: warmWrite,
				CandidatesTokenCount:    warmOutput,
				TotalTokenCount:         warmPrompt + warmOutput,
			},
			CustomMetadata: map[string]any{
				DetailKey: stubDetail{read: i64(warmWrite), write: i64(0)},
			},
		},
	}
}

// collect runs events through a meter and returns what OnSpend saw.
func collect(t *testing.T, cfg Config, evs ...*session.Event) []Spend {
	t.Helper()
	var got []Spend
	prev := cfg.OnSpend
	cfg.OnSpend = func(s Spend) {
		got = append(got, s)
		if prev != nil {
			prev(s)
		}
	}
	m := New(cfg)
	for i, ev := range evs {
		if err := m.Observe(ev); err != nil {
			t.Fatalf("observe %d: %v", i, err)
		}
	}
	return got
}

// The buckets on the Spend are the buckets the call was priced from.
// Pre-#356 every one of these was absent from the type, so the /usage
// endpoint reported eight zeros and no way to tell them from measurements.
func TestSpendCarriesTheBucketsItWasPricedFrom(t *testing.T) {
	spends := collect(t, Config{Limits: Limits{Pricer: tieredPricer()}}, warmingEvent(true), hitEvent())
	if len(spends) != 2 {
		t.Fatalf("OnSpend fired %d times, want 2", len(spends))
	}
	warm, hit := spends[0], spends[1]

	// The warm turn: the write bucket is what the genai projection cannot
	// carry, and it is the difference between the two prices.
	if got := warm.Call.CacheWriteTokens; got != warmWrite {
		t.Errorf("warm turn cache-write tokens = %d, want %d", got, warmWrite)
	}
	if got := warm.Call.CachedInputTokens; got != 0 {
		t.Errorf("warm turn cached tokens = %d, want 0 — nothing was served from cache on the turn that filled it", got)
	}
	if got := warm.Call.UncachedInputTokens; got != warmInput {
		t.Errorf("warm turn fresh input = %d, want %d", got, warmInput)
	}

	// The hit turn: the same prompt, now a read.
	if got := hit.Call.CachedInputTokens; got != warmWrite {
		t.Errorf("hit turn cached tokens = %d, want %d", got, warmWrite)
	}
	if got := hit.Call.CacheWriteTokens; got != 0 {
		t.Errorf("hit turn cache-write tokens = %d, want a reported zero", got)
	}

	// The three input buckets sum to the prompt on both, which is what
	// makes "input tokens" a number a report can add up.
	for _, s := range spends {
		sum := s.Call.UncachedInputTokens + s.Call.CachedInputTokens + s.Call.CacheWriteTokens
		if sum != warmPrompt {
			t.Errorf("input buckets sum to %d, want the prompt's %d", sum, warmPrompt)
		}
	}
}

// The reference figure is the whole point of the second pass: what this
// call would have cost with no cache at all. Nothing downstream of the
// meter holds the rates, so if it is not computed here it cannot be
// computed.
func TestSpendPricesTheUncachedCounterfactual(t *testing.T) {
	spends := collect(t, Config{Limits: Limits{Pricer: tieredPricer()}}, warmingEvent(true), hitEvent())

	// Every prompt token at the fresh-input rate, output unchanged. The
	// same number for both turns: it is a property of the prompt, not of
	// how the backend happened to serve it.
	wantRef := (warmPrompt*2.0 + warmOutput*10.0) / 1e6
	for i, s := range spends {
		if math.Abs(s.CostUSDUncachedReference-wantRef) > 1e-12 {
			t.Errorf("turn %d reference = $%.6f, want $%.6f", i+1, s.CostUSDUncachedReference, wantRef)
		}
	}

	// The delta is the saving, and on the warming turn it runs the other
	// way: a cache write bills at 1.25x fresh input, so warming costs
	// $0.0144 MORE than not caching at all. That is not a bug to floor
	// away — it is the up-front half of the trade, and an operator whose
	// workload warms a cache it never reuses is being told something
	// true. The payback lands on the first hit, and after one it is
	// already 2.6x ahead.
	warm, hit := spends[0], spends[1]
	if want := warmWrite * (2.5 - 2.0) / 1e6; math.Abs((warm.CostUSD-warm.CostUSDUncachedReference)-want) > 1e-12 {
		t.Errorf("warming premium = $%.6f, want $%.6f", warm.CostUSD-warm.CostUSDUncachedReference, want)
	}
	if want := warmWrite * (2.0 - 0.2) / 1e6; math.Abs((hit.CostUSDUncachedReference-hit.CostUSD)-want) > 1e-12 {
		t.Errorf("cache saving on the hit turn = $%.6f, want $%.6f", hit.CostUSDUncachedReference-hit.CostUSD, want)
	}
	net := (warm.CostUSDUncachedReference + hit.CostUSDUncachedReference) - (warm.CostUSD + hit.CostUSD)
	if net <= 0 {
		t.Errorf("the pair is $%.6f net, so this fixture no longer shows caching paying back", net)
	}
}

// A call the pricer cannot price still reports a reference, and reports
// it equal to the cost. The flat rate has no cache notion at all, so any
// other number would be an invention — and a zero one renders as "caching
// cost you everything you spent".
func TestAnUnpricedCallReportsNoSaving(t *testing.T) {
	spends := collect(t, Config{Limits: Limits{Pricer: tieredPricer(), RatePer1K: 0.01}},
		pricedEvent("a-model-with-no-rate-card", 1000, 400, 100))
	if len(spends) != 1 {
		t.Fatalf("OnSpend fired %d times, want 1", len(spends))
	}
	s := spends[0]
	if !s.Unpriced {
		t.Fatalf("the pricer resolved a model the table does not carry; this fixture no longer tests the fallback")
	}
	if s.CostUSD == 0 {
		t.Fatal("the flat rate produced no cost")
	}
	if s.CostUSDUncachedReference != s.CostUSD {
		t.Errorf("reference = $%.6f, want it equal to the cost $%.6f", s.CostUSDUncachedReference, s.CostUSD)
	}
}

// Model is the id the price was resolved against, because that is the
// only key a per-model breakdown can group on without its rows failing
// to add up to the total above them. When the pricer resolves the event's
// echo, the echo wins; when the event names nothing — the shape every
// streaming Gemini call arrives in — the configured name is reported
// rather than an empty row.
func TestSpendNamesTheModelThePriceResolvedAgainst(t *testing.T) {
	cfg := Config{Limits: Limits{Pricer: tieredPricer(), Model: "claude-haiku-4-5"}}

	echoed := collect(t, cfg, pricedEvent("claude-sonnet-5", 1000, 0, 100))
	if got := echoed[0].Model; got != "claude-sonnet-5" {
		t.Errorf("Model = %q, want the event's own echo", got)
	}

	silent := collect(t, cfg, pricedEvent("", 1000, 0, 100))
	if got := silent[0].Model; got != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want the configured name — an unnamed call is not an unknown model", got)
	}
	// And the price agrees with the name: haiku's rates, not sonnet's.
	if want := (1000*1.0 + 100*5.0) / 1e6; math.Abs(silent[0].CostUSD-want) > 1e-12 {
		t.Errorf("cost $%.6f does not match the model the row is filed under (want $%.6f)", silent[0].CostUSD, want)
	}

	// No pricer at all: the name is still reported, so a flat-rate meter
	// produces a breakdown instead of one anonymous row.
	flat := collect(t, Config{Limits: Limits{RatePer1K: 0.01}}, pricedEvent("claude-sonnet-5", 1000, 0, 100))
	if got := flat[0].Model; got != "claude-sonnet-5" {
		t.Errorf("Model = %q on a flat-rate meter, want the event's echo", got)
	}
}

// Thinking and tool-use counts are breakdowns of buckets already
// counted, not additions to them. A report that summed either into its
// totals would double-count it, so the relationship is pinned here where
// the numbers are produced.
func TestThoughtsAndToolUseAreSubsetsNotAdditions(t *testing.T) {
	ev := &session.Event{
		LLMResponse: model.LLMResponse{
			ModelVersion: "claude-sonnet-5",
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:        1000,
				ToolUsePromptTokenCount: 250,
				CandidatesTokenCount:    100,
				ThoughtsTokenCount:      400,
				TotalTokenCount:         1500,
			},
		},
	}
	s := collect(t, Config{Limits: Limits{Pricer: tieredPricer()}}, ev)[0]

	if got, want := s.Call.OutputTokens, 500; got != want {
		t.Errorf("billable output = %d, want %d — thinking bills at the output rate", got, want)
	}
	if s.ThoughtsTokens != 400 {
		t.Errorf("ThoughtsTokens = %d, want 400", s.ThoughtsTokens)
	}
	if s.ThoughtsTokens > int64(s.Call.OutputTokens) {
		t.Error("ThoughtsTokens is not inside OutputTokens; a report adding the two would exceed the call")
	}
	if s.ToolUseTokens != 250 {
		t.Errorf("ToolUseTokens = %d, want 250", s.ToolUseTokens)
	}
	prompt := s.Call.UncachedInputTokens + s.Call.CachedInputTokens + s.Call.CacheWriteTokens
	if s.ToolUseTokens > int64(prompt) {
		t.Error("ToolUseTokens is not inside the prompt; a report adding the two would exceed the call")
	}
	if s.Tokens != 1500 {
		t.Errorf("Tokens = %d, want the provider's own total 1500 rather than a recomputed one", s.Tokens)
	}
}

// The event's timestamp rides along, because the row is read as a
// history and "when the hook ran" is not when the call landed.
func TestSpendCarriesTheEventTimestamp(t *testing.T) {
	at := time.Date(2026, 9, 19, 14, 3, 7, 0, time.UTC)
	ev := pricedEvent("claude-sonnet-5", 1000, 0, 100)
	ev.Timestamp = at

	if got := collect(t, Config{Limits: Limits{Pricer: tieredPricer()}}, ev)[0].At; !got.Equal(at) {
		t.Errorf("At = %v, want %v", got, at)
	}
}

// Nothing above changed what the ledger reads. The four fields that were
// on Spend before the breakdown joined them still mean what they meant,
// which is what lets eventlog.SpendRecord keep ignoring the rest.
func TestTheLedgerFieldsAreUnchanged(t *testing.T) {
	s := collect(t, Config{Limits: Limits{Pricer: tieredPricer()}}, warmingEvent(true))[0]
	if s.Author != "" {
		t.Errorf("Author = %q, want the event's (empty here)", s.Author)
	}
	if s.Tokens != warmPrompt+warmOutput {
		t.Errorf("Tokens = %d, want %d", s.Tokens, warmPrompt+warmOutput)
	}
	if math.Abs(s.CostUSD-warmCostUSD) > 1e-9 {
		t.Errorf("CostUSD = $%.6f, want the rate card's $%.6f", s.CostUSD, warmCostUSD)
	}
	if s.Unpriced {
		t.Error("Unpriced is set on a call the table prices")
	}
}
