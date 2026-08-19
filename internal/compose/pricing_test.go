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

package compose

import (
	"math"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/pricing"
)

// TestRatePer1K pins the pricing wiring: echo keeps the inflated
// offline-smoke rate, catalog-known gemini models derive from
// pkg/pricing's builtin table (average of input/output per-MTok,
// scaled to per-1K), and catalog misses keep the pre-catalog flat
// spike rate instead of dropping to zero.
func TestRatePer1K(t *testing.T) {
	if got := RatePer1K("echo"); got != 0.05 {
		t.Errorf("RatePer1K(echo) = %v, want 0.05 (offline smoke tests trip caps with it)", got)
	}

	// gemini-3.5-flash is in the builtin catalog; the flat rate is
	// the blended average, computed from the catalog rather than
	// hard-coded so a builtin regen doesn't break this test.
	r, ok := builtinCatalog().Lookup("gemini-3.5-flash")
	if !ok || r.IsZero() {
		t.Fatal("builtin catalog lost gemini-3.5-flash; pick another catalog-known model for this test")
	}
	want := (r.InputPerMTok + r.OutputPerMTok) / 2 / 1000
	if got := RatePer1K("gemini-3.5-flash"); math.Abs(got-want) > 1e-12 {
		t.Errorf("RatePer1K(gemini-3.5-flash) = %v, want %v (builtin catalog blend)", got, want)
	}
	if want <= 0 {
		t.Errorf("derived gemini rate %v is not positive; budget metering would be free", want)
	}

	// Longest-prefix behavior rides along from pricing.Lookup: a
	// dated variant of a catalog model prices like its base ID.
	if got := RatePer1K("gemini-3.5-flash-20260520"); math.Abs(got-want) > 1e-12 {
		t.Errorf("RatePer1K(dated gemini-3.5-flash) = %v, want %v (prefix match)", got, want)
	}

	// Catalog miss: the pre-catalog flat spike rate, never zero.
	if got := RatePer1K("gemini-9.9-imaginary"); got != 0.0006 {
		t.Errorf("RatePer1K(unknown gemini) = %v, want 0.0006 fallback", got)
	}

	// Claude models price from the same catalog blend (P1.3b).
	cr, ok := builtinCatalog().Lookup("claude-opus-4-7")
	if !ok || cr.IsZero() {
		t.Fatal("builtin catalog lost claude-opus-4-7; pick another catalog-known model for this test")
	}
	cwant := (cr.InputPerMTok + cr.OutputPerMTok) / 2 / 1000
	if got := RatePer1K("claude-opus-4-7"); math.Abs(got-cwant) > 1e-12 {
		t.Errorf("RatePer1K(claude-opus-4-7) = %v, want %v (builtin catalog blend)", got, cwant)
	}
	if got := RatePer1K("claude-99-imaginary"); got != 0.003 {
		t.Errorf("RatePer1K(unknown claude) = %v, want 0.003 fallback", got)
	}

	// The scripted replay shares echo's inflated offline rate.
	if got := RatePer1K("scripted"); got != 0.05 {
		t.Errorf("RatePer1K(scripted) = %v, want 0.05 (offline test double)", got)
	}

	if got := RatePer1K("something-else"); got != 0.001 {
		t.Errorf("RatePer1K(non-gemini) = %v, want 0.001", got)
	}
}

// TestBuildModel_ErrorsAndMocks pins BuildModel's credential-free
// paths: the mocks construct, scripted demands MAST_SCRIPT, claude-*
// without credentials names every way to supply them, and unknown
// names enumerate the accepted shapes.
func TestBuildModel_ErrorsAndMocks(t *testing.T) {
	ctx := t.Context()

	if llm, err := BuildModel(ctx, "", "echo"); err != nil || llm == nil {
		t.Fatalf("BuildModel(echo) = (%v, %v), want a model", llm, err)
	}

	t.Setenv("MAST_SCRIPT", "")
	if _, err := BuildModel(ctx, "", "scripted"); err == nil || !strings.Contains(err.Error(), "MAST_SCRIPT") {
		t.Errorf("BuildModel(scripted) without MAST_SCRIPT: err = %v, want mention of MAST_SCRIPT", err)
	}

	// No creds anywhere: the claude-* error must name the recovery
	// paths (API key, Vertex project, explicit --provider).
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	if _, err := BuildModel(ctx, "", "claude-sonnet-4-6"); err == nil ||
		!strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("BuildModel(claude, no creds): err = %v, want guidance naming ANTHROPIC_API_KEY", err)
	}

	// A Gemini-family alias says nothing about Anthropic's backend, so a
	// claude-* override under a gemini root detects like the no-alias
	// path — here, with no credentials at all, that means the same
	// guidance rather than a refusal of the alias.
	if _, err := BuildModel(ctx, ProviderGemini, "claude-sonnet-4-6"); err == nil ||
		!strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("BuildModel(provider=gemini, claude-*): err = %v, want the no-alias credential guidance", err)
	}

	// An alias from neither family still refuses: nothing to detect.
	if _, err := BuildModel(ctx, "echo", "claude-sonnet-4-6"); err == nil ||
		!strings.Contains(err.Error(), "cannot serve claude-*") {
		t.Errorf("BuildModel(provider=echo, claude-*): err = %v, want a refusal naming the anthropic aliases", err)
	}

	if _, err := BuildModel(ctx, "", "gpt-42"); err == nil || !strings.Contains(err.Error(), "claude-*") {
		t.Errorf("BuildModel(gpt-42): err = %v, want the accepted-shapes enumeration", err)
	}

	// First-party anthropic constructs without network as soon as a
	// key is present (client dialing happens per-request).
	t.Setenv("ANTHROPIC_API_KEY", "test-key-not-real")
	if llm, err := BuildModel(ctx, "anthropic", "claude-sonnet-4-6"); err != nil || llm == nil {
		t.Fatalf("BuildModel(anthropic, claude-sonnet-4-6) = (%v, %v), want a model", llm, err)
	}

	// ...and so does the cross-provider override the doc comment on
	// NewModelResolver promises: a claude-* specialist under a Gemini
	// root resolves its own backend from the key it just found.
	if llm, err := BuildModel(ctx, ProviderVertex, "claude-sonnet-4-6"); err != nil || llm == nil {
		t.Fatalf("BuildModel(provider=vertex, claude-sonnet-4-6) = (%v, %v), want a model", llm, err)
	}
}

// TestBuiltinCatalogConstructs guards the OnceValue panic path: empty
// Options must never fail.
func TestBuiltinCatalogConstructs(t *testing.T) {
	if _, err := pricing.NewCatalog(pricing.Options{}); err != nil {
		t.Fatalf("NewCatalog(empty): %v", err)
	}
}
