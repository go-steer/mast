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
// (docs/spike-findings.md): a paused session carries an event with a
// non-nil RequestedInput; the matching resume is a later user turn
// whose FunctionResponse.ID equals that RequestedInput's InterruptID.
// A RequestedInput with no such later FunctionResponse is therefore a
// *pending* interrupt, and a session with pending interrupts is paused.
//
// State labels are derived strictly from what the store can prove:
//
//   - StatePaused:  at least one pending (unresolved) RequestedInput.
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
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/genai"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
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

// ErrNotFound reports that no session with the requested ID exists in
// the store (under the store's app name).
var ErrNotFound = errors.New("session not found")

// ErrAlreadyAborted reports that an abort marker is already present.
var ErrAlreadyAborted = errors.New("session already aborted")

// PendingInput is a RequestedInput interrupt that has not been resolved
// by a later matching FunctionResponse. It carries everything an
// operator needs to script a resume.
type PendingInput struct {
	// InterruptID is the resume correlation key: the resume turn's
	// FunctionResponse.ID must equal it (spike-2 verified contract).
	InterruptID string `json:"interrupt_id"`
	// Message is the human-readable prompt from the pausing node.
	Message string `json:"message,omitempty"`
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
}

// Detail is the show-view projection: Summary plus event count and the
// full pending-interrupt records.
type Detail struct {
	Summary
	EventCount int            `json:"event_count"`
	Pending    []PendingInput `json:"pending,omitempty"`
}

// Store wraps an ADK session.Service with the operator-facing
// projections. It works over any Service implementation — the SQLite /
// Postgres database service and the in-memory service alike.
type Store struct {
	svc     adksession.Service
	appName string
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
	// Silence GORM's default trace logger: ADK's service probes
	// app/user state rows that legitimately may not exist, and the
	// resulting "record not found" traces would pollute CLI output.
	svc, err := database.NewSessionService(sqlite.Open(path),
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("open session db %q: %w", path, err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		return nil, fmt.Errorf("migrate session db %q: %w", path, err)
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
// IDs, not the daemon-internal user ID).
func (s *Store) Get(ctx context.Context, userID, sessionID string) (*Detail, error) {
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
	return project(resp.Session, s.opsState(ctx, userID, sessionID)), nil
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
	if v, ok := s.opsState(ctx, userID, sessionID)[abortReasonKey]; ok && v != "" {
		return fmt.Errorf("session %q: %w", sessionID, ErrAlreadyAborted)
	}

	return s.appendOpsDelta(ctx, userID, sessionID, "operator-abort", "operator",
		fmt.Sprintf("session aborted by operator: %s", reason),
		map[string]any{
			abortReasonKey: reason,
			abortTimeKey:   time.Now().UTC().Format(time.RFC3339Nano),
		})
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
	return s.appendOpsDelta(ctx, userID, sessionID, "shutdown-interrupt-clear", "daemon",
		"turn completed within the shutdown drain window; interruption marker cleared",
		map[string]any{interruptReasonKey: "", interruptTimeKey: ""})
}

// appendOpsDelta appends one marker event to the session's companion
// ops row, creating the row on first use — the shared mechanics of the
// abort and interruption markers. It never touches the primary row
// (see opsSuffix for why that is load-bearing).
func (s *Store) appendOpsDelta(ctx context.Context, userID, sessionID, invocation, author, text string, delta map[string]any) error {
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

// opsState reads the companion ops row's marker keys. A missing row —
// or any read failure — means "no markers": the ops row is an overlay,
// and a session without one is simply unmarked.
func (s *Store) opsState(ctx context.Context, userID, sessionID string) map[string]string {
	resp, err := s.svc.Get(ctx, &adksession.GetRequest{
		AppName: s.appName, UserID: userID, SessionID: opsSessionID(sessionID),
	})
	if err != nil {
		return nil
	}
	out := make(map[string]string, 4)
	for _, k := range []string{abortReasonKey, abortTimeKey, interruptReasonKey, interruptTimeKey} {
		if v, err := resp.Session.State().Get(k); err == nil {
			out[k] = fmt.Sprintf("%v", v)
		}
	}
	return out
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
		if sess.ID() == sessionID {
			return sess.UserID(), nil
		}
	}
	return "", fmt.Errorf("session %q (app %q): %w", sessionID, s.appName, ErrNotFound)
}

// project computes the Detail view from a fully-loaded primary session
// plus its companion ops-row marker state (nil = no ops row). Marker
// keys are also read from the primary's own state for back-compat:
// v0.1.0 wrote abort markers there, before the write-lease constraint
// moved marker writes to the ops row (issues #45/#46).
func project(sess adksession.Session, ops map[string]string) *Detail {
	d := &Detail{
		Summary: Summary{
			ID:            sess.ID(),
			AppName:       sess.AppName(),
			UserID:        sess.UserID(),
			LastEventTime: sess.LastUpdateTime(),
			State:         StateIdle,
		},
		EventCount: sess.Events().Len(),
		Pending:    scanPending(sess.Events()),
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

	if len(d.Pending) > 0 {
		// Paused trumps interrupted: a session that reached a HITL pause
		// is resumable and should say so, whatever a shutdown marker says.
		d.State = StatePaused
		for _, p := range d.Pending {
			d.PendingInterruptIDs = append(d.PendingInterruptIDs, p.InterruptID)
		}
		return d
	}

	// The cleared state is the empty string, not a deleted key.
	if reason := markerValue(interruptReasonKey); reason != "" {
		d.State = StateInterrupted
		d.InterruptReason = reason
	}
	return d
}

// scanPending walks the event log in order and returns the
// RequestedInput interrupts with no later matching resolution. A
// resolution is any later event carrying a FunctionResponse part whose
// ID equals the pending InterruptID — exactly the resume wire shape
// verified in spike 2 (docs/spike-findings.md, Q2).
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
