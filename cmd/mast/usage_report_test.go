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

package main

import (
	"math"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/providers/usage"
)

// The turns below are the measured Claude fixture pkg/budget prices
// against (usage_detail_test.go): claude-sonnet-5 on Vertex us-east5,
// 2026-09-13. A 28,804-token system block written to the cache on the
// first call and served from it on the second, with 10 fresh prompt
// tokens around it.
const (
	warmFresh  = 10
	warmWrite  = 28_804
	warmPrompt = warmFresh + warmWrite
	warmOutput = 4

	hitFresh  = 46
	hitRead   = warmWrite
	hitPrompt = hitFresh + hitRead
	hitOutput = 200
)

// metered builds one model call as an adapter hands it to the meter: the
// genai counters every provider fills in, plus the sidecar the Anthropic
// adapter attaches for the buckets genai has no field for.
func metered(model string, at time.Time, prompt, cachedRead, cacheWrite, out int32) *adksession.Event {
	ev := &adksession.Event{
		Author:    "coordinator",
		Timestamp: at,
		LLMResponse: adkmodel.LLMResponse{
			ModelVersion: model,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     prompt,
				CandidatesTokenCount: out,
				TotalTokenCount:      prompt + out,
			},
		},
	}
	if cachedRead > 0 || cacheWrite > 0 {
		d := &usage.Detail{ServedModel: model}
		if cachedRead > 0 {
			d.CacheReadTokens = usage.Int64(int64(cachedRead))
		}
		if cacheWrite > 0 {
			d.CacheWriteTokens = usage.Int64(int64(cacheWrite))
		}
		usage.Attach(&ev.LLMResponse, d)
	}
	return ev
}

// The issue this discharges: GET /sessions/{id}/usage declared an
// eight-field token breakdown and answered with two of them — a turn
// count and a dollar figure — on every release the route has existed
// (#356). Everything below is read off the served handler's producer,
// not off usagetrack directly, because the gap was the wiring.
//
// #356's criterion asks for two models across a cache-warming turn and a
// cache-hit turn. It takes three turns to stage honestly: a cache entry
// belongs to the model that wrote it, so warming and hitting have to be
// the same model, and the second model is a turn of its own.
func TestUsageReportsTheBreakdownItPromises(t *testing.T) {
	// A real backend id and real model ids, so the builtin catalog
	// resolves and the costs below are the shipped rate card's — an
	// offline-fake model name would switch the pricer off entirely.
	pool := newMeterPool(nil, nil, "anthropic", "claude-sonnet-5")
	const sid = "usage-breakdown"

	base := time.Date(2026, 9, 19, 14, 30, 0, 0, time.UTC)
	turns := []*adksession.Event{
		metered("claude-sonnet-5", base, warmPrompt, 0, warmWrite, warmOutput),
		metered("claude-sonnet-5", base.Add(9*time.Second), hitPrompt, hitRead, 0, hitOutput),
		metered("claude-haiku-4-5", base.Add(21*time.Second), 1_200, 0, 0, 300),
	}
	for i, ev := range turns {
		if err := pool.meter(sid).Observe(ev); err != nil {
			t.Fatalf("turn %d refused: %v", i+1, err)
		}
	}

	info := pool.usageInfo(sid)

	// Both models under PerModel.
	if len(info.PerModel) != 2 {
		t.Fatalf("PerModel = %v, want a row per model", info.PerModel)
	}
	sonnet, ok := info.PerModel["claude-sonnet-5"]
	if !ok {
		t.Fatalf("no claude-sonnet-5 row in %v", info.PerModel)
	}
	haiku, ok := info.PerModel["claude-haiku-4-5"]
	if !ok {
		t.Fatalf("no claude-haiku-4-5 row in %v", info.PerModel)
	}
	if sonnet.Turns != 2 || haiku.Turns != 1 {
		t.Errorf("turns per model: sonnet %d, haiku %d; want 2 and 1", sonnet.Turns, haiku.Turns)
	}
	if sonnet.InputTokens != warmPrompt+hitPrompt || haiku.InputTokens != 1_200 {
		t.Errorf("input per model: sonnet %d, haiku %d; want %d and 1200",
			sonnet.InputTokens, haiku.InputTokens, warmPrompt+hitPrompt)
	}

	// Per-turn entries with their own buckets, and a cached count that
	// distinguishes the two turns that look most alike and cost most
	// differently.
	if len(info.PerTurn) != 3 {
		t.Fatalf("PerTurn has %d rows, want 3", len(info.PerTurn))
	}
	warm, hit, plain := info.PerTurn[0], info.PerTurn[1], info.PerTurn[2]

	if warm.InputTokensCached != 0 {
		t.Errorf("warming turn reports %d cached tokens; it read nothing from a cache", warm.InputTokensCached)
	}
	if warm.InputTokensUncached != warmPrompt {
		t.Errorf("warming turn: %d uncached of a %d prompt — a written block was sent, not served",
			warm.InputTokensUncached, warmPrompt)
	}
	if hit.InputTokensCached != hitRead {
		t.Errorf("cache-hit turn reports %d cached tokens, want %d", hit.InputTokensCached, hitRead)
	}
	if hit.InputTokensUncached != hitFresh {
		t.Errorf("cache-hit turn: %d uncached, want %d", hit.InputTokensUncached, hitFresh)
	}
	for _, row := range info.PerTurn {
		if row.InputTokensCached+row.InputTokensUncached != row.InputTokens {
			t.Errorf("turn %d: cached+uncached != input (%d + %d != %d)",
				row.Turn, row.InputTokensCached, row.InputTokensUncached, row.InputTokens)
		}
		if row.OutputTokens == 0 || row.TotalTokens == 0 || row.CostUSD == 0 {
			t.Errorf("turn %d is still mostly zeroes: %+v", row.Turn, row)
		}
		if row.At.IsZero() {
			t.Errorf("turn %d has no timestamp", row.Turn)
		}
	}
	if warm.Turn != 1 || hit.Turn != 2 || plain.Turn != 3 {
		t.Errorf("turn numbers are %d/%d/%d, want 1/2/3", warm.Turn, hit.Turn, plain.Turn)
	}
	if !hit.At.After(warm.At) {
		t.Errorf("rows are not in call order: turn 2 at %s, turn 1 at %s", hit.At, warm.At)
	}
	if plain.Model != "claude-haiku-4-5" {
		t.Errorf("turn 3 model = %q, want the id its price resolved against", plain.Model)
	}

	// Totals that sum to the per-turn entries.
	var in, cached, uncached, out int64
	var cost float64
	for _, row := range info.PerTurn {
		in += row.InputTokens
		cached += row.InputTokensCached
		uncached += row.InputTokensUncached
		out += row.OutputTokens
		cost += row.CostUSD
	}
	switch {
	case info.Overall.InputTokens != in:
		t.Errorf("Overall.InputTokens = %d, rows sum to %d", info.Overall.InputTokens, in)
	case info.Overall.InputTokensCached != cached:
		t.Errorf("Overall.InputTokensCached = %d, rows sum to %d", info.Overall.InputTokensCached, cached)
	case info.Overall.InputTokensUncached != uncached:
		t.Errorf("Overall.InputTokensUncached = %d, rows sum to %d", info.Overall.InputTokensUncached, uncached)
	case info.Overall.OutputTokens != out:
		t.Errorf("Overall.OutputTokens = %d, rows sum to %d", info.Overall.OutputTokens, out)
	}
	if math.Abs(info.Overall.CostUSD-cost) > 1e-9 {
		t.Errorf("Overall.CostUSD = %v, rows sum to %v", info.Overall.CostUSD, cost)
	}
	if info.Overall.Turns != 3 {
		t.Errorf("Overall.Turns = %d, want 3", info.Overall.Turns)
	}

	// And the money is the shipped rate card's, not an arithmetic
	// accident: sonnet at $2.00/$0.20/$2.50/$10.00 per MTok for
	// fresh/cached/written input and output, haiku at half those.
	const (
		wantWarm  = (warmFresh*2.0 + warmWrite*2.5 + warmOutput*10.0) / 1e6
		wantHit   = (hitFresh*2.0 + hitRead*0.2 + hitOutput*10.0) / 1e6
		wantPlain = (1_200*1.0 + 300*5.0) / 1e6
	)
	for i, want := range []float64{wantWarm, wantHit, wantPlain} {
		if got := info.PerTurn[i].CostUSD; math.Abs(got-want) > 1e-9 {
			t.Errorf("turn %d cost $%.6f, want $%.6f", i+1, got, want)
		}
	}
}

// The reference figure is what the same calls would have cost with no
// prompt cache at all, and it is not clamped to the cost: a turn that
// writes a cache entry pays a premium for it, so a session that has
// warmed a cache it has not reused yet is genuinely behind.
func TestUsageReportsACacheThatHasNotPaidForItselfYet(t *testing.T) {
	warming := newMeterPool(nil, nil, "anthropic", "claude-sonnet-5")
	if err := warming.meter("warm").Observe(
		metered("claude-sonnet-5", time.Now(), warmPrompt, 0, warmWrite, warmOutput)); err != nil {
		t.Fatalf("warming turn refused: %v", err)
	}
	got := warming.usageInfo("warm").Overall
	if got.CostUSDUncachedReference == 0 {
		// Below the cost is the claim, and zero is below the cost — but
		// zero is what "nobody computed this" looks like, which is the
		// state this issue is about.
		t.Fatalf("no reference figure at all against a cost of $%.6f", got.CostUSD)
	}
	if got.CostUSDUncachedReference >= got.CostUSD {
		t.Errorf("after one warming turn the reference is $%.6f against a cost of $%.6f; the write premium has been floored away",
			got.CostUSDUncachedReference, got.CostUSD)
	}

	// Reused once, the pair is ahead — which is the whole reason to warm
	// a cache, and the direction an operator is checking for.
	pair := newMeterPool(nil, nil, "anthropic", "claude-sonnet-5")
	for _, ev := range []*adksession.Event{
		metered("claude-sonnet-5", time.Now(), warmPrompt, 0, warmWrite, warmOutput),
		metered("claude-sonnet-5", time.Now(), hitPrompt, hitRead, 0, hitOutput),
	} {
		if err := pair.meter("pair").Observe(ev); err != nil {
			t.Fatalf("turn refused: %v", err)
		}
	}
	got = pair.usageInfo("pair").Overall
	if got.CostUSDUncachedReference <= got.CostUSD {
		t.Errorf("after warm+hit the reference is $%.6f against a cost of $%.6f; one reuse should already be ahead",
			got.CostUSDUncachedReference, got.CostUSD)
	}
}

// And the breakdown survives the projection the endpoint actually
// answers through. TestAttachWiringLeavesNoCapabilityUnwired proves
// UsageFn is non-nil; it cannot tell a populated report from the
// two-field one the route returned for four releases, which was also
// non-nil.
func TestAttachWiringProjectsTheUsageBreakdown(t *testing.T) {
	pool := newMeterPool(nil, nil, "anthropic", "claude-sonnet-5")
	const sid = "sess-usage"
	if err := pool.meter(sid).Observe(
		metered("claude-sonnet-5", time.Now(), hitPrompt, hitRead, 0, hitOutput)); err != nil {
		t.Fatalf("turn refused: %v", err)
	}

	w := attachWiring{
		appName:     appName,
		userID:      defaultUserID,
		baseContext: t.Context(),
		usage:       pool.usageInfo,
	}
	fn := w.config(sid).UsageFn
	if fn == nil {
		t.Fatal("UsageFn is nil")
	}
	got := fn()

	if got.Overall.InputTokens != hitPrompt {
		t.Errorf("UsageFn reports %d input tokens, want %d", got.Overall.InputTokens, hitPrompt)
	}
	if got.Overall.InputTokensCached != hitRead {
		t.Errorf("UsageFn reports %d cached tokens, want %d", got.Overall.InputTokensCached, hitRead)
	}
	if got.Overall.OutputTokens != hitOutput {
		t.Errorf("UsageFn reports %d output tokens, want %d", got.Overall.OutputTokens, hitOutput)
	}
	if len(got.PerTurn) != 1 {
		t.Errorf("UsageFn returned %d per-turn rows, want 1", len(got.PerTurn))
	}
}

// A session with no metered call yet answers rather than 500s, and says
// nothing it has not measured. The handler reaches this path on every
// /usage poll that arrives before the first model call.
func TestUsageOnAnUntouchedSession(t *testing.T) {
	info := newMeterPool(nil, nil, "anthropic", "claude-sonnet-5").usageInfo("cold")
	if info.Overall.Turns != 0 || info.Overall.InputTokens != 0 || info.Overall.CostUSD != 0 {
		t.Errorf("a session that has not run reports %+v", info.Overall)
	}
	if len(info.PerTurn) != 0 || len(info.PerModel) != 0 {
		t.Errorf("a session that has not run has a breakdown: %d turns, %d models", len(info.PerTurn), len(info.PerModel))
	}
}
