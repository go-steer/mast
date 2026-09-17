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

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// negativeUsageEvent is one model call whose provider reported
// impossible counts in every bucket the meter reads.
func negativeUsageEvent() *session.Event {
	return &session.Event{
		LLMResponse: model.LLMResponse{
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				TotalTokenCount:         -500,
				PromptTokenCount:        -300,
				CachedContentTokenCount: -100,
				CandidatesTokenCount:    -150,
				ThoughtsTokenCount:      -50,
			},
		},
	}
}

// recordingPricer captures the Call the meter derived, which is the
// only way to see what pricing was asked about rather than what it
// answered.
type recordingPricer struct {
	calls []Call
	usd   float64
}

func (p *recordingPricer) PriceCall(_, _ string, c Call) (float64, bool) {
	p.calls = append(p.calls, c)
	return p.usd, true
}

// A negative count is floored where the metadata is read, so no reader
// downstream of the meter ever sees one: not the durable spend ledger
// (Config.OnSpend), not the session totals behind `GET /usage`
// (Meter.Snapshot, which is verbatim what cmd/mast hands the attach
// surface), and not the buckets handed to a Pricer.
//
// Pre-fix this failed on the ledger, the totals and the buckets. The
// metric is covered separately in pkg/observability — it guarded its
// own two counts already, which is exactly the shape #332 warns about:
// four readers, each deciding for itself.
func TestNegativeCountsAreFlooredForEveryReader(t *testing.T) {
	p := &recordingPricer{usd: 0.25}
	var ledger []Spend
	m := New(Config{
		Limits:  Limits{Pricer: p, Backend: "gemini", Model: "gemini-2.5-pro"},
		OnSpend: func(s Spend) { ledger = append(ledger, s) },
	})

	if err := m.Observe(negativeUsageEvent()); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	if len(ledger) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(ledger))
	}
	if got := ledger[0].Tokens; got != 0 {
		t.Errorf("ledger row tokens = %d, want 0 — a negative delta on a durable ledger is not correctable after the fact", got)
	}

	tokens, cost, calls := m.Snapshot()
	if tokens != 0 {
		t.Errorf("Snapshot tokens = %d, want 0", tokens)
	}
	if calls != 1 {
		t.Errorf("Snapshot calls = %d, want 1 — the call happened", calls)
	}
	if cost != p.usd {
		t.Errorf("Snapshot cost = %v, want %v — the pricer still priced the call", cost, p.usd)
	}

	if len(p.calls) != 1 {
		t.Fatalf("pricer saw %d calls, want 1", len(p.calls))
	}
	if want := (Call{}); p.calls[0] != want {
		t.Errorf("priced call = %+v, want %+v — every bucket floored", p.calls[0], want)
	}
}

// A ceiling comparison reads the same floored counts. Without the
// floor a credit walks the running total backwards, so a session that
// has already spent its budget buys itself more room by being
// miscounted — the failure that makes this a guardrail bug and not an
// invoicing one.
func TestANegativeCountCannotBuyBackBudget(t *testing.T) {
	m := NewMeter(Limits{MaxTokens: 1000})
	if err := m.Observe(usageEvent(900)); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := m.Observe(negativeUsageEvent()); err != nil {
		t.Fatalf("miscounted call: %v", err)
	}
	if tokens, _, _ := m.Snapshot(); tokens != 900 {
		t.Fatalf("tokens after the miscount = %d, want 900", tokens)
	}
	if err := m.Observe(usageEvent(200)); err == nil {
		t.Error("1100 tokens against a 1000 cap was allowed; the miscount bought back budget")
	}
}

// Flooring reads the event, it does not rewrite it. Every other hook on
// the runner's event stream sees the same session.Event, and a meter
// that corrected the record in place would take the raw counter away
// from whoever has to diagnose the provider.
func TestFlooringDoesNotMutateTheEvent(t *testing.T) {
	ev := negativeUsageEvent()
	before := *ev.UsageMetadata
	m := New(Config{Limits: Limits{RatePer1K: 1}})
	if err := m.Observe(ev); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !reflect.DeepEqual(*ev.UsageMetadata, before) {
		t.Errorf("event usage metadata = %+v, want it untouched at %+v", *ev.UsageMetadata, before)
	}
}

// The floor removes impossible values; it does not touch plausible
// ones, and it must not disturb the over-report cap that fitBucket
// applies in the other direction.
func TestFlooredUsageLeavesRealCountsAlone(t *testing.T) {
	in := &genai.GenerateContentResponseUsageMetadata{
		TotalTokenCount:         1230,
		PromptTokenCount:        1000,
		CachedContentTokenCount: 400,
		CandidatesTokenCount:    150,
		ThoughtsTokenCount:      80,
		ToolUsePromptTokenCount: 12,
	}
	if got := flooredUsage(in); !reflect.DeepEqual(got, *in) {
		t.Errorf("flooredUsage rewrote a valid record:\n got %+v\nwant %+v", got, *in)
	}
	if got := flooredUsage(nil); !reflect.DeepEqual(got, genai.GenerateContentResponseUsageMetadata{}) {
		t.Errorf("flooredUsage(nil) = %+v, want the zero record", got)
	}
}

// The placement, asserted rather than trusted.
//
// #332 is not really about the clamp — it is about where it goes.
// Upstream's argument, which mast takes: "pricing was never the only
// reader of these numbers, and a thoughts term guarded locally would
// have kept the negative out of the invoice while leaving it in Totals
// and in the monotonic OTel counter." A floor that lives at one read
// site survives exactly until someone adds a second read site, and
// nothing about the new code would look wrong.
//
// So: this package reads a provider's usage metadata in one function,
// Observe, which hands the floored value down. A new reader fails here,
// and the fix is to take the value Observe already has rather than to
// add a name to the list below.
func TestUsageMetadataIsReadInOneFunction(t *testing.T) {
	const allowed = "Observe"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "UsageMetadata" {
					return true
				}
				pos := fset.Position(sel.Pos())
				if fn.Name.Name != allowed {
					t.Errorf("%s:%d: %s reads UsageMetadata; only %s may — see the comment above",
						name, pos.Line, fn.Name.Name, allowed)
					return true
				}
				seen++
				return true
			})
		}
	}
	// A test that scans for something and finds nothing passes for the
	// wrong reason.
	if seen == 0 {
		t.Errorf("found no UsageMetadata read in %s; this test is no longer checking anything", allowed)
	}
}
