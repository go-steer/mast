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

package budget

import "google.golang.org/genai"

// flooredUsage is this package's only read of a provider's usage
// metadata: every token count clamped at zero, once, before anything
// downstream sees it.
//
// A negative count is not a number this package can price, cap or
// report — it is a provider miscount, and the only safe reading of it
// is "nothing". What makes the placement matter rather than the clamp
// is that pricing was never the only reader: the same counts reach the
// durable spend ledger, the session totals behind `GET /usage`, and the
// ceiling comparisons that stop a run. A guard inside priceOf would
// keep the negative out of the invoice and leave it in all three. So
// the floor goes where the metadata enters (Meter.Observe), and fold,
// priceOf and callOf are handed the floored value rather than the
// event's raw one. TestUsageMetadataIsReadInOneFunction keeps it that
// way.
//
// This is the one place go-steer/core-agent and mast diverged on the
// 2026-09-03 thoughts-billing fix (#266/#267/#268, convergent between
// the forks): upstream's TurnUsage.Clamped() floors, mast's read never
// did. The argument for the placement is upstream's; see
// docs/sibling-sync.md.
//
// The clamp only removes impossible values. A count the provider
// over-reports is a different failure and is handled where the buckets
// have to add up: fitBucket clips each input bucket into the room the
// prompt has left.
func flooredUsage(u *genai.GenerateContentResponseUsageMetadata) genai.GenerateContentResponseUsageMetadata {
	if u == nil {
		return genai.GenerateContentResponseUsageMetadata{}
	}
	// A copy, not a mutation of the caller's record: the event is the
	// session's, it is shared with every other Observe-shaped hook on
	// the stream (pkg/observability's, the transcript writer's), and a
	// meter that silently rewrote what a provider reported would make
	// the raw counter unrecoverable for anyone diagnosing the provider.
	// The remaining fields are per-modality detail slices this package
	// does not read; they ride along unchanged.
	out := *u
	out.TotalTokenCount = floorCount(out.TotalTokenCount)
	out.PromptTokenCount = floorCount(out.PromptTokenCount)
	out.CachedContentTokenCount = floorCount(out.CachedContentTokenCount)
	out.CandidatesTokenCount = floorCount(out.CandidatesTokenCount)
	out.ThoughtsTokenCount = floorCount(out.ThoughtsTokenCount)
	out.ToolUsePromptTokenCount = floorCount(out.ToolUsePromptTokenCount)
	return out
}

// floorCount reads one token count. Negative is not a count.
func floorCount(n int32) int32 {
	if n < 0 {
		return 0
	}
	return n
}
