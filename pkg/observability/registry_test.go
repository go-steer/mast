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

package observability

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
)

// usageEvent builds a synthetic model event the way pkg/agent's echo
// model synthesizes usage: prompt + candidates counts with the total
// derived (real models populate UsageMetadata the same way).
func usageEvent(prompt, candidates int32) *session.Event {
	usage := &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     prompt,
		CandidatesTokenCount: candidates,
	}
	usage.TotalTokenCount = prompt + candidates
	return &session.Event{
		LLMResponse: model.LLMResponse{
			Content:       genai.NewContentFromText("ok", genai.RoleModel),
			UsageMetadata: usage,
		},
		Author: "test-agent",
	}
}

func TestObserveCountsModelCallsAndTokens(t *testing.T) {
	r := New()
	const wl = "gke-triage"

	r.Observe(usageEvent(100, 16), wl)
	r.Observe(usageEvent(50, 8), wl)

	// Events without UsageMetadata (function responses, control
	// events) contribute nothing.
	r.Observe(&session.Event{Author: "control"}, wl)
	r.Observe(nil, wl)

	if got := testutil.ToFloat64(r.modelCalls.WithLabelValues(wl)); got != 2 {
		t.Errorf("mast_model_calls_total = %v, want 2", got)
	}
	if got := testutil.ToFloat64(r.tokens.WithLabelValues(wl, TokenKindPrompt)); got != 150 {
		t.Errorf("mast_tokens_total{kind=prompt} = %v, want 150", got)
	}
	if got := testutil.ToFloat64(r.tokens.WithLabelValues(wl, TokenKindCandidates)); got != 24 {
		t.Errorf("mast_tokens_total{kind=candidates} = %v, want 24", got)
	}
}

func TestObserveCountsHITLPauses(t *testing.T) {
	r := New()
	const wl = "gke-triage"

	pause := &session.Event{
		Author: "approval-gate",
		RequestedInput: &session.RequestInput{
			InterruptID: "approve-ImagePullBackOff",
			Message:     "approve the rollback?",
		},
	}
	r.Observe(pause, wl)

	if got := testutil.ToFloat64(r.hitlPauses.WithLabelValues(wl)); got != 1 {
		t.Errorf("mast_hitl_pauses_total = %v, want 1", got)
	}
	// A pause event carries no usage; it must not count as a model call.
	if got := testutil.ToFloat64(r.modelCalls.WithLabelValues(wl)); got != 0 {
		t.Errorf("mast_model_calls_total = %v, want 0", got)
	}
}

func TestExplicitHelpers(t *testing.T) {
	r := New()
	const wl = "gke-triage"

	r.TurnComplete(wl, OutcomeOK)
	r.TurnComplete(wl, OutcomeOK)
	r.TurnComplete(wl, OutcomeBudgetExceeded)
	r.HITLPause(wl)
	r.HITLResume(wl)
	r.BudgetTrip(wl)
	r.AddCost(wl, 0.0125)
	r.AddCost(wl, 0)  // ignored
	r.AddCost(wl, -1) // ignored

	if got := testutil.ToFloat64(r.turns.WithLabelValues(wl, OutcomeOK)); got != 2 {
		t.Errorf("mast_turns_total{outcome=ok} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(r.turns.WithLabelValues(wl, OutcomeBudgetExceeded)); got != 1 {
		t.Errorf("mast_turns_total{outcome=budget_exceeded} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.hitlPauses.WithLabelValues(wl)); got != 1 {
		t.Errorf("mast_hitl_pauses_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.hitlResumes.WithLabelValues(wl)); got != 1 {
		t.Errorf("mast_hitl_resumes_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.budgetTrips.WithLabelValues(wl)); got != 1 {
		t.Errorf("mast_budget_trips_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.costUSD.WithLabelValues(wl)); got != 0.0125 {
		t.Errorf("mast_cost_usd_total = %v, want 0.0125", got)
	}
}

// TestV02DurableExecutionCounters exercises the #50 families: each
// typed method increments its own family, and the bounded label
// vocabularies (operation / source / outcome) stay separated.
func TestV02DurableExecutionCounters(t *testing.T) {
	r := New()
	const wl = "gke-triage"

	r.MarkerWriteFailure(wl, MarkerOpMark)
	r.MarkerWriteFailure(wl, MarkerOpMark)
	r.MarkerWriteFailure(wl, MarkerOpClear)
	r.MarkerWriteFailure(wl, MarkerOpPause)
	r.Abort(wl)
	r.Abort(wl)
	r.GatePause(wl, GatePauseOperator)
	r.GatePause(wl, GatePausePlannedStop)
	r.GatePause(wl, GatePausePlannedStop)
	r.TimedPauseFire(wl, TimedPauseResumed)
	r.TimedPauseFire(wl, TimedPauseSkipped)
	r.TimedPauseFire(wl, TimedPauseError)

	if got := testutil.ToFloat64(r.markerFailures.WithLabelValues(wl, MarkerOpMark)); got != 2 {
		t.Errorf("mast_marker_write_failures_total{operation=mark} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(r.markerFailures.WithLabelValues(wl, MarkerOpClear)); got != 1 {
		t.Errorf("mast_marker_write_failures_total{operation=clear} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.markerFailures.WithLabelValues(wl, MarkerOpPause)); got != 1 {
		t.Errorf("mast_marker_write_failures_total{operation=pause} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.aborts.WithLabelValues(wl)); got != 2 {
		t.Errorf("mast_aborts_total = %v, want 2", got)
	}
	if got := testutil.ToFloat64(r.gatePauses.WithLabelValues(wl, GatePauseOperator)); got != 1 {
		t.Errorf("mast_gate_pauses_total{source=operator} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.gatePauses.WithLabelValues(wl, GatePausePlannedStop)); got != 2 {
		t.Errorf("mast_gate_pauses_total{source=planned_stop} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(r.timedPauseFires.WithLabelValues(wl, TimedPauseResumed)); got != 1 {
		t.Errorf("mast_timed_pause_fires_total{outcome=resumed} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.timedPauseFires.WithLabelValues(wl, TimedPauseSkipped)); got != 1 {
		t.Errorf("mast_timed_pause_fires_total{outcome=skipped} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.timedPauseFires.WithLabelValues(wl, TimedPauseError)); got != 1 {
		t.Errorf("mast_timed_pause_fires_total{outcome=error} = %v, want 1", got)
	}
}

func TestNilRegistryIsSafe(t *testing.T) {
	var r *Registry
	r.Observe(usageEvent(1, 1), "wl")
	r.TurnComplete("wl", OutcomeOK)
	r.HITLPause("wl")
	r.HITLResume("wl")
	r.BudgetTrip("wl")
	r.AddCost("wl", 1)
	r.AutoResume("wl", AutoResumeResumed)
	r.MarkerWriteFailure("wl", MarkerOpMark)
	r.Abort("wl")
	r.GatePause("wl", GatePauseOperator)
	r.TimedPauseFire("wl", TimedPauseResumed)
}

func TestPrimeMaterializesAllFamiliesAtZero(t *testing.T) {
	r := New()
	r.Prime("gke-triage")

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	for _, sample := range []string{
		`mast_turns_total{outcome="ok",workload="gke-triage"} 0`,
		`mast_turns_total{outcome="error",workload="gke-triage"} 0`,
		`mast_turns_total{outcome="budget_exceeded",workload="gke-triage"} 0`,
		`mast_model_calls_total{workload="gke-triage"} 0`,
		`mast_tokens_total{kind="prompt",workload="gke-triage"} 0`,
		`mast_tokens_total{kind="candidates",workload="gke-triage"} 0`,
		`mast_cost_usd_total{workload="gke-triage"} 0`,
		`mast_hitl_pauses_total{workload="gke-triage"} 0`,
		`mast_hitl_resumes_total{workload="gke-triage"} 0`,
		`mast_budget_trips_total{workload="gke-triage"} 0`,
		`mast_marker_write_failures_total{operation="mark",workload="gke-triage"} 0`,
		`mast_marker_write_failures_total{operation="clear",workload="gke-triage"} 0`,
		`mast_marker_write_failures_total{operation="pause",workload="gke-triage"} 0`,
		`mast_aborts_total{workload="gke-triage"} 0`,
		`mast_gate_pauses_total{source="operator",workload="gke-triage"} 0`,
		`mast_gate_pauses_total{source="planned_stop",workload="gke-triage"} 0`,
		`mast_timed_pause_fires_total{outcome="resumed",workload="gke-triage"} 0`,
		`mast_timed_pause_fires_total{outcome="skipped",workload="gke-triage"} 0`,
		`mast_timed_pause_fires_total{outcome="error",workload="gke-triage"} 0`,
		`mast_a2a_server_tasks_total{outcome="submitted",workload="gke-triage"} 0`,
		`mast_a2a_server_tasks_total{outcome="working",workload="gke-triage"} 0`,
		`mast_a2a_server_tasks_total{outcome="input-required",workload="gke-triage"} 0`,
		`mast_a2a_server_tasks_total{outcome="completed",workload="gke-triage"} 0`,
		`mast_a2a_server_tasks_total{outcome="failed",workload="gke-triage"} 0`,
		`mast_a2a_server_tasks_total{outcome="canceled",workload="gke-triage"} 0`,
		`mast_a2a_server_tasks_total{outcome="rejected",workload="gke-triage"} 0`,
		`mast_agui_runs_total{outcome="success",workload="gke-triage"} 0`,
		`mast_agui_runs_total{outcome="error",workload="gke-triage"} 0`,
		`mast_agui_runs_total{outcome="aborted",workload="gke-triage"} 0`,
		`mast_agui_runs_total{outcome="rejected",workload="gke-triage"} 0`,
		`mast_agui_run_duration_seconds_bucket{workload="gke-triage",le="0.1"} 0`,
		`mast_agui_run_duration_seconds_sum{workload="gke-triage"} 0`,
		`mast_agui_run_duration_seconds_count{workload="gke-triage"} 0`,
	} {
		if !strings.Contains(out, sample) {
			t.Errorf("primed scrape missing %q", sample)
		}
	}
}

func TestMetricsHandlerServesFamilies(t *testing.T) {
	r := New()
	const wl = "gke-triage"
	r.Observe(usageEvent(100, 16), wl)
	r.TurnComplete(wl, OutcomeOK)
	r.HITLPause(wl)
	r.HITLResume(wl)
	r.BudgetTrip(wl)
	r.AddCost(wl, 0.02)

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != 200 {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	for _, family := range []string{
		"mast_turns_total",
		"mast_model_calls_total",
		"mast_tokens_total",
		"mast_cost_usd_total",
		"mast_hitl_pauses_total",
		"mast_hitl_resumes_total",
		"mast_budget_trips_total",
	} {
		if !strings.Contains(out, family) {
			t.Errorf("scrape output missing family %q", family)
		}
	}
	for _, sample := range []string{
		`mast_turns_total{outcome="ok",workload="gke-triage"} 1`,
		`mast_tokens_total{kind="prompt",workload="gke-triage"} 100`,
	} {
		if !strings.Contains(out, sample) {
			t.Errorf("scrape output missing sample %q\nscrape:\n%s", sample, out)
		}
	}
}
