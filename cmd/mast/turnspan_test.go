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
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordSpans points span recording at a fresh recorder for the
// duration of one test.
//
// It has to be the *global* provider, because that is where the defect
// lives: mast's tracer and ADK's are both bound to it (ADK's telemetry
// package is internal and cannot be handed a provider), so a test with
// a local provider would prove nothing about the parenting this file
// exists to check.
//
// But the global can only be swapped once and mean it. OTel's default
// provider hands out delegating tracers and binds them to the first
// real provider installed, under a sync.Once; a tracer obtained at
// package init — which `tracer` is — keeps writing to that first
// provider no matter how many times SetTracerProvider is called
// afterwards. So the global gets set exactly once here too, to a
// provider whose one processor forwards to whichever recorder is
// current. Second and later tests get a clean recorder without touching
// the global at all.
//
// These tests do not run in parallel, and each filters by the session
// under test, so spans from a concurrent package test cannot be
// mistaken for theirs.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	installRecorderOnce.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(currentRecorder)))
	})
	sr := tracetest.NewSpanRecorder()
	currentRecorder.swap(sr)
	t.Cleanup(func() { currentRecorder.swap(nil) })
	return sr
}

var (
	installRecorderOnce sync.Once
	currentRecorder     = &swappableRecorder{}
)

// swappableRecorder is the single span processor the global provider
// ever receives; it forwards to the recorder the running test installed,
// and drops spans from every other test in the package.
type swappableRecorder struct {
	mu  sync.Mutex
	cur *tracetest.SpanRecorder
}

func (s *swappableRecorder) swap(sr *tracetest.SpanRecorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = sr
}

func (s *swappableRecorder) get() *tracetest.SpanRecorder {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *swappableRecorder) OnStart(parent context.Context, span sdktrace.ReadWriteSpan) {
	if sr := s.get(); sr != nil {
		sr.OnStart(parent, span)
	}
}

func (s *swappableRecorder) OnEnd(span sdktrace.ReadOnlySpan) {
	if sr := s.get(); sr != nil {
		sr.OnEnd(span)
	}
}

func (s *swappableRecorder) Shutdown(context.Context) error   { return nil }
func (s *swappableRecorder) ForceFlush(context.Context) error { return nil }

// turnSpanFor returns the single mast.turn span for this session,
// failing if there is not exactly one.
func turnSpanFor(t *testing.T, sr *tracetest.SpanRecorder, sid string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() != "mast.turn" {
			continue
		}
		if attrString(s, "mast.session.id") == sid {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d mast.turn spans for session %q, want exactly 1", len(found), sid)
	}
	return found[0]
}

func attrString(s sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key && kv.Value.Type() == attribute.STRING {
			return kv.Value.AsString()
		}
	}
	return ""
}

func hasAttr(s sdktrace.ReadOnlySpan, key string) bool {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return true
		}
	}
	return false
}

// A turn with no HTTP request behind it — a scheduled fire, an
// auto-resume, this harness — used to hand runner.Run a context with no
// span on it, so ADK's `invoke_agent` became a **trace root**: every
// model call and tool call under it belonged to a trace that started
// nowhere, and nothing on it named the session.
//
// The assertion that matters is not "a span exists" but "ADK's spans
// are underneath it". A root span nobody parents off is the same
// disconnected trace with one more entry in it.
func TestATurnWithNoRequestBehindItIsStillOneTrace(t *testing.T) {
	sr := recordSpans(t)
	h := newTurnHarness(t, &blockableModel{})
	sid := "trace-parenting"

	if err := h.turn(context.Background(), sid); err != nil {
		t.Fatalf("turn: %v", err)
	}

	root := turnSpanFor(t, sr, sid)
	if root.Parent().IsValid() {
		t.Errorf("mast.turn has parent %v; with no request behind the turn it should be the trace root", root.Parent().SpanID())
	}

	// ADK owns everything below: invoke_agent, generate_content,
	// execute_tool. We cannot import its telemetry package, so the
	// check is structural — anything else recorded in this trace must
	// descend from our span.
	var adk int
	for _, s := range sr.Ended() {
		if s.Name() == "mast.turn" {
			continue
		}
		if s.SpanContext().TraceID() != root.SpanContext().TraceID() {
			continue
		}
		adk++
		if !s.Parent().IsValid() {
			t.Errorf("span %q in the turn's trace is itself a root; ADK's tree is not parented under mast.turn", s.Name())
		}
	}
	if adk == 0 {
		t.Fatal("no ADK spans landed in the turn's trace — the parenting assertion above proved nothing")
	}
}

// The span is the record on the unattended paths, so it has to carry
// enough to find the turn again: which session, which workload, what
// kind of turn, how it ended, what it cost.
func TestTurnSpanCarriesTheFactsAnOperatorSearchesBy(t *testing.T) {
	sr := recordSpans(t)
	h := newTurnHarness(t, &blockableModel{})
	sid := "trace-attrs"

	if err := h.turn(context.Background(), sid); err != nil {
		t.Fatalf("turn: %v", err)
	}

	s := turnSpanFor(t, sr, sid)
	for key, want := range map[string]string{
		"mast.workload.name": "(test)",
		"mast.turn.kind":     "test",
		"mast.turn.outcome":  "ok",
	} {
		if got := attrString(s, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// Numeric attributes: presence, not value. Cost is zero under a
	// fake model and the queue wait is whatever the machine did.
	for _, key := range []string{"mast.cost.usd", "mast.turn.queued_ms"} {
		if !hasAttr(s, key) {
			t.Errorf("%s missing; the span cannot answer what the turn cost or how long it queued", key)
		}
	}
	if s.Status().Code == codes.Error {
		t.Errorf("a clean turn recorded span status Error (%q)", s.Status().Description)
	}
}

// The turn label the chokepoint already carries is `kind:detail`, and
// the span splits it. The cases below are the labels the six producers
// build today (grep `runTurnPre(`/`runTurn(` for them) — a colonless
// label is legal and becomes a bare kind, which is right for `oneshot`
// and would be a silent loss of grouping for anything else.
func TestTurnLabelSplitsIntoKindAndDetail(t *testing.T) {
	for _, tc := range []struct {
		label, kind, detail string
	}{
		{"inject:alert-firing", "inject", "alert-firing"},
		{"attach:inject", "attach", "inject"},
		{"resume:int-7", "resume", "int-7"},
		{"scheduled:2026-08-20T09:00:00Z", "scheduled", "2026-08-20T09:00:00Z"},
		{"autoresume:sess-1", "autoresume", "sess-1"},
		{"a2a:message/send", "a2a", "message/send"},
		{"agui:run", "agui", "run"},
		{"oneshot", "oneshot", ""},
	} {
		t.Run(tc.label, func(t *testing.T) {
			sr := recordSpans(t)
			sid := "split-" + tc.kind
			_, ts := startTurnSpan(context.Background(), nil, "(test)", sid, tc.label)
			ts.end(nil)

			s := turnSpanFor(t, sr, sid)
			if got := attrString(s, "mast.turn.kind"); got != tc.kind {
				t.Errorf("kind = %q, want %q", got, tc.kind)
			}
			if got := attrString(s, "mast.turn.detail"); got != tc.detail {
				t.Errorf("detail = %q, want %q", got, tc.detail)
			}
			if tc.detail == "" && hasAttr(s, "mast.turn.detail") {
				t.Error("an empty detail was stamped rather than omitted")
			}
		})
	}
}

// A turn the chokepoint refuses is the first one an operator goes
// looking for, and it is the one mast_turns_total cannot show them —
// that counter has only ever counted turns that started. The span has
// to record it, with the error on it.
func TestARefusedTurnStillLeavesASpan(t *testing.T) {
	sr := recordSpans(t)
	h := newTurnHarness(t, &blockableModel{})
	sid := "trace-refused"
	ctx := context.Background()

	// Give the session a row, then terminally abort it: the chokepoint
	// refuses every subsequent turn before the runner is reached.
	if err := h.turn(ctx, sid); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	if err := h.store.Abort(ctx, "", sid, "operator test"); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	err := h.turn(ctx, sid)
	if err == nil {
		t.Fatal("an aborted session accepted a turn")
	}

	// Two turns ran on this session; the refused one is the second.
	var refused sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "mast.turn" && attrString(s, "mast.session.id") == sid &&
			attrString(s, "mast.turn.outcome") == "refused" {
			refused = s
		}
	}
	if refused == nil {
		t.Fatal("the refused turn left no span with outcome=refused")
	}
	if refused.Status().Code != codes.Error {
		t.Errorf("refused turn span status = %v, want Error", refused.Status().Code)
	}
	if len(refused.Events()) == 0 {
		t.Error("refused turn span records no error event; the reason is only in the log")
	}
	// The refusal must not be counted as a started turn — changing
	// that would move every dashboard's denominator.
	if hasAttr(refused, "mast.cost.usd") {
		t.Error("a turn refused before the runner reports a cost")
	}
}

// The outcome on the span and the outcome on mast_turns_total come from
// one call, so they cannot drift. Pinned because they used to be two
// statements a hundred lines apart.
func TestSpanOutcomeAndMetricOutcomeComeFromOneCall(t *testing.T) {
	sr := recordSpans(t)
	h := newTurnHarness(t, &blockableModel{})
	sid := "trace-outcome-agreement"

	if err := h.turn(context.Background(), sid); err != nil {
		t.Fatalf("turn: %v", err)
	}

	span := attrString(turnSpanFor(t, sr, sid), "mast.turn.outcome")
	metric := turnsTotalOutcome(t, h)
	if span != metric {
		t.Errorf("span outcome %q, mast_turns_total outcome %q", span, metric)
	}
}

// turnsTotalOutcome scrapes the registry's own /metrics handler — the
// same text an operator's Prometheus reads — and returns the single
// mast_turns_total outcome with a nonzero count.
func turnsTotalOutcome(t *testing.T, h *turnHarness) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.obs.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	var got []string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "mast_turns_total{") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] == "0" {
			continue
		}
		_, after, ok := strings.Cut(line, `outcome="`)
		if !ok {
			t.Fatalf("mast_turns_total sample has no outcome label: %q", line)
		}
		outcome, _, ok := strings.Cut(after, `"`)
		if !ok {
			t.Fatalf("unterminated outcome label: %q", line)
		}
		got = append(got, outcome)
	}
	if len(got) != 1 {
		t.Fatalf("mast_turns_total has %d nonzero outcomes (%s), want 1", len(got), strings.Join(got, ","))
	}
	return got[0]
}

// Guard on the assumption the whole file rests on: `tracer` is taken
// from the global provider at package init, before any test can install
// a recorder, and OTel's global is specified to bind those early
// tracers to the provider installed later. If that stopped holding,
// every assertion above would pass vacuously against zero spans.
//
// It is also what forced recordSpans to install once and swap the
// recorder underneath: the binding happens on the *first* real provider
// only, so the original one-provider-per-test version left tests 2..n
// reading an empty recorder while the spans went to test 1's.
func TestTracerDelegatesToALateProvider(t *testing.T) {
	sr := recordSpans(t)
	_, span := tracer.Start(context.Background(), "delegation-probe")
	span.End()
	for _, s := range sr.Ended() {
		if s.Name() == "delegation-probe" {
			return
		}
	}
	t.Fatal("the package-level tracer did not reach a provider installed after init; the span assertions in this file would be vacuous")
}
