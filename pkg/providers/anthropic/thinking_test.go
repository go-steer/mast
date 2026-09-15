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

package anthropic

import (
	"testing"

	"google.golang.org/genai"
)

// These tests exist because the ones they replaced could not have
// caught #369. convert_test.go asserted that a thinking budget produced
// `thinking.type=enabled` — a statement about what mast sends and never
// about what the model accepts — so it passed while every live call to
// mast's own DefaultModel returned 400. The shapes below are pinned
// against a measured matrix rather than against the SDK's type set:
//
//	model             enabled    adaptive   disabled   effort
//	claude-haiku-4-5  accepted   400        accepted   400 (no such param)
//	claude-opus-4-5   accepted   400        accepted   accepted
//	claude-opus-4-6   accepted*  accepted   accepted   accepted, no "xhigh"
//	claude-opus-4-7   400        accepted   accepted   accepted
//	claude-opus-4-8   400        accepted   accepted   accepted
//	claude-opus-5     400        accepted   accepted   accepted
//	claude-sonnet-5   400        accepted   accepted   accepted
//
//	* with a deprecation warning naming adaptive as the replacement.
//
// Measured 2026-09-15 against Vertex region `global`, anthropic-sdk-go
// v1.43.0, one request per cell.

func thinkingFor(t *testing.T, modelID string, tc *genai.ThinkingConfig) (enabled *int64, adaptive string, disabled bool) {
	t.Helper()
	contents := []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}}}
	p, err := buildParams(modelID, contents, &genai.GenerateContentConfig{ThinkingConfig: tc}, false, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams(%s): %v", modelID, err)
	}
	if p.Thinking.OfEnabled != nil {
		b := p.Thinking.OfEnabled.BudgetTokens
		enabled = &b
	}
	if p.Thinking.OfAdaptive != nil {
		adaptive = string(p.Thinking.OfAdaptive.Display)
		if adaptive == "" {
			adaptive = "(default)"
		}
	}
	disabled = p.Thinking.OfDisabled != nil
	return
}

// A positive budget must never produce the enabled shape on a model that
// 400s it. This is the defect in #369: mast's own DefaultModel is
// claude-opus-5, and the only thinking config mast could build was the
// one that model rejects.
func TestThinkingShapeSplitsByModel(t *testing.T) {
	t.Parallel()
	budget := &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](2048)}

	for _, m := range []string{"claude-haiku-4-5", "claude-opus-4-5", "claude-sonnet-4-5"} {
		t.Run("legacy/"+m, func(t *testing.T) {
			t.Parallel()
			enabled, adaptive, _ := thinkingFor(t, m, budget)
			if enabled == nil || *enabled != 2048 {
				t.Errorf("%s: enabled = %v, want budget 2048 — this model 400s adaptive", m, enabled)
			}
			if adaptive != "" {
				t.Errorf("%s: sent adaptive (%s), which this model 400s", m, adaptive)
			}
		})
	}

	// 4-6 accepts both and the vendor's own deprecation warning says to
	// prefer adaptive, so it sits on the adaptive side of the line.
	for _, m := range []string{
		"claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8",
		"claude-opus-5", "claude-sonnet-5", "claude-sonnet-4-6",
	} {
		t.Run("adaptive/"+m, func(t *testing.T) {
			t.Parallel()
			enabled, adaptive, _ := thinkingFor(t, m, budget)
			if enabled != nil {
				t.Errorf("%s: sent enabled(%d), which this model 400s", m, *enabled)
			}
			if adaptive == "" {
				t.Errorf("%s: no adaptive thinking param built", m)
			}
		})
	}
}

// An unknown model takes adaptive, not the shape that used to be safe.
// Anthropic deprecated enabled at 4-6 and removed it at 4-7, so the
// migration runs one way and the legacy list is closed: guessing
// "enabled" for a model released tomorrow guesses wrong.
func TestUnknownModelTakesAdaptive(t *testing.T) {
	t.Parallel()
	enabled, adaptive, _ := thinkingFor(t, "claude-something-9", &genai.ThinkingConfig{
		ThinkingBudget: genai.Ptr[int32](2048),
	})
	if enabled != nil {
		t.Errorf("unknown model: sent enabled(%d), want adaptive", *enabled)
	}
	if adaptive == "" {
		t.Error("unknown model: no adaptive thinking param built")
	}
}

// The empty model ID resolves to DefaultModel before the shape is
// chosen — the one call site that made #369 a live defect rather than a
// latent one.
func TestDefaultModelTakesAdaptive(t *testing.T) {
	t.Parallel()
	enabled, adaptive, _ := thinkingFor(t, "", &genai.ThinkingConfig{
		ThinkingBudget: genai.Ptr[int32](2048),
	})
	if enabled != nil {
		t.Errorf("DefaultModel (%s): sent enabled(%d), the shape it 400s", DefaultModel, *enabled)
	}
	if adaptive == "" {
		t.Errorf("DefaultModel (%s): no adaptive thinking param built", DefaultModel)
	}
}

// A zero budget is the only way genai can say "do not think", and
// before #369 it was the one request that could not say it: mast sent
// no thinking param and the model thought anyway (measured — opus-5
// returns a signed thinking block on a bare request). `disabled` is the
// one shape every model in the matrix accepts.
func TestZeroBudgetDisablesThinking(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"claude-opus-5", "claude-opus-4-5", "claude-haiku-4-5"} {
		t.Run(m, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []*genai.ThinkingConfig{
				{ThinkingBudget: genai.Ptr[int32](0)},
				{ThinkingBudget: genai.Ptr[int32](-1)},
				// "Show me the thinking" loses to "do not think":
				// there is nothing to show.
				{ThinkingBudget: genai.Ptr[int32](0), IncludeThoughts: true},
			} {
				enabled, adaptive, disabled := thinkingFor(t, m, tc)
				if !disabled {
					t.Errorf("%s %+v: thinking not disabled (enabled=%v adaptive=%q)", m, tc, enabled, adaptive)
				}
				if enabled != nil || adaptive != "" {
					t.Errorf("%s %+v: disabled must be the only shape set", m, tc)
				}
			}
		})
	}
}

// display is the Anthropic equivalent IncludeThoughts never had. The
// default is omitted: the signature still comes back so a tool loop
// still replays (#357), but mast does not pay for reasoning text that
// #370 filters out of every surface it has. Asking for it is the
// caller's explicit decision, and the one OQ 5 will route.
func TestIncludeThoughtsSelectsDisplay(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		tc      *genai.ThinkingConfig
		want    string
		wantAny bool
	}{
		{"budget alone omits the text", &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](2048)}, "omitted", true},
		{"IncludeThoughts asks for it", &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](2048), IncludeThoughts: true}, "summarized", true},
		// IncludeThoughts alone had no Anthropic equivalent before
		// adaptive existed; it has one now, so it stops being a no-op.
		{"IncludeThoughts alone", &genai.ThinkingConfig{IncludeThoughts: true}, "summarized", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, adaptive, _ := thinkingFor(t, "claude-opus-5", tt.tc)
			if adaptive != tt.want {
				t.Errorf("display = %q, want %q", adaptive, tt.want)
			}
		})
	}
}

// An absent ThinkingConfig still sends no thinking param at all, so a
// caller who never mentions thinking gets a byte-identical request and
// the vendor's own default. #369 changes what mast does when asked, not
// what it does when not asked.
func TestNoThinkingConfigSendsNothing(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"claude-opus-5", "claude-opus-4-5"} {
		enabled, adaptive, disabled := thinkingFor(t, m, nil)
		if enabled != nil || adaptive != "" || disabled {
			t.Errorf("%s: thinking param set for a nil ThinkingConfig", m)
		}
	}
	// IncludeThoughts is false and there is no budget: nothing was
	// asked for, so nothing is sent.
	enabled, adaptive, disabled := thinkingFor(t, "claude-opus-5", &genai.ThinkingConfig{})
	if enabled != nil || adaptive != "" || disabled {
		t.Error("empty ThinkingConfig: thinking param set")
	}
}

// A pinned snapshot has to resolve to its family or the shape is chosen
// off a string the table has never seen. Anthropic writes the suffix
// with a dash on the first-party API and with an @ on Vertex; both
// appear in mast configs, and anthropic.go's own doc comment uses the @
// form.
func TestBaseModelIDStripsSnapshots(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ in, want string }{
		{"claude-opus-4-5", "claude-opus-4-5"},
		{"claude-opus-4-5-20251101", "claude-opus-4-5"},
		{"claude-opus-4-5@20251101", "claude-opus-4-5"},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5"},
		{"claude-opus-5", "claude-opus-5"},
		// Not a snapshot: eight digits is the shape, and a shorter or
		// longer run is part of the name.
		{"claude-opus-4-5-2025", "claude-opus-4-5-2025"},
		{"claude-opus-4-5-202511010", "claude-opus-4-5-202511010"},
		{"claude-opus-4-5-2025110x", "claude-opus-4-5-2025110x"},
	} {
		if got := baseModelID(tt.in); got != tt.want {
			t.Errorf("baseModelID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// The point of the stripping: a pinned legacy model still gets the
	// legacy shape.
	for _, m := range []string{"claude-opus-4-5-20251101", "claude-opus-4-5@20251101", "claude-haiku-4-5-20251001"} {
		if !legacyThinkingModel(m) {
			t.Errorf("legacyThinkingModel(%q) = false; a pinned snapshot would be sent adaptive, which it 400s", m)
		}
	}
}
