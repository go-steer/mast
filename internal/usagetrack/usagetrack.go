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

// Package usagetrack folds a session's metered model calls into the
// token breakdown `GET /sessions/{id}/usage` promises.
//
// The wire contract (attach.UsageInfo) has declared an eight-field
// breakdown since the attach surface was ported, and mast filled in two
// of them: a turn count and a dollar figure, with every token field left
// at its zero value. Three of those fields are omitempty and five are
// not, so a caller could not tell a measured zero from an unmeasured one
// — and every one of them was the second kind. This package is the
// producer that was never ported (#356).
//
// # It reads the meter, not the event stream
//
// The counts it reports are the ones pkg/budget already derived on the
// way to pricing the call, handed over through Config.OnSpend. Nothing
// here re-reads a session event, and that is the design rather than a
// convenience: splitting a prompt into cached, written and fresh input
// is the meter's own reading of the provider's counters (budget.callOf,
// budget.fitBucket, budget.flooredUsage), not a copy of them, and a
// report derived any other way would describe a different call than the
// one the money was computed from. The recurring defect shape in this
// repo is a second reader that agrees the day it is written; the
// mitigation is not to have one.
//
// # It is in-process
//
// A Tracker covers the calls this daemon metered. The durable spend
// ledger stores author, tokens and cost per call and no buckets, so a
// session resumed after a restart has cumulative turns and cost — those
// come from the meter, through Info's Cumulative argument, and stay
// authoritative so /usage and /guardrails never disagree about what a
// session has spent — but its per-model and per-turn detail begins at
// the restart. See Info.
package usagetrack

import (
	"sync"
	"time"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/budget"
)

// MaxTurns bounds how many per-call rows one session keeps. Past it the
// oldest is dropped.
//
// A cap rather than a slice that grows: the sessions mast is built for
// are unattended and long, one entry lands per model call, and nothing
// else in the daemon would ever free them. 500 rows is a few hundred
// kilobytes per session and further back than any operator reads — the
// TUI renders the last 20 — while Overall and PerModel keep covering
// every call, so what a drop costs is the detail of a turn, never a
// token or a cent.
const MaxTurns = 500

// Tracker accumulates one session's usage. The zero value is not ready;
// use New.
type Tracker struct {
	mu      sync.Mutex
	calls   int
	overall totals
	byModel map[string]*totals
	turns   []attach.UsageTurn
}

// totals is one accumulator — the session's or one model's.
type totals struct {
	input    int64
	cached   int64
	output   int64
	thoughts int64
	turns    int
	cost     float64
	ref      float64
}

// New returns an empty Tracker.
func New() *Tracker {
	return &Tracker{byModel: map[string]*totals{}}
}

// Cumulative is the meter's own running figures for the session,
// including any spend restored from a previous process. See Info.
type Cumulative struct {
	Turns   int
	CostUSD float64
}

// Record folds one metered call in. Safe for concurrent use: a
// coordinator's sub-agent tool and a planner's private runner both meter
// into the same session, from their own goroutines.
func (t *Tracker) Record(s budget.Spend) {
	// The three input buckets are mutually exclusive and sum to the
	// prompt (budget.Call). Cached is the subset billed at the cache-read
	// rate; a cache *write* is fresh input at a premium, so it belongs on
	// the uncached side of the split an operator reads.
	input := int64(s.Call.UncachedInputTokens + s.Call.CachedInputTokens + s.Call.CacheWriteTokens)
	cached := int64(s.Call.CachedInputTokens)

	t.mu.Lock()
	defer t.mu.Unlock()

	t.calls++
	t.overall.add(input, cached, int64(s.Call.OutputTokens), s.ThoughtsTokens, s.CostUSD, s.CostUSDUncachedReference)

	m, ok := t.byModel[s.Model]
	if !ok {
		m = &totals{}
		t.byModel[s.Model] = m
	}
	m.add(input, cached, int64(s.Call.OutputTokens), s.ThoughtsTokens, s.CostUSD, s.CostUSDUncachedReference)

	at := s.At
	if at.IsZero() {
		// A producer that left the event unstamped. Now is wrong by the
		// length of one model call and right about the ordering, which is
		// what the row is read for.
		at = time.Now()
	}
	t.turns = append(t.turns, attach.UsageTurn{
		// Absolute and 1-based over the whole session, not an index into
		// the slice: once rows start dropping, the gap between the first
		// number and 1 is the only thing that says they did.
		Turn:                     t.calls,
		At:                       at,
		Model:                    s.Model,
		InputTokens:              input,
		InputTokensCached:        cached,
		InputTokensUncached:      input - cached,
		OutputTokens:             int64(s.Call.OutputTokens),
		ThoughtsTokens:           s.ThoughtsTokens,
		ToolUseTokens:            s.ToolUseTokens,
		TotalTokens:              s.Tokens,
		CostUSD:                  s.CostUSD,
		CostUSDUncachedReference: s.CostUSDUncachedReference,
	})
	if len(t.turns) > MaxTurns {
		// Shift rather than a ring buffer: this runs once per model call,
		// a call takes seconds, and the copy is a few hundred kilobytes.
		// A ring would have to be unrolled on every read anyway, in the
		// one place where getting the order wrong is silent.
		copy(t.turns, t.turns[len(t.turns)-MaxTurns:])
		t.turns = t.turns[:MaxTurns]
	}
}

func (u *totals) add(input, cached, output, thoughts int64, cost, ref float64) {
	u.turns++
	u.input += input
	u.cached += cached
	u.output += output
	u.thoughts += thoughts
	u.cost += cost
	u.ref += ref
}

func (u *totals) info() attach.UsageTotals {
	return attach.UsageTotals{
		InputTokens:              u.input,
		InputTokensCached:        u.cached,
		InputTokensUncached:      u.input - u.cached,
		OutputTokens:             u.output,
		ThoughtsTokens:           u.thoughts,
		Turns:                    u.turns,
		CostUSD:                  u.cost,
		CostUSDUncachedReference: u.ref,
	}
}

// Info projects the tracker into the wire shape, reconciled against the
// meter's own cumulative figures.
//
// Overall.Turns and Overall.CostUSD are taken from c rather than from
// what this tracker saw, because the meter is what a ceiling is enforced
// against and it carries spend restored from previous processes. A
// /usage that disagreed with /guardrails about what a session has spent
// would send an operator to raise a cap that is not the one holding.
//
// The token buckets under them cannot be reconciled the same way — the
// ledger never stored any — so on a resumed session they cover this
// process while the two figures above cover the session. What that means
// for the caching figure is spelled out rather than rounded off: spend
// restored from a previous process is carried into the reference at
// exactly what it cost, so it contributes no saving and no loss, and the
// delta an operator reads is the part that can actually be attributed.
func (t *Tracker) Info(c Cumulative) attach.UsageInfo {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := attach.UsageInfo{Overall: t.overall.info()}
	out.Overall.Turns = c.Turns
	out.Overall.CostUSD = c.CostUSD
	if unattributed := c.CostUSD - t.overall.cost; unattributed > 0 {
		out.Overall.CostUSDUncachedReference = t.overall.ref + unattributed
	}

	// PerModel is a breakdown, and there is nothing to break down when
	// one model answered every call — the contract has said so since the
	// field existed (attach.UsageInfo).
	if len(t.byModel) > 1 {
		out.PerModel = make(map[string]attach.UsageTotals, len(t.byModel))
		for name, u := range t.byModel {
			out.PerModel[name] = u.info()
		}
	}

	if len(t.turns) > 0 {
		out.PerTurn = make([]attach.UsageTurn, len(t.turns))
		copy(out.PerTurn, t.turns)
	}
	return out
}
