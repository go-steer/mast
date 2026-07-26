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

// Package observability holds mast's telemetry surface: the FIXED
// Prometheus metric registry, and env-gated OTel trace-export setup.
//
// Design contract (docs/observability-design.md):
//
//   - Metric names live here and only here. Callers increment
//     pre-declared families through typed methods; they cannot mint new
//     metric names or labels. That is the cardinality-control point
//     (open question #5: specialists/workloads emit events, not
//     metrics).
//   - The session eventlog is the source of truth; these metrics are a
//     real-time *view* derived from the same event stream the budget
//     meter folds (pkg/budget.Meter.Observe) — Observe here is shaped
//     the same way and is fed from the same loop.
//   - Session ID is never a metric label (cardinality). Correlation at
//     session grain goes through logs and traces.
//   - Traces are ADK v2's own span tree; mast decorates, it does not
//     re-invent (see otel.go).
package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"google.golang.org/adk/v2/session"
)

// Turn outcomes for TurnComplete. A fixed vocabulary — free-form
// outcome strings would be a label-cardinality leak.
const (
	OutcomeOK             = "ok"
	OutcomeError          = "error"
	OutcomeBudgetExceeded = "budget_exceeded"
)

// Token kinds for the mast_tokens_total{kind} label.
const (
	TokenKindPrompt     = "prompt"
	TokenKindCandidates = "candidates"
)

// Registry is the fixed set of mast metric families. Construct one per
// process with New and expose it via Handler on the inject listener.
type Registry struct {
	reg *prometheus.Registry

	turns       *prometheus.CounterVec
	modelCalls  *prometheus.CounterVec
	tokens      *prometheus.CounterVec
	costUSD     *prometheus.CounterVec
	hitlPauses  *prometheus.CounterVec
	hitlResumes *prometheus.CounterVec
	budgetTrips *prometheus.CounterVec
}

// New constructs the registry with every base family pre-registered.
// The underlying prometheus.Registry is private: nothing outside this
// package can register additional collectors through it.
func New() *Registry {
	r := &Registry{reg: prometheus.NewRegistry()}

	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
		r.reg.MustRegister(c)
		return c
	}

	r.turns = counter("mast_turns_total",
		"Turns driven through the runner, by workload and outcome.",
		"workload", "outcome")
	r.modelCalls = counter("mast_model_calls_total",
		"Model calls observed on the event stream (events carrying UsageMetadata).",
		"workload")
	r.tokens = counter("mast_tokens_total",
		"Provider tokens observed, by kind (prompt|candidates).",
		"workload", "kind")
	r.costUSD = counter("mast_cost_usd_total",
		"Accumulated cost in USD as derived by the budget meter's pricing model.",
		"workload")
	r.hitlPauses = counter("mast_hitl_pauses_total",
		"HITL interrupts emitted (RequestedInput events).",
		"workload")
	r.hitlResumes = counter("mast_hitl_resumes_total",
		"HITL resumes fed back into paused sessions.",
		"workload")
	r.budgetTrips = counter("mast_budget_trips_total",
		"Turns aborted because a budget ceiling was crossed.",
		"workload")

	return r
}

// Prime materializes every family's time series for the given workload
// at zero, so a scrape sees all base families from process start
// (before the first turn) and PromQL rate()/increase() have a defined
// origin. Call once at startup per served workload.
func (r *Registry) Prime(workload string) {
	if r == nil {
		return
	}
	for _, outcome := range []string{OutcomeOK, OutcomeError, OutcomeBudgetExceeded} {
		r.turns.WithLabelValues(workload, outcome)
	}
	r.modelCalls.WithLabelValues(workload)
	for _, kind := range []string{TokenKindPrompt, TokenKindCandidates} {
		r.tokens.WithLabelValues(workload, kind)
	}
	r.costUSD.WithLabelValues(workload)
	r.hitlPauses.WithLabelValues(workload)
	r.hitlResumes.WithLabelValues(workload)
	r.budgetTrips.WithLabelValues(workload)
}

// Handler returns the Prometheus scrape handler for this registry,
// suitable for mounting at /metrics on an existing mux.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

// Observe folds one runner event into the counters. Shaped like
// pkg/budget's Meter.Observe so both hooks sit side by side on the
// event-stream loop. Events without UsageMetadata contribute no
// model-call or token counts; nil events are ignored. Safe on a nil
// *Registry so callers can leave telemetry unwired.
func (r *Registry) Observe(ev *session.Event, workload string) {
	if r == nil || ev == nil {
		return
	}
	if ev.RequestedInput != nil {
		r.hitlPauses.WithLabelValues(workload).Inc()
	}
	u := ev.UsageMetadata
	if u == nil {
		return
	}
	r.modelCalls.WithLabelValues(workload).Inc()
	if u.PromptTokenCount > 0 {
		r.tokens.WithLabelValues(workload, TokenKindPrompt).Add(float64(u.PromptTokenCount))
	}
	if u.CandidatesTokenCount > 0 {
		r.tokens.WithLabelValues(workload, TokenKindCandidates).Add(float64(u.CandidatesTokenCount))
	}
}

// TurnComplete records one finished turn with the given outcome (one
// of the Outcome* constants).
func (r *Registry) TurnComplete(workload, outcome string) {
	if r == nil {
		return
	}
	r.turns.WithLabelValues(workload, outcome).Inc()
}

// HITLPause records a HITL interrupt explicitly, for callers that
// detect the pause outside the event stream. Callers already feeding
// events through Observe must not also call this for the same
// interrupt (Observe counts RequestedInput events itself).
func (r *Registry) HITLPause(workload string) {
	if r == nil {
		return
	}
	r.hitlPauses.WithLabelValues(workload).Inc()
}

// HITLResume records an operator resume being fed into a session.
func (r *Registry) HITLResume(workload string) {
	if r == nil {
		return
	}
	r.hitlResumes.WithLabelValues(workload).Inc()
}

// BudgetTrip records a turn aborted on a budget ceiling.
func (r *Registry) BudgetTrip(workload string) {
	if r == nil {
		return
	}
	r.budgetTrips.WithLabelValues(workload).Inc()
}

// AddCost accumulates spend (in USD) attributed to a workload. The
// amount comes from the budget meter — pricing stays in one place;
// this is only the export surface. Non-positive deltas are ignored.
func (r *Registry) AddCost(workload string, usd float64) {
	if r == nil || usd <= 0 {
		return
	}
	r.costUSD.WithLabelValues(workload).Add(usd)
}
