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

// Package session is the operator-facing read/inspect surface over
// ADK's session store (docs/durable-execution-design.md, "Operator-facing
// surface"). It answers the questions durability raises for an operator:
// which sessions exist, which are paused waiting for input, on what
// interrupt, and with what response schema.
//
// The pause model it inspects is the one verified in spike 2
// (docs/spike-findings.md) and extended by the v0.2 pause/abort design
// (docs/durable-execution-design.md, "The v0.2 pause/abort mechanics"):
// a paused session carries a pending interrupt — an event with a
// non-nil RequestedInput, OR an unanswered LongRunningToolIDs entry (a
// long-running tool park: request_operator_input, pause_session; both
// spellings of ADK's one pause primitive) — or an active gate-pause
// record on its ops row. The matching resume for an interrupt is a
// later user turn whose FunctionResponse.ID equals the interrupt ID.
// The LongRunningToolIDs source was added by the v0.2 design's
// adversarial gate (finding H1): without it, v0.1 planner parks
// projected idle/interrupted — invisible to operators and, worse,
// auto-resume candidates.
//
// State labels are derived strictly from what the store can prove:
//
//   - StatePaused:  at least one pending (unresolved) interrupt, or an
//     active gate pause (Store.PauseGate).
//   - StateAborted: an operator abort marker is present (written by
//     Store.Abort to the session's companion ops row — see opsSuffix;
//     v0.1.0 markers in the primary row's state are still honored).
//   - StateInterrupted: a daemon shutdown cut a turn short — the daemon
//     wrote an interruption marker before draining (Store.MarkInterrupted)
//     and no clean completion cleared it (Store.ClearInterrupted). This
//     is still strictly log-proven: the process that WAS running the
//     turn recorded the fact durably before it stopped.
//   - StateIdle:    everything else. The store cannot distinguish "a turn
//     is in flight right now" from "the last turn completed" — that is
//     in-process runner state, not event-log state — so this package
//     deliberately does not claim "running" or "completed".
//
// Precedence: aborted > paused > interrupted > idle.
package transcript

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/genai"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/eventlog"
)

// Session states derived from the event log. See the package doc for
// why there is no "running" or "completed".
const (
	StatePaused      = "paused"
	StateAborted     = "aborted"
	StateInterrupted = "interrupted"
	StateIdle        = "idle"
)

// Session-state keys written by Store.Abort. Unprefixed, so they land
// in per-session state (not app:/user: scope) and survive in the same
// store the runner reads.
const (
	abortReasonKey = "mast_abort_reason"
	abortTimeKey   = "mast_abort_time"
)

// Session-state keys written by Store.MarkInterrupted and cleared (set
// to the empty string, not deleted — last-write-wins is the only state
// semantic ADK guarantees) by Store.ClearInterrupted.
const (
	interruptReasonKey = "mast_interrupted_reason"
	interruptTimeKey   = "mast_interrupted_time"
)

// Session-state keys written by Store.AckEffects: the operator's
// acknowledgement watermark for ambiguous prior effects
// (docs/durable-execution-design.md, "Recorded-effect outbox").
// Dangling mutating tool calls at or before the watermark no longer
// trip the outbox's ambiguous-effect mode. Deliberately NOT part of
// the state ladder in project() — an ack changes what the next turn
// may do, not what the session is.
const (
	effectsAckTimeKey   = "mast_effects_ack_time"
	effectsAckReasonKey = "mast_effects_ack_reason"
)

// Session-state keys written by Store.RecordAutoResumeAttempt (and
// blanked by Store.ClearAutoResumeAttempts): the per-session restart-loop
// breaker for boot-time auto-resume (cmd/mast, #41). Like the other
// markers they live on the companion ops row and are NOT part of the
// project() state ladder — an attempt count changes whether the daemon
// will try again, not what the session is.
const (
	autoResumeAttemptsKey = "mast_autoresume_attempts"
	autoResumeLastKey     = "mast_autoresume_last"
)

// opsSuffix derives the companion ops row's session ID from the
// primary's. All operator/daemon marker writes (abort, interruption)
// go to the ops row, NEVER to the primary session row, because ADK's
// database session service enforces optimistic concurrency: a session
// handle is a write lease, and any out-of-band append to a row
// invalidates every other holder's handle — the runner's next
// AppendEvent on a live turn fails with "stale session error"
// (adk/v2 session/database/service.go; core-agent hit the identical
// failure with subagents writing to the parent's row and fixed it the
// same way, with derived session IDs — see their
// docs/eventlog-decisions.md). The suffix is reserved: List hides ops
// rows, and a real session must not use an ID ending in it.
const opsSuffix = ":mast-ops"

func opsSessionID(sid string) string { return sid + opsSuffix }

// IsReservedSessionID reports whether sessionID names a companion ops
// row rather than a real session. Every surface that accepts a
// session ID must refuse reserved IDs (#56) — a runner turn driven
// into an ops row would hold its write lease and corrupt subsequent
// marker writes; a Get would present marker storage as a phantom
// session.
func IsReservedSessionID(sessionID string) bool {
	return strings.HasSuffix(sessionID, opsSuffix)
}

// errReserved builds the uniform rejection for reserved IDs. It wraps
// ErrNotFound so read paths (CLI show, daemon lookups) degrade to the
// same "no such session" handling an operator typo gets.
func errReserved(sessionID string) error {
	return fmt.Errorf("session ID %q uses the reserved ops-row suffix %q: %w", sessionID, opsSuffix, ErrNotFound)
}

// ErrNotFound reports that no session with the requested ID exists in
// the store (under the store's app name).
var ErrNotFound = errors.New("session not found")

// ErrAlreadyAborted reports that an abort marker is already present.
var ErrAlreadyAborted = errors.New("session already aborted")

// PendingInput is a pending interrupt — a RequestedInput, or a
// long-running tool park — that has not been resolved by a later
// matching FunctionResponse. It carries everything an operator needs
// to script a resume.
type PendingInput struct {
	// InterruptID is the resume correlation key: the resume turn's
	// FunctionResponse.ID must equal it (spike-2 verified contract).
	InterruptID string `json:"interrupt_id"`
	// Message is the human-readable prompt from the pausing node (for
	// long-running parks: the tool call's "message" argument, if any).
	Message string `json:"message,omitempty"`
	// LongRunning marks a long-running tool park (pause_session,
	// request_operator_input) rather than a RequestedInput. The resume
	// wire shape is identical either way.
	LongRunning bool `json:"long_running,omitempty"`
	// ToolName is the parked tool's name (long-running parks only).
	ToolName string `json:"tool_name,omitempty"`
	// Author is the agent that raised the interrupt.
	Author string `json:"author,omitempty"`
	// RaisedAt is the timestamp of the pausing event.
	RaisedAt time.Time `json:"raised_at"`
	// ResponseSchema, when non-nil, is the JSON schema the resume
	// response payload must conform to.
	ResponseSchema *jsonschema.Schema `json:"response_schema,omitempty"`
	// Payload is optional context the pausing node attached.
	Payload any `json:"payload,omitempty"`
}

// Summary is the list-view projection of one session.
type Summary struct {
	ID            string    `json:"id"`
	AppName       string    `json:"app_name"`
	UserID        string    `json:"user_id"`
	LastEventTime time.Time `json:"last_event_time"`
	State         string    `json:"state"`
	// PendingInterruptIDs are the unresolved interrupt IDs (empty
	// unless State is StatePaused).
	PendingInterruptIDs []string `json:"pending_interrupt_ids,omitempty"`
	// AbortReason is set when State is StateAborted.
	AbortReason string `json:"abort_reason,omitempty"`
	// InterruptReason is set when State is StateInterrupted: the reason
	// recorded by the daemon whose shutdown cut the session's turn short.
	InterruptReason string `json:"interrupt_reason,omitempty"`
	// InterruptedAt is set when State is StateInterrupted: when the daemon
	// recorded the interruption (from interruptTimeKey). Drives the
	// auto-resume freshness window (cmd/mast, #41).
	InterruptedAt time.Time `json:"interrupted_at,omitempty"`
	// PauseReason / PauseMessage are set when an active gate pause
	// contributes to StatePaused (Store.PauseGate).
	PauseReason  string `json:"pause_reason,omitempty"`
	PauseMessage string `json:"pause_message,omitempty"`
}

// Detail is the show-view projection: Summary plus event count, the
// full pending-interrupt records, the active gate pause if any, and any
// operator edits that were applied to a mutating call.
type Detail struct {
	Summary
	EventCount int            `json:"event_count"`
	Pending    []PendingInput `json:"pending,omitempty"`
	GatePause  *PauseRecord   `json:"gate_pause,omitempty"`
	// AppliedEdits are the calls an operator rewrote before they ran,
	// oldest first. They are projected from state rather than read off
	// the transcript because the transcript cannot answer the question:
	// ADK re-fires a parked call verbatim, so the durable FunctionCall
	// part records the arguments the *model* proposed while the response
	// beside it is the result of running the *operator's*
	// (pkg/approval.AppliedEdit).
	AppliedEdits []approval.AppliedEdit `json:"applied_edits,omitempty"`
}

// Store wraps an ADK session.Service with the operator-facing
// projections. It works over any Service implementation — the SQLite /
// Postgres database service and the in-memory service alike.
type Store struct {
	svc     adksession.Service
	appName string

	// opsMu serializes ALL ops-row writes through this store (#64):
	// appendOpsDelta is a Get-then-Append read-modify-write, and two
	// concurrent writers (an operator Abort landing during the
	// shutdown pre-mark pass) otherwise collide on the row's write
	// lease. The daemon routes every marker writer through one Store.
	opsMu sync.Mutex
}

// NewStore wraps an already-open session service (the path the daemon
// uses: same service instance the runner writes through).
func NewStore(svc adksession.Service, appName string) *Store {
	return &Store{svc: svc, appName: appName}
}

// Open opens the SQLite session DB at path read-through (the path the
// CLI uses: `mast sessions list/show --session-db=...` against a DB a
// daemon owns or owned). The file must already exist — opening a
// missing path would silently create an empty store and report zero
// sessions for what is actually an operator typo.
func Open(path, appName string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("session db %q: %w", path, err)
	}
	// Same storage hardening as every other SQLite construction
	// (busy_timeout + WAL + write serialization; GORM logger silent
	// inside): the CLI is read-mostly but AutoMigrate writes, and a
	// list/show racing a live daemon should wait on the lock, not
	// fail (#64). eventlog.OpenSessionService is the shared one-call
	// building block.
	svc, err := eventlog.OpenSessionService(context.Background(), sqlite.Open(path))
	if err != nil {
		return nil, fmt.Errorf("open session db %q: %w", path, err)
	}
	return NewStore(svc, appName), nil
}

// List returns summaries for all sessions under the store's app name,
// most recent last-event first. userID narrows to one user; empty
// lists all users.
//
// Note: ADK's Service.List returns sessions without events, and paused
// state is an event-log property, so List issues one Get per session.
// Fine at operator-CLI scale; a paged/indexed path is a v0.2+ concern
// alongside the eventlog query surface (docs/fork-design.md P1.3).
func (s *Store) List(ctx context.Context, userID string) ([]Summary, error) {
	resp, err := s.svc.List(ctx, &adksession.ListRequest{AppName: s.appName, UserID: userID})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	summaries := make([]Summary, 0, len(resp.Sessions))
	for _, sess := range resp.Sessions {
		// Companion ops rows are marker storage, not sessions; their
		// content surfaces through the primary's projection.
		if strings.HasSuffix(sess.ID(), opsSuffix) {
			continue
		}
		d, err := s.Get(ctx, sess.UserID(), sess.ID())
		if err != nil {
			return nil, fmt.Errorf("inspect session %q: %w", sess.ID(), err)
		}
		summaries = append(summaries, d.Summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if !summaries[i].LastEventTime.Equal(summaries[j].LastEventTime) {
			return summaries[i].LastEventTime.After(summaries[j].LastEventTime)
		}
		return summaries[i].ID < summaries[j].ID
	})
	return summaries, nil
}

// Get returns the detail view for one session. An empty userID is
// resolved by scanning List for the session ID (the CLI knows session
// IDs, not the daemon-internal user ID). Reserved ops-row IDs are
// refused as not-found (#56).
func (s *Store) Get(ctx context.Context, userID, sessionID string) (*Detail, error) {
	if IsReservedSessionID(sessionID) {
		return nil, errReserved(sessionID)
	}
	if userID == "" {
		var err error
		userID, err = s.findUserID(ctx, sessionID)
		if err != nil {
			return nil, err
		}
	}
	resp, err := s.svc.Get(ctx, &adksession.GetRequest{
		AppName:   s.appName,
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("get session %q: %w", sessionID, err)
	}
	markers, pauses := s.opsSnapshot(ctx, userID, sessionID)
	return project(resp.Session, markers, pauses[pauseGateKey]), nil
}

// Abort appends a durable operator-abort marker for the session.
//
// Semantics contract (minimal and honest — read before relying on it):
//
//   - Abort is a marker, not preemption. It does NOT cancel a turn that
//     is in flight in some daemon process; it appends an event whose
//     StateDelta records the abort reason and time in the session's
//     companion ops row (see opsSuffix — writing to the primary row
//     would invalidate a live runner handle and kill the turn, the
//     opposite of this contract; issue #46). The abort event is
//     therefore NOT part of the primary transcript the model sees —
//     previously incidental, since the daemon refuses resumes on
//     aborted sessions anyway.
//   - ADK's workflow reconstruction does not read the marker: as far as
//     the engine is concerned, a pending RequestedInput is still
//     resumable. It is mast's surface that treats the marker as
//     terminal — List/Get report StateAborted with pending interrupts
//     cleared, and the daemon's /resume handler refuses aborted
//     sessions (cmd/mast). A real engine-level terminal state is the
//     v0.2 programmatic-pause/abort work
//     (docs/durable-execution-design.md, Phasing).
//   - Idempotency: a second Abort returns ErrAlreadyAborted rather than
//     stacking markers. Legacy v0.1.0 abort markers (written to the
//     primary row's state) count.
func (s *Store) Abort(ctx context.Context, userID, sessionID, reason string) error {
	if IsReservedSessionID(sessionID) {
		return errReserved(sessionID)
	}
	if userID == "" {
		var err error
		userID, err = s.findUserID(ctx, sessionID)
		if err != nil {
			return err
		}
	}
	// Aborting a session that doesn't exist is an operator error
	// (ErrNotFound), so probe the primary read-only before writing.
	resp, err := s.svc.Get(ctx, &adksession.GetRequest{
		AppName:   s.appName,
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil {
		return fmt.Errorf("get session %q: %w", sessionID, err)
	}
	if _, err := resp.Session.State().Get(abortReasonKey); err == nil {
		return fmt.Errorf("session %q: %w", sessionID, ErrAlreadyAborted)
	}
	markers, pauses := s.opsSnapshot(ctx, userID, sessionID)
	if v, ok := markers[abortReasonKey]; ok && v != "" {
		return fmt.Errorf("session %q: %w", sessionID, ErrAlreadyAborted)
	}

	// Abort purges pause records (v0.2 pause/abort design, timed-pause
	// section): an aborted session holds no resumable tokens and no
	// live timers. Purged = blanked, so a purged token reads as
	// not-found rather than already-resumed.
	delta := map[string]any{
		abortReasonKey: reason,
		abortTimeKey:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	for key, rec := range pauses {
		if rec.Active() {
			delta[key] = ""
		}
	}
	return s.appendOpsDelta(ctx, userID, sessionID, "operator-abort", "operator",
		fmt.Sprintf("session aborted by operator: %s", reason),
		delta)
}

// MarkInterrupted appends a durable interrupted-by-shutdown marker for
// the session (docs/durable-execution-design.md, "Shutdown contract").
//
// The daemon writes it for every session with a turn in flight when a
// shutdown begins, BEFORE draining — so a SIGKILL mid-drain leaves the
// marker on disk — and clears it via ClearInterrupted when the turn
// completes inside the drain window. The marker lives in the
// companion ops row (see opsSuffix): writing it to the primary row
// would invalidate the live runner handle and kill the very turn
// being marked (issue #45). Like the abort marker it is state, not
// preemption: the engine ignores it, and a later turn on the session
// proceeds normally (reconstruct-and-re-execute); it exists so
// operators can see which sessions a restart cut short.
//
// The primary session need not exist yet (a turn interrupted before
// the runner's auto-create): the marker parks in the ops row and
// surfaces if/when the primary appears. userID must then be explicit —
// with userID == "" resolution scans primaries and returns
// ErrNotFound. Re-marking overwrites (last write wins) — a second
// shutdown racing the first is not worth an error.
func (s *Store) MarkInterrupted(ctx context.Context, userID, sessionID, reason string) error {
	if IsReservedSessionID(sessionID) {
		return errReserved(sessionID)
	}
	if reason == "" {
		return errors.New("mark interrupted: reason must be non-empty (the empty string is the cleared state)")
	}
	return s.appendOpsDelta(ctx, userID, sessionID, "shutdown-interrupt", "daemon",
		fmt.Sprintf("turn interrupted by daemon shutdown: %s", reason),
		map[string]any{
			interruptReasonKey: reason,
			interruptTimeKey:   time.Now().UTC().Format(time.RFC3339Nano),
		})
}

// ClearInterrupted resolves a MarkInterrupted marker after the turn
// completed inside the drain window. Clearing an unmarked session is a
// harmless no-op event (shutdown-path callers cannot atomically check).
func (s *Store) ClearInterrupted(ctx context.Context, userID, sessionID string) error {
	if IsReservedSessionID(sessionID) {
		return errReserved(sessionID)
	}
	return s.appendOpsDelta(ctx, userID, sessionID, "shutdown-interrupt-clear", "daemon",
		"turn completed within the shutdown drain window; interruption marker cleared",
		map[string]any{interruptReasonKey: "", interruptTimeKey: ""})
}

// AckEffects records the operator's acknowledgement of ambiguous prior
// effects: dangling mutating tool calls persisted at or before now stop
// tripping the recorded-effect outbox's ambiguous-effect mode
// (pkg/effects). The marker is a watermark, not a state — List/Get
// derivation ignores it. Re-acking overwrites (last write wins); the
// new watermark also covers intents the first ack already covered.
func (s *Store) AckEffects(ctx context.Context, userID, sessionID, reason string) error {
	if IsReservedSessionID(sessionID) {
		return errReserved(sessionID)
	}
	return s.appendOpsDelta(ctx, userID, sessionID, "effects-ack", "operator",
		fmt.Sprintf("operator acknowledged ambiguous prior effects: %s", reason),
		map[string]any{
			effectsAckTimeKey:   time.Now().UTC().Format(time.RFC3339Nano),
			effectsAckReasonKey: reason,
		})
}

// EffectsAckedAt returns the session's effects-acknowledgement
// watermark, if one was recorded. Read failures and missing markers
// both report false — the outbox then treats every dangling intent as
// unacknowledged, which is the fail-closed direction.
func (s *Store) EffectsAckedAt(ctx context.Context, userID, sessionID string) (time.Time, bool) {
	if IsReservedSessionID(sessionID) {
		return time.Time{}, false
	}
	if userID == "" {
		var err error
		userID, err = s.findUserID(ctx, sessionID)
		if err != nil {
			return time.Time{}, false
		}
	}
	v, ok := s.opsState(ctx, userID, sessionID)[effectsAckTimeKey]
	if !ok || v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// InterruptedCandidate is one session projected as StateInterrupted, with
// the material the daemon's boot-time auto-resume pass needs (cmd/mast,
// #41): the resume identity (SessionID/UserID), the freshness inputs
// (InterruptedAt), the supersession-recheck inputs (LastEventTime/
// EventCount), and the loaded Events for the effects dangling scan.
type InterruptedCandidate struct {
	SessionID       string
	UserID          string
	InterruptReason string
	InterruptedAt   time.Time
	LastEventTime   time.Time
	EventCount      int
	Events          adksession.Events
}

// ScanInterrupted returns every session under the store's app name that
// currently projects as StateInterrupted, oldest interruption first. It
// mirrors List (iterate primary rows, skip companion ops rows, Get+
// project each) rather than the ops-row scans (ScanPauses): interrupted
// state is a projection over the primary transcript plus the ops-row
// marker, so a candidate must be found the way List finds sessions.
//
// Like List it issues one Get per session; fine at single-instance
// operator scale (docs/fork-design.md P1.3 tracks an indexed path).
func (s *Store) ScanInterrupted(ctx context.Context) ([]InterruptedCandidate, error) {
	resp, err := s.svc.List(ctx, &adksession.ListRequest{AppName: s.appName})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	var out []InterruptedCandidate
	for _, sess := range resp.Sessions {
		if strings.HasSuffix(sess.ID(), opsSuffix) {
			continue
		}
		full, err := s.svc.Get(ctx, &adksession.GetRequest{
			AppName:   s.appName,
			UserID:    sess.UserID(),
			SessionID: sess.ID(),
		})
		if err != nil {
			return nil, fmt.Errorf("inspect session %q: %w", sess.ID(), err)
		}
		markers, pauses := s.opsSnapshot(ctx, sess.UserID(), sess.ID())
		d := project(full.Session, markers, pauses[pauseGateKey])
		if d.State != StateInterrupted {
			continue
		}
		out = append(out, InterruptedCandidate{
			SessionID:       sess.ID(),
			UserID:          sess.UserID(),
			InterruptReason: d.InterruptReason,
			InterruptedAt:   d.InterruptedAt,
			LastEventTime:   d.LastEventTime,
			EventCount:      d.EventCount,
			Events:          full.Session.Events(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].InterruptedAt.Equal(out[j].InterruptedAt) {
			return out[i].InterruptedAt.Before(out[j].InterruptedAt)
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out, nil
}

// AutoResumeAttempts reports how many boot-time auto-resume attempts have
// been recorded for the session and when the last one was stamped (#41
// restart-loop breaker). A missing or unparseable counter reads as zero —
// the fail-open direction for the breaker is to allow the attempt.
func (s *Store) AutoResumeAttempts(ctx context.Context, userID, sessionID string) (int, time.Time) {
	m := s.opsState(ctx, userID, sessionID)
	var n int
	if v, ok := m[autoResumeAttemptsKey]; ok && v != "" {
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			n = 0
		}
	}
	var last time.Time
	if v, ok := m[autoResumeLastKey]; ok && v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			last = t
		}
	}
	return n, last
}

// RecordAutoResumeAttempt durably increments the session's auto-resume
// attempt counter and stamps the attempt time, returning the new count.
// The daemon calls it BEFORE driving the continuation turn so an attempt
// that crashes the process (the exact restart-loop threat) is still
// counted (#41 M2). The boot pass is a single sequential goroutine, so
// the read-then-write needs no cross-call locking beyond appendOpsDelta's
// own ops-row serialization.
func (s *Store) RecordAutoResumeAttempt(ctx context.Context, userID, sessionID string) (int, error) {
	n, _ := s.AutoResumeAttempts(ctx, userID, sessionID)
	n++
	if err := s.appendOpsDelta(ctx, userID, sessionID, "autoresume-attempt", "daemon",
		fmt.Sprintf("boot-time auto-resume attempt %d", n),
		map[string]any{
			autoResumeAttemptsKey: fmt.Sprintf("%d", n),
			autoResumeLastKey:     time.Now().UTC().Format(time.RFC3339Nano),
		}); err != nil {
		return 0, err
	}
	return n, nil
}

// ClearAutoResumeAttempts blanks the attempt counter after a successful
// auto-resume, so a session that later interrupts again starts fresh.
func (s *Store) ClearAutoResumeAttempts(ctx context.Context, userID, sessionID string) error {
	return s.appendOpsDelta(ctx, userID, sessionID, "autoresume-clear", "daemon",
		"boot-time auto-resume succeeded; attempt counter cleared",
		map[string]any{autoResumeAttemptsKey: "", autoResumeLastKey: ""})
}

// appendOpsDelta appends one marker event to the session's companion
// ops row, creating the row on first use — the shared mechanics of the
// abort and interruption markers. It never touches the primary row
// (see opsSuffix for why that is load-bearing).
func (s *Store) appendOpsDelta(ctx context.Context, userID, sessionID, invocation, author, text string, delta map[string]any) error {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	if userID == "" {
		var err error
		userID, err = s.findUserID(ctx, sessionID)
		if err != nil {
			return err
		}
	}
	opsID := opsSessionID(sessionID)
	var sess adksession.Session
	if resp, err := s.svc.Get(ctx, &adksession.GetRequest{
		AppName: s.appName, UserID: userID, SessionID: opsID,
	}); err == nil {
		sess = resp.Session
	} else {
		created, cerr := s.svc.Create(ctx, &adksession.CreateRequest{
			AppName: s.appName, UserID: userID, SessionID: opsID,
		})
		if cerr != nil {
			// A racing writer may have created it between our Get and
			// Create; one retry settles it.
			retry, gerr := s.svc.Get(ctx, &adksession.GetRequest{
				AppName: s.appName, UserID: userID, SessionID: opsID,
			})
			if gerr != nil {
				return fmt.Errorf("open ops row for session %q: %w", sessionID, cerr)
			}
			sess = retry.Session
		} else {
			sess = created.Session
		}
	}
	ev := adksession.NewEvent(ctx, invocation)
	ev.Author = author
	ev.Content = genai.NewContentFromText(text, genai.RoleUser)
	for k, v := range delta {
		ev.Actions.StateDelta[k] = v
	}
	if err := s.svc.AppendEvent(ctx, sess, ev); err != nil {
		return fmt.Errorf("append %s event to ops row of session %q: %w", invocation, sessionID, err)
	}
	return nil
}

// opsState reads the companion ops row's scalar marker keys. A missing
// row — or any read failure — means "no markers": the ops row is an
// overlay, and a session without one is simply unmarked.
func (s *Store) opsState(ctx context.Context, userID, sessionID string) map[string]string {
	markers, _ := s.opsSnapshot(ctx, userID, sessionID)
	return markers
}

// opsSnapshot reads the ops row once and splits its state into scalar
// markers (every "mast_"-prefixed key that is not a pause record —
// enumeration, not a whitelist, so a new marker key can never be
// silently dropped again) and parsed pause records.
func (s *Store) opsSnapshot(ctx context.Context, userID, sessionID string) (map[string]string, map[string]*PauseRecord) {
	resp, err := s.svc.Get(ctx, &adksession.GetRequest{
		AppName: s.appName, UserID: userID, SessionID: opsSessionID(sessionID),
	})
	if err != nil {
		return nil, nil
	}
	markers := make(map[string]string)
	for k, v := range resp.Session.State().All() {
		if !strings.HasPrefix(k, "mast_") || k == pauseGateKey || strings.HasPrefix(k, pauseIntrKeyPrefix) {
			continue
		}
		markers[k] = fmt.Sprintf("%v", v)
	}
	return markers, parsePauseRecords(resp.Session, sessionID)
}

// findUserID resolves the user ID owning sessionID by scanning the
// app's session list. Session IDs are the operator-visible handle
// (e.g. "incident-<uid>"); the user ID is a daemon-internal constant
// the operator should not need to know.
func (s *Store) findUserID(ctx context.Context, sessionID string) (string, error) {
	resp, err := s.svc.List(ctx, &adksession.ListRequest{AppName: s.appName})
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	for _, sess := range resp.Sessions {
		if IsReservedSessionID(sess.ID()) {
			continue // ops rows are marker storage, never a lookup hit
		}
		if sess.ID() == sessionID {
			return sess.UserID(), nil
		}
	}
	return "", fmt.Errorf("session %q (app %q): %w", sessionID, s.appName, ErrNotFound)
}

// project computes the Detail view from a fully-loaded primary session
// plus its companion ops-row marker state (nil = no ops row) and the
// active gate-pause record, if any. Marker keys are also read from the
// primary's own state for back-compat: v0.1.0 wrote abort markers
// there, before the write-lease constraint moved marker writes to the
// ops row (issues #45/#46).
func project(sess adksession.Session, ops map[string]string, gate *PauseRecord) *Detail {
	d := &Detail{
		Summary: Summary{
			ID:            sess.ID(),
			AppName:       sess.AppName(),
			UserID:        sess.UserID(),
			LastEventTime: sess.LastUpdateTime(),
			State:         StateIdle,
		},
		EventCount:   sess.Events().Len(),
		Pending:      scanPending(sess.Events()),
		AppliedEdits: scanAppliedEdits(sess.Events()),
	}

	markerValue := func(key string) string {
		if v, ok := ops[key]; ok && v != "" {
			return v
		}
		if v, err := sess.State().Get(key); err == nil {
			return fmt.Sprintf("%v", v)
		}
		return ""
	}

	if reason := markerValue(abortReasonKey); reason != "" {
		// Abort trumps paused: the marker is terminal on mast's
		// surface, so pending interrupts are reported resolved-by-abort.
		d.State = StateAborted
		d.AbortReason = reason
		d.Pending = nil
		return d
	}

	if gate.Active() {
		// Gate pause (plane B) is the second paused source: the
		// chokepoint refuses every turn kind until it is resumed.
		d.State = StatePaused
		d.PauseReason = string(gate.Reason)
		d.PauseMessage = gate.Message
		d.GatePause = gate
	}

	if len(d.Pending) > 0 {
		// Paused trumps interrupted: a session that reached a pause —
		// HITL RequestedInput or a long-running park — is resumable and
		// should say so, whatever a shutdown marker says.
		d.State = StatePaused
		for _, p := range d.Pending {
			d.PendingInterruptIDs = append(d.PendingInterruptIDs, p.InterruptID)
		}
	}
	if d.State == StatePaused {
		return d
	}

	// The cleared state is the empty string, not a deleted key.
	if reason := markerValue(interruptReasonKey); reason != "" {
		d.State = StateInterrupted
		d.InterruptReason = reason
		if ts := markerValue(interruptTimeKey); ts != "" {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				d.InterruptedAt = t
			}
		}
	}
	return d
}

// scanPending walks the event log in order and returns the pending
// interrupts with no later matching resolution, from both spellings of
// ADK's pause primitive: RequestedInput events, and long-running tool
// parks (unanswered LongRunningToolIDs — request_operator_input,
// pause_session). A resolution is any later event carrying a
// FunctionResponse part whose ID equals the pending interrupt ID —
// exactly the resume wire shape verified in spike 2
// (docs/spike-findings.md, Q2).
//
// The LongRunningToolIDs source cannot false-positive on coordinator
// task delegations: ADK's TaskAgentTool and TransferToAgentTool are
// not long-running, so delegations never enter LongRunningToolIDs
// (verified against v2.1.0 in the v0.2 pause/abort design's gate).
func scanPending(events adksession.Events) []PendingInput {
	var order []string
	byID := make(map[string]PendingInput)
	for ev := range events.All() {
		if ri := ev.RequestedInput; ri != nil && ri.InterruptID != "" {
			if _, seen := byID[ri.InterruptID]; !seen {
				order = append(order, ri.InterruptID)
			}
			byID[ri.InterruptID] = PendingInput{
				InterruptID:    ri.InterruptID,
				Message:        ri.Message,
				Author:         ev.Author,
				RaisedAt:       ev.Timestamp,
				ResponseSchema: ri.ResponseSchema,
				Payload:        ri.Payload,
			}
		}
		for _, id := range ev.LongRunningToolIDs {
			if id == "" {
				continue
			}
			if _, seen := byID[id]; seen {
				// A RequestedInput event carries its interrupt ID in
				// LongRunningToolIDs too (one primitive, two spellings) —
				// keep the richer RequestedInput record.
				continue
			}
			order = append(order, id)
			p := PendingInput{
				InterruptID: id,
				Author:      ev.Author,
				RaisedAt:    ev.Timestamp,
				LongRunning: true,
			}
			// The parked FunctionCall rides the same event; its name and
			// "message" argument are the best operator-facing context.
			if ev.Content != nil {
				for _, part := range ev.Content.Parts {
					if part == nil || part.FunctionCall == nil || part.FunctionCall.ID != id {
						continue
					}
					p.ToolName = part.FunctionCall.Name
					if m, ok := part.FunctionCall.Args["message"].(string); ok {
						p.Message = m
					}
					if part.FunctionCall.Name == toolconfirmation.FunctionCallName {
						// A parked mutating call (the write gate, W2). The
						// operator-facing detail lives inside the call's
						// args; without this the pause projects as a bare
						// interrupt ID on an ADK-internal tool name, which
						// tells an operator nothing about what they are
						// being asked to authorize.
						d := approval.DescribeConfirmation(part.FunctionCall.Args)
						p.Message = d.Summary()
						p.Payload = d.Request
						p.ResponseSchema = approval.VerdictSchema()
					}
				}
			}
			byID[id] = p
		}
		if ev.Content == nil {
			continue
		}
		for _, part := range ev.Content.Parts {
			if part == nil || part.FunctionResponse == nil {
				continue
			}
			delete(byID, part.FunctionResponse.ID)
		}
	}
	var pending []PendingInput
	for _, id := range order {
		if p, ok := byID[id]; ok {
			pending = append(pending, p)
			delete(byID, id) // an ID re-raised after resolution appears in order twice
		}
	}
	return pending
}

// scanAppliedEdits walks the event log in order and returns the write
// gate's applied-edit records, oldest first.
//
// They are read from the events' state deltas rather than from the
// session's collapsed state so the order is the order the calls ran in,
// and a record the projection cannot decode is skipped rather than
// failing the whole show view: a malformed audit row should cost the
// operator that row, not the session.
func scanAppliedEdits(events adksession.Events) []approval.AppliedEdit {
	var edits []approval.AppliedEdit
	for ev := range events.All() {
		delta := ev.Actions.StateDelta
		if len(delta) == 0 {
			continue
		}
		keys := make([]string, 0, len(delta))
		for k := range delta {
			if strings.HasPrefix(k, approval.EditStateKeyPrefix) {
				keys = append(keys, k)
			}
		}
		// One event can carry more than one record only if ADK ever
		// batches parallel tool calls into a single event; sort so the
		// order is at least deterministic if it does.
		sort.Strings(keys)
		for _, k := range keys {
			e, err := approval.DecodeAppliedEdit(delta[k])
			if err != nil {
				continue
			}
			edits = append(edits, e)
		}
	}
	return edits
}
