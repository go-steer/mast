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

// Durable budget spend (#175).
//
// A max_cost_usd ceiling that a restart resets is a ceiling on what a
// workload spends *per process*. mast's restarts are automatic and
// unattended, so a crash loop could spend the cap once per restart,
// indefinitely — the exact situation an operator bought the ceiling for.
// #166 made the watchdog halt durable and stopped there, because a trip
// is one latched bit and spend is an accumulator that has to come back
// to the cent.
//
// This is that accumulator, kept as a ledger: one row per priced model
// call, append-only, in a table mast owns on the connection the eventlog
// overlay already holds. Folding a session's rows forward reconstructs
// what it spent.
//
// # Why a ledger and not a checkpoint
//
// Persisting the accumulator itself means picking a moment to write it
// and then reconciling that moment against a transcript that has moved
// on — a durability problem dressed as an arithmetic one. A ledger
// written at the same granularity the accumulator moves has nothing to
// reconcile: the rows *are* the accumulator. The only thing a crash can
// lose is a call whose row had not landed, which is bounded by one model
// call and is always an undercount. A ceiling can be a little late; it
// must never be charged for money nobody spent.
//
// # Why not replay the session's own events
//
// The tempting alternative is to keep no new state and fold the
// session's priced events back through a fresh meter. It does not work
// here, for three reasons that are properties of the substrate rather
// than of the design:
//
//   - ADK's database session service does not persist ModelVersion. Its
//     storageEvent carries UsageMetadata and Author and has no column
//     for the model (adk/v2 v2.2.0, session/database/storage_session.go).
//     Every replayed call would therefore miss pkg/pricing's catalog and
//     fall back to the flat per-1K rate, which pkg/budget measured at
//     5.9x off on a real cache-warm session. A restored ceiling that
//     wrong is worse than no ceiling, because it looks right.
//   - Replay prices history at today's rates. The builtin catalog is
//     regenerated weekly, so a rate change would retroactively rewrite
//     what a session already spent. What a call cost is a fact of the
//     moment it was made; the ledger records it then.
//   - Replay's cost grows with the transcript, on the first turn after
//     every restart — and the sessions that survive restarts are the
//     long ones.
//
// # Row volume
//
// One row per model call is the same order as agent_eventlog, which
// already writes one row per event, and a fold is a single indexed
// aggregate rather than a scan of the rows. The ledger doubles as the
// per-call cost history an operator would otherwise reconstruct from
// logs.

package eventlog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// SpendRecord is one priced model call.
//
// Author is the event's author verbatim, not the budget scope it
// resolved to. A roster edit between processes can add or remove a
// specialist's scope, and a row that had already collapsed the author
// into "session" could never be re-attributed; recording the author lets
// the fold's reader attribute against whatever scopes it currently
// carries, which is what a live meter does with a live event.
//
// CostUSD is what the call cost when it was made, priced by the catalog
// in force at the time. Unpriced marks a call the catalog could not
// price, so a restored figure can be labelled the same mixed-model
// approximation a live one would be.
type SpendRecord struct {
	Author   string
	Tokens   int64
	CostUSD  float64
	Unpriced bool
	// At is the wall-clock time of the call. Zero means "now".
	At time.Time
}

// SpendTotals is one accumulator's folded form.
type SpendTotals struct {
	Tokens  int64
	CostUSD float64
	Calls   int
}

// SpendState is what folding a session's ledger produces: the session's
// total, and the same rows broken out by author so per-specialist
// ceilings come back too.
//
// Session includes every call ByAuthor also accounts for — the two are
// projections of the same rows, computed in the same query.
type SpendState struct {
	Session  SpendTotals
	ByAuthor map[string]SpendTotals
	// Unpriced counts calls the catalog could not price, mirroring
	// budget.Meter.Unpriced.
	Unpriced int
}

// budgetSpendRow is the GORM model for the agent_budget_spend table.
// Append-only: one row per priced model call, ordered by Seq.
//
// Same composite index as agent_guardrail_log, on the same (app, user,
// session) triple every mast-owned table keys on, so a fold is one
// indexed aggregate.
type budgetSpendRow struct {
	Seq       int64  `gorm:"primaryKey;autoIncrement"`
	AppName   string `gorm:"not null;index:idx_budget_spend_session,priority:1"`
	UserID    string `gorm:"not null;index:idx_budget_spend_session,priority:2"`
	SessionID string `gorm:"not null;index:idx_budget_spend_session,priority:3"`
	Author    string
	Tokens    int64
	CostUSD   float64
	Unpriced  bool
	At        time.Time `gorm:"not null"`
}

// TableName pins the table name independent of GORM's pluralization,
// matching agent_eventlog, agent_run_lock, and agent_guardrail_log.
func (budgetSpendRow) TableName() string { return "agent_budget_spend" }

// SpendStore persists and folds a session's per-call spend.
//
// A nil *SpendStore is usable and does nothing: Append is a no-op and
// Fold reports empty state — the "no durable store configured" case at
// the call sites, same shape as GuardrailStore.
//
// Safe for concurrent use.
type SpendStore struct {
	db *gorm.DB
}

// NewSpendStore constructs a store over an existing GORM connection —
// typically Handle.DB, the connection the guardrail store and
// pkg/attach's session ACL store already share. AutoMigrates
// agent_budget_spend; safe against an existing database.
func NewSpendStore(ctx context.Context, db *gorm.DB) (*SpendStore, error) {
	if db == nil {
		return nil, errors.New("eventlog: NewSpendStore: db is required")
	}
	if err := db.WithContext(ctx).AutoMigrate(&budgetSpendRow{}); err != nil {
		return nil, fmt.Errorf("eventlog: AutoMigrate agent_budget_spend: %w", err)
	}
	return &SpendStore{db: db}, nil
}

// Append writes one call's spend.
//
// Callers pass a bounded background context deliberately, as the
// guardrail store's writers do: the call that crosses a ceiling is the
// one whose turn context is being cancelled by the crossing, and it is
// also the most important row in the ledger.
func (s *SpendStore) Append(ctx context.Context, app, user, sid string, rec SpendRecord) error {
	if s == nil || s.db == nil {
		return nil
	}
	if app == "" || sid == "" {
		return fmt.Errorf("eventlog: SpendStore.Append: app and session are required (got app=%q sid=%q)", app, sid)
	}
	at := rec.At
	if at.IsZero() {
		at = time.Now()
	}
	row := budgetSpendRow{
		AppName:   app,
		UserID:    user,
		SessionID: sid,
		Author:    rec.Author,
		Tokens:    rec.Tokens,
		CostUSD:   rec.CostUSD,
		Unpriced:  rec.Unpriced,
		At:        at,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("eventlog: SpendStore.Append: %w", err)
	}
	return nil
}

// spendAgg is one author's aggregate, as the fold query returns it.
type spendAgg struct {
	Author   string
	Tokens   int64
	CostUSD  float64
	Calls    int
	Unpriced int
}

// Fold reconstructs the session's spend from its ledger.
//
// One GROUP BY author, summed in the database rather than in Go: the
// ledger is one row per model call, and a long-running session is the
// case this exists for.
//
// A session with no rows folds to the zero value and no error — the
// common case, since most sessions a process sees are its own.
func (s *SpendStore) Fold(ctx context.Context, app, user, sid string) (SpendState, error) {
	var st SpendState
	if s == nil || s.db == nil {
		return st, nil
	}
	var aggs []spendAgg
	err := s.db.WithContext(ctx).
		Model(&budgetSpendRow{}).
		Select("author AS author, "+
			"COALESCE(SUM(tokens), 0) AS tokens, "+
			"COALESCE(SUM(cost_usd), 0) AS cost_usd, "+
			"COUNT(*) AS calls, "+
			"COALESCE(SUM(CASE WHEN unpriced = ? THEN 1 ELSE 0 END), 0) AS unpriced", true).
		Where("app_name = ? AND user_id = ? AND session_id = ?", app, user, sid).
		Group("author").
		Scan(&aggs).Error
	if err != nil {
		return SpendState{}, fmt.Errorf("eventlog: SpendStore.Fold: %w", err)
	}
	if len(aggs) == 0 {
		return st, nil
	}
	st.ByAuthor = make(map[string]SpendTotals, len(aggs))
	for _, a := range aggs {
		st.Session.Tokens += a.Tokens
		st.Session.CostUSD += a.CostUSD
		st.Session.Calls += a.Calls
		st.Unpriced += a.Unpriced
		st.ByAuthor[a.Author] = SpendTotals{Tokens: a.Tokens, CostUSD: a.CostUSD, Calls: a.Calls}
	}
	return st, nil
}

// History returns the session's spend rows oldest-first — the per-call
// cost trail behind a folded total, for an operator asking where the
// money went rather than how much of it there was.
func (s *SpendStore) History(ctx context.Context, app, user, sid string) ([]SpendRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	var rows []budgetSpendRow
	err := s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", app, user, sid).
		Order("seq ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("eventlog: SpendStore.History: %w", err)
	}
	out := make([]SpendRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, SpendRecord{
			Author:   r.Author,
			Tokens:   r.Tokens,
			CostUSD:  r.CostUSD,
			Unpriced: r.Unpriced,
			At:       r.At,
		})
	}
	return out, nil
}
