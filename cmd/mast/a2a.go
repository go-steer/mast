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

// A2A-server wiring for the daemon (docs/a2a-design.md, "Mast as A2A
// server"). pkg/a2a owns the wire protocol, auth, and HTTP surface but
// never imports the runtime; this file supplies the Backend seam —
// GetTask over the transcript store's state projection and CancelTask
// over the same abort machinery the /abort door uses — and projects the
// bundle's a2a: section into the exposed skills.
//
// Stage A serves the card, tasks/get, and tasks/cancel behind pluggable
// bearer auth. message/send (turn execution through runTurnPre) is Stage
// B; message/stream is Stage C.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"log/slog"

	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	buildversion "github.com/go-steer/mast/internal/version"
	"github.com/go-steer/mast/pkg/a2a"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// a2aBackend implements a2a.Backend over the daemon's transcript store,
// abort machinery, and — for message/send (Stage B) — the same
// runTurnPre chokepoint every other turn kind funnels through. A task id
// IS a mast session id; there is one exposed workload per single-session
// daemon (Stage A), so every task resolves to workloadName for the scope
// check.
type a2aBackend struct {
	store        *transcript.Store
	obs          *observability.Registry
	tracker      *turnTracker
	logger       *slog.Logger
	workloadName string

	// message/send (Stage B) execution seams — the same objects the
	// inject/attach/resume paths thread into runTurnPre.
	r         *runner.Runner
	meters    *meterPool
	wds       *watchdogPool
	turnLocks *sessionTurnLocks
	bundle    *workload.Bundle
	reg       *taskRegistry
}

// taskRegistry is the in-process record of A2A task outcomes. A
// transcript-only read cannot prove a turn finished — an idle session is
// indistinguishable from "completed" versus "about to run" in the event
// log — so message/send records the terminal state and the agent's
// answer here. GetTask consults it first and falls back to the
// transcript projection for tasks this process did not run (e.g. after a
// restart). Grows one entry per task, consistent with meterPool /
// watchdogPool at v0.2 single-instance scale; bounded eviction is a
// follow-on.
type taskRegistry struct {
	mu   sync.Mutex
	byID map[string]taskRecord
}

// taskRecord is one task's in-process snapshot.
type taskRecord struct {
	workload  string
	state     a2a.TaskState
	message   string
	output    string
	contextID string
}

func newTaskRegistry() *taskRegistry {
	return &taskRegistry{byID: map[string]taskRecord{}}
}

func (tr *taskRegistry) set(id string, rec taskRecord) {
	tr.mu.Lock()
	tr.byID[id] = rec
	tr.mu.Unlock()
}

// record writes a task record while preserving the cancel-wins invariant:
// once CancelTask has moved a record to Canceled, a racing SubmitMessage
// write must not clobber it. Without this, a turn that completes exactly as
// a tasks/cancel lands could resurrect "completed" — leaking the model's
// answer as a result artifact — and defeat the cancel. A Canceled write
// always lands (cancel is authoritative and idempotent); any other state
// is dropped when the current record is already Canceled. Used for both the
// in-flight marker and the terminal write so ordering never matters.
func (tr *taskRegistry) record(id string, rec taskRecord) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if rec.state != a2a.TaskStateCanceled {
		if cur, ok := tr.byID[id]; ok && cur.state == a2a.TaskStateCanceled {
			return
		}
	}
	tr.byID[id] = rec
}

// clearInFlight drops a non-terminal in-flight record so GetTask falls back
// to the transcript, which is authoritative for working / input-required /
// paused / aborted state. A terminal record (a racing cancel, or a prior
// completion on a continued task) is left untouched — dropping a cancel
// here would resurrect the pre-cancel state.
func (tr *taskRegistry) clearInFlight(id string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if cur, ok := tr.byID[id]; ok && !cur.state.Terminal() {
		delete(tr.byID, id)
	}
}

func (tr *taskRegistry) get(id string) (taskRecord, bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	rec, ok := tr.byID[id]
	return rec, ok
}

// turnCapture accumulates the final assistant answer and any HITL-pause
// signal from a turn's event stream, for building the A2A reply. Its
// onEvent runs synchronously inside runTurnPre's event loop, on the same
// goroutine as SubmitMessage, so it needs no locking.
type turnCapture struct {
	lastText      string
	inputRequired bool
	interruptMsg  string
}

func (c *turnCapture) onEvent(ev *session.Event) {
	if ev == nil {
		return
	}
	if ev.RequestedInput != nil {
		c.inputRequired = true
		if ev.RequestedInput.Message != "" {
			c.interruptMsg = ev.RequestedInput.Message
		}
	}
	if len(ev.LongRunningToolIDs) > 0 {
		c.inputRequired = true
	}
	// Capture the last model-authored text; StreamingModeNone emits one
	// complete event per model response, so the final such event is the
	// answer.
	if ev.Content == nil || ev.Content.Role != genai.RoleModel {
		return
	}
	var sb strings.Builder
	for _, part := range ev.Content.Parts {
		if part != nil && part.Text != "" {
			sb.WriteString(part.Text)
		}
	}
	if sb.Len() > 0 {
		c.lastText = sb.String()
	}
}

// A2A id namespaces. Task id == session id, so the "a2a-" prefix both
// keeps a minted task clear of the inject "incident-" namespace and marks
// the task as one this server owns (see isA2ATaskID). "ctx-" prefixes a
// server-minted contextId.
const (
	a2aTaskPrefix    = "a2a-"
	a2aContextPrefix = "ctx-"
)

// mintID returns a fresh crypto-random id under the given namespace prefix.
func mintID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("a2a: crypto/rand unavailable: %v", err))
	}
	return prefix + hex.EncodeToString(b[:])
}

// mintTaskID returns a fresh, reserved-safe task/session id for a new
// A2A task.
func mintTaskID() string { return mintID(a2aTaskPrefix) }

// mintContextID returns a fresh contextId to group a task's messages when
// the client does not supply one (A2A v0.3 has the server assign it).
func mintContextID() string { return mintID(a2aContextPrefix) }

// isA2ATaskID reports whether id names a task/session this A2A server owns
// — one mintTaskID produced. The A2A surface addresses only tasks it
// minted, so a scope-holding caller cannot read, cancel, or (via
// message/send) drive a turn into a session owned by another surface
// (inject "incident-*", attach, autoresume) by presenting that session's
// id as a task id. Reserved ops-row ids are excluded belt-and-suspenders.
func isA2ATaskID(id string) bool {
	return strings.HasPrefix(id, a2aTaskPrefix) && !transcript.IsReservedSessionID(id)
}

// GetTask snapshots a task's state from the session's log-proven state.
// A transcript-only read never reports "completed" — the event log
// cannot prove a turn finished versus is mid-flight — so idle sessions
// map to "working" (the in-process task registry supplies "completed" in
// Stage B).
func (b *a2aBackend) GetTask(ctx context.Context, taskID string) (a2a.TaskInfo, error) {
	if !isA2ATaskID(taskID) {
		return a2a.TaskInfo{}, a2a.ErrTaskNotFound
	}
	// The in-process registry is authoritative for tasks this process ran
	// — it alone can prove "completed" / "failed", which the event log
	// cannot. Fall back to the transcript projection for tasks not in the
	// registry (e.g. after a restart).
	if b.reg != nil {
		if rec, ok := b.reg.get(taskID); ok {
			return a2a.TaskInfo{
				WorkloadName:  rec.workload,
				State:         rec.state,
				StatusMessage: rec.message,
				Output:        rec.output,
				ContextID:     rec.contextID,
			}, nil
		}
	}
	d, err := b.store.Get(ctx, "", taskID)
	if err != nil {
		if errors.Is(err, transcript.ErrNotFound) {
			return a2a.TaskInfo{}, a2a.ErrTaskNotFound
		}
		return a2a.TaskInfo{}, err
	}
	state, msg := mapTranscriptState(d)
	return a2a.TaskInfo{WorkloadName: b.workloadName, State: state, StatusMessage: msg}, nil
}

// SubmitMessage runs a message/send turn through runTurnPre — inheriting
// the turn lock, cancel registry, abort/gate-pause refusal, budget
// meter, watchdog, and effects outbox by construction — and projects the
// outcome onto the A2A task lifecycle. Execution is synchronous.
func (b *a2aBackend) SubmitMessage(ctx context.Context, p a2a.SubmitParams) (string, a2a.TaskInfo, error) {
	// Drain gate: refuse new work once shutdown has begun, mirroring the
	// inject handler (a queued turn cut mid-drain is refused in runTurnPre).
	if b.tracker.isDraining() {
		return "", a2a.TaskInfo{}, fmt.Errorf("a2a: server draining, not accepting new tasks: %w", a2a.ErrUnavailable)
	}

	taskID := p.TaskID
	switch {
	case taskID == "":
		taskID = mintTaskID()
	case !isA2ATaskID(taskID):
		// A continuation may only target a task this server minted; a caller
		// must not be able to drive a turn into another surface's session
		// (inject "incident-*", attach, autoresume) by presenting its id.
		return "", a2a.TaskInfo{}, a2a.ErrTaskNotFound
	}

	// Resolve the contextId grouping this task's messages: honor a
	// caller-supplied one, inherit a continued task's, else mint one (A2A
	// v0.3 has the server assign contextId when the client omits it).
	contextID := p.ContextID
	if contextID == "" {
		if b.reg != nil {
			if rec, ok := b.reg.get(taskID); ok && rec.contextID != "" {
				contextID = rec.contextID
			}
		}
		if contextID == "" {
			contextID = mintContextID()
		}
	}

	// Mark the task in-flight so a concurrent tasks/get reports "working",
	// without clobbering a cancel that raced in ahead of the marker.
	if b.reg != nil {
		b.reg.record(taskID, taskRecord{workload: b.workloadName, state: a2a.TaskStateWorking, contextID: contextID})
	}

	// Same wallclock ceiling as the inject/resume paths (#47).
	if b.bundle != nil && b.bundle.Budget.MaxWallclockSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(b.bundle.Budget.MaxWallclockSeconds)*time.Second)
		defer cancel()
	}

	msg := genai.NewContentFromText(p.Text, genai.RoleUser)
	var cap turnCapture
	err := runTurnPre(ctx, b.r, b.logger, b.store, b.meters, b.wds, b.obs, b.tracker, b.turnLocks,
		b.workloadName, taskID, msg, "a2a:message/send", nil, cap.onEvent)

	// Drain can begin after the pre-check while we are blocked on the turn
	// lock; runTurnPre then returns ErrUnavailable. Surface it as retryable
	// (-32000) rather than folding it into a failed task, and drop the
	// in-flight marker so a concurrent tasks/get does not observe a phantom
	// "working".
	if de := drainErr(err); de != nil {
		if b.reg != nil {
			b.reg.clearInFlight(taskID)
		}
		return "", a2a.TaskInfo{}, de
	}

	state, statusMsg, output := b.classifyTurn(ctx, taskID, &cap, err)
	if b.reg != nil {
		if state.Terminal() {
			b.reg.record(taskID, taskRecord{
				workload:  b.workloadName,
				state:     state,
				message:   statusMsg,
				output:    output,
				contextID: contextID,
			})
		} else {
			// Gate-paused (working) or HITL (input-required): the transcript
			// is authoritative for non-terminal state, so drop the in-flight
			// marker rather than pin a stale record that would shadow the
			// transcript after the gate clears.
			b.reg.clearInFlight(taskID)
		}
		// Return the registry's authoritative view, not the local snapshot: a
		// tasks/cancel that raced our terminal write wins (record keeps
		// Canceled sticky), so the synchronous reply must report canceled with
		// no artifact rather than leak this turn's answer. On the normal
		// terminal path the registry holds exactly what we just wrote; on the
		// non-terminal path clearInFlight removed the record and we fall
		// through to the local snapshot below.
		if rec, ok := b.reg.get(taskID); ok {
			return taskID, a2a.TaskInfo{
				WorkloadName:  rec.workload,
				State:         rec.state,
				StatusMessage: rec.message,
				Output:        rec.output,
				ContextID:     rec.contextID,
			}, nil
		}
	}
	return taskID, a2a.TaskInfo{
		WorkloadName:  b.workloadName,
		State:         state,
		StatusMessage: statusMsg,
		Output:        output,
		ContextID:     contextID,
	}, nil
}

// drainErr maps a runTurnPre drain refusal onto the A2A retryable
// sentinel. runTurnPre returns inject.ErrUnavailable when drain begins
// after SubmitMessage's pre-check while the turn is blocked on the
// per-session lock (main.go). That path shares SubmitMessage's isDraining
// predicate, so it fires only in the narrow post-pre-check race — but when
// it does the caller deserves a retryable -32000, not a permanent failed
// task. Returns nil for every other error so the caller falls through to
// classifyTurn (a chokepoint conflict is aborted/paused, classifyTurn's
// job; anything else is a genuine failure).
func drainErr(err error) error {
	if errors.Is(err, inject.ErrUnavailable) {
		return fmt.Errorf("a2a: server draining, task not accepted: %w", a2a.ErrUnavailable)
	}
	return nil
}

// classifyTurn maps a runTurnPre outcome onto the A2A task lifecycle.
// A nil error is completed (or input-required if the turn paused for
// HITL); a chokepoint conflict (aborted / gate-paused) projects the
// session's durable state; any other error is failed.
func (b *a2aBackend) classifyTurn(ctx context.Context, taskID string, cap *turnCapture, err error) (a2a.TaskState, string, string) {
	switch {
	case err == nil:
		if cap.inputRequired {
			return a2a.TaskStateInputRequired, cap.interruptMsg, ""
		}
		return a2a.TaskStateCompleted, "", cap.lastText
	case errors.Is(err, inject.ErrConflict):
		// The chokepoint refused the turn: the session is aborted or
		// gate-paused. Project its durable state rather than "failed".
		if d, gerr := b.store.Get(ctx, "", taskID); gerr == nil {
			st, m := mapTranscriptState(d)
			return st, m, ""
		}
		return a2a.TaskStateFailed, err.Error(), ""
	default:
		// Runner error, budget trip, wallclock/ctx cancellation.
		return a2a.TaskStateFailed, err.Error(), ""
	}
}

// CancelTask routes to the same terminal-abort path the /abort door
// uses: marker first (the durable truth), then sweep the in-flight
// turn's cancel handle. Idempotent — a second cancel of an
// already-aborted session succeeds and re-reports canceled.
func (b *a2aBackend) CancelTask(ctx context.Context, taskID, reason string) (a2a.TaskInfo, error) {
	if !isA2ATaskID(taskID) {
		return a2a.TaskInfo{}, a2a.ErrTaskNotFound
	}
	err := recordAbort(ctx, b.store, b.obs, b.workloadName, taskID, reason)
	switch {
	case err == nil:
		// New abort landed; sweep any in-flight turn.
		if b.tracker.cancelSession(taskID) {
			b.logger.Info("a2a tasks/cancel cancelled in-flight turn", "task", taskID)
		}
	case errors.Is(err, transcript.ErrAlreadyAborted):
		// Idempotent: already terminal, nothing to sweep.
	case errors.Is(err, transcript.ErrNotFound):
		return a2a.TaskInfo{}, a2a.ErrTaskNotFound
	default:
		return a2a.TaskInfo{}, err
	}
	// Re-read for the resulting snapshot. If the read fails after a
	// successful marker write, report canceled synthetically — the abort
	// is the durable truth regardless of a transient read error.
	d, gerr := b.store.Get(ctx, "", taskID)
	state, msg := a2a.TaskStateCanceled, reason
	if gerr == nil {
		state, msg = mapTranscriptState(d)
	}
	// Keep the in-process registry consistent: GetTask reads it first, so
	// a cancel of a task this process ran (or is running) must overwrite
	// its record, otherwise a stale "completed"/"working" would shadow the
	// cancel. record() makes a Canceled write authoritative, so this wins
	// even against a message/send turn that finalizes concurrently.
	if b.reg != nil {
		b.reg.record(taskID, taskRecord{workload: b.workloadName, state: state, message: msg})
	}
	return a2a.TaskInfo{WorkloadName: b.workloadName, State: state, StatusMessage: msg}, nil
}

// mapTranscriptState projects mast's log-proven session state onto the
// A2A task lifecycle (docs/a2a-design.md "State mapping"). It never
// returns "completed": the transcript cannot prove a turn finished.
func mapTranscriptState(d *transcript.Detail) (a2a.TaskState, string) {
	switch d.State {
	case transcript.StateAborted:
		return a2a.TaskStateCanceled, d.AbortReason
	case transcript.StatePaused:
		// A pending (unresolved) interrupt is a HITL request the task is
		// blocked on — input-required. A gate-only pause (timed/operator,
		// no pending interrupt) is still the daemon's own commitment —
		// working.
		if len(d.PendingInterruptIDs) > 0 {
			return a2a.TaskStateInputRequired, d.PauseMessage
		}
		return a2a.TaskStateWorking, d.PauseMessage
	case transcript.StateInterrupted:
		return a2a.TaskStateWorking, d.InterruptReason
	default: // StateIdle — cannot prove completed from the log alone.
		return a2a.TaskStateWorking, ""
	}
}

// a2aExposedSkills projects the bundle's a2a: section into the server's
// exposed-skill list. Empty (nil) when the workload does not opt in.
func a2aExposedSkills(bundle *workload.Bundle) []a2a.ExposedSkill {
	if bundle == nil || !bundle.A2A.Expose {
		return nil
	}
	skillName := bundle.A2A.SkillName
	if skillName == "" {
		skillName = bundle.Name
	}
	desc := bundle.A2A.SkillDescription
	if desc == "" {
		desc = bundle.Description
	}
	return []a2a.ExposedSkill{{
		WorkloadName: bundle.Name,
		SkillName:    skillName,
		Description:  desc,
		Scopes:       bundle.A2A.Auth.Scopes,
	}}
}

// a2aValidator builds the endpoint's token validator from MAST_A2A_TOKEN.
// Unset means unauthenticated (dev only), mirroring the inject door. The
// static principal carries the union of every exposed skill's scopes so
// it can invoke any of them.
func a2aValidator(logger *slog.Logger, skills []a2a.ExposedSkill) (a2a.TokenValidator, error) {
	token := os.Getenv("MAST_A2A_TOKEN")
	if token == "" {
		logger.Warn("MAST_A2A_TOKEN not set; A2A endpoint is unauthenticated (dev only)")
		return nil, nil
	}
	seen := map[string]bool{}
	var scopes []string
	for _, sk := range skills {
		for _, s := range sk.Scopes {
			if !seen[s] {
				seen[s] = true
				scopes = append(scopes, s)
			}
		}
	}
	return a2a.NewStaticBearerValidator(map[string]*a2a.Principal{
		token: {Subject: "mast-a2a-static", Scopes: scopes},
	})
}

// a2aRateLimiter builds the endpoint's rate limiter from MAST_A2A_RATE
// (requests/second per caller×workload) and MAST_A2A_BURST (bucket depth;
// defaults to ceil(rate), min 1). MAST_A2A_RATE unset means no rate
// limiting (nil), mirroring the auth seam's unset-means-off default. A set
// but malformed value fails startup (fail-fast) rather than silently
// disabling the limit.
func a2aRateLimiter(logger *slog.Logger) (a2a.RateLimiter, error) {
	raw := os.Getenv("MAST_A2A_RATE")
	if raw == "" {
		return nil, nil
	}
	perSecond, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil, fmt.Errorf("a2a: invalid MAST_A2A_RATE %q: %w", raw, err)
	}
	burst := int(math.Ceil(perSecond))
	if burst < 1 {
		burst = 1
	}
	if b := os.Getenv("MAST_A2A_BURST"); b != "" {
		burst, err = strconv.Atoi(b)
		if err != nil {
			return nil, fmt.Errorf("a2a: invalid MAST_A2A_BURST %q: %w", b, err)
		}
	}
	lim, err := a2a.NewTokenBucketLimiter(perSecond, burst)
	if err != nil {
		return nil, err
	}
	logger.Info("A2A rate limiting enabled", "rate_per_sec", perSecond, "burst", burst)
	return lim, nil
}

// buildA2AServer constructs the A2A server for the daemon, or (nil, nil)
// when no workload opts into A2A exposure (the server is simply not
// started). baseCtx is the daemon's turn lifetime.
func buildA2AServer(
	logger *slog.Logger,
	listen string,
	bundle *workload.Bundle,
	backend a2a.Backend,
	metric a2a.TaskMetric,
	baseCtx context.Context,
) (*a2a.Server, error) {
	skills := a2aExposedSkills(bundle)
	if len(skills) == 0 {
		logger.Info("A2A listener requested but no workload opts into A2A exposure (a2a.expose); A2A disabled")
		return nil, nil
	}
	validator, err := a2aValidator(logger, skills)
	if err != nil {
		return nil, err
	}
	limiter, err := a2aRateLimiter(logger)
	if err != nil {
		return nil, err
	}
	desc := ""
	if bundle != nil {
		desc = bundle.Description
	}
	return a2a.New(a2a.Config{
		Listen:          listen,
		Skills:          skills,
		Validator:       validator,
		Limiter:         limiter,
		Backend:         backend,
		CardName:        "mast",
		CardDescription: desc,
		CardVersion:     buildversion.Version,
		Metric:          metric,
		Logger:          logger,
		BaseContext:     baseCtx,
	})
}

// a2aListener binds the A2A server's listener eagerly so a bad bind
// address fails serve() at startup rather than in a background goroutine
// (mirrors buildAttach).
func a2aListener(listen string) (net.Listener, error) {
	return net.Listen("tcp", listen)
}
