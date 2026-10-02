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

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-steer/mast/internal/attach"
	"github.com/go-steer/mast/internal/compose"
	"github.com/go-steer/mast/internal/eventlog"
	"github.com/go-steer/mast/internal/usagetrack"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// meterPool hands out one budget.Meter per session, sized from the
// workload bundle's budget block and the roster's per-specialist
// budgets.
type meterPool struct {
	mu   sync.Mutex
	cfg  budget.Config
	byID map[string]*budget.Meter

	// usage is the session's token breakdown, folded from the same
	// OnSpend hook the ledger is written from (#356). One per meter,
	// created and discarded with it: the counts it holds are this
	// process's, because the ledger stores no buckets to restore them
	// from.
	usage map[string]*usagetrack.Tracker

	// spend is the durable ledger (#175). Nil when the daemon has no
	// durable session store, in which case a ceiling is enforced against
	// this process's spend only — the pre-v0.5 behavior, and the reason
	// serve warns about it at startup. A nil *eventlog.SpendStore is
	// itself safe to call, so nothing below needs a nil check.
	spend *eventlog.SpendStore
	// guards is read, not written, here: the grants an operator handed
	// over on a reset live in the guardrail log, and replaying spend
	// without them would revoke a rescue the operator already paid for.
	// watchdogPool owns the writing.
	guards *eventlog.GuardrailStore
	app    string
	user   string
	// restored latches per session once the durable state has been
	// folded in, so the fold is one read per session per process rather
	// than one per turn. Set on success only — the same rule
	// watchdogPool follows, and safe for a second reason here:
	// budget.Meter.Restore refuses a second fold outright, so a retry
	// after a failed read cannot double-count.
	restored map[string]bool
	// writeFailures counts ledger appends dropped per session, so a
	// broken database says so once at Error and then stops shouting on
	// every model call.
	writeFailures map[string]int
	logger        *slog.Logger
}

func newMeterPool(bundle *workload.Bundle, specs []specialists.Spec, provider, modelName string) *meterPool {
	// Pricing lives in the shared core (internal/compose.MeterLimits)
	// so the daemon and mast.RunWorkload derive identical costs.
	limits := compose.MeterLimits(provider, modelName)
	if bundle != nil {
		limits.MaxCostUSD = bundle.Budget.MaxCostUSD
		// Workload turn ceiling: one "turn" = one model call (see
		// budget.Limits.MaxTurns for the vocabulary).
		limits.MaxTurns = bundle.Budget.MaxTurns
	}
	// Not a ceiling but a policy about what a specialist stopped by one
	// may still say: one model call, report tool only (pkg/budget's
	// finalreport.go). Off unless the bundle asks.
	finalReport := bundle != nil && bundle.Budget.FinalReport
	// Per-specialist ceilings compose under the workload's; a
	// specialist that declares a tighter cap spends it on its own and
	// closes that one path — the turn routes on (pkg/budget, "Scopes",
	// and budget.Scope for who a ceiling belonged to).
	cfg := budget.Config{
		Limits:      limits,
		Scopes:      compose.MeterScopes(specs, provider, modelName),
		FinalReport: finalReport,
	}
	return &meterPool{
		cfg:           cfg,
		byID:          map[string]*budget.Meter{},
		usage:         map[string]*usagetrack.Tracker{},
		restored:      map[string]bool{},
		writeFailures: map[string]int{},
	}
}

func (mp *meterPool) meter(sessionID string) *budget.Meter {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	m, ok := mp.byID[sessionID]
	if !ok {
		cfg := mp.cfg
		tracker := usagetrack.New()
		// Each session's meter writes its own rows, so the hook closes
		// over the session ID here rather than the pool threading it
		// through budget.Spend. pkg/budget stays a package about
		// arithmetic that knows nothing about sessions.
		//
		// The tracker goes on the same hook and not a second one: the
		// ledger and the usage report are two readings of the same call,
		// and a call that reached one and not the other is a discrepancy
		// with no way to explain it. The tracker is folded first because
		// it only takes its own lock, while the ledger append blocks on a
		// database write.
		cfg.OnSpend = func(s budget.Spend) {
			tracker.Record(s)
			mp.record(sessionID, s)
		}
		m = budget.New(cfg)
		mp.byID[sessionID] = m
		if mp.usage == nil {
			mp.usage = map[string]*usagetrack.Tracker{}
		}
		mp.usage[sessionID] = tracker
	}
	return m
}

// usageInfo is what GET /sessions/{id}/usage answers with.
//
// The turn count and cost come off the meter rather than the tracker, so
// the number here is the one the ceiling is enforced against — including
// spend restored from a previous process, which the tracker has no
// buckets for and cannot hold. usagetrack.Tracker.Info owns the
// reconciliation; this is only the two reads.
func (mp *meterPool) usageInfo(sessionID string) attach.UsageInfo {
	_, cost, calls := mp.meter(sessionID).Snapshot()

	mp.mu.Lock()
	t := mp.usage[sessionID]
	mp.mu.Unlock()
	if t == nil {
		return attach.UsageInfo{Overall: attach.UsageTotals{Turns: calls, CostUSD: cost}}
	}
	return t.Info(usagetrack.Cumulative{Turns: calls, CostUSD: cost})
}

// durable gives the pool a ledger to write spend to and the guardrail
// log to read grants from, so a ceiling stops bounding what a workload
// spends *per process*.
//
// Wired on the same condition as watchdogPool.durable, and for a related
// reason: the ledger is what makes a crossed ceiling survive a restart,
// and POST /guardrails/reset is the only thing that clears one. Without
// an attach listener the operator's recourse would be editing
// max_cost_usd in the bundle and restarting — real, but a different
// promise than the endpoint makes. --attach-listen implies --session-db
// (#329), so the store exists exactly when the reset does.
func (mp *meterPool) durable(spend *eventlog.SpendStore, guards *eventlog.GuardrailStore, logger *slog.Logger) {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	mp.spend, mp.guards = spend, guards
	mp.app, mp.user, mp.logger = appName, defaultUserID, logger
	if mp.restored == nil {
		mp.restored = map[string]bool{}
	}
	if mp.writeFailures == nil {
		mp.writeFailures = map[string]int{}
	}
}

// record appends one priced call to the ledger. Called from the meter's
// OnSpend hook, on the turn's own goroutine, outside the meter's lock.
//
// Synchronous, on a background context with a short deadline. A model
// call takes seconds and an indexed insert takes microseconds, so a
// queue would buy nothing but a window in which the rows that matter
// most are the ones still in it. The context is not the turn's, for the
// guardrail store's reason: the call that crosses a ceiling is the one
// whose turn is being cancelled by the crossing.
func (mp *meterPool) record(sessionID string, s budget.Spend) {
	mp.mu.Lock()
	store, app, user, logger := mp.spend, mp.app, mp.user, mp.logger
	mp.mu.Unlock()
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), spendWriteTimeout)
	defer cancel()
	err := store.Append(ctx, app, user, sessionID, eventlog.SpendRecord{
		Author:   s.Author,
		Tokens:   s.Tokens,
		CostUSD:  s.CostUSD,
		Unpriced: s.Unpriced,
	})
	if err == nil {
		return
	}
	mp.mu.Lock()
	mp.writeFailures[sessionID]++
	n := mp.writeFailures[sessionID]
	mp.mu.Unlock()
	if logger == nil {
		return
	}
	if n == 1 {
		// The ceiling still holds in this process — the in-memory meter
		// has the spend. What is lost is its survival of a restart, which
		// is the whole point of the ledger and is worth saying plainly,
		// once.
		logger.Error("budget spend is not durable: could not append to the ledger; this session's ceiling will not survive a restart",
			"session", sessionID, "cost_usd", fmt.Sprintf("%.6f", s.CostUSD), "error", err.Error())
		return
	}
	logger.Debug("budget spend ledger append failed again",
		"session", sessionID, "dropped_calls", n, "error", err.Error())
}

// spendWriteTimeout bounds one ledger append. Long enough that a busy
// SQLite writer is not cut off mid-lock, short enough that a wedged
// database cannot stall the event loop of every turn behind it.
const spendWriteTimeout = 5 * time.Second

// restore folds the session's durable spend, and the grants operators
// have handed it, into this process's meter — once per session.
//
// Called at the top of every turn, and from the guardrail read and reset
// paths, for the reason watchdogPool.restore is: a session's first
// appearance in a fresh daemon is a turn or an operator poll, and either
// one must see what the session already spent rather than a clean cap.
//
// Grants are replayed here, which #166 deliberately did not do. Its
// reasoning was that raising a ceiling over an accumulator that had
// forgotten what it spent is arithmetic on a number that no longer means
// anything — true then, and it stops being true on the line above. With
// spend durable the reverse becomes the bug: a session an operator
// rescued at $5.02 against a $5.00 cap would come back with the spend and
// without the rescue, wedged by a restart the operator never made and
// with their own grant sitting in the audit log. Only grants that
// recorded which scope they were aimed at are replayed; see
// eventlog.GuardrailRecord.GrantScope for why a pre-#175 row cannot be.
//
// Fails open, loudly, the same way and for the same reason: a guardrail
// that cannot be read is a reason to log and continue, not to convert a
// storage fault into an outage. The ceiling is still armed against this
// process's own spend, and every ledger row stays on disk for the next
// attempt.
func (mp *meterPool) restore(ctx context.Context, sessionID string) {
	mp.mu.Lock()
	spend, guards, app, user, done, logger := mp.spend, mp.guards, mp.app, mp.user, mp.restored[sessionID], mp.logger
	mp.mu.Unlock()
	if spend == nil || done {
		return
	}

	st, err := spend.Fold(ctx, app, user, sessionID)
	if err != nil {
		if logger != nil {
			logger.Warn("could not read this session's durable spend; the ceiling is enforced against this process only",
				"session", sessionID, "error", err.Error())
		}
		return
	}
	grants, err := guards.Fold(ctx, app, user, sessionID)
	if err != nil {
		// Both halves or neither. Restoring spend without the grants that
		// answer it would wedge a session an operator already rescued,
		// which is a worse failure than the one this whole path fixes —
		// so leave the latch unset and try again next turn.
		if logger != nil {
			logger.Warn("could not read this session's guardrail grants; leaving its durable spend unrestored for now",
				"session", sessionID, "error", err.Error())
		}
		return
	}

	prior := budget.Prior{
		Session:  budget.Totals(st.Session),
		Unpriced: st.Unpriced,
	}
	if len(st.ByAuthor) > 0 {
		prior.ByAuthor = make(map[string]budget.Totals, len(st.ByAuthor))
		for author, t := range st.ByAuthor {
			prior.ByAuthor[author] = budget.Totals(t)
		}
	}

	m := mp.meter(sessionID)
	if !prior.IsZero() {
		if rerr := m.Restore(prior); rerr != nil {
			// budget.ErrRestored: a concurrent caller — an operator poll
			// racing the turn that woke the session — got there first.
			// The fold is idempotent and the meter is already correct, so
			// this is a latch to set, not a failure to report.
			if !errors.Is(rerr, budget.ErrRestored) && logger != nil {
				logger.Warn("could not restore this session's durable spend",
					"session", sessionID, "error", rerr.Error())
			}
			mp.markRestored(sessionID)
			return
		}
	}
	mp.markRestored(sessionID)

	for scope, g := range grants.GrantsByScope {
		add := budget.Limits{MaxCostUSD: g.BudgetAddedUSD, MaxTokens: g.TokensAdded, MaxTurns: g.TurnsAdded}
		if _, gerr := m.Grant(scope, add); gerr != nil && logger != nil {
			// The named specialist is gone from the roster. The grant is
			// unapplicable rather than lost — it is still in the audit
			// log — but an operator whose rescue silently stopped applying
			// needs to hear it.
			logger.Warn("could not replay an operator grant after restart; the scope it named is no longer in this workload",
				"session", sessionID, "scope", scope, "error", gerr.Error())
		}
	}

	if logger == nil || (prior.IsZero() && len(grants.GrantsByScope) == 0) {
		return
	}
	tokens, cost, calls := m.Snapshot()
	logger.Info("restored spend recorded before this process started",
		"session", sessionID,
		"cost_usd", fmt.Sprintf("%.4f", cost), "tokens", tokens, "model_calls", calls,
		"grants_replayed", len(grants.GrantsByScope))
}

// markRestored latches the session so the fold does not repeat.
func (mp *meterPool) markRestored(sessionID string) {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	if mp.restored == nil {
		mp.restored = map[string]bool{}
	}
	mp.restored[sessionID] = true
}

// preflight refuses a turn on a session that is already past a ceiling,
// before any model call.
//
// The meter alone would catch it one call later — enforcement is derived
// on each event, so the first priced event of the next turn crosses the
// same ceiling and aborts the turn. That was tolerable while a restart
// cleared the accumulator: the wedge lasted a process. Durable spend
// makes it permanent, so a scheduler firing every minute against a
// session nobody has reset would buy one model call a minute, forever.
// A ceiling that keeps costing money after it trips is not a ceiling.
//
// Same shape and same error as the watchdog's refusal, including the
// reset endpoint in the text, because from the caller's side they are
// the same event: this session will not run until an operator says so.
func (mp *meterPool) preflight(sessionID string) error {
	// Precluded, not crossed. Since W10.2 a ceiling is checked before the
	// call as well as after, so a wedged session no longer has to have
	// *crossed* anything — a workload that stopped exactly on its turn cap
	// has zero trips and cannot make another call. Asking Trips() here
	// would let it start a turn that refuses its own first call and answer
	// "I am out of budget" forever, which is the wedge this function
	// exists to turn into a 409.
	//
	// The session's own ceilings, not the roster's (W10.3). A spent
	// specialist closes one path; the session still has a coordinator, the
	// rest of the roster and every non-dispatching tool, and refusing the
	// turn at the door would make a per-specialist cap a way to kill the
	// workload — the opposite of what a per-specialist cap is for.
	trips := mp.meter(sessionID).PrecludedSession()
	if len(trips) == 0 {
		return nil
	}
	return fmt.Errorf("session %q is over its budget (%s); it refuses new turns until an operator resets it (POST /sessions/%s/guardrails/reset)",
		sessionID, joinReasons(trips), sessionID)
}
