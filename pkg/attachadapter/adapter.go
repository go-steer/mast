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

	// ToolsFn, when set, supplies GET /sessions/.../tools (typically
	// the workload bundle's tool catalog). Nil reports an empty list.
	ToolsFn func() []attach.ToolInfo
}

// Adapter implements attach.Registrant plus the optional capability
// interfaces mast's daemon can honestly serve: StatusProvider,
// UsageProvider, ToolsProvider, InterruptProvider,
// DescriptionProvider, and OperatorEventTarget.
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
func (ad *Adapter) AttachUsage() attach.UsageInfo {
	if ad.cfg.UsageFn == nil {
		return attach.UsageInfo{}
	}
	return ad.cfg.UsageFn()
}

// AttachTools implements attach.ToolsProvider. Empty when no ToolsFn
// is wired.
func (ad *Adapter) AttachTools() []attach.ToolInfo {
	if ad.cfg.ToolsFn == nil {
		return nil
	}
	return ad.cfg.ToolsFn()
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
