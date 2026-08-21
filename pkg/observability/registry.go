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
	// OutcomeWatchdogHalt is a turn the behavioral watchdog stopped
	// under --watchdog=enforce. Distinct from error because it is the
	// backstop working, and distinct from budget_exceeded because the
	// session has runway left — an alert on this one should page
	// differently from either.
	OutcomeWatchdogHalt = "watchdog_halt"
)

// Token kinds for the mast_tokens_total{kind} label.
const (
	TokenKindPrompt     = "prompt"
	TokenKindCandidates = "candidates"
)

// Marker-write operations for MarkerWriteFailure
// (mast_marker_write_failures_total{operation}). One per durable
// interruption-marker store write the shutdown drain performs; a
// non-zero count means an interrupted turn a restart may never
// surface (the Error log at the failure site names the session).
const (
	MarkerOpMark  = "mark"  // MarkInterrupted (beginDrain / late-start mark)
	MarkerOpClear = "clear" // ClearInterrupted (clean finish during drain)
	MarkerOpPause = "pause" // PauseGate on a planned stop with --pause-sessions
)

// Gate-pause sources for GatePause (mast_gate_pauses_total{source}).
const (
	GatePauseOperator    = "operator"     // POST /pause (incl. hard pause)
	GatePausePlannedStop = "planned_stop" // drain mark with --pause-sessions
)

// Timed-pause fire outcomes (mast_timed_pause_fires_total{outcome}).
// A fixed vocabulary mirroring the scheduler fire callback's branches.
const (
	// TimedPauseResumed: the timer drove a resume/consume to completion.
	TimedPauseResumed = "resumed"
	// TimedPauseSkipped: the fire was a benign no-op — an operator
	// resumed first, or the daemon was draining when the timer fired.
	TimedPauseSkipped = "skipped"
	// TimedPauseError: the resume/consume the timer attempted failed;
	// the timer is rescheduled and will retry.
	TimedPauseError = "error"
)

// Scheduled-trigger outcomes (mast_scheduled_fires_total{outcome}) for
// the v0.4 W4.1 cadence. The family counts what the cadence DID with
// each tick, which is why three of the four outcomes are not runs: a
// scheduled workload that stopped doing its work is indistinguishable
// from a healthy one unless the ticks it declined to run are counted
// too.
const (
	// ScheduledFireRan: the tick drove a turn to completion.
	ScheduledFireRan = "ran"
	// ScheduledFireSkipped: the tick came due while the daemon was
	// draining, so no turn was started. Not an error and not retried —
	// the next tick is the retry.
	ScheduledFireSkipped = "skipped"
	// ScheduledFireError: the turn the tick started failed. The cadence
	// is unaffected; the next tick fires on schedule.
	ScheduledFireError = "error"
	// ScheduledFireMissed: the tick passed with nobody to run it — the
	// daemon was down, or a previous run overran the interval — and was
	// coalesced away rather than caught up. Incremented once per
	// skipped tick, so a crash-looping daemon is visible as a rising
	// missed count rather than as silence.
	ScheduledFireMissed = "missed"
)

// Monitoring-notification outcomes (mast_monitor_notifications_total
// {outcome}) for the v0.5 W4.5 egress leg. The family counts what a
// monitoring cycle DID about telling somebody, which is why "quiet" is
// an outcome: a cycle that decided there was nothing to say is the
// intended common case, and a monitor whose chat has gone silent is
// only distinguishable from a healthy quiet one by whether the quiet
// count is still rising.
const (
	// MonitorNotifyPosted: a new message opened a timeline.
	MonitorNotifyPosted = "posted"
	// MonitorNotifyAppended: the assessment was added to the message the
	// previous speaking cycle posted.
	MonitorNotifyAppended = "appended"
	// MonitorNotifyReplaced: the append could not be applied (the
	// ingress had forgotten the message body) and the whole timeline was
	// re-sent as an edit.
	MonitorNotifyReplaced = "replaced"
	// MonitorNotifyRolled: the append overflowed the platform's message
	// limit and switchboard rolled the timeline into a threaded
	// continuation, which mast now addresses instead.
	MonitorNotifyRolled = "rolled"
	// MonitorNotifyQuiet: the classification was empty, so no model was
	// woken and nothing was sent.
	MonitorNotifyQuiet = "quiet"
	// MonitorNotifyHealth: mast spoke about itself — the monitoring
	// cycle failed, or recovered after failing. Counted apart from the
	// assessments because it is the one message a cycle sends that no
	// model wrote.
	MonitorNotifyHealth = "health"
	// MonitorNotifyError: the ingress refused, or could not be reached.
	// The cycle's work is not replayed; the next cycle is a fresher
	// sample.
	MonitorNotifyError = "error"
)

// monitorNotifyOutcomes is the fixed label set primed for
// mast_monitor_notifications_total{outcome}, kept beside the vocabulary
// so Prime and MonitorNotify cannot drift.
var monitorNotifyOutcomes = []string{
	MonitorNotifyPosted,
	MonitorNotifyAppended,
	MonitorNotifyReplaced,
	MonitorNotifyRolled,
	MonitorNotifyQuiet,
	MonitorNotifyHealth,
	MonitorNotifyError,
}

// Auto-resume outcomes for AutoResume (mast_autoresume_total{outcome}).
// A fixed vocabulary, mirroring cmd/mast's boot-time auto-resume
// decision tree (#41): every interrupted candidate the boot pass
// inspects lands in exactly one of these.
const (
	// AutoResumeResumed: a continuation turn ran to completion and the
	// interruption marker was cleared.
	AutoResumeResumed = "resumed"
	// AutoResumeCleared: the trailing event was already a clean model
	// turn (stale marker / clear race); the marker was cleared without
	// running a turn.
	AutoResumeCleared = "cleared"
	// AutoResumeSkippedStale: the interruption is older than the
	// freshness window; the marker was left for an operator.
	AutoResumeSkippedStale = "skipped_stale"
	// AutoResumeSkippedAmbiguous: the session carries a dangling mutating
	// intent (ambiguous effect); left for an operator ack.
	AutoResumeSkippedAmbiguous = "skipped_ambiguous"
	// AutoResumeSkippedLoopbreak: the per-session attempt breaker or the
	// per-boot cap tripped.
	AutoResumeSkippedLoopbreak = "skipped_loopbreak"
	// AutoResumeSkippedSuperseded: a concurrent turn advanced the session
	// after the scan (M1 TOCTOU recheck).
	AutoResumeSkippedSuperseded = "skipped_superseded"
	// AutoResumeSkippedUnsupported: a shape slice-1 does not drive
	// (non-coordinator dispatch, dangling sub-agent delegation, or a
	// multi-event repair).
	AutoResumeSkippedUnsupported = "skipped_unsupported"
	// AutoResumeError: the continuation turn was attempted and failed;
	// the marker was left in place.
	AutoResumeError = "error"
)

// A2A server task outcomes for A2ATask (mast_a2a_server_tasks_total
// {workload,outcome}). The vocabulary mirrors the A2A task-lifecycle
// states mast reports (docs/a2a-design.md "Task lifecycle mapping"); the
// string VALUES match pkg/a2a's TaskState constants — the server passes
// string(state), so these two lists must not drift.
const (
	A2ATaskSubmitted     = "submitted"
	A2ATaskWorking       = "working"
	A2ATaskInputRequired = "input-required"
	A2ATaskCompleted     = "completed"
	A2ATaskFailed        = "failed"
	A2ATaskCanceled      = "canceled"
	A2ATaskRejected      = "rejected"
)

// a2aTaskOutcomes is the fixed label set primed for
// mast_a2a_server_tasks_total{outcome}. Kept beside the counter so Prime
// and the A2ATask vocabulary can never drift.
var a2aTaskOutcomes = []string{
	A2ATaskSubmitted,
	A2ATaskWorking,
	A2ATaskInputRequired,
	A2ATaskCompleted,
	A2ATaskFailed,
	A2ATaskCanceled,
	A2ATaskRejected,
}

// AG-UI server run outcomes for AGUIRun (mast_agui_runs_total
// {workload,outcome}). A fixed vocabulary mirroring the AG-UI run
// dispositions the server reports (docs/ag-ui-design.md): a completed run, a
// run that paused for human input (a clean interrupt, resumable via
// RunAgentInput.Resume), an errored run, an operator/client abort, and a
// pre-stream refusal (auth/scope/rate-limit/drain/not-resumable). The string
// VALUES match pkg/agui's internal outcome constants — the server passes them
// through, so these two lists must not drift. Both sides are pinned to the same
// literals: pkg/agui's own test locks its unexported constants, and a cmd/mast
// test locks these exported ones, so a move on either side fails a build.
const (
	AGUIRunSuccess     = "success"
	AGUIRunInterrupted = "interrupted"
	AGUIRunError       = "error"
	AGUIRunAborted     = "aborted"
	AGUIRunRejected    = "rejected"
)

// aguiRunOutcomes is the fixed label set primed for
// mast_agui_runs_total{outcome}. Kept beside the counter so Prime and the
// AGUIRun vocabulary can never drift.
var aguiRunOutcomes = []string{
	AGUIRunSuccess,
	AGUIRunInterrupted,
	AGUIRunError,
	AGUIRunAborted,
	AGUIRunRejected,
}

// Registry is the fixed set of mast metric families. Construct one per
// process with New and expose it via Handler on the inject listener.
type Registry struct {
	reg *prometheus.Registry

	turns           *prometheus.CounterVec
	modelCalls      *prometheus.CounterVec
	tokens          *prometheus.CounterVec
	costUSD         *prometheus.CounterVec
	hitlPauses      *prometheus.CounterVec
	hitlResumes     *prometheus.CounterVec
	budgetTrips     *prometheus.CounterVec
	autoResume      *prometheus.CounterVec
	markerFailures  *prometheus.CounterVec
	aborts          *prometheus.CounterVec
	gatePauses      *prometheus.CounterVec
	timedPauseFires *prometheus.CounterVec
	scheduledFires  *prometheus.CounterVec
	monitorNotifies *prometheus.CounterVec
	monitorDigests  *prometheus.CounterVec
	a2aTasks        *prometheus.CounterVec
	aguiRuns        *prometheus.CounterVec
	aguiRunDur      *prometheus.HistogramVec
}

// autoResumeOutcomes is the fixed label set primed for
// mast_autoresume_total{outcome}. Kept beside the counter so Prime and
// the AutoResume vocabulary can never drift.
var autoResumeOutcomes = []string{
	AutoResumeResumed,
	AutoResumeCleared,
	AutoResumeSkippedStale,
	AutoResumeSkippedAmbiguous,
	AutoResumeSkippedLoopbreak,
	AutoResumeSkippedSuperseded,
	AutoResumeSkippedUnsupported,
	AutoResumeError,
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

	// histogram mirrors counter for latency families. The default buckets
	// span the sub-second-to-minutes range a turn's wallclock lands in
	// (prometheus.DefBuckets tops out at 10s, too short for a multi-round
	// model turn), so an explicit bucket set is used.
	histogram := func(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
		h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
		r.reg.MustRegister(h)
		return h
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
	r.autoResume = counter("mast_autoresume_total",
		"Boot-time auto-resume decisions, by workload and outcome.",
		"workload", "outcome")

	// v0.2 durable-execution families (#50). These count the operator
	// and shutdown chokepoints the pause/abort/stop spine drives —
	// deliberately low-cardinality (bounded label vocabularies, no
	// session ID) so the whole family set stays a fixed view.
	r.markerFailures = counter("mast_marker_write_failures_total",
		"Durable marker writes that failed during the shutdown drain (mark/clear = interruption marker, pause = planned-stop gate-pause write), by operation.",
		"workload", "operation")
	r.aborts = counter("mast_aborts_total",
		"Terminal aborts durably recorded through the operator surface.",
		"workload")
	r.gatePauses = counter("mast_gate_pauses_total",
		"Gate pauses durably recorded, by source (operator request or planned stop).",
		"workload", "source")
	r.timedPauseFires = counter("mast_timed_pause_fires_total",
		"Timed-pause scheduler fires, by outcome.",
		"workload", "outcome")
	r.scheduledFires = counter("mast_scheduled_fires_total",
		"Scheduled-trigger ticks, by what became of them (ran, skipped, error, missed).",
		"workload", "outcome")
	r.monitorNotifies = counter("mast_monitor_notifications_total",
		"Monitoring cycles by what they told the chat ingress (posted, appended, replaced, rolled, quiet, health, error).",
		"workload", "outcome")
	r.monitorDigests = counter("mast_monitor_digest_wakes_total",
		"Monitoring cycles that spoke because the notify deadman expired rather than because anything changed.",
		"workload")

	// A2A server (#78). Task lifecycle outcomes for inbound A2A tasks
	// driven through the runTurnPre chokepoint (Stage B) plus cancels
	// routed to the abort machinery (Stage A). Low-cardinality: bounded
	// outcome vocabulary, no task/session ID.
	r.a2aTasks = counter("mast_a2a_server_tasks_total",
		"A2A server task lifecycle transitions, by workload and outcome.",
		"workload", "outcome")

	// AG-UI server (#84). Run outcomes + wallclock duration for inbound
	// AG-UI runs driven through the runTurnPre chokepoint. Low-cardinality:
	// bounded outcome vocabulary, no thread/run/session ID.
	r.aguiRuns = counter("mast_agui_runs_total",
		"AG-UI server run outcomes, by workload and outcome.",
		"workload", "outcome")
	r.aguiRunDur = histogram("mast_agui_run_duration_seconds",
		"AG-UI server run wallclock duration in seconds, by workload.",
		[]float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
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
	for _, outcome := range []string{OutcomeOK, OutcomeError, OutcomeBudgetExceeded, OutcomeWatchdogHalt} {
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
	for _, outcome := range autoResumeOutcomes {
		r.autoResume.WithLabelValues(workload, outcome)
	}
	for _, op := range []string{MarkerOpMark, MarkerOpClear, MarkerOpPause} {
		r.markerFailures.WithLabelValues(workload, op)
	}
	r.aborts.WithLabelValues(workload)
	for _, src := range []string{GatePauseOperator, GatePausePlannedStop} {
		r.gatePauses.WithLabelValues(workload, src)
	}
	for _, outcome := range []string{TimedPauseResumed, TimedPauseSkipped, TimedPauseError} {
		r.timedPauseFires.WithLabelValues(workload, outcome)
	}
	for _, outcome := range []string{ScheduledFireRan, ScheduledFireSkipped, ScheduledFireError, ScheduledFireMissed} {
		r.scheduledFires.WithLabelValues(workload, outcome)
	}
	for _, outcome := range monitorNotifyOutcomes {
		r.monitorNotifies.WithLabelValues(workload, outcome)
	}
	r.monitorDigests.WithLabelValues(workload)
	for _, outcome := range a2aTaskOutcomes {
		r.a2aTasks.WithLabelValues(workload, outcome)
	}
	for _, outcome := range aguiRunOutcomes {
		r.aguiRuns.WithLabelValues(workload, outcome)
	}
	r.aguiRunDur.WithLabelValues(workload)
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

// AutoResume records one boot-time auto-resume decision with the given
// outcome (one of the AutoResume* constants).
func (r *Registry) AutoResume(workload, outcome string) {
	if r == nil {
		return
	}
	r.autoResume.WithLabelValues(workload, outcome).Inc()
}

// MarkerWriteFailure records a failed interruption-marker store write
// during the shutdown drain, by operation (one of the MarkerOp*
// constants). A non-zero count is an alert condition: a marker that
// did not land means a restart may never surface the interrupted turn.
func (r *Registry) MarkerWriteFailure(workload, operation string) {
	if r == nil {
		return
	}
	r.markerFailures.WithLabelValues(workload, operation).Inc()
}

// Abort records a terminal abort durably recorded through the operator
// surface (the marker write succeeded; the in-flight turn is swept
// separately).
func (r *Registry) Abort(workload string) {
	if r == nil {
		return
	}
	r.aborts.WithLabelValues(workload).Inc()
}

// GatePause records a gate pause durably recorded, by source (one of
// the GatePause* constants: an operator request or a planned stop).
func (r *Registry) GatePause(workload, source string) {
	if r == nil {
		return
	}
	r.gatePauses.WithLabelValues(workload, source).Inc()
}

// TimedPauseFire records one timed-pause scheduler fire with the given
// outcome (one of the TimedPause* constants).
func (r *Registry) TimedPauseFire(workload, outcome string) {
	if r == nil {
		return
	}
	r.timedPauseFires.WithLabelValues(workload, outcome).Inc()
}

// ScheduledFire records one scheduled-trigger tick with the given
// outcome (one of the ScheduledFire* constants). Called once per tick,
// including the ticks that produced no run at all.
func (r *Registry) ScheduledFire(workload, outcome string) {
	if r == nil {
		return
	}
	r.scheduledFires.WithLabelValues(workload, outcome).Inc()
}

// MonitorNotify records what one monitoring cycle did about telling
// somebody (one of the MonitorNotify* constants). Called once per cycle
// that reached the egress leg, including the quiet ones.
func (r *Registry) MonitorNotify(workload, outcome string) {
	if r == nil {
		return
	}
	r.monitorNotifies.WithLabelValues(workload, outcome).Inc()
}

// MonitorDigestWake records a cycle that spoke because the notify
// deadman expired rather than because the classification changed. Its
// own family rather than a notification outcome: the operator question
// it answers is "has this workload been silent long enough that I had
// to prove I was alive", which is not the same question as how the
// message was delivered.
func (r *Registry) MonitorDigestWake(workload string) {
	if r == nil {
		return
	}
	r.monitorDigests.WithLabelValues(workload).Inc()
}

// A2ATask records one A2A server task lifecycle transition with the
// given outcome (one of the A2ATask* constants; a pkg/a2a TaskState
// value). Safe on a nil *Registry.
func (r *Registry) A2ATask(workload, outcome string) {
	if r == nil {
		return
	}
	r.a2aTasks.WithLabelValues(workload, outcome).Inc()
}

// AGUIRun records one AG-UI server run outcome (one of the AGUIRun*
// constants; a pkg/agui outcome value). Safe on a nil *Registry.
func (r *Registry) AGUIRun(workload, outcome string) {
	if r == nil {
		return
	}
	r.aguiRuns.WithLabelValues(workload, outcome).Inc()
}

// AGUIRunDuration records one AG-UI run's wallclock duration in seconds.
// Safe on a nil *Registry; negative durations are ignored.
func (r *Registry) AGUIRunDuration(workload string, seconds float64) {
	if r == nil || seconds < 0 {
		return
	}
	r.aguiRunDur.WithLabelValues(workload).Observe(seconds)
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
