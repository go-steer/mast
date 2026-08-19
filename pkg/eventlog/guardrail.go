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

// Originally derived from go-steer/core-agent@4ac03378193b6705779d531c420175f9eca38e96:pkg/agent/guardrail_persist.go

// Durable guardrail state.
//
// A halt that a restart clears is not a halt. mast's enforce-mode
// watchdog trip lived only in the daemon's watchdogPool, so a crash, an
// OOM kill, or a pod roll started a fresh process with the backstop
// disarmed — and the runaway-loop → crash → restart cycle enforce mode
// exists to break could repeat indefinitely, each restart handing the
// loop a clean slate. mast is the unattended sibling: the restart is
// automatic, and nobody is watching it happen.
//
// The fix is to make the trip a fact in the database rather than a
// field in a struct. Two row kinds are appended as the state changes,
// and folding them forward reconstructs the halt in the next process:
//
//	trip  — a backstop halted the session, and why
//	reset — an operator cleared it, and what runway they added
//
// The reset row doubles as the audit trail: two rows meaning "the
// operator reset this" would be two things to keep in agreement.
//
// Upstream writes both as rows in the ADK session's own event stream
// and folds the session's events forward. mast cannot: an out-of-band
// Get-then-AppendEvent while the runner holds the session bumps
// last_update_time and trips ADK's optimistic-concurrency check, which
// is the constraint that already forced attachadapter to defer its
// interrupt audit to between turns — and a reset arrives from an
// operator mid-incident, which is exactly when a turn is running.
// Upstream solves it with a pending queue drained between turns; mast
// writes to a table it owns instead, on the connection pkg/eventlog
// already holds for its overlay. Different table, no session row
// touched, so there is nothing to race and no queue to drain.
//
// The fold is last-writer-wins per guardrail rather than a tally — a
// trip is a latch, not a count — while granted runway accumulates,
// because two resets that each hand over $5 have handed over $10. That
// asymmetry, and the state shape below, come from upstream's
// pkg/attach/guardrail_events.go in the same commit.

package eventlog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Guardrail row kinds.
const (
	// GuardrailKindTrip records a backstop halting the session. It is
	// written by the daemon, not by an operator: a trip is something
	// that happened TO the session.
	GuardrailKindTrip = "trip"
	// GuardrailKindReset records an operator clearing a halt, and any
	// runway they bought with it.
	GuardrailKindReset = "reset"
)

// Guardrail names. These match pkg/attach's GuardrailWatchdog /
// GuardrailCostCeiling / GuardrailAll wire vocabulary; they are spelled
// again here rather than imported because pkg/attach imports this
// package's database handle, and a storage layer should not depend on
// the HTTP surface that happens to be its first caller.
const (
	GuardrailWatchdog    = "watchdog"
	GuardrailCostCeiling = "cost_ceiling"
	GuardrailAll         = "all"
)

// GuardrailRecord is one appended fact about a session's guardrails.
//
// Kind is GuardrailKindTrip or GuardrailKindReset. Guardrail names
// which backstop the row is about — for a reset, GuardrailAll means the
// operator cleared everything. Signal and Reason carry the halt's own
// text verbatim, so a restored halt says what the original said rather
// than a reconstruction of it.
type GuardrailRecord struct {
	Kind      string
	Guardrail string
	Signal    string
	Reason    string
	// Caller is the authenticated identity behind a reset. Empty for a
	// trip (nobody did it) and for a reset that arrived on an
	// unauthenticated surface.
	Caller string
	// BudgetAddedUSD / TokensAdded / TurnsAdded record the runway an
	// operator granted alongside a reset. Recorded for the audit trail
	// whether or not a reader applies them.
	BudgetAddedUSD float64
	TokensAdded    int64
	TurnsAdded     int
	// GrantScope names the budget scope a grant was aimed at: nil for a
	// row that does not carry one, and a non-nil "" for a grant aimed at
	// the session.
	//
	// The nil-vs-empty distinction is load-bearing rather than fussy.
	// Rows written before this column existed (#166, v0.4) read back as
	// nil, and a nil is genuinely unknown: it could be a session grant or
	// a specialist's. Replaying it as a session grant would raise a
	// ceiling by an amount no operator ever granted the session, so a
	// reader that applies grants must skip the nils and apply only what
	// it can attribute (#175).
	GrantScope *string
	// At is the wall-clock time of the fact. Zero means "now".
	At time.Time
}

// SessionScope is the GrantScope value for a grant aimed at the session
// rather than at one specialist — a non-nil pointer to the empty string,
// which is what distinguishes it from a row that recorded no scope at
// all.
func SessionScope() *string {
	s := ""
	return &s
}

// GuardrailGrant is the runway granted to one budget scope over a
// session's life.
type GuardrailGrant struct {
	BudgetAddedUSD float64
	TokensAdded    int64
	TurnsAdded     int
}

// GuardrailState is what folding a session's guardrail rows forward
// produces: the halt a fresh process must honor, and the runway
// operators have granted over the session's life.
type GuardrailState struct {
	// WatchdogTripped is latched by a trip row naming the watchdog and
	// cleared by a reset row naming it (or naming everything).
	WatchdogTripped bool
	WatchdogSignal  string
	WatchdogReason  string
	// TrippedAt is when the surviving halt was recorded — the answer to
	// "how long has this been stuck?", which a restart otherwise erases
	// along with the halt.
	TrippedAt time.Time

	// CostTripped mirrors the watchdog latch for the budget ceiling.
	// mast's daemon does not restore it, and since #175 that is a
	// decision rather than a gap: a cost trip is derived from
	// accumulator-vs-ceiling on every event (pkg/budget has no trip
	// flag), so restoring the spend and the grants restores the trip
	// with them. A latch alongside the arithmetic would be a second
	// answer to the same question, free to disagree with it. The fold
	// reports the field because the rows carry it and a reader that can
	// act on it should not have to re-derive it.
	CostTripped bool
	CostReason  string

	// BudgetAddedUSD / TokensAdded / TurnsAdded accumulate across every
	// reset in the session's history, whichever scope each was aimed at.
	// This is the audit total — "what has been handed to this session" —
	// and it stays whole-session so the figure #166 reported does not
	// change meaning.
	BudgetAddedUSD float64
	TokensAdded    int64
	TurnsAdded     int

	// GrantsByScope is the same runway split by the scope it was granted
	// to, keyed by specialist name with "" for the session. It is the
	// figure a reader replays, and it counts only rows that recorded a
	// scope: a pre-#175 row cannot be attributed, so it contributes to
	// the audit totals above and to nothing else. The two therefore
	// disagree on a database with legacy rows, deliberately — one answers
	// "what did operators give this session", the other "what can be
	// given back to it after a restart".
	GrantsByScope map[string]GuardrailGrant
}

// Halted reports whether the folded state leaves the session refusing
// turns.
func (s GuardrailState) Halted() bool { return s.WatchdogTripped || s.CostTripped }

// guardrailLogRow is the GORM model for the agent_guardrail_log table.
// Append-only: one row per state transition, ordered by Seq.
//
// The composite index matches the (app, user, session) triple every
// other mast-owned table keys on, so a fold is one indexed scan.
type guardrailLogRow struct {
	Seq            int64  `gorm:"primaryKey;autoIncrement"`
	AppName        string `gorm:"not null;index:idx_guardrail_log_session,priority:1"`
	UserID         string `gorm:"not null;index:idx_guardrail_log_session,priority:2"`
	SessionID      string `gorm:"not null;index:idx_guardrail_log_session,priority:3"`
	Kind           string `gorm:"not null"`
	Guardrail      string `gorm:"not null"`
	Signal         string
	Reason         string `gorm:"type:text"`
	Caller         string
	BudgetAddedUSD float64
	TokensAdded    int64
	TurnsAdded     int
	// Pointer so AutoMigrate's added column is NULL on rows written
	// before it existed, and a fold can tell "no scope recorded" from
	// "the session". See GuardrailRecord.GrantScope.
	GrantScope *string
	At         time.Time `gorm:"not null"`
}

// TableName pins the table name independent of GORM's pluralization,
// matching agent_eventlog and agent_run_lock.
func (guardrailLogRow) TableName() string { return "agent_guardrail_log" }

// GuardrailStore persists and folds a session's guardrail transitions.
//
// A nil *GuardrailStore is usable and does nothing: Append is a no-op
// and Fold reports empty state. That is the "no durable store
// configured" case at the call sites, and it keeps the daemon's wiring
// free of nil checks — the same shape a nil watchdog.Enforcer has.
//
// Safe for concurrent use.
type GuardrailStore struct {
	db *gorm.DB
}

// NewGuardrailStore constructs a store over an existing GORM
// connection — typically Handle.DB, the same overlay connection
// pkg/attach's SessionACLStore shares. AutoMigrates
// agent_guardrail_log; safe against an existing database.
func NewGuardrailStore(ctx context.Context, db *gorm.DB) (*GuardrailStore, error) {
	if db == nil {
		return nil, errors.New("eventlog: NewGuardrailStore: db is required")
	}
	if err := db.WithContext(ctx).AutoMigrate(&guardrailLogRow{}); err != nil {
		return nil, fmt.Errorf("eventlog: AutoMigrate agent_guardrail_log: %w", err)
	}
	return &GuardrailStore{db: db}, nil
}

// Append writes one guardrail fact.
//
// Callers pass a background context deliberately: a row written because
// of an operator's reset must not be droppable by that operator's HTTP
// client hanging up, and a trip row is written from the alert path of a
// turn whose context is being cancelled by the halt itself.
func (s *GuardrailStore) Append(ctx context.Context, app, user, sid string, rec GuardrailRecord) error {
	if s == nil || s.db == nil {
		return nil
	}
	if app == "" || sid == "" {
		return fmt.Errorf("eventlog: GuardrailStore.Append: app and session are required (got app=%q sid=%q)", app, sid)
	}
	switch rec.Kind {
	case GuardrailKindTrip, GuardrailKindReset:
	default:
		return fmt.Errorf("eventlog: GuardrailStore.Append: unknown kind %q (want %q or %q)",
			rec.Kind, GuardrailKindTrip, GuardrailKindReset)
	}
	if rec.Guardrail == "" {
		return errors.New("eventlog: GuardrailStore.Append: guardrail is required")
	}
	at := rec.At
	if at.IsZero() {
		at = time.Now()
	}
	row := guardrailLogRow{
		AppName:        app,
		UserID:         user,
		SessionID:      sid,
		Kind:           rec.Kind,
		Guardrail:      rec.Guardrail,
		Signal:         rec.Signal,
		Reason:         rec.Reason,
		Caller:         rec.Caller,
		BudgetAddedUSD: rec.BudgetAddedUSD,
		TokensAdded:    rec.TokensAdded,
		TurnsAdded:     rec.TurnsAdded,
		GrantScope:     rec.GrantScope,
		At:             at,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("eventlog: GuardrailStore.Append: %w", err)
	}
	return nil
}

// Fold reconstructs the session's guardrail state from its rows.
//
// A trip latches its guardrail; a reset naming that guardrail — or
// naming GuardrailAll — clears it. Granted runway accumulates across
// every reset rather than latching, because two grants of $5 are $10.
//
// A session with no rows folds to the zero value and no error, which is
// the overwhelmingly common case: guardrail rows are written only when
// something halts or an operator intervenes.
func (s *GuardrailStore) Fold(ctx context.Context, app, user, sid string) (GuardrailState, error) {
	var st GuardrailState
	if s == nil || s.db == nil {
		return st, nil
	}
	var rows []guardrailLogRow
	err := s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", app, user, sid).
		Order("seq ASC").
		Find(&rows).Error
	if err != nil {
		return GuardrailState{}, fmt.Errorf("eventlog: GuardrailStore.Fold: %w", err)
	}
	for _, r := range rows {
		switch r.Kind {
		case GuardrailKindTrip:
			switch r.Guardrail {
			case GuardrailWatchdog:
				st.WatchdogTripped = true
				st.WatchdogSignal = r.Signal
				st.WatchdogReason = r.Reason
				st.TrippedAt = r.At
			case GuardrailCostCeiling:
				st.CostTripped = true
				st.CostReason = r.Reason
				st.TrippedAt = r.At
			}
		case GuardrailKindReset:
			// Grants accumulate whichever guardrail the reset named:
			// the row records what the operator handed over, and a
			// grant that arrived with a watchdog reset is still money
			// they spent.
			st.BudgetAddedUSD += r.BudgetAddedUSD
			st.TokensAdded += r.TokensAdded
			st.TurnsAdded += r.TurnsAdded
			if r.GrantScope != nil && (r.BudgetAddedUSD != 0 || r.TokensAdded != 0 || r.TurnsAdded != 0) {
				if st.GrantsByScope == nil {
					st.GrantsByScope = map[string]GuardrailGrant{}
				}
				g := st.GrantsByScope[*r.GrantScope]
				g.BudgetAddedUSD += r.BudgetAddedUSD
				g.TokensAdded += r.TokensAdded
				g.TurnsAdded += r.TurnsAdded
				st.GrantsByScope[*r.GrantScope] = g
			}
			if r.Guardrail == GuardrailWatchdog || r.Guardrail == GuardrailAll {
				st.WatchdogTripped = false
				st.WatchdogSignal = ""
				st.WatchdogReason = ""
				st.TrippedAt = time.Time{}
			}
			if r.Guardrail == GuardrailCostCeiling || r.Guardrail == GuardrailAll {
				st.CostTripped = false
				st.CostReason = ""
			}
		}
	}
	return st, nil
}

// History returns the session's guardrail rows oldest-first — the audit
// trail an operator reads to answer "how many times has this happened,
// and who cleared it?".
func (s *GuardrailStore) History(ctx context.Context, app, user, sid string) ([]GuardrailRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	var rows []guardrailLogRow
	err := s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", app, user, sid).
		Order("seq ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("eventlog: GuardrailStore.History: %w", err)
	}
	out := make([]GuardrailRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, GuardrailRecord{
			Kind:           r.Kind,
			Guardrail:      r.Guardrail,
			Signal:         r.Signal,
			Reason:         r.Reason,
			Caller:         r.Caller,
			BudgetAddedUSD: r.BudgetAddedUSD,
			TokensAdded:    r.TokensAdded,
			TurnsAdded:     r.TurnsAdded,
			GrantScope:     r.GrantScope,
			At:             r.At,
		})
	}
	return out, nil
}
