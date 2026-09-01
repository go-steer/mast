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

package pricing

import (
	"strings"
	"testing"
)

// The same model served by two backends is two prices. They agree today
// on everything mast ships, which is exactly why the thing to pin is not
// a number but a key: LookupFor must read the (backend, model) row when
// one exists, so that the day a backend's price moves, the move lands
// without anyone having to notice it first.
func TestLookupFor_PrefersTheQualifiedRow(t *testing.T) {
	t.Parallel()
	c := &Catalog{builtin: map[string]Rates{
		"m":                  {InputPerMTok: 1},
		"anthropic/m":        {InputPerMTok: 2},
		"anthropic-vertex/m": {InputPerMTok: 3},
	}}
	for _, tc := range []struct {
		backend string
		want    float64
		why     string
	}{
		{"anthropic", 2, "the first-party row"},
		{"anthropic-vertex", 3, "the Vertex row"},
		{"", 1, "no backend named, so the bare row"},
		{"gemini", 1, "no row for this pair, so the bare row"},
	} {
		r, ok := c.LookupFor(tc.backend, "m")
		if !ok || r.InputPerMTok != tc.want {
			t.Errorf("LookupFor(%q, m) = %+v ok=%v, want InputPerMTok %v (%s)",
				tc.backend, r, ok, tc.want, tc.why)
		}
	}
}

// A pair no table prices must fall back to the bare ID rather than
// report a miss. Vertex does not serve every Claude model, and a
// workload that asks for one it does serve tomorrow should get the
// model's price today, not $0 and not a refusal.
func TestLookupFor_FallsBackToTheBareID(t *testing.T) {
	t.Parallel()
	c, err := NewCatalog(Options{})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	bare, ok := c.Lookup("claude-mythos-5")
	if !ok || bare.IsZero() {
		t.Fatal("builtin lost claude-mythos-5; pick another model for this test")
	}
	if _, ok := c.Lookup("anthropic-vertex/claude-mythos-5"); ok {
		t.Skip("upstream now prices claude-mythos-5 on Vertex; pick another unserved pair")
	}
	got, ok := c.LookupFor("anthropic-vertex", "claude-mythos-5")
	if !ok {
		t.Fatal("an unserved pair reported a miss; the bare row was right there")
	}
	if got != bare {
		t.Errorf("LookupFor(anthropic-vertex, claude-mythos-5) = %+v, want the bare %+v", got, bare)
	}
}

// A backend nobody has priced anything for must not silently drag every
// lookup to the bare table's *prefix* neighbourhood or to another
// backend's row. The "/" in a qualified key is what keeps "anthropic"
// from prefix-matching "anthropic-vertex/...", and this pins it.
func TestLookupFor_BackendsDoNotBleedIntoEachOther(t *testing.T) {
	t.Parallel()
	c := &Catalog{builtin: map[string]Rates{
		"anthropic-vertex/m": {InputPerMTok: 3},
	}}
	if r, ok := c.LookupFor("anthropic", "m"); ok {
		t.Errorf("LookupFor(anthropic, m) resolved to %+v; only anthropic-vertex has a row, "+
			"and a backend prefix must not match a longer backend's key", r)
	}
	// The prefix rule still applies *within* a backend, which is what
	// makes dated variants work without a row each.
	if r, ok := c.LookupFor("anthropic-vertex", "m-20260101"); !ok || r.InputPerMTok != 3 {
		t.Errorf("LookupFor(anthropic-vertex, m-20260101) = %+v ok=%v, want the m row; "+
			"prefix fallback must survive qualification", r, ok)
	}
}

// Qualified keys are ordinary catalog keys, so every layer can override
// one. An operator who is billed differently on one backend writes that
// backend's key into their pricing file and the override wins there and
// nowhere else — which is the whole reason the two generated tables
// share a layer instead of sitting behind a separate lookup.
func TestLookupForWithSource_AttributesTheWinningLayer(t *testing.T) {
	t.Parallel()
	c := &Catalog{
		userManual: map[string]Rates{"anthropic-vertex/m": {InputPerMTok: 99}},
		builtin: map[string]Rates{
			"m":                  {InputPerMTok: 1},
			"anthropic-vertex/m": {InputPerMTok: 3},
		},
	}
	r, src, ok := c.LookupForWithSource("anthropic-vertex", "m")
	if !ok || r.InputPerMTok != 99 {
		t.Fatalf("LookupForWithSource = %+v ok=%v, want the user override at 99", r, ok)
	}
	if src != SourceUserManual {
		t.Errorf("source = %q, want %q", src, SourceUserManual)
	}
	// The override is scoped to its pair. First-party still prices off
	// the bare row.
	if r, src, _ := c.LookupForWithSource("anthropic", "m"); r.InputPerMTok != 1 || src != SourceBuiltin {
		t.Errorf("LookupForWithSource(anthropic, m) = %+v from %q, want the builtin bare row at 1",
			r, src)
	}
}

// The generator emits a row per (backend, model) pair even when the two
// backends charge the same, and these are the shape rules that keeps it
// honest. The bare table's companion tables — tiers and context windows
// — are keyed on model IDs alone, so a qualified key must never appear
// in the bare table, and a qualified key's model half must always exist
// in it.
func TestBuiltinByBackend_Shape(t *testing.T) {
	t.Parallel()
	backends := map[string]bool{
		"anthropic": true, "anthropic-vertex": true, "gemini": true, "vertex": true,
	}
	bare := Builtin()
	qualified := BuiltinByBackend()
	if len(qualified) == 0 {
		t.Fatal("no backend-qualified rows; regenerate with `go run ./dev/regen-builtin-pricing`")
	}
	for key, r := range qualified {
		backend, model, found := strings.Cut(key, "/")
		if !found {
			t.Errorf("%q has no backend prefix; it would shadow a bare row in the merged layer", key)
			continue
		}
		if !backends[backend] {
			t.Errorf("%q names backend %q, which is not one mast resolves; "+
				"a key nothing can construct is a row nothing will ever read", key, backend)
		}
		if _, ok := bare[model]; !ok {
			t.Errorf("%q prices a model the bare table does not carry; "+
				"the fallback path and the tier/context-window tables all key on %q", key, model)
		}
		if r.IsZero() {
			t.Errorf("%q priced at zero; a missing pair must be absent so it falls back, not free", key)
		}
	}
	for key := range bare {
		if strings.Contains(key, "/") {
			t.Errorf("qualified key %q leaked into the bare table; "+
				"TestBuiltinTablesHaveMatchingKeys demands a tier and a window for it", key)
		}
	}
}

// Every model is priced on the backend that actually serves it. Claude
// is first-party everywhere and on Vertex for the subset Google resells;
// Gemini is served by both the Developer API and Vertex for all of it.
// A gap here means a live pairing quietly reverts to the bare rate.
func TestBuiltinByBackend_CoversTheServingBackends(t *testing.T) {
	t.Parallel()
	qualified := BuiltinByBackend()
	for model := range Builtin() {
		var want []string
		switch {
		case strings.HasPrefix(model, "claude-"):
			// Not anthropic-vertex: Google resells a subset, and the
			// generator emits a pair only where upstream prices one.
			want = []string{"anthropic"}
		case strings.HasPrefix(model, "gemini-"):
			want = []string{"gemini", "vertex"}
		default:
			t.Errorf("builtin carries %q, which is in neither family; "+
				"the generator would emit no qualified row for it at all", model)
			continue
		}
		for _, backend := range want {
			if _, ok := qualified[backend+"/"+model]; !ok {
				t.Errorf("no %q row for %q; that pairing prices off the bare table by accident",
					backend, model)
			}
		}
	}
}
