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

// Package transcript is the operator's read projection over mast's
// sessions: list them, show one — its state, its pending interrupts, the
// edits and captures its approvals recorded — export its approval
// decisions, and look up a resume token. It reads the same ADK session
// store a daemon or a library embed writes through, so a session written
// by either reads identically here.
//
// It is the read half. The writes — pause and abort markers, interrupt
// bookkeeping, token consumption, auto-resume attempts — are the
// daemon's and the root package's, and live under internal/ (#301). To
// pause or resume from Go, use mast.Pause, mast.ResumeSession and
// mast.ResumeByToken.
//
// The record types are aliases of the store's own, so a value from
// mast.ListSessions or mast.Pause is the same type as one from here.
package transcript

import (
	"context"
	"io"

	adksession "google.golang.org/adk/v2/session"

	itr "github.com/go-steer/mast/internal/transcript"
	"github.com/go-steer/mast/pkg/approval"
)

// The records the read methods return, and the pause types mast.Pause
// takes and returns.
type (
	// Summary is one session in a List.
	Summary = itr.Summary
	// Detail is one session as Get projects it.
	Detail = itr.Detail
	// PendingInput is an interrupt a session is waiting on.
	PendingInput = itr.PendingInput
	// PauseRecord is the durable record behind a resume token.
	PauseRecord = itr.PauseRecord
	// PauseSpec is what mast.Pause records.
	PauseSpec = itr.PauseSpec
	// PauseHandle is what mast.Pause returns: the token and its expiry.
	PauseHandle = itr.PauseHandle
	// Reason classifies a pause.
	Reason = itr.Reason
	// ExportOptions selects and redacts what ExportDecisions writes.
	ExportOptions = itr.ExportOptions
	// ExportMeta is the header line ExportDecisions writes first.
	ExportMeta = itr.ExportMeta
)

// Session states, as Detail.State and Summary.State report them.
const (
	StatePaused      = itr.StatePaused
	StateAborted     = itr.StateAborted
	StateInterrupted = itr.StateInterrupted
	StateIdle        = itr.StateIdle
)

// What a session is waiting on, as Detail.Awaiting reports it.
const (
	AwaitingApproval = itr.AwaitingApproval
	AwaitingInput    = itr.AwaitingInput
)

// Pause planes, as recorded in PauseRecord.Plane.
const (
	PlaneGate      = itr.PlaneGate
	PlaneInterrupt = itr.PlaneInterrupt
)

// Pause reasons a PauseSpec may carry; ValidReasons lists them.
const (
	ReasonBudgetExhaustion  = itr.ReasonBudgetExhaustion
	ReasonWatchdogAnomaly   = itr.ReasonWatchdogAnomaly
	ReasonCostCoolDown      = itr.ReasonCostCoolDown
	ReasonMaintenanceWindow = itr.ReasonMaintenanceWindow
	ReasonRateLimitBackoff  = itr.ReasonRateLimitBackoff
	ReasonAmbiguity         = itr.ReasonAmbiguity
	ReasonOperator          = itr.ReasonOperator
	ReasonA2ATaskPending    = itr.ReasonA2ATaskPending
	ReasonOther             = itr.ReasonOther
)

// DefaultTokenTTL is how long a resume token lives when a PauseSpec
// names no TokenTTL.
const DefaultTokenTTL = itr.DefaultTokenTTL

// Redaction modes for ExportOptions.
const (
	RedactionNone           = itr.RedactionNone
	RedactionApproverDigest = itr.RedactionApproverDigest
)

// Errors the read methods and mast's resume calls return; compare with
// errors.Is.
var (
	ErrNotFound       = itr.ErrNotFound
	ErrTokenNotFound  = itr.ErrTokenNotFound
	ErrTokenExpired   = itr.ErrTokenExpired
	ErrAlreadyResumed = itr.ErrAlreadyResumed
)

// ValidReasons lists the pause reasons a PauseSpec may carry.
func ValidReasons() []string { return itr.ValidReasons() }

// Store reads mast's sessions out of an ADK session service.
type Store struct {
	s *itr.Store
}

// NewStore reads sessions for appName ("mast" for anything mast wrote)
// out of svc — the same service passed as mast.Config.Sessions or opened
// by the daemon's --session-db.
func NewStore(svc adksession.Service, appName string) *Store {
	return &Store{s: itr.NewStore(svc, appName)}
}

// List summarizes every session for userID; an empty userID lists every
// user's.
func (s *Store) List(ctx context.Context, userID string) ([]Summary, error) {
	return s.s.List(ctx, userID)
}

// Get projects one session. An empty userID finds the session under
// whichever user owns it. ErrNotFound when there is none.
func (s *Store) Get(ctx context.Context, userID, sessionID string) (*Detail, error) {
	return s.s.Get(ctx, userID, sessionID)
}

// Decisions returns a session's durable approval decisions, oldest
// first.
func (s *Store) Decisions(ctx context.Context, userID, sessionID string) ([]approval.Decision, error) {
	return s.s.Decisions(ctx, userID, sessionID)
}

// ExportDecisions writes the decisions opts selects as JSONL — a meta
// line, then one mast.decision/v1 record per line — and returns how many
// records it wrote.
func (s *Store) ExportDecisions(ctx context.Context, w io.Writer, opts ExportOptions) (int, error) {
	return s.s.ExportDecisions(ctx, w, opts)
}

// FindToken returns the pause record behind a resume token, consumed or
// not. ErrTokenNotFound when there is none.
func (s *Store) FindToken(ctx context.Context, token string) (*PauseRecord, error) {
	return s.s.FindToken(ctx, token)
}
