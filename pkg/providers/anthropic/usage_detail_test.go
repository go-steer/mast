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

// The cache-write count, from the wire to the bill (#352).
//
// pkg/budget owns the arithmetic and pkg/providers/usage owns the
// record; what is measured here is the part only this adapter can get
// wrong — that a number Anthropic sent actually survives the genai
// projection, across the pause_turn loop, and onto the response the
// runner turns into an event.

package anthropic

import (
	"context"
	"math"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/budget"
	providerusage "github.com/go-steer/mast/pkg/providers/usage"
)

// cacheWarmingSSEFixture is a real measured turn, replayed: a
// 28,804-token system block sent with cache_control ephemeral to
// claude-sonnet-5 on Vertex (us-east5) on 2026-09-13, answered in four
// tokens. The counts below are that response's, not invented ones.
//
//	input_tokens=10 cache_creation_input_tokens=28804
//	cache_read_input_tokens=0 output_tokens=4
const cacheWarmingSSEFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_vrtx_011Cf23KmBvy6xNCBR8cRGsH","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"cache_creation_input_tokens":28804,"cache_read_input_tokens":0,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ack"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

`

// cacheHitSSEFixture is the next turn of the same probe: the prompt
// served from the entry the turn above wrote.
const cacheHitSSEFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_vrtx_011Cf23LuN642SEHDymCjVBL","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":28804,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ack"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

`

// warmRates is claude-sonnet-5 as of 2026-09-09, restated here rather
// than read from pkg/pricing: the assertion below is a dollar figure,
// and a vendor moving a rate is not a defect in this adapter.
type warmRates struct{ in, read, write, out float64 }

var sonnet5 = warmRates{in: 2, read: 0.2, write: 2.5, out: 10}

func (r warmRates) PriceCall(_, _ string, c budget.Call) (float64, bool) {
	const million = 1e6
	return float64(c.UncachedInputTokens)/million*r.in +
		float64(c.CachedInputTokens)/million*r.read +
		float64(c.CacheWriteTokens)/million*r.write +
		float64(c.OutputTokens)/million*r.out, true
}

// finalOf drives one offline turn and returns its terminal response.
func finalOf(t *testing.T, sse string) *model.LLMResponse {
	t.Helper()
	l, _ := newOfflineLLM(t, "claude-sonnet-5", sse)
	var final *model.LLMResponse
	for resp, err := range l.GenerateContent(context.Background(), &model.LLMRequest{Contents: userText("hi")}, true) {
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if resp.TurnComplete {
			final = resp
		}
	}
	if final == nil {
		t.Fatal("no terminal response")
	}
	return final
}

// The count Anthropic sent has to still be there at the end of the
// stream. genai's usage metadata has nowhere to put it, so if the
// sidecar does not carry it nothing does.
func TestCacheWarmingTurnCarriesItsWriteCount(t *testing.T) {
	t.Parallel()
	final := finalOf(t, cacheWarmingSSEFixture)

	// The genai record is unchanged and still right on its own terms:
	// the prompt total includes all three input buckets.
	if u := final.UsageMetadata; u.PromptTokenCount != 10+28804 || u.CachedContentTokenCount != 0 || u.CandidatesTokenCount != 4 {
		t.Errorf("usage = prompt %d, cached %d, candidates %d; want 28814/0/4",
			u.PromptTokenCount, u.CachedContentTokenCount, u.CandidatesTokenCount)
	}

	d := providerusage.FromEvent(&session.Event{LLMResponse: *final})
	if d == nil {
		t.Fatal("no usage sidecar on the terminal response")
	}
	if d.CacheWriteTokens == nil || *d.CacheWriteTokens != 28804 {
		t.Errorf("CacheWriteTokens = %v, want 28804 — the count is lost", d.CacheWriteTokens)
	}
	// Stated, not omitted: this turn read nothing from cache and the
	// adapter knows that, which is not the same as having no opinion.
	if d.CacheReadTokens == nil || *d.CacheReadTokens != 0 {
		t.Errorf("CacheReadTokens = %v, want a stated 0", d.CacheReadTokens)
	}
	// Anthropic's output count already includes thinking, so there is no
	// separate reasoning figure to report. nil says so.
	if d.ReasoningTokens != nil {
		t.Errorf("ReasoningTokens = %v, want nil; Anthropic reports no such bucket", *d.ReasoningTokens)
	}
	if d.ServedModel != "claude-sonnet-5" {
		t.Errorf("ServedModel = %q, want the echoed model", d.ServedModel)
	}
	if d.ProviderRequestID != "msg_vrtx_011Cf23KmBvy6xNCBR8cRGsH" {
		t.Errorf("ProviderRequestID = %q, want the message id", d.ProviderRequestID)
	}
}

// End to end, which is the claim #352 actually makes: this turn bills
// what the rate card says it costs. Before the sidecar the same
// fixture billed $0.057668 — the write bucket at the fresh-input rate.
func TestCacheWarmingTurnBillsTheRateCardFigure(t *testing.T) {
	t.Parallel()
	final := finalOf(t, cacheWarmingSSEFixture)

	m := budget.NewMeter(budget.Limits{Pricer: sonnet5, Model: "claude-sonnet-5"})
	if err := m.Observe(&session.Event{LLMResponse: *final}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	_, got, _ := m.Snapshot()
	want := (10*sonnet5.in + 28804*sonnet5.write + 4*sonnet5.out) / 1e6 // $0.072070
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("turn billed $%.6f, want $%.6f (folded into fresh input it bills $%.6f)",
			got, want, (28814*sonnet5.in+4*sonnet5.out)/1e6)
	}
}

// The other measured turn. Cache hits were priced correctly before this
// change and must stay that way: the sidecar restates a number the
// genai field already carried, and restating it must not move the bill.
func TestCacheHitTurnIsUnmoved(t *testing.T) {
	t.Parallel()
	final := finalOf(t, cacheHitSSEFixture)

	d := providerusage.FromEvent(&session.Event{LLMResponse: *final})
	if d == nil || d.CacheReadTokens == nil || *d.CacheReadTokens != 28804 {
		t.Fatalf("CacheReadTokens = %v, want 28804", d)
	}
	if d.CacheWriteTokens == nil || *d.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %v, want a stated 0", d.CacheWriteTokens)
	}

	m := budget.NewMeter(budget.Limits{Pricer: sonnet5, Model: "claude-sonnet-5"})
	if err := m.Observe(&session.Event{LLMResponse: *final}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	_, got, _ := m.Snapshot()
	want := (10*sonnet5.in + 28804*sonnet5.read + 4*sonnet5.out) / 1e6 // $0.005821
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("cache-hit turn billed $%.6f, want $%.6f", got, want)
	}
}

// A pause_turn turn spans several requests and the buckets are summed
// across all of them — addUsage folds cache_creation like the rest, so
// the sidecar has to report the turn's total rather than the last
// request's.
func TestPauseTurnLoopSumsTheWriteCount(t *testing.T) {
	t.Parallel()
	l, _ := newOfflineLLMSeq(t, "claude-sonnet-5", []string{pauseTurnSSEFixture, cacheWarmingSSEFixture})
	var final *model.LLMResponse
	for resp, err := range l.GenerateContent(context.Background(), &model.LLMRequest{Contents: userText("hi")}, true) {
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if resp.TurnComplete {
			final = resp
		}
	}
	if final == nil {
		t.Fatal("no terminal response")
	}
	d := providerusage.FromEvent(&session.Event{LLMResponse: *final})
	if d == nil || d.CacheWriteTokens == nil {
		t.Fatalf("no cache-write count on a continued turn: %v", d)
	}
	// The pause_turn fixture writes no cache entry, so the total is the
	// second request's alone — but it is a total, not a snapshot of
	// whichever request happened to finish the turn.
	if *d.CacheWriteTokens != 28804 {
		t.Errorf("CacheWriteTokens = %d, want the turn's 28804", *d.CacheWriteTokens)
	}
}
