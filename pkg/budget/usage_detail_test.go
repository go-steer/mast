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

// The provider-usage sidecar (#352): what the meter does with buckets
// genai's usage metadata cannot carry.
//
// These tests price against an explicit rate table, not the shipping
// catalog — see ratePricer. internal/compose owns the other half, that
// the real rates resolve.

package budget

import (
	"math"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// stubDetail implements Detailer the way a provider adapter does.
// Deliberately local: pkg/providers/usage imports pkg/budget, so a test
// in this package cannot import it back, and the contract being
// implementable by a type this package has never heard of is the
// property worth having.
type stubDetail struct{ read, write *int64 }

func (d stubDetail) UsageBuckets() Buckets {
	return Buckets{CacheReadTokens: d.read, CacheWriteTokens: d.write}
}

func i64(n int64) *int64 { return &n }

// ---------------------------------------------------------------------
// The measured turn.
//
// Real counts, not arithmetic: claude-sonnet-5 on Vertex (us-east5),
// 2026-09-13, a 28,804-token system block sent with cache_control
// ephemeral and answered in four tokens.
//
//	warm turn: input=10  cache_creation=28804 cache_read=0     output=4
//	next turn: input=10  cache_creation=0     cache_read=28804 output=4
//
// The adapter folds all three input buckets into PromptTokenCount
// (pkg/providers/anthropic/stream.go), so the genai record alone cannot
// tell the first line from a turn that sent 28,814 fresh tokens — and
// those two cost different money.
const (
	warmInput  = 10
	warmWrite  = 28_804
	warmOutput = 4
	warmPrompt = warmInput + warmWrite // what PromptTokenCount carries

	// claude-sonnet-5, from tieredPricer: $2 / $0.20 / $2.50 / $10 per
	// MTok. Stated here because the assertion below is a dollar figure.
	warmCostUSD = (warmInput*2.0 + warmWrite*2.5 + warmOutput*10.0) / 1e6 // $0.072070

	// What the same turn bills with the write bucket folded into fresh
	// input, which is every mast release up to and including v0.8.0.
	warmCostUSDFolded = (warmPrompt*2.0 + warmOutput*10.0) / 1e6 // $0.057668
)

// warmingEvent is that turn as the meter sees it: the genai projection
// the adapter can produce, plus the sidecar carrying what the
// projection dropped.
func warmingEvent(withSidecar bool) *session.Event {
	ev := &session.Event{
		LLMResponse: model.LLMResponse{
			ModelVersion: "claude-sonnet-5",
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:        warmPrompt,
				CachedContentTokenCount: 0,
				CandidatesTokenCount:    warmOutput,
				TotalTokenCount:         warmPrompt + warmOutput,
			},
		},
	}
	if withSidecar {
		ev.CustomMetadata = map[string]any{
			DetailKey: stubDetail{read: i64(0), write: i64(warmWrite)},
		}
	}
	return ev
}

// The exit criterion of #352, pinned to the fixture above rather than
// to a tolerance: a cache-warming Claude turn prices at the rate card's
// own figure.
func TestCacheWarmingTurnPricesAtTheRateCardFigure(t *testing.T) {
	m := NewMeter(Limits{Pricer: tieredPricer()})
	if err := m.Observe(warmingEvent(true)); err != nil {
		t.Fatalf("observe: %v", err)
	}
	_, got, _ := m.Snapshot()
	if math.Abs(got-warmCostUSD) > 1e-9 {
		t.Fatalf("cache-warming turn priced at $%.6f, want $%.6f", got, warmCostUSD)
	}
	if m.Unpriced() != 0 {
		t.Errorf("the pricer missed the call; the figure above is the flat rate, not a price")
	}
}

// The bug, stated as the gap between the two. Folding cache writes into
// fresh input is not a rounding error on a warm: it is a fifth of the
// turn, and more than a cent on a single 28k-token prompt.
func TestFoldingCacheWritesIntoFreshInputUndercountsTheTurn(t *testing.T) {
	with := NewMeter(Limits{Pricer: tieredPricer()})
	if err := with.Observe(warmingEvent(true)); err != nil {
		t.Fatalf("with sidecar: %v", err)
	}
	without := NewMeter(Limits{Pricer: tieredPricer()})
	if err := without.Observe(warmingEvent(false)); err != nil {
		t.Fatalf("without sidecar: %v", err)
	}
	_, priced, _ := with.Snapshot()
	_, folded, _ := without.Snapshot()

	if math.Abs(folded-warmCostUSDFolded) > 1e-9 {
		t.Fatalf("no-sidecar figure = $%.6f, want the pre-#352 $%.6f", folded, warmCostUSDFolded)
	}
	// cache_creation_tokens x input_rate x 0.25, which is where #352's
	// estimate came from — confirmed against the measured turn.
	if want := warmWrite * 2.0 * 0.25 / 1e6; math.Abs((priced-folded)-want) > 1e-9 {
		t.Errorf("undercount = $%.6f, want $%.6f", priced-folded, want)
	}
	if priced-folded <= 0.01 {
		t.Errorf("undercount $%.6f is under a cent; this fixture no longer measures the bug", priced-folded)
	}
}

// The second measured turn: the same prompt served from the cache it
// warmed. Steady-state hits were never the bug, and the sidecar must not
// make them one — this is the regression guard on the other side.
func TestSteadyStateCacheHitIsUnchangedByTheSidecar(t *testing.T) {
	ev := &session.Event{
		LLMResponse: model.LLMResponse{
			ModelVersion: "claude-sonnet-5",
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:        warmPrompt,
				CachedContentTokenCount: warmWrite,
				CandidatesTokenCount:    warmOutput,
				TotalTokenCount:         warmPrompt + warmOutput,
			},
			// The adapter states both counters on every turn, including
			// the zero: "no cache entry was written" is a measurement.
			CustomMetadata: map[string]any{
				DetailKey: stubDetail{read: i64(warmWrite), write: i64(0)},
			},
		},
	}
	m := NewMeter(Limits{Pricer: tieredPricer()})
	if err := m.Observe(ev); err != nil {
		t.Fatalf("observe: %v", err)
	}
	_, got, _ := m.Snapshot()
	want := (warmInput*2.0 + warmWrite*0.2 + warmOutput*10.0) / 1e6
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("cache-hit turn priced at $%.6f, want $%.6f", got, want)
	}
}

// ---------------------------------------------------------------------
// callOf: what the two sources mean, and what happens when they
// disagree with arithmetic.

func TestCallOf(t *testing.T) {
	tests := []struct {
		name                            string
		prompt, cached, out             int32
		detail                          any
		uncached, read, write, produced int
	}{{
		// No sidecar at all: unchanged from before #352, which is what
		// every event read back from storage and every adapter that
		// attaches nothing still gets.
		name:   "no sidecar falls back to the genai fields",
		prompt: 1000, cached: 400, out: 50,
		uncached: 600, read: 400, write: 0, produced: 50,
	}, {
		name:   "a stated read overrides the genai field",
		prompt: 1000, cached: 400, out: 50,
		detail:   stubDetail{read: i64(250)},
		uncached: 750, read: 250, write: 0, produced: 50,
	}, {
		// The whole point: a bucket with no genai field at all.
		name:   "a stated write comes out of the uncached bucket",
		prompt: 1000, cached: 0, out: 50,
		detail:   stubDetail{read: i64(0), write: i64(900)},
		uncached: 100, read: 0, write: 900, produced: 50,
	}, {
		// nil is not zero: the provider said nothing about reads, so the
		// genai field stands.
		name:   "an unstated bucket leaves the genai field alone",
		prompt: 1000, cached: 400, out: 50,
		detail:   stubDetail{write: i64(100)},
		uncached: 500, read: 400, write: 100, produced: 50,
	}, {
		// The clamp, generalized. Over-reporting must not produce
		// negative uncached tokens — those bill as a credit, and a
		// ceiling that moves away from you never fires.
		name:   "an over-reported read is fitted to the prompt",
		prompt: 1000, cached: 4000, out: 50,
		uncached: 0, read: 1000, write: 0, produced: 50,
	}, {
		name:   "an over-reported write is fitted to what the read left",
		prompt: 1000, cached: 0, out: 50,
		detail:   stubDetail{read: i64(600), write: i64(900)},
		uncached: 0, read: 600, write: 400, produced: 50,
	}, {
		name:   "both over-reported: reads win, writes absorb the error",
		prompt: 1000, cached: 0, out: 50,
		detail:   stubDetail{read: i64(5000), write: i64(5000)},
		uncached: 0, read: 1000, write: 0, produced: 50,
	}, {
		name:   "a negative count is not a count",
		prompt: 1000, cached: 0, out: 50,
		detail:   stubDetail{read: i64(-10), write: i64(-10)},
		uncached: 1000, read: 0, write: 0, produced: 50,
	}, {
		// Anything may write to CustomMetadata; only a Detailer is read.
		name:   "a foreign value under the key is ignored, not panicked on",
		prompt: 1000, cached: 400, out: 50,
		detail:   map[string]any{"cache_write_tokens": 900},
		uncached: 600, read: 400, write: 0, produced: 50,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := &session.Event{
				LLMResponse: model.LLMResponse{
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						PromptTokenCount:        tc.prompt,
						CachedContentTokenCount: tc.cached,
						CandidatesTokenCount:    tc.out,
					},
				},
			}
			if tc.detail != nil {
				ev.CustomMetadata = map[string]any{DetailKey: tc.detail}
			}
			got := callOf(ev)
			want := Call{
				UncachedInputTokens: tc.uncached,
				CachedInputTokens:   tc.read,
				CacheWriteTokens:    tc.write,
				OutputTokens:        tc.produced,
			}
			if got != want {
				t.Errorf("callOf = %+v, want %+v", got, want)
			}
			// The invariant behind the fitting, asserted rather than
			// implied: the three input buckets partition the prompt.
			if sum := got.UncachedInputTokens + got.CachedInputTokens + got.CacheWriteTokens; sum != int(tc.prompt) {
				t.Errorf("input buckets sum to %d, want the prompt's %d", sum, tc.prompt)
			}
		})
	}
}

// A nil-safe Detailer is worth having: pkg/providers/usage returns the
// zero Buckets from a nil *Detail so an adapter need not branch.
func TestBucketsOfToleratesAnAbsentMap(t *testing.T) {
	if got := bucketsOf(&session.Event{}); got != (Buckets{}) {
		t.Errorf("bucketsOf(no CustomMetadata) = %+v, want the zero Buckets", got)
	}
}
