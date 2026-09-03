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

// Originally derived from go-steer/core-agent@25d8531cf8d1d69459471009a9e7e2e9b0dff1e2
// (pkg/attach/guardrails.go), with mast's budget shape added: the wire
// names and status codes are core-agent's so one client speaks both,
// and the token / turn / per-specialist fields are mast's, because a
// dollars-only view of mast's budget reports "tripped: true, spent
// $0.0061" on a session stopped by a turn cap.
//
// Guardrail state + reset (#135).
//
// mast's cost ceiling is real and unattended: `budget:` in the workload
// bundle bounds the session, `budget:` on a specialist bounds that
// specialist, and pkg/budget's meter cancels the run context the moment
// either is crossed. What did not exist was any way out — the meter
// derives enforcement from accumulator-vs-ceiling on every event, so a
// session past its cap crosses it again on the first priced event of
// the next turn, forever. Before this endpoint the only recovery was
// restarting the daemon, which drops every other session's in-flight
// turn with it.
//
//   - GET  /sessions/{id}/guardrails       — what is armed, what tripped, why
//   - POST /sessions/{id}/guardrails/reset — clear it, with the runway to make that stick
//
// "Reset" means raising the ceiling. It never zeroes or re-windows the
// accumulator: /usage, the eventlog-derived cost, and the ceiling check
// all keep counting the same dollars, so a post-incident review of a
// session that spent $40 does not find it reporting $10.
//
// A reset that would provably not work is refused rather than
// performed: a trip whose accumulator is still past the ceiling after
// the requested grant gets 409 with the numbers, not a 200 that
// re-trips on the operator's next turn.

package attach

import "errors"

// ErrGuardrailRetrip is returned by GuardrailResetter implementations
// when the requested reset would be undone by the next turn — a cost
// trip whose accumulated spend is still at or past the ceiling once
// the requested budget is added. The handler maps it to 409 Conflict:
// the request was well-formed and the operator is authorized, but the
// state makes it a no-op, and a 200 that silently achieves nothing is
// exactly the failure mode this endpoint exists to remove. The error
// text carries the spend and the ceiling so the operator knows how
// much to add.
var ErrGuardrailRetrip = errors.New("attach: reset would immediately re-trip; additional budget required")

// Guardrail names accepted by GuardrailResetRequest.Guardrail and
// reported in GuardrailResetResponse.Reset.
const (
	// GuardrailWatchdog is the behavioral watchdog.
	GuardrailWatchdog = "watchdog"
	// GuardrailCostCeiling is the budget meter's ceilings — cost,
	// tokens, and turns, for the session and for each specialist
	// scope. One name covers all of them because they are one
	// enforcement point: any crossed ceiling stops the same run.
	GuardrailCostCeiling = "cost_ceiling"
	// GuardrailAll targets every guardrail. The default when the
	// request names none.
	GuardrailAll = "all"
)

// GuardrailInfo is the response shape of GET /sessions/.../guardrails —
// the operator-facing answer to "why is this session refusing my
// prompts, and what do I do about it?"
type GuardrailInfo struct {
	Watchdog    WatchdogInfo    `json:"watchdog"`
	CostCeiling CostCeilingInfo `json:"cost_ceiling"`
	// Halted is true when at least one guardrail has tripped, so a
	// client can render the banner without knowing which backstops
	// exist.
	Halted bool `json:"halted"`
}

// WatchdogInfo reports the watchdog's configured posture and whether it
// has halted the session.
type WatchdogInfo struct {
	// Mode is the resolved watchdog mode. mast ships "warn" only.
	Mode string `json:"mode"`
	// Tripped is true when a runaway pattern halted the agent and the
	// operator hasn't reset it. Always false while Advisory is true.
	Tripped bool `json:"tripped"`
	// Reason is the operator-facing explanation: the trip's, or — for
	// an advisory watchdog, which never trips — the most recent
	// alert's. Empty when nothing has fired.
	Reason string `json:"reason,omitempty"`

	// Advisory is true when this watchdog cannot halt a session
	// whatever it observes — mast's logs its alerts and lets the turn
	// run on. A client that renders a "watchdog armed" indicator
	// without it would be promising an enforcement that isn't there;
	// the reset is still meaningful (it clears the accumulated signal
	// state), it just isn't recovery.
	//
	// mast-native: core-agent's watchdog has an enforce mode, so its
	// GuardrailInfo needs no such field.
	Advisory bool `json:"advisory,omitempty"`
	// Alerts counts the signals that have fired on this session and
	// not yet been cleared by a reset.
	Alerts int `json:"alerts,omitempty"`
}

// CostCeilingInfo reports the configured budget ceilings, the session's
// usage against them, and whether one has halted the session.
//
// Three dimensions, not one: mast's budget bounds cost, tokens, and
// model calls independently, and a turn-capped session that has spent
// six tenths of a cent is halted just as hard as one that blew $10.
// MaxTurnUSD / MaxSessionUSD / SessionCostUSD keep core-agent's names
// so a client written against either daemon renders the dollar case
// unchanged.
type CostCeilingInfo struct {
	// MaxTurnUSD is the per-turn spend cap. Always 0 here: mast meters
	// per session and per specialist, and has no per-turn dollar
	// bound. Reported rather than omitted so a shared client's
	// "unlimited" rendering is a fact and not a missing field.
	MaxTurnUSD float64 `json:"max_turn_usd"`
	// MaxSessionUSD is the session's cost cap, including any budget
	// added by a prior reset. 0 means that bound is disabled.
	MaxSessionUSD float64 `json:"max_session_usd"`
	// SessionCostUSD is the session's cumulative spend — the same
	// accumulator the ceiling check measures.
	SessionCostUSD float64 `json:"session_cost_usd"`

	// MaxTokens / Tokens and MaxTurns / Turns are the other two
	// dimensions, same rules: 0 max means unbounded, one "turn" is one
	// model call (pkg/budget's unit, not one operator prompt).
	//
	// mast-native.
	MaxTokens int64 `json:"max_tokens"`
	Tokens    int64 `json:"tokens"`
	MaxTurns  int   `json:"max_turns"`
	Turns     int   `json:"turns"`

	Tripped bool   `json:"tripped"`
	Reason  string `json:"reason,omitempty"`
	// WouldRetrip is true when clearing the trip without additional
	// budget would be undone by the next turn's enforcement pass.
	// mast derives enforcement from usage-vs-ceiling with no flag in
	// between, so for mast this is true whenever Tripped is: there is
	// nothing a bare reset could clear. Clients use it to require a
	// budget input before offering the reset.
	WouldRetrip bool `json:"would_retrip"`

	// Scopes reports each specialist that carries its own ceilings.
	//
	// A crossed scope closes that specialist's path and leaves the
	// session running (v0.6 W10.3); through v0.5 it stopped the whole
	// session, because cancelling the run context was the meter's only
	// lever. That is why this list is not redundant with Tripped: a
	// workload can be serving turns through the rest of its roster
	// while a specialist here sits spent, and an operator who resets
	// the session cap without reading this has bought nothing for the
	// path that actually stopped.
	//
	// mast-native. Empty when no specialist declares a budget.
	Scopes []ScopeCeilingInfo `json:"scopes"`
}

// Configured reports whether any budget ceiling is in force. False
// means the session has no cost guardrail at all, which is what the
// cost_ceiling capability flag advertises.
func (c CostCeilingInfo) Configured() bool {
	return c.MaxSessionUSD > 0 || c.MaxTokens > 0 || c.MaxTurns > 0 || c.MaxTurnUSD > 0
}

// ScopeCeilingInfo is one specialist's ceilings and usage under the
// session's. Name is the specialist's spec name, which is the author
// the meter attributes events to.
type ScopeCeilingInfo struct {
	Name       string  `json:"name"`
	MaxCostUSD float64 `json:"max_cost_usd"`
	CostUSD    float64 `json:"cost_usd"`
	MaxTokens  int64   `json:"max_tokens"`
	Tokens     int64   `json:"tokens"`
	MaxTurns   int     `json:"max_turns"`
	Turns      int     `json:"turns"`
	Tripped    bool    `json:"tripped"`
	Reason     string  `json:"reason,omitempty"`
}

// GuardrailResetRequest is the body of POST
// /sessions/.../guardrails/reset. An empty body is legal and means
// "clear whatever tripped".
type GuardrailResetRequest struct {
	// Guardrail selects what to clear: "watchdog", "cost_ceiling", or
	// "all" (the default when empty).
	Guardrail string `json:"guardrail,omitempty"`
	// AdditionalBudgetUSD raises the cost ceiling by this many dollars
	// as part of the reset. Required when cost is what tripped —
	// otherwise the reset provably re-trips. Rejected (not silently
	// dropped) when Guardrail is "watchdog": budget has no meaning
	// there, and quietly discarding it would let an operator believe
	// they'd bought runway they hadn't.
	AdditionalBudgetUSD float64 `json:"additional_budget_usd,omitempty"`

	// AdditionalTokens / AdditionalTurns raise the other two
	// dimensions. A session stopped by `max_turns: 40` needs turns,
	// not dollars, and a dollars-only reset endpoint would leave it
	// exactly as wedged as before.
	//
	// mast-native.
	AdditionalTokens int64 `json:"additional_tokens,omitempty"`
	AdditionalTurns  int   `json:"additional_turns,omitempty"`

	// Scope names the specialist whose ceilings to raise. Empty is the
	// session's own. A raise never *imposes* a ceiling: a dimension
	// that was unlimited stays unlimited.
	//
	// mast-native.
	Scope string `json:"scope,omitempty"`

	// Caller is the authenticated identity performing the reset,
	// recorded on the audit event so a post-incident review can answer
	// "who handed this session more runway?".
	//
	// json:"-" deliberately: it is stamped by the handler from the auth
	// context and never read off the wire. A client-supplied value
	// would be an attribution a caller writes about themselves.
	Caller string `json:"-"`
}

// GuardrailResetResponse reports what the reset actually did. The
// post-reset state is echoed so a client needs no follow-up GET.
type GuardrailResetResponse struct {
	// Reset names the guardrails this call cleared. Empty when nothing
	// was tripped — not an error: a defensive reset is legitimate.
	Reset []string `json:"reset"`
	// BudgetAddedUSD / TokensAdded / TurnsAdded are what the ceilings
	// were actually raised by (0 when none was requested, or when the
	// targeted dimension was already unlimited).
	BudgetAddedUSD float64 `json:"budget_added_usd,omitempty"`
	TokensAdded    int64   `json:"tokens_added,omitempty"`
	TurnsAdded     int     `json:"turns_added,omitempty"`
	// Guardrails is the post-reset state, same shape as GET.
	Guardrails GuardrailInfo `json:"guardrails"`
	// Message is the operator-facing one-liner a client renders.
	Message string `json:"message,omitempty"`
}

// GuardrailProvider is the optional capability for
// GET /sessions/.../guardrails. Absence reports zero-value state
// rather than 501, matching the other read-only projections.
type GuardrailProvider interface {
	AttachGuardrails() GuardrailInfo
}

// GuardrailResetter is the optional capability for
// POST /sessions/.../guardrails/reset. Unlike the read side, absence is
// a 501: an operator who POSTs a reset must know whether it took
// effect.
//
// Implementations return an error for a reset that cannot work —
// notably a cost trip the requested budget doesn't clear, which the
// handler translates to 409 rather than 500 (see ErrGuardrailRetrip).
// The returned response should still carry Guardrails in that case, so
// the 409 body tells the operator how much would have been enough.
type GuardrailResetter interface {
	AttachResetGuardrail(req GuardrailResetRequest) (GuardrailResetResponse, error)
}
