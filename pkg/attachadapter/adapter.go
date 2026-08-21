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
// (pkg/attachadapter), deliberately reshaped: core-agent's adapter
// wraps its persistent *agent.Agent loop (inbox, wake signal, usage
// tracker); mast's daemon is runner-driven — turns execute on demand
// against an ADK runner and there is no per-session goroutine between
// turns. This adapter therefore owns the inbox semantics itself: it
// serializes injected messages into one-at-a-time RunTurn calls and
// emits the typed operator events (status-update, turn-complete,
// turn-error) around each turn, so attach subscribers see the same
// wire protocol core-agent produces.

// Package attachadapter bridges a mast serve-daemon session into
// pkg/attach's Registrant contract, so operator frontends (mast-web)
// can list, tail, and inject into sessions over the attach protocol.
//
// One Adapter represents one ADK session triple. All adapters in a
// daemon share the daemon's eventlog handle — attach's broadcaster
// filters the stream per session. Construction is explicit
// (attachadapter.New with a Config); the daemon registers the result
// on an attach.SessionRegistry.
package attachadapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/auth"
	"github.com/go-steer/mast/pkg/digest"
	"github.com/go-steer/mast/pkg/eventlog"
)

// TurnResult carries what the daemon's turn runner knows about a
// finished turn. Zero values are honest "unknown" — the terminal
// turn-complete event reports them as-is and authoritative cost
// arrives via the usage snapshot (UsageFn), matching the attach
// spec's "cost deferred" wire semantics.
type TurnResult struct {
	TokensIn  int
	TokensOut int
}

// Config wires an Adapter to one session's turn machinery. AppName,
// UserID, SessionID, EventLog, and RunTurn are required.
type Config struct {
	// AppName / UserID / SessionID form the ADK session key —
	// attach uses (AppName, SessionID) for URL lookup and the
	// broadcaster uses all three to filter the eventlog stream.
	AppName   string
	UserID    string
	SessionID string

	// EventLog is the daemon-wide eventlog handle. Attach requires
	// it for live-tail (the broadcaster pumps from Stream.Watch).
	EventLog *eventlog.Handle

	// RunTurn executes one turn for this session: append message as
	// user content, drive the runner until the turn completes, and
	// return what is known about the turn's token usage. The
	// adapter guarantees calls are serialized per session. ctx is
	// canceled when an operator interrupts the turn (POST
	// /interrupt) or the daemon shuts down.
	RunTurn func(ctx context.Context, message string) (TurnResult, error)

	// BaseContext bounds every turn this adapter runs; defaults to
	// context.Background(). Pass the daemon's serve context so
	// in-flight turns die with the process's graceful shutdown.
	BaseContext context.Context

	// ModelName, when set, is reported on status frames.
	ModelName string

	// Description, when set, feeds the agent card + session list.
	Description string

	// UsageFn, when set, supplies the GET /sessions/.../usage
	// snapshot (typically from the session's budget meter). Nil
	// reports zero usage rather than 501 — same behavior as a
	// core-agent registrant with an empty tracker.
	UsageFn func() attach.UsageInfo

	// ToolsFn, when set, supplies GET /sessions/.../tools. Nil reports
	// an empty list — which reads to an operator exactly like a daemon
	// that holds no tools, so a caller with tools to report should set
	// it. The daemon builds it from the MCP toolsets it wired, because
	// that is where the per-server attribution the endpoint reports
	// still exists (cmd/mast's toolCatalog; #133). Callers that only
	// have the workload bundle's declared tool_catalog can project that
	// instead, at the cost of reporting what was declared rather than
	// what the servers actually serve.
	ToolsFn func() []attach.ToolInfo

	// SubagentsFn, when set, supplies GET /sessions/.../subagents: the
	// specialist roster the daemon loaded, as opposed to the live
	// instances /agents reports. Nil reports an empty list. A daemon
	// running a workload bundle has a roster and should set it — an
	// empty catalog reads as "this daemon has no specialists", which is
	// the wrong answer for every bundle mast ships (#134).
	SubagentsFn func() []attach.SubagentCatalogInfo

	// GuardrailsFn, when set, supplies GET /sessions/.../guardrails:
	// which backstops are armed, which have tripped, and the spend
	// behind that. Nil reports everything off — the truthful answer
	// for a caller with no budget meter, and the wrong one for a
	// daemon running a bundle that declares `budget:` (#135).
	GuardrailsFn func() attach.GuardrailInfo

	// ResetGuardrailFn, when set, services POST
	// /sessions/.../guardrails/reset. Nil is a 501 rather than a
	// silent no-op: a session wedged past its ceiling stays wedged for
	// the daemon's lifetime, so an operator has to learn immediately
	// that this daemon can't hand it more runway.
	ResetGuardrailFn func(req attach.GuardrailResetRequest) (attach.GuardrailResetResponse, error)
}

// Adapter implements attach.Registrant plus the optional capability
// interfaces mast's daemon can honestly serve: StatusProvider,
// UsageProvider, ToolsProvider, SubagentCatalogProvider,
// GuardrailProvider, GuardrailResetter, InterruptProvider,
// DescriptionProvider, and OperatorEventTarget.
//
// It also implements CapabilityReporter, because satisfying an
// interface is not the same as being wired: the guardrail methods
// exist on every Adapter and answer with real data only where the
// daemon set the corresponding Config func. The report is what the
// capabilities frame advertises.
//
// Not AgentsProvider: /agents lists spawned background instances, and
// mast has none to list — every dispatch shape resolves its
// specialists inside the turn. The configured roster goes to
// SubagentCatalogProvider instead, which is the distinction #134 was
// filed over.
type Adapter struct {
	cfg Config

	mu       sync.Mutex
	queue    []queuedMessage
	draining bool
	// cancelTurn interrupts the in-flight RunTurn; nil between turns.
	cancelTurn context.CancelFunc

	// auditPending records that AttachInterrupt fired against the
	// current turn; drain appends the audit event after that turn
	// returns (see appendInterruptAudit).
	auditPending bool

	emitMu sync.Mutex
	emit   func(eventType string, payload any)
}

type queuedMessage struct {
	text   string
	caller auth.Caller
}

// New validates cfg and returns an Adapter ready to Register on an
// attach.SessionRegistry.
func New(cfg Config) (*Adapter, error) {
	switch {
	case cfg.AppName == "" || cfg.UserID == "" || cfg.SessionID == "":
		return nil, errors.New("attachadapter: AppName, UserID, and SessionID are required")
	case cfg.EventLog == nil:
		return nil, errors.New("attachadapter: EventLog is required (attach live-tail pumps from Stream.Watch)")
	case cfg.RunTurn == nil:
		return nil, errors.New("attachadapter: RunTurn is required")
	}
	if cfg.BaseContext == nil {
		cfg.BaseContext = context.Background()
	}
	return &Adapter{cfg: cfg}, nil
}

// AppName implements attach.Registrant.
func (ad *Adapter) AppName() string { return ad.cfg.AppName }

// UserID implements attach.Registrant.
func (ad *Adapter) UserID() string { return ad.cfg.UserID }

// SessionID implements attach.Registrant.
func (ad *Adapter) SessionID() string { return ad.cfg.SessionID }

// EventLog implements attach.Registrant.
func (ad *Adapter) EventLog() *eventlog.Handle { return ad.cfg.EventLog }

// Inject implements attach.Registrant: queue a message and run it as
// its own turn once earlier queued messages finish. Unlike
// core-agent's inbox (whose agent loop drains the whole batch into
// one turn), mast maps one injected message to one turn — the daemon
// has no long-lived loop to batch for.
func (ad *Adapter) Inject(message string) error {
	return ad.InjectAs(message, auth.Caller{})
}

// InjectAs implements attach.Registrant. The caller rides the turn
// context (auth.WithCaller) so the eventlog metadata extractor and
// any caller-aware substrate see who triggered the turn.
func (ad *Adapter) InjectAs(message string, caller auth.Caller) error {
	ad.mu.Lock()
	ad.queue = append(ad.queue, queuedMessage{text: message, caller: caller})
	ad.startDrainLocked()
	ad.mu.Unlock()
	return nil
}

// RequestWake implements attach.Registrant. Core-agent's wake signal
// re-checks an idle agent loop's inbox; mast's daemon runs turns on
// demand, so wake only kicks the drainer in case a message is queued
// with no drainer running (a state that shouldn't occur — this is
// belt-and-braces, not a scheduler).
func (ad *Adapter) RequestWake() {
	ad.mu.Lock()
	ad.startDrainLocked()
	ad.mu.Unlock()
}

// startDrainLocked launches the drain goroutine when messages are
// queued and no drainer is running. Callers must hold ad.mu.
func (ad *Adapter) startDrainLocked() {
	if ad.draining || len(ad.queue) == 0 {
		return
	}
	ad.draining = true
	go ad.drain()
}

// drain runs queued messages one turn at a time until the queue is
// empty, emitting the typed operator-event sequence around each turn
// (status-update streaming → turn-complete | turn-error →
// status-update idle) per the attach wire spec.
func (ad *Adapter) drain() {
	for {
		ad.mu.Lock()
		if len(ad.queue) == 0 {
			ad.draining = false
			ad.mu.Unlock()
			return
		}
		msg := ad.queue[0]
		ad.queue = ad.queue[1:]

		turnCtx := ad.cfg.BaseContext
		if msg.caller.Identity != "" {
			turnCtx = auth.WithCaller(turnCtx, msg.caller)
		}
		runCtx, cancel := context.WithCancel(turnCtx)
		ad.cancelTurn = cancel
		ad.mu.Unlock()

		ad.emitEvent(attach.EventStatusUpdate, attach.StatusUpdate{
			Model:     ad.cfg.ModelName,
			TurnState: attach.TurnStateStreaming,
		})

		promptID := newPromptID()
		started := time.Now()
		result, err := ad.cfg.RunTurn(runCtx, msg.text)

		ad.mu.Lock()
		ad.cancelTurn = nil
		auditPending := ad.auditPending
		ad.auditPending = false
		ad.mu.Unlock()
		cancel()

		if auditPending {
			// Operator interrupted this turn: record the audit event
			// NOW — the turn has returned, so no runner session
			// handle is live, and the next turn cannot start until
			// this loop iteration continues. Appending from the
			// protocol layer at interrupt time instead would race the
			// unwinding turn's final event flush and stale its handle
			// (the ADK write-lease constraint, #57).
			ad.appendInterruptAudit()
		}

		if err != nil {
			ad.emitEvent(attach.EventTurnError, attach.ClassifyTurnError(err))
		} else {
			ad.emitEvent(attach.EventTurnComplete, attach.TurnComplete{
				PromptID:  promptID,
				Model:     ad.cfg.ModelName,
				TokensIn:  result.TokensIn,
				TokensOut: result.TokensOut,
				// cost_usd omitted — authoritative cost arrives on
				// the usage snapshot (UsageFn), per spec §2.5.
				LatencyMs: time.Since(started).Milliseconds(),
			})
		}
		ad.emitEvent(attach.EventStatusUpdate, attach.StatusUpdate{
			TurnState: attach.TurnStateIdle,
		})
	}
}

// AttachInterrupt implements attach.InterruptProvider: cancel the
// in-flight turn's context. Returns false when no turn is running.
// Queued messages are NOT discarded — the next one still runs, same
// as core-agent's interrupt (which stops the turn, not the agent).
func (ad *Adapter) AttachInterrupt() bool {
	ad.mu.Lock()
	cancel := ad.cancelTurn
	if cancel != nil {
		ad.auditPending = true
	}
	ad.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// AuditsInterrupts implements attach.InterruptSelfAuditor (a
// capability marker, never called for effect): this adapter records
// the operator-interrupt audit event from its own turn loop, so the
// protocol layer's fallback append — which would stale the unwinding
// turn's session handle — is suppressed.
func (ad *Adapter) AuditsInterrupts() {}

// appendInterruptAudit writes the operator-interrupt audit event.
// Called from drain between turns — the only window this adapter can
// guarantee holds no live runner session handle. Best-effort: the
// cancel already fired; a failed audit write is log-worthy at most,
// and this layer has no logger, so it is silently dropped like the
// protocol-layer fallback it replaces.
func (ad *Adapter) appendInterruptAudit() {
	if ad.cfg.EventLog == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	getResp, err := ad.cfg.EventLog.Service.Get(ctx, &session.GetRequest{
		AppName:   ad.cfg.AppName,
		UserID:    ad.cfg.UserID,
		SessionID: ad.cfg.SessionID,
	})
	if err != nil {
		return
	}
	ev := session.NewEvent(ctx, "attach-interrupt")
	ev.Author = "attach/interrupt"
	ev.CustomMetadata = map[string]any{"source": "operator"}
	_ = ad.cfg.EventLog.Service.AppendEvent(ctx, getResp.Session, ev)
}

// AttachStatus implements attach.StatusProvider.
func (ad *Adapter) AttachStatus() attach.StatusInfo {
	ad.mu.Lock()
	running := ad.cancelTurn != nil
	ad.mu.Unlock()
	state := "idle"
	if running {
		state = "running"
	}
	return attach.StatusInfo{State: state, ModelName: ad.cfg.ModelName}
}

// AttachUsage implements attach.UsageProvider. Zero UsageInfo when
// no UsageFn is wired.
//
// The digest block is stapled on here rather than inside UsageFn
// because pkg/digest's counters are process-global — the MCP wrap
// calls digest.Process from whatever goroutine is running a tool, with
// no session in hand — so every UsageFn would otherwise have to
// remember to fetch the same snapshot. Decorating once is the cheaper
// contract, and it self-gates: with --mcp-digest=false nothing ever
// calls Process, the snapshot is empty, and the field is omitted.
func (ad *Adapter) AttachUsage() attach.UsageInfo {
	info := attach.UsageInfo{}
	if ad.cfg.UsageFn != nil {
		info = ad.cfg.UsageFn()
	}
	if snap := digest.Telemetry(); len(snap.MethodCounts) > 0 {
		info.DigestMethods = &attach.DigestMethodsInfo{
			Counts:     snap.MethodCounts,
			BytesSaved: snap.BytesSaved,
		}
	}
	return info
}

// AttachTools implements attach.ToolsProvider. Empty when no ToolsFn
// is wired.
func (ad *Adapter) AttachTools() []attach.ToolInfo {
	if ad.cfg.ToolsFn == nil {
		return nil
	}
	return ad.cfg.ToolsFn()
}

// AttachSubagentCatalog implements attach.SubagentCatalogProvider.
// Empty when no SubagentsFn is wired.
func (ad *Adapter) AttachSubagentCatalog() []attach.SubagentCatalogInfo {
	if ad.cfg.SubagentsFn == nil {
		return nil
	}
	return ad.cfg.SubagentsFn()
}

// AttachGuardrails implements attach.GuardrailProvider. Zero state —
// nothing armed, nothing tripped — when no GuardrailsFn is wired.
func (ad *Adapter) AttachGuardrails() attach.GuardrailInfo {
	if ad.cfg.GuardrailsFn == nil {
		return attach.GuardrailInfo{}
	}
	return ad.cfg.GuardrailsFn()
}

// AttachResetGuardrail implements attach.GuardrailResetter. Without a
// ResetGuardrailFn it returns attach.ErrCapabilityNotRegistered, which
// the handler renders as 501.
func (ad *Adapter) AttachResetGuardrail(req attach.GuardrailResetRequest) (attach.GuardrailResetResponse, error) {
	if ad.cfg.ResetGuardrailFn == nil {
		return attach.GuardrailResetResponse{}, attach.ErrCapabilityNotRegistered
	}
	return ad.cfg.ResetGuardrailFn(req)
}

// AttachCapabilities implements attach.CapabilityReporter: what this
// adapter is actually wired for, as opposed to what its method set
// happens to satisfy. Interrupt is unconditional (the adapter owns the
// turn's cancel func); the guardrail keys follow the Config funcs.
//
// CostCeiling asks the projection rather than assuming: a daemon
// serving a bundle with no `budget:` block has the guardrail surface
// wired and no ceiling to trip, and advertising a spend cap there
// would have a client render a limit that does not exist.
func (ad *Adapter) AttachCapabilities() attach.CapabilityReport {
	rep := attach.CapabilityReport{Interrupt: true}
	if ad.cfg.GuardrailsFn != nil {
		rep.Guardrails = ad.cfg.ResetGuardrailFn != nil
		rep.CostCeiling = ad.cfg.GuardrailsFn().CostCeiling.Configured()
	}
	return rep
}

// Description implements attach.DescriptionProvider.
func (ad *Adapter) Description() string { return ad.cfg.Description }

// SetOperatorEventEmitter implements attach.OperatorEventTarget: the
// broadcaster installs the typed-event callback at first-subscriber
// time and clears it (f == nil) when the last subscriber disconnects.
func (ad *Adapter) SetOperatorEventEmitter(f func(eventType string, payload any)) {
	ad.emitMu.Lock()
	ad.emit = f
	ad.emitMu.Unlock()
}

// emitEvent delivers a typed operator event to the installed emitter,
// if any. Never blocks turn progress beyond the emitter's own cost —
// attach's broadcaster emitter is buffered/drop-oldest by design.
func (ad *Adapter) emitEvent(eventType string, payload any) {
	ad.emitMu.Lock()
	f := ad.emit
	ad.emitMu.Unlock()
	if f != nil {
		f(eventType, payload)
	}
}

// newPromptID mints the per-turn correlation ID threaded into the
// terminal turn event. Random hex; uniqueness matters, format doesn't.
func newPromptID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "prompt-unknown"
	}
	return "p-" + hex.EncodeToString(b[:])
}

// Compile-time pin (#65): losing this capability silently re-enables
// the protocol layer's fallback audit append, which stales the
// interrupted turn's session handle — the write-lease violation #57
// fixed.
var _ attach.InterruptSelfAuditor = (*Adapter)(nil)
