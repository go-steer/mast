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

// Pause records and resume tokens — the storage half of the v0.2
// programmatic pause/abort surface (docs/durable-execution-design.md,
// "The v0.2 pause/abort mechanics").
//
// Two pause planes share one record shape:
//
//   - Gate pause (plane B): a single per-session record under
//     pauseGateKey on the companion ops row. While active, the daemon's
//     turn chokepoint refuses every turn kind on the session.
//   - Interrupt pause (plane A): one record per parked interrupt, under
//     pauseIntrKeyPrefix + interruptID. The park itself is the ADK
//     LongRunningToolIDs event; the record adds the resume token,
//     scope, and timer metadata.
//
// Per-interrupt keys are deliberate (adversarial-gate finding M7): a
// shared JSON map would make mint/consume a cross-process
// read-modify-write; per-key records never collide. Records are
// consumed, not deleted — ConsumedAt distinguishes "resumed already"
// (a structured no-op) from "no such token" (an operator error).
// Tokens are minted here, never caller-chosen, so a weak caller token
// cannot undermine the capability.
package transcript

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	adksession "google.golang.org/adk/v2/session"
)

// Ops-row keys for pause records. The gate record is single per
// session; interrupt records are keyed by their interrupt ID. A
// blanked value ("") means purged (by abort) — consumed records keep
// their JSON with ConsumedAt set, so a replayed resume can be told
// apart from a bad token.
const (
	pauseGateKey       = "mast_pause_gate"
	pauseIntrKeyPrefix = "mast_pause_intr:"
)

// tokenPrefix marks mast resume tokens (docs/durable-execution-design.md,
// "Resume tokens": mrt_ + base64url-encoded 128-bit random).
const tokenPrefix = "mrt_"

// DefaultTokenTTL is the default resume-token lifetime (open question
// #2, resolved via #9's direction). PauseSpec.TokenTTL may only
// shorten it; lengthening is exclusively the audited ExtendToken path.
const DefaultTokenTTL = 7 * 24 * time.Hour

// Reason is the pause-reason taxonomy (open question #1, resolved:
// enum with an `other` escape hatch).
type Reason string

const (
	ReasonBudgetExhaustion  Reason = "budget_exhaustion"
	ReasonWatchdogAnomaly   Reason = "watchdog_anomaly"
	ReasonCostCoolDown      Reason = "cost_cool_down"
	ReasonMaintenanceWindow Reason = "maintenance_window"
	ReasonRateLimitBackoff  Reason = "rate_limit_backoff"
	ReasonAmbiguity         Reason = "ambiguity"
	ReasonOperator          Reason = "operator"
	ReasonA2ATaskPending    Reason = "a2a_task_pending"
	ReasonOther             Reason = "other"
)

var validReasons = map[Reason]bool{
	ReasonBudgetExhaustion: true, ReasonWatchdogAnomaly: true,
	ReasonCostCoolDown: true, ReasonMaintenanceWindow: true,
	ReasonRateLimitBackoff: true, ReasonAmbiguity: true,
	ReasonOperator: true, ReasonA2ATaskPending: true, ReasonOther: true,
}

// ValidReasons lists the accepted Reason values, for error messages
// and CLI help.
func ValidReasons() []string {
	return []string{
		string(ReasonBudgetExhaustion), string(ReasonWatchdogAnomaly),
		string(ReasonCostCoolDown), string(ReasonMaintenanceWindow),
		string(ReasonRateLimitBackoff), string(ReasonAmbiguity),
		string(ReasonOperator), string(ReasonA2ATaskPending), string(ReasonOther),
	}
}

// Pause/token errors. ErrTokenNotFound wraps ErrNotFound so surfaces
// that map not-found to 400/exit-1 handle both uniformly.
var (
	ErrTokenNotFound  = fmt.Errorf("resume token not found: %w", ErrNotFound)
	ErrTokenExpired   = errors.New("resume token expired (the pause remains; extend-token is the recovery)")
	ErrAlreadyResumed = errors.New("pause already resumed")
)

// PauseSpec is the caller-facing pause request
// (docs/durable-execution-design.md, final PauseSpec). Interrupt is a
// daemon-level instruction (cancel the in-flight turn) and is not
// persisted in the record.
type PauseSpec struct {
	Reason    Reason
	Message   string
	Metadata  map[string]any
	ResumeAt  time.Time
	Interrupt bool
	TokenTTL  time.Duration
}

func (spec *PauseSpec) validate() error {
	if !validReasons[spec.Reason] {
		return fmt.Errorf("unknown pause reason %q (want one of %s)", spec.Reason, strings.Join(ValidReasons(), ", "))
	}
	if spec.TokenTTL < 0 {
		return errors.New("token TTL must not be negative")
	}
	if spec.TokenTTL > DefaultTokenTTL {
		return fmt.Errorf("token TTL %s exceeds the %s default: lengthening at mint is not offered — extend-token is the audited operator path", spec.TokenTTL, DefaultTokenTTL)
	}
	return nil
}

func (spec *PauseSpec) ttl() time.Duration {
	if spec.TokenTTL > 0 {
		return spec.TokenTTL
	}
	return DefaultTokenTTL
}

// Pause planes, as recorded in PauseRecord.Plane.
const (
	PlaneGate      = "gate"
	PlaneInterrupt = "interrupt"
)

// PauseRecord is the durable pause record + resume token, stored as
// JSON on the session's companion ops row. Scope (App, User) is
// checked at resume before any execution (open question #9, ratified);
// v0.2's single-tenant reality makes scope the (app, user) pair, and a
// tenant field slots in here when multi-tenancy lands.
type PauseRecord struct {
	Token       string         `json:"token"`
	Plane       string         `json:"plane"`
	InterruptID string         `json:"interrupt_id,omitempty"`
	App         string         `json:"app"`
	User        string         `json:"user"`
	SessionID   string         `json:"session_id"`
	Reason      Reason         `json:"reason"`
	Message     string         `json:"message,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	MintedAt    time.Time      `json:"minted_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
	ResumeAt    time.Time      `json:"resume_at,omitzero"`
	ConsumedAt  time.Time      `json:"consumed_at,omitzero"`

	// ConsumedBy names who spent the token — the audit answer to "who
	// approved this?", which is the whole reason a durable HITL record
	// beats an in-memory prompt. Callers pass an authenticated identity
	// where they have one ("alice@example.com", or "alice@example.com
	// (asserted by sa:switchboard)" when a relay asserted it via the
	// proxy path); where there is no request behind the consume they
	// pass the mechanism instead — "mast:scheduler", "library
	// ResumeByToken", "operator resume --token --session-db". Never a
	// client-supplied string: an attribution a caller writes about
	// itself proves nothing. See pkg/auth.Attribution.
	ConsumedBy string `json:"consumed_by,omitempty"`
}

// Active reports whether the record still gates/awaits a resume.
func (r *PauseRecord) Active() bool { return r != nil && r.ConsumedAt.IsZero() }

// Expired reports whether the record's token has passed its TTL. An
// expired token refuses resume but the pause itself remains.
func (r *PauseRecord) Expired(now time.Time) bool { return now.After(r.ExpiresAt) }

// PauseHandle is what a successful pause returns to its caller.
type PauseHandle struct {
	Token     string    `json:"token"`
	SessionID string    `json:"session_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func mintToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint resume token: %w", err)
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// PauseGate writes (or updates) the session's plane-B gate pause. The
// session must exist (pausing a typo is an operator error). A second
// gate pause on an already-gated session updates reason, message,
// metadata, and resume_at in place — the token and its expiry are kept
// (open question #5: single-writer per session, no stacking). Pausing
// an aborted session is refused: aborted is terminal. The returned
// bool reports whether a NEW gate pause was opened (a fresh token
// minted) as opposed to an in-place refresh of an already-active one —
// so a caller counting distinct pauses does not double-count a refresh
// (#50).
func (s *Store) PauseGate(ctx context.Context, userID, sessionID string, spec PauseSpec) (PauseHandle, bool, error) {
	if IsReservedSessionID(sessionID) {
		return PauseHandle{}, false, errReserved(sessionID)
	}
	if err := spec.validate(); err != nil {
		return PauseHandle{}, false, err
	}
	if userID == "" {
		var err error
		userID, err = s.findUserID(ctx, sessionID)
		if err != nil {
			return PauseHandle{}, false, err
		}
	}
	d, err := s.Get(ctx, userID, sessionID)
	if err != nil {
		return PauseHandle{}, false, err
	}
	if d.State == StateAborted {
		return PauseHandle{}, false, fmt.Errorf("session %q: %w", sessionID, ErrAlreadyAborted)
	}

	now := time.Now().UTC()
	rec := s.opsPauseRecords(ctx, userID, sessionID)[pauseGateKey]
	action := "gate pause"
	created := !rec.Active()
	if rec.Active() {
		// In-place update: reason/message/metadata/resume_at refresh,
		// token + expiry stay.
		action = "gate pause updated"
		rec.Reason, rec.Message, rec.Metadata, rec.ResumeAt = spec.Reason, spec.Message, spec.Metadata, spec.ResumeAt
	} else {
		token, err := mintToken()
		if err != nil {
			return PauseHandle{}, false, err
		}
		rec = &PauseRecord{
			Token: token, Plane: PlaneGate,
			App: s.appName, User: userID, SessionID: sessionID,
			Reason: spec.Reason, Message: spec.Message, Metadata: spec.Metadata,
			MintedAt: now, ExpiresAt: now.Add(spec.ttl()), ResumeAt: spec.ResumeAt,
		}
	}
	if err := s.writePauseRecord(ctx, userID, sessionID, pauseGateKey, rec,
		fmt.Sprintf("%s (%s): %s", action, rec.Reason, rec.Message)); err != nil {
		return PauseHandle{}, false, err
	}
	return PauseHandle{Token: rec.Token, SessionID: sessionID, ExpiresAt: rec.ExpiresAt}, created, nil
}

// PauseInterrupt mints the plane-A pause record for a parked
// interrupt (the pause_session tool body and the graph RequestInput
// helper call this). The park itself is the caller's ADK event; this
// only records the token. Re-minting for the same interrupt ID
// overwrites (last write wins — interrupt IDs are model-minted unique;
// a collision means a re-fire of the same call).
func (s *Store) PauseInterrupt(ctx context.Context, userID, sessionID, interruptID string, spec PauseSpec) (PauseHandle, error) {
	if IsReservedSessionID(sessionID) {
		return PauseHandle{}, errReserved(sessionID)
	}
	if interruptID == "" {
		return PauseHandle{}, errors.New("pause interrupt: interrupt ID must be non-empty")
	}
	if err := spec.validate(); err != nil {
		return PauseHandle{}, err
	}
	token, err := mintToken()
	if err != nil {
		return PauseHandle{}, err
	}
	now := time.Now().UTC()
	rec := &PauseRecord{
		Token: token, Plane: PlaneInterrupt, InterruptID: interruptID,
		App: s.appName, User: userID, SessionID: sessionID,
		Reason: spec.Reason, Message: spec.Message, Metadata: spec.Metadata,
		MintedAt: now, ExpiresAt: now.Add(spec.ttl()), ResumeAt: spec.ResumeAt,
	}
	if err := s.writePauseRecord(ctx, userID, sessionID, pauseIntrKeyPrefix+interruptID, rec,
		fmt.Sprintf("interrupt pause (%s) for %s: %s", rec.Reason, interruptID, rec.Message)); err != nil {
		return PauseHandle{}, err
	}
	return PauseHandle{Token: token, SessionID: sessionID, ExpiresAt: rec.ExpiresAt}, nil
}

// GatePause returns the session's active gate-pause record, if any.
// Read failures report nil — consistent with every other marker read:
// the ops row is an overlay, and an unreadable overlay must not wedge
// the session (the fail-open direction is deliberate here; abort and
// gate refusal are availability guards, not safety guards — the
// safety guard is the effects outbox, which fails closed).
func (s *Store) GatePause(ctx context.Context, userID, sessionID string) *PauseRecord {
	if IsReservedSessionID(sessionID) {
		return nil
	}
	if userID == "" {
		var err error
		userID, err = s.findUserID(ctx, sessionID)
		if err != nil {
			return nil
		}
	}
	rec := s.opsPauseRecords(ctx, userID, sessionID)[pauseGateKey]
	if !rec.Active() {
		return nil
	}
	return rec
}

// PauseRecords returns all pause records for one session (active and
// consumed), keyed as stored. Used by show, the boot scan, and tests.
func (s *Store) PauseRecords(ctx context.Context, userID, sessionID string) (map[string]*PauseRecord, error) {
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
	return s.opsPauseRecords(ctx, userID, sessionID), nil
}

// ScanPauses walks every session's ops row and returns all ACTIVE
// pause records. The daemon runs it once at boot to seed the token
// index and the timed-pause scheduler's heap; it is also FindToken's
// substrate. O(sessions) — the P1.3 eventlog/query surface owns
// fleet-scale indexing.
func (s *Store) ScanPauses(ctx context.Context) ([]*PauseRecord, error) {
	resp, err := s.svc.List(ctx, &adksession.ListRequest{AppName: s.appName})
	if err != nil {
		return nil, fmt.Errorf("scan pauses: list sessions: %w", err)
	}
	var out []*PauseRecord
	for _, sess := range resp.Sessions {
		if !strings.HasSuffix(sess.ID(), opsSuffix) {
			continue
		}
		primary := strings.TrimSuffix(sess.ID(), opsSuffix)
		for key, rec := range parsePauseRecords(sess, primary) {
			_ = key
			if rec.Active() {
				out = append(out, rec)
			}
		}
	}
	return out, nil
}

// FindToken resolves a resume token to its pause record (active or
// consumed — the caller distinguishes via ConsumedAt to give
// already_resumed its structured no-op).
func (s *Store) FindToken(ctx context.Context, token string) (*PauseRecord, error) {
	if token == "" || !strings.HasPrefix(token, tokenPrefix) {
		return nil, fmt.Errorf("token %q: %w", token, ErrTokenNotFound)
	}
	resp, err := s.svc.List(ctx, &adksession.ListRequest{AppName: s.appName})
	if err != nil {
		return nil, fmt.Errorf("find token: list sessions: %w", err)
	}
	for _, sess := range resp.Sessions {
		if !strings.HasSuffix(sess.ID(), opsSuffix) {
			continue
		}
		primary := strings.TrimSuffix(sess.ID(), opsSuffix)
		for _, rec := range parsePauseRecords(sess, primary) {
			if rec.Token == token {
				return rec, nil
			}
		}
	}
	return nil, ErrTokenNotFound
}

// ConsumeToken marks the token's pause record consumed — for a gate
// pause this IS the resume (the chokepoint stops refusing); for an
// interrupt pause the caller drives the resume turn and consumes on
// the durable append of the resume FunctionResponse
// (adversarial-gate finding M5: consumption keys on the append, not
// on turn completion — a resume turn that fails later has still
// legitimately ended the pause). Scope is checked before anything
// else (OQ #9): under v0.2's single-tenant reality the store's app is
// the scope, and FindToken already lists within s.appName so a
// cross-app token reads as not-found; the belt-and-suspenders App
// check below is where a per-tenant/per-user check slots in when
// multi-tenancy lands. Expired tokens refuse with ErrTokenExpired and
// leave the pause intact — this is the operator-facing resume path.
func (s *Store) ConsumeToken(ctx context.Context, token, by string) (*PauseRecord, error) {
	return s.consumeToken(ctx, token, by, true)
}

// ConsumeScheduled consumes for the timed-pause scheduler. Unlike the
// operator path it does NOT enforce the token's TTL: a resume_at is the
// daemon's own scheduled commitment, and the operator-facing token
// expiry (a guard against stale possession) must not veto it. A
// resume_at legitimately set beyond the token's life — the only way to
// schedule a pause longer than the TTL cap, which mint can only shorten
// — would otherwise livelock the scheduler, firing forever against an
// expired token. The already-consumed no-op and scope check still hold.
func (s *Store) ConsumeScheduled(ctx context.Context, token, by string) (*PauseRecord, error) {
	return s.consumeToken(ctx, token, by, false)
}

func (s *Store) consumeToken(ctx context.Context, token, by string, enforceExpiry bool) (*PauseRecord, error) {
	rec, err := s.FindToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if rec.App != s.appName {
		// Scope mismatch reads as not-found: possession of a token from
		// another scope must not confirm this scope's session exists.
		return nil, ErrTokenNotFound
	}
	if !rec.ConsumedAt.IsZero() {
		return rec, fmt.Errorf("token %s (session %q, consumed %s by %s): %w",
			token, rec.SessionID, rec.ConsumedAt.Format(time.RFC3339), rec.ConsumedBy, ErrAlreadyResumed)
	}
	if enforceExpiry && rec.Expired(time.Now().UTC()) {
		return rec, fmt.Errorf("token %s (session %q, expired %s): %w",
			token, rec.SessionID, rec.ExpiresAt.Format(time.RFC3339), ErrTokenExpired)
	}
	rec.ConsumedAt = time.Now().UTC()
	rec.ConsumedBy = by
	key := pauseGateKey
	if rec.Plane == PlaneInterrupt {
		key = pauseIntrKeyPrefix + rec.InterruptID
	}
	if err := s.writePauseRecord(ctx, rec.User, rec.SessionID, key, rec,
		fmt.Sprintf("pause resumed (%s) by %s", rec.Reason, by)); err != nil {
		return nil, err
	}
	return rec, nil
}

// ExtendToken moves the token's expiry to now+ttl — the audited
// operator path for lengthening a token's life (mint can only
// shorten). Consumed tokens cannot be extended.
func (s *Store) ExtendToken(ctx context.Context, token string, ttl time.Duration) (*PauseRecord, error) {
	if ttl <= 0 {
		return nil, errors.New("extend token: TTL must be positive")
	}
	rec, err := s.FindToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if rec.App != s.appName {
		return nil, ErrTokenNotFound
	}
	if !rec.ConsumedAt.IsZero() {
		return rec, fmt.Errorf("token %s: %w", token, ErrAlreadyResumed)
	}
	rec.ExpiresAt = time.Now().UTC().Add(ttl)
	key := pauseGateKey
	if rec.Plane == PlaneInterrupt {
		key = pauseIntrKeyPrefix + rec.InterruptID
	}
	if err := s.writePauseRecord(ctx, rec.User, rec.SessionID, key, rec,
		fmt.Sprintf("resume token extended to %s (operator action)", rec.ExpiresAt.Format(time.RFC3339))); err != nil {
		return nil, err
	}
	return rec, nil
}

// writePauseRecord serializes one record into its ops-row key with an
// audit event.
func (s *Store) writePauseRecord(ctx context.Context, userID, sessionID, key string, rec *PauseRecord, text string) error {
	blob, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode pause record: %w", err)
	}
	return s.appendOpsDelta(ctx, userID, sessionID, "pause", "operator", text,
		map[string]any{key: string(blob)})
}

// opsPauseRecords reads and parses all pause records on the session's
// ops row. Read failure = no records (overlay semantics).
func (s *Store) opsPauseRecords(ctx context.Context, userID, sessionID string) map[string]*PauseRecord {
	resp, err := s.svc.Get(ctx, &adksession.GetRequest{
		AppName: s.appName, UserID: userID, SessionID: opsSessionID(sessionID),
	})
	if err != nil {
		return nil
	}
	return parsePauseRecords(resp.Session, sessionID)
}

// parsePauseRecords extracts pause records from an ops-row session's
// state. Blanked values ("" — the abort purge) and unparseable blobs
// are skipped; a corrupt record must not wedge projection.
func parsePauseRecords(sess adksession.Session, sessionID string) map[string]*PauseRecord {
	out := make(map[string]*PauseRecord)
	for k, v := range sess.State().All() {
		if k != pauseGateKey && !strings.HasPrefix(k, pauseIntrKeyPrefix) {
			continue
		}
		blob, ok := v.(string)
		if !ok || blob == "" {
			continue
		}
		var rec PauseRecord
		if err := json.Unmarshal([]byte(blob), &rec); err != nil {
			continue
		}
		if rec.SessionID == "" {
			rec.SessionID = sessionID
		}
		out[k] = &rec
	}
	return out
}
