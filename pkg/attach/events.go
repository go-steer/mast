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

package attach

import "time"

// protocolVersion is the SSE event-stream protocol semver this
// server speaks. Bumped on any change to the contract per
// go-steer/core-tui's docs/sse-event-stream-protocol.md.
//
// Negotiation (#389): a client MAY declare the version it speaks on an
// /events request via the ?protocol= query param or the
// X-Attach-Protocol-Version header. The server echoes this constant
// back on every /events response header and rejects a declared major
// that differs from protocolVersion's major with 409 Conflict (a
// malformed declaration is 400). Clients that declare nothing are
// accepted unchanged for back-compat. See negotiateProtocolVersion.
//
// v1.1.0 (core-tui#42): turn-complete.cost_usd demoted from required
// to optional with documented fallback semantics (the immediately-
// following usage-update carries authoritative cost). This server
// emits TurnComplete with CostUSD = nil so the field is omitted from
// the wire entirely — the "cost deferred" signal is explicit.
//
// v1.2.0 (#277): tool-result response payloads now carry a
// `latency_ms` sidecar (int64, milliseconds) reporting the wall-
// clock time spent in the upstream tool call. Additive — consumers
// on older schema versions simply don't see the field. Populated
// by both the MCP digest wrap (pkg/mcp/digest_wrap.go) and the
// plain rename passthrough (pkg/mcp/namespace.go), so operators
// see per-call timing whether digest is enabled or not.
//
// v1.3.0 (#223 Phase 4): tool-result response payloads now carry
// an optional `savings` object reporting the digest wrap's per-call
// byte + token reduction, router path, and (agentic path only)
// subagent usage. Sidecar rides the same response-map channel as
// v1.2.0's latency_ms. Fully additive.
//
// v1.4.0 (#329): capabilities frame extended with four optional
// fields — `features` (feature-flag map), `slash_commands` (dynamic
// list of server-side slash names), `agent` (name/version/model/
// provider/url/description identity block), and `caller_id` (resolved
// Caller.Identity). Enables backend-agnostic clients (mast-web) to
// render without a code change per producer. Also spec'd an optional
// `capabilities` merge field on status-update for future hot changes.
//
// ADK v2.2.0 (no version bump): the `agent` frame embeds ADK's
// session.Event verbatim, and v2.2.0 put JSON tags on that struct —
// its fields went from PascalCase (`Content`, `Author`, `Actions`,
// `StateDelta`) to camelCase with omitempty, for adk-python interop.
// mast does not own that shape and never specified it, so the version
// stays at 1.4.0 rather than claiming a schema change a client could
// negotiate around. Recorded here because it is nonetheless visible on
// the wire: a consumer reading those field names must accept both
// casings, as mast-web's attach protocol normalizer already does.
// Decoding is unaffected in both directions — encoding/json matches
// field names case-insensitively — so session rows written by either
// version still load.
//
// v1.5.0 (#206): new `canceled` value in the turn-error `kind` enum
// (§2.6), carrying `retryable: false`. A cancelled turn used to be
// reported as `transient_network` / `retryable: true`, which told a
// client to offer a re-run of work an operator — or the watchdog —
// had just deliberately stopped. Additive in the same way
// `cost_ceiling` was: no new event type, no new field, and §2.6
// already requires a consumer to treat an unrecognized kind as
// `unknown`, so an older client has a defined fallback rather than
// undefined behavior. It is still a behavior change for this one
// input, and deliberately so: a client keying a retry affordance off
// `retryable` stops offering one after an interrupt.
//
// v1.6.0 (#208): new `watchdog_halt` value in the turn-error `kind`
// enum (§2.6), carrying `retryable: false`. v1.5.0 covered the turn the
// watchdog cancels in flight; this covers the refusal that follows it —
// the halt every subsequent turn hits until an operator resets. Those
// used to be classified by substring-scanning a reason built out of the
// looping tool's name and its model-supplied arguments, which reported
// a guardrail halt as `config_error`, `model_not_found` or `auth_error`
// depending on what the agent had decided to call things. Additive on
// the same terms as 1.5.0: a new kind value, no new event type, no new
// field, §2.6's `unknown` fallback intact.
//
// The two version lines have diverged, and a client should not read
// across them. core-agent shipped the same `canceled` value at its
// 1.8.0, having spent 1.5.0-1.7.0 on session titles, ACLs and wake
// frames that mast does not have. So "≥1.5.0" is a statement about a
// mast server only; feature-detect against `event_types` and the
// `features` map rather than inferring a kind's existence from a
// number. Both servers do agree on the fallback that makes this safe:
// §2.6 requires an unrecognized kind to read as `unknown`.
const protocolVersion = "1.6.0"

// SSE event-type names per the protocol spec (section 2).
const (
	EventCapabilities = "capabilities"
	EventStatusUpdate = "status-update"
	EventUsageUpdate  = "usage-update"
	EventInbox        = "inbox"
	EventTurnComplete = "turn-complete"
	EventTurnError    = "turn-error"

	// EventAgent is the legacy event type carrying ADK session.Event
	// payloads (stream-chunk / tool-call / tool-result are all
	// multiplexed onto this one event today). Kept for back-compat
	// indefinitely — Phase 1 clients in poll mode rely on it, and
	// even push-mode clients still consume it for the model's
	// streamed text output.
	EventAgent = "agent"
)

// supportedEventTypes lists every event type this server emits.
// Surfaced in the Capabilities event on stream open so consumers
// can detect push-mode support without probing. The list includes
// legacy sub-types (stream-chunk / tool-call / tool-result) even
// though they all ride on EventAgent today — the consumer cares
// about the logical surface, not the SSE event name they currently
// share.
var supportedEventTypes = []string{
	EventStatusUpdate,
	EventUsageUpdate,
	EventInbox,
	EventTurnComplete,
	EventTurnError,
	"stream-chunk",
	"tool-call",
	"tool-result",
}

// Capabilities is the first frame on every newly-opened stream
// (spec section 2.1). Required so clients can decide push vs poll.
//
// v1.4.0 (#329) added the Features/SlashCommands/Agent/CallerID
// fields. All four are optional — older clients that don't know
// about them ignore silently; older servers omit them and the
// consumer sees the pre-1.4.0 shape unchanged.
type Capabilities struct {
	ProtocolVersion string   `json:"protocol_version"`
	EventTypes      []string `json:"event_types"`
	Server          string   `json:"server,omitempty"`

	// Features is a feature-flag map derived from live runtime state
	// (are MCP servers registered? does the daemon have multi-session
	// on? etc.). Consumers should treat absent keys as "off / unknown"
	// and unknown keys as forward-compat additions. Suggested initial
	// keys are defined as the feature* string constants below.
	Features map[string]bool `json:"features,omitempty"`

	// SlashCommands lists the server-side slash-command names the
	// producer will accept via POST /sessions/.../slash/<name>. Derived
	// from capability-interface presence, not a registry table. Clients
	// use this to render only the slashes that will actually work
	// against the connected agent.
	SlashCommands []string `json:"slash_commands,omitempty"`

	// Agent identifies the producing agent — same source that feeds
	// the /.well-known/agent-card.json endpoint plus per-session
	// runtime state (model/provider). Consolidates fields today
	// scattered across the agent card, GET /status, and the free-form
	// Server banner. Absent when the server doesn't know its own
	// identity (rare — implies neither AgentCardConfig nor a
	// StatusProvider is wired).
	Agent *AgentIdentity `json:"agent,omitempty"`

	// CallerID is the resolved Caller.Identity after the auth
	// middleware ran, echoed back to the consumer as a display hint.
	// The canonical source is GET /whoami (which also carries admin
	// + auth source). Empty when the caller couldn't be resolved.
	CallerID string `json:"caller_id,omitempty"`
}

// Feature flag keys advertised on Capabilities.Features. String
// constants (not a typed enum) so downstream clients — mast-web,
// the coretui adapter, operator scripts — don't have to know a Go
// type to reason about them. Servers MAY advertise additional keys
// clients don't know about (forward-compat); clients MUST NOT crash
// on unknown ones.
const (
	// featureMultiSession is true when the server enforces per-session
	// ACLs (Options.MultiSessionEnabled) — clients rendering a
	// session picker use it to decide whether to show "your sessions"
	// vs an unfiltered fleet view.
	featureMultiSession = "multi_session"
	// featurePermsStream is true when the agent implements the
	// PromptBrokerProvider capability — clients gate the
	// /perms/stream + /perms/respond wiring on it.
	featurePermsStream = "perms_stream"
	// featureCostCeiling is true when the session has a budget ceiling
	// in force — cost, tokens, or model calls — and can therefore be
	// halted for spend. Sourced from the guardrail capability
	// (CapabilityReport.CostCeiling), so it tracks what the workload
	// bundle actually declared rather than what the daemon could in
	// principle enforce.
	featureCostCeiling = "cost_ceiling"
	// featureGuardrails is true when GET /guardrails and POST
	// /guardrails/reset are serviceable — the client can show a
	// tripped-guardrail banner and offer the reset (#135). Distinct
	// from cost_ceiling, which says a spend cap exists; a session can
	// carry no budget at all and still answer the guardrail surface.
	featureGuardrails = "guardrails"
	// featureObserverMode is true when the producer exposes a
	// LiveAgent observer surface. Reserved for the observer-mode
	// integration; absent today.
	featureObserverMode = "observer_mode"
	// featureMCP is true when the agent implements MCPProvider
	// (there's at least one MCP server declared).
	featureMCP = "mcp"
	// featureSpecialists is true when the agent supports the
	// SubagentSpawner capability (POST /slash/subagent will work
	// against the agent).
	featureSpecialists = "specialists"
	// featureCrossDaemon is true when the server hosts the peer
	// registry (Options.PeerRegistry != nil) — clients use it to
	// enable the multi-daemon fleet picker.
	featureCrossDaemon = "cross_daemon"
	// featureInterrupt is true when the agent implements
	// InterruptProvider — clients gate ESC → cancel wiring on it.
	featureInterrupt = "interrupt"
)

// AgentIdentity is the capabilities.agent block — the producer's
// own identity, consolidating agent-card + per-session status
// fields the client would otherwise have to fan-out fetches to
// assemble. Every field is optional; consumers render only what's
// present.
type AgentIdentity struct {
	Name        string `json:"name,omitempty"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
	Provider    string `json:"provider,omitempty"`
	URL         string `json:"url,omitempty"`
}

// Turn-state values per spec section 2.2.
const (
	TurnStateIdle               = "idle"
	TurnStateStreaming          = "streaming"
	TurnStateAwaitingPermission = "awaiting_permission"
	TurnStateAwaitingElicit     = "awaiting_elicit"
)

// StatusUpdate is emitted on session-level state changes (turn
// start/end, model swap, perm-mode change, provider change) and
// also once right after Capabilities as a full snapshot.
//
// Merge semantics: fields not present in an update are unchanged on
// the consumer side. TurnState is always present on every emission
// (snapshot or delta) per spec.
//
// Capabilities (v1.4.0+) is an optional merge frame carrying hot
// changes to the capabilities the server advertised on stream
// open — e.g. an MCP server registers mid-session and features.mcp
// flips true, or a new slash provider gets wired. Semantics: any
// field the server sets is merged into the consumer's cached
// capabilities; fields absent from the update stay as-is. Not
// emitted by this server today (spec'd for future use); consumers
// MUST tolerate its absence.
type StatusUpdate struct {
	Model        string        `json:"model,omitempty"`
	Provider     string        `json:"provider,omitempty"`
	PermMode     string        `json:"perm_mode,omitempty"`
	TurnState    string        `json:"turn_state"`
	ContextPct   *int          `json:"context_pct,omitempty"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// UsageUpdate is emitted after each turn finalizes and as a
// cumulative snapshot on stream open. ByModel is optional — present
// when the tracker has more than one model bucketed (typical when
// --agentic-tools routes subtasks to a small model). LastTurn is
// optional — populated on turn-end emissions (the tracker.Append
// callback path), omitted on the snapshot emission at stream open
// where there's no meaningful "last turn" to attribute.
//
// LastTurn carries the just-completed turn's per-turn cost so
// operator surfaces (remote TUI's per-turn footer) can render
// authoritative cost without needing client-side pricing lookups.
// The server's tracker owns the pricing catalog + cache-discount
// math; clients that recompute inevitably drift.
type UsageUpdate struct {
	TokensInTotal  int                     `json:"tokens_in_total"`
	TokensOutTotal int                     `json:"tokens_out_total"`
	CostUSDTotal   float64                 `json:"cost_usd_total"`
	TurnsTotal     int                     `json:"turns_total"`
	ByModel        map[string]UsageByModel `json:"by_model,omitempty"`
	LastTurn       *UsageLastTurn          `json:"last_turn,omitempty"`
}

// UsageLastTurn captures the just-completed turn's per-turn footer
// fields — enough for a "◇ 12,345 in · 456 out · $0.0125 · gemini-3.1-pro"
// row without a follow-up round-trip. Cached input is surfaced when
// present so future TUI iterations can render a "· 8k cached" tag
// alongside the base tokens.
type UsageLastTurn struct {
	TokensIn       int     `json:"tokens_in"`
	TokensInCached int     `json:"tokens_in_cached,omitempty"`
	TokensOut      int     `json:"tokens_out"`
	CostUSD        float64 `json:"cost_usd"`
	Model          string  `json:"model,omitempty"`
}

// UsageByModel is one model's bucket inside UsageUpdate.ByModel.
type UsageByModel struct {
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
	Turns     int     `json:"turns"`
}

// Inbox states per spec section 2.4. The spec reserves room for
// future states (e.g. "injected"); consumers tolerate unknown values.
const (
	InboxStateQueued   = "queued"
	InboxStateDequeued = "dequeued"
)

// InboxEvent fires when an operator-typed prompt changes inbox
// state. PromptID is the correlation handle that links this event
// to downstream turn-complete / turn-error for the same turn.
type InboxEvent struct {
	State    string    `json:"state"`
	PromptID string    `json:"prompt_id"`
	QueuedAt time.Time `json:"queued_at,omitempty"`
}

// TurnComplete fires once per turn after the last stream-chunk for
// that turn and before the next turn's events.
//
// CostUSD is *float64 (optional) per spec v1.1.0 §2.5 — servers
// whose model layer doesn't know pricing (this server, since
// agent.* deliberately has no pricing reference) leave it nil and
// the immediately-following usage-update carries authoritative
// cost. Servers with in-band pricing populate it.
type TurnComplete struct {
	PromptID  string   `json:"prompt_id"`
	Model     string   `json:"model"`
	TokensIn  int      `json:"tokens_in"`
	TokensOut int      `json:"tokens_out"`
	CostUSD   *float64 `json:"cost_usd,omitempty"`
	LatencyMs int64    `json:"latency_ms"`
}

// TurnError kinds per spec section 2.6. Consumers MUST treat unknown
// values as TurnErrorUnknown (forward-compat for new categories).
const (
	TurnErrorConfig        = "config_error"
	TurnErrorAuth          = "auth_error"
	TurnErrorModelNotFound = "model_not_found"
	TurnErrorRateLimited   = "rate_limited"
	TurnErrorTransientNet  = "transient_network"
	// TurnErrorCostCeiling fires when a configured per-turn or
	// per-session cost ceiling is exceeded (#145). Agent refuses new
	// turns until the operator calls ResetCostCeiling on the agent
	// (typically via a slash command). Retryable=false on this kind
	// — the host should surface the message + halt automated retry.
	TurnErrorCostCeiling = "cost_ceiling"
	// TurnErrorCanceled fires when the turn's context was cancelled
	// (#206): an operator interrupt over attach, the watchdog halting
	// a runaway loop in flight under --watchdog=enforce, or the
	// parent context going away at shutdown. Retryable=false — every
	// one of those is somebody deciding the turn should stop, and a
	// client that re-drives it is undoing that decision.
	TurnErrorCanceled = "canceled"
	// TurnErrorWatchdogHalt fires when the behavioral watchdog halted
	// the session (#208) — the refusal Preflight returns for the turn
	// that tripped it and for every turn after, until an operator
	// resets. Retryable=false, and more emphatically than the rest: a
	// retry re-drives the loop the halt exists to break. Distinct from
	// cost_ceiling because the remedy differs (fix what looped, then
	// reset) and distinct from canceled because that one describes the
	// in-flight cancel, not the standing refusal that follows it.
	TurnErrorWatchdogHalt = "watchdog_halt"
	TurnErrorUnknown      = "unknown"
)

// TurnError is emitted on a pipeline failure that should reach the
// operator. Successful retries do NOT emit this (the spec's
// "if something is wrong, tell the operator" contract — successful
// internal retries are not operator-facing failures).
type TurnError struct {
	Kind      string `json:"kind"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Hint      string `json:"hint,omitempty"`
}

// OperatorEventTarget is the optional capability a Registrant can
// implement so the broadcaster wires its Emit method as the agent's
// typed operator-event callback at first-subscriber time, and clears
// it when the last subscriber disconnects.
//
// Registrants that implement neither this nor the deprecated
// EmitTarget still get the legacy `event: agent` frames pumped from
// the eventlog (back-compat with every poll-mode client) — they just
// won't emit typed events (capabilities still fires from the
// broadcaster directly, and the snapshot frames still flow because
// they read agent state via StatusProvider / UsageProvider, not via
// Emit).
type OperatorEventTarget interface {
	SetOperatorEventEmitter(func(eventType string, payload any))
}

// EmitTarget is the pre-#506 shape of OperatorEventTarget. The
// broadcaster still probes it as a fallback, so registrants built
// against the old method name keep emitting typed events — the
// failure mode of dropping the fallback would be SILENT (an
// interface assertion quietly failing means the operator stream
// just goes dark), which is why this stays for a full deprecation
// cycle rather than being cut over.
//
// Deprecated: implement OperatorEventTarget.
type EmitTarget interface {
	SetAttachEmitter(func(eventType string, payload any))
}
