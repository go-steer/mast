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

package usagetrack

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/budget"
)

// call builds one metered call the way pkg/budget hands it over.
func call(model string, fresh, cached, write, out int, cost, ref float64) budget.Spend {
	return budget.Spend{
		Tokens:  int64(fresh + cached + write + out),
		CostUSD: cost,
		Model:   model,
		Call: budget.Call{
			UncachedInputTokens: fresh,
			CachedInputTokens:   cached,
			CacheWriteTokens:    write,
			OutputTokens:        out,
		},
		At:                       time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
		CostUSDUncachedReference: ref,
	}
}

func live(cost float64) Cumulative { return Cumulative{Turns: 0, CostUSD: cost} }

// The headline numbers add up out of the per-turn rows, which is what
// makes the breakdown a breakdown rather than a second opinion.
func TestOverallSumsThePerTurnRows(t *testing.T) {
	tr := New()
	tr.Record(call("sonnet", 10, 0, 800, 4, 0.05, 0.04))
	tr.Record(call("sonnet", 10, 800, 0, 6, 0.01, 0.04))

	info := tr.Info(Cumulative{Turns: 2, CostUSD: 0.06})
	if len(info.PerTurn) != 2 {
		t.Fatalf("PerTurn has %d rows, want 2", len(info.PerTurn))
	}

	var in, cached, uncached, out int64
	var cost float64
	for _, row := range info.PerTurn {
		in += row.InputTokens
		cached += row.InputTokensCached
		uncached += row.InputTokensUncached
		out += row.OutputTokens
		cost += row.CostUSD
	}
	if info.Overall.InputTokens != in {
		t.Errorf("Overall.InputTokens = %d, per-turn rows sum to %d", info.Overall.InputTokens, in)
	}
	if info.Overall.InputTokensCached != cached {
		t.Errorf("Overall.InputTokensCached = %d, rows sum to %d", info.Overall.InputTokensCached, cached)
	}
	if info.Overall.InputTokensUncached != uncached {
		t.Errorf("Overall.InputTokensUncached = %d, rows sum to %d", info.Overall.InputTokensUncached, uncached)
	}
	if info.Overall.OutputTokens != out {
		t.Errorf("Overall.OutputTokens = %d, rows sum to %d", info.Overall.OutputTokens, out)
	}
	if math.Abs(info.Overall.CostUSD-cost) > 1e-12 {
		t.Errorf("Overall.CostUSD = %v, rows sum to %v", info.Overall.CostUSD, cost)
	}
	// And the split is exhaustive: cached + uncached is the whole prompt.
	if info.Overall.InputTokensCached+info.Overall.InputTokensUncached != info.Overall.InputTokens {
		t.Error("cached + uncached does not equal the input total")
	}
}

// A cache write is fresh input at a premium, not a cached read, so it
// lands on the uncached side of the split an operator reads. Getting this
// backwards would report a cache-warming turn as a cache hit — the two
// turns that look most alike and cost most differently.
func TestACacheWriteIsNotACacheHit(t *testing.T) {
	tr := New()
	tr.Record(call("sonnet", 10, 0, 28804, 4, 0.0721, 0.0577))

	got := tr.Info(live(0.0721)).Overall
	if got.InputTokensCached != 0 {
		t.Errorf("InputTokensCached = %d on a turn that read nothing from cache, want a reported zero", got.InputTokensCached)
	}
	if want := int64(10 + 28804); got.InputTokensUncached != want {
		t.Errorf("InputTokensUncached = %d, want %d — the written block was sent, not served", got.InputTokensUncached, want)
	}
}

// PerModel is a breakdown, and one model is not a breakdown of anything.
// The rule predates this package (attach.UsageInfo); it is pinned here
// because this is now the only producer.
func TestPerModelAppearsOnlyWhenModelsDiffer(t *testing.T) {
	one := New()
	one.Record(call("sonnet", 100, 0, 0, 10, 0.01, 0.01))
	one.Record(call("sonnet", 100, 0, 0, 10, 0.01, 0.01))
	if got := one.Info(live(0.02)).PerModel; got != nil {
		t.Errorf("PerModel = %v on a single-model session, want nothing to break down", got)
	}

	two := New()
	two.Record(call("sonnet", 100, 0, 0, 10, 0.02, 0.02))
	two.Record(call("haiku", 300, 0, 0, 30, 0.01, 0.01))
	info := two.Info(live(0.03))
	if len(info.PerModel) != 2 {
		t.Fatalf("PerModel has %d entries, want 2", len(info.PerModel))
	}
	if got := info.PerModel["haiku"]; got.InputTokens != 300 || got.Turns != 1 {
		t.Errorf("haiku row = %+v, want 300 input over 1 turn", got)
	}
	var sum int64
	for _, m := range info.PerModel {
		sum += m.InputTokens
	}
	if sum != info.Overall.InputTokens {
		t.Errorf("PerModel rows sum to %d input tokens, Overall says %d", sum, info.Overall.InputTokens)
	}
}

// Thinking is a subset of the output it is billed inside, and the
// tracker must not fold it in a second time.
func TestThinkingIsNotAddedToOutputTwice(t *testing.T) {
	s := call("sonnet", 100, 0, 0, 500, 0.01, 0.01)
	s.ThoughtsTokens = 400
	s.ToolUseTokens = 25

	tr := New()
	tr.Record(s)
	info := tr.Info(live(0.01))

	if info.Overall.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500 — thinking is inside it", info.Overall.OutputTokens)
	}
	if info.Overall.ThoughtsTokens != 400 {
		t.Errorf("ThoughtsTokens = %d, want 400", info.Overall.ThoughtsTokens)
	}
	if info.PerTurn[0].ToolUseTokens != 25 {
		t.Errorf("per-turn ToolUseTokens = %d, want 25", info.PerTurn[0].ToolUseTokens)
	}
}

// Overall's turn count and cost come from the meter, so a resumed
// session reports what it has actually spent against its ceiling rather
// than what this process happens to have watched. The breakdown beside
// them is this process's, which is all the ledger can support.
func TestRestoredSpendKeepsTheHeadlineFiguresWhole(t *testing.T) {
	tr := New()
	tr.Record(call("sonnet", 1000, 500, 0, 100, 0.01, 0.03))

	// The meter carries three prior calls worth $0.40 from before the
	// restart, plus the one above.
	info := tr.Info(Cumulative{Turns: 4, CostUSD: 0.41})

	if info.Overall.Turns != 4 {
		t.Errorf("Turns = %d, want the meter's 4 — /usage and /guardrails must agree", info.Overall.Turns)
	}
	if math.Abs(info.Overall.CostUSD-0.41) > 1e-12 {
		t.Errorf("CostUSD = %v, want the meter's 0.41", info.Overall.CostUSD)
	}
	if len(info.PerTurn) != 1 {
		t.Errorf("PerTurn has %d rows, want 1 — the ledger stores no buckets to rebuild the others from", len(info.PerTurn))
	}
	// Restored spend saved nothing it can prove: it enters the reference
	// at exactly its cost, so the delta stays the attributable part.
	if want := 0.03 + 0.40; math.Abs(info.Overall.CostUSDUncachedReference-want) > 1e-12 {
		t.Errorf("reference = %v, want %v", info.Overall.CostUSDUncachedReference, want)
	}
	if got := info.Overall.CostUSDUncachedReference - info.Overall.CostUSD; math.Abs(got-0.02) > 1e-12 {
		t.Errorf("attributable saving = %v, want the live turn's 0.02", got)
	}
}

// A warming turn costs more than not caching at all. The tracker carries
// that through instead of flooring it — see budget.Spend's field doc.
func TestAWarmingSessionReportsItsCacheCostingMoney(t *testing.T) {
	tr := New()
	tr.Record(call("sonnet", 10, 0, 28804, 4, 0.0721, 0.0577))

	info := tr.Info(live(0.0721))
	if info.Overall.CostUSDUncachedReference >= info.Overall.CostUSD {
		t.Fatalf("reference %v is not below the cost %v; the write premium has been floored away",
			info.Overall.CostUSDUncachedReference, info.Overall.CostUSD)
	}
}

// Per-turn history is bounded, because the sessions this daemon is for
// do not end. Overall keeps covering every call, and the absolute turn
// number is what says rows were dropped.
func TestPerTurnIsBoundedAndSaysSo(t *testing.T) {
	tr := New()
	const n = MaxTurns + 25
	for i := 0; i < n; i++ {
		tr.Record(call("sonnet", 100, 0, 0, 10, 0.001, 0.001))
	}

	info := tr.Info(Cumulative{Turns: n, CostUSD: float64(n) * 0.001})
	if len(info.PerTurn) != MaxTurns {
		t.Fatalf("PerTurn has %d rows, want the cap of %d", len(info.PerTurn), MaxTurns)
	}
	if got := info.PerTurn[0].Turn; got != 26 {
		t.Errorf("oldest kept row is turn %d, want 26 — the gap from 1 is the only record that rows were dropped", got)
	}
	if got := info.PerTurn[len(info.PerTurn)-1].Turn; got != n {
		t.Errorf("newest row is turn %d, want %d", got, n)
	}
	if info.Overall.InputTokens != int64(n*100) {
		t.Errorf("Overall.InputTokens = %d, want %d — a dropped row must not drop its tokens",
			info.Overall.InputTokens, n*100)
	}
}

// Info hands out a copy. A caller that held the tracker's own slice
// would race every later call, and the /usage handler serializes its
// answer on the HTTP goroutine.
func TestInfoDoesNotAliasTheTracker(t *testing.T) {
	tr := New()
	tr.Record(call("sonnet", 100, 0, 0, 10, 0.01, 0.01))

	info := tr.Info(live(0.01))
	info.PerTurn[0].InputTokens = 999_999
	if got := tr.Info(live(0.01)).PerTurn[0].InputTokens; got != 100 {
		t.Errorf("mutating the returned slice changed the tracker (InputTokens = %d)", got)
	}
}

// A coordinator's sub-agent tool and a planner's private runner meter
// into the same session from their own goroutines, and OnSpend runs
// outside the meter's lock.
func TestConcurrentRecord(t *testing.T) {
	tr := New()
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.Record(call("sonnet", 100, 0, 0, 10, 0.001, 0.001))
		}()
	}
	wg.Wait()

	info := tr.Info(Cumulative{Turns: n, CostUSD: n * 0.001})
	if len(info.PerTurn) != n {
		t.Errorf("PerTurn has %d rows, want %d", len(info.PerTurn), n)
	}
	if info.Overall.InputTokens != int64(n*100) {
		t.Errorf("Overall.InputTokens = %d, want %d", info.Overall.InputTokens, n*100)
	}
	seen := make(map[int]bool, n)
	for _, row := range info.PerTurn {
		if seen[row.Turn] {
			t.Fatalf("turn %d numbered twice", row.Turn)
		}
		seen[row.Turn] = true
	}
}

// An unstamped event still produces an ordered row rather than a
// zero-time one, which renders as 00:00:00 for every call.
func TestAnUnstampedCallGetsATime(t *testing.T) {
	s := call("sonnet", 100, 0, 0, 10, 0.01, 0.01)
	s.At = time.Time{}

	tr := New()
	tr.Record(s)
	if tr.Info(live(0.01)).PerTurn[0].At.IsZero() {
		t.Error("per-turn row has no timestamp")
	}
}
