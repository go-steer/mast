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

// Package usage carries the provider-usage record that genai's
// UsageMetadata has no room for.
//
// genai.GenerateContentResponseUsageMetadata is Gemini's shape. Every
// provider mast speaks to is projected onto it, and the projection is
// lossy in both directions: Anthropic's cache_creation_input_tokens has
// nowhere to go at all (pkg/providers/anthropic/stream.go folds it into
// the prompt total, where it bills at 1x instead of 1.25x), and a
// counter a provider simply did not report is indistinguishable from
// one it reported as zero.
//
// Detail is the sidecar that fixes both. An adapter attaches one beside
// the genai record, under budget.DetailKey in
// model.LLMResponse.CustomMetadata; pkg/budget's meter reads the
// buckets it prices and ignores the rest. See
// docs/model-support-design.md §4.3, and #352 for why this landed
// before pkg/budget froze rather than with the rest of the
// multi-provider work.
//
// Scope: this package is the M0 subset — what the two shipped adapters
// can state today. The profile-shaped fields §4.3 also names (backend,
// region, and the self-hosted KV block) belong to M1–M4 and are #312's
// territory, post-v1.0.
package usage

import (
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/budget"
)

// Detail is one model call as the provider described it, normalized
// across providers.
//
// Every count is a pointer, and a nil one means the provider did not
// say — which is not zero (model-support-design R3). The distinction is
// the point of the type: "this turn wrote no cache entry" and "this
// backend has no cache-write concept" price the same today and must not
// be reported the same, or a bucket that goes missing after a provider
// upgrade looks exactly like a bucket that was empty.
//
// The value is in-process only. It rides on an ADK map that does get
// persisted with the event, so it must stay JSON-marshalable, but
// nothing reads it back: an event rehydrated from storage carries the
// sidecar as a plain map and prices from the genai fields, the same as
// an event that never had one.
type Detail struct {
	// CacheReadTokens is the prompt subset served from a cache —
	// Anthropic's cache_read_input_tokens, Gemini's
	// cachedContentTokenCount, and (M3, not yet) OpenAI's cached_tokens.
	CacheReadTokens *int64 `json:"cache_read_tokens,omitempty"`

	// CacheWriteTokens is the prompt subset that created a cache entry —
	// Anthropic's cache_creation_input_tokens, billed at a premium over
	// fresh input rather than a discount. The bucket this package exists
	// for: genai has no field for it and pkg/budget cannot price what it
	// cannot see.
	//
	// Nil for Gemini, which has no equivalent: its explicit caches bill
	// storage per hour, not per written token.
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`

	// ReasoningTokens is thinking billed apart from the visible output —
	// Gemini's thoughtsTokenCount. Nil for Anthropic, whose output count
	// already includes thinking, so it is a genuine "did not say" rather
	// than a zero.
	//
	// Recorded, not priced: no rate card mast knows charges reasoning
	// separately from output, and the meter keeps folding it into the
	// output bucket. When one does, the count is already here.
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`

	// ToolUseTokens is prompt spent on tool definitions and server-side
	// tool traffic — Gemini's toolUsePromptTokenCount. Prompt-side, and
	// already inside the prompt total, so it is a breakdown rather than
	// an addition.
	ToolUseTokens *int64 `json:"tool_use_tokens,omitempty"`

	// ServedModel is what the backend says it actually ran, which is not
	// always what was asked for. Duplicated from LLMResponse.ModelVersion
	// on purpose: that field is a pricing key the adapter may have had to
	// substitute (pkg/providers/anthropic/llm.go falls back to the
	// requested id when the echo is a Vertex resource path), and this one
	// is the unedited echo.
	ServedModel string `json:"served_model,omitempty"`

	// ProviderRequestID is the id a support ticket needs — Anthropic's
	// message id, Gemini's responseId.
	ProviderRequestID string `json:"provider_request_id,omitempty"`
}

// UsageBuckets implements budget.Detailer: the subset of Detail the
// meter prices. A nil *Detail reports nothing said, so a caller need
// not branch on whether an adapter produced one.
func (d *Detail) UsageBuckets() budget.Buckets {
	if d == nil {
		return budget.Buckets{}
	}
	return budget.Buckets{
		CacheReadTokens:  d.CacheReadTokens,
		CacheWriteTokens: d.CacheWriteTokens,
	}
}

// Attach puts d on resp under budget.DetailKey, allocating the
// CustomMetadata map if the response has none and leaving any other
// keys alone. A nil resp or a nil d is a no-op: an adapter with nothing
// to say attaches nothing, rather than an empty record that reads as
// "asked and answered".
func Attach(resp *adkmodel.LLMResponse, d *Detail) {
	if resp == nil || d == nil {
		return
	}
	if resp.CustomMetadata == nil {
		resp.CustomMetadata = make(map[string]any, 1)
	}
	resp.CustomMetadata[budget.DetailKey] = d
}

// FromEvent returns the sidecar an event carries, or nil. Only the
// in-process form is recognized — see Detail on why a rehydrated event
// has none.
func FromEvent(ev *session.Event) *Detail {
	if ev == nil {
		return nil
	}
	d, _ := ev.CustomMetadata[budget.DetailKey].(*Detail)
	return d
}

// Int64 returns a pointer to n, for building a Detail from counters a
// provider stated. Use it only where the provider really did say: a
// pointer to zero and a nil pointer are different claims.
func Int64(n int64) *int64 { return &n }
