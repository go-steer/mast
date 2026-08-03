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

// Package mast is the top-level convenience API for embedding the mast
// agent runtime in a Go program — the 90% path for library-embedded
// consumers (docs/library-api-design.md, "Top-level convenience API").
// It delegates to the pkg/ subsystems for everything; no type defined
// here duplicates a subsystem type: workloads are pkg/workload.Bundle,
// specialists are pkg/specialists.Spec, session projections are
// pkg/transcript values, budget ceilings are pkg/budget.Limits.
//
// # Stability
//
// This package is one of the five stable-from-v0.1 surfaces
// (docs/library-api-design.md, import-surface table): it follows
// semver, and breaking changes get a deprecation cycle. The v0.1
// surface is deliberately minimal — Config, Result, Run, RunWorkload,
// ListSessions, ResumeSession; server mode, lifecycle hooks, and
// option-func variants land in later versions.
//
// # Slim consumers
//
// This is the batteries-included surface: it imports the dispatch
// subsystems (pkg/graph, pkg/router, pkg/planner), several of which
// are denylisted for the slim-embed guarantee. Consumers who need the
// minimal dependency graph (docs/library-api-design.md, "Slim-embed
// guarantee") must NOT import this package — they compose the slim
// slice directly (pkg/agent, pkg/specialists, optionally pkg/workload,
// pkg/budget, pkg/transcript), as examples/deploy/slim does.
//
// # Example
//
// Programmatic bundle registration, no filesystem:
//
//	res, err := mast.RunWorkload(ctx, mast.Config{ModelName: "echo"},
//	    workload.Bundle{Name: "triage", Specialists: []string{"classify", "_fallback"}},
//	    []specialists.Spec{
//	        {Name: "classify", Mode: specialists.ModeSingleTurn, Instruction: "..."},
//	        {Name: "_fallback", Mode: specialists.ModeTask, Instruction: "..."},
//	    },
//	    `{"reason":"CrashLoopBackOff"}`)
package mast

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/internal/compose"
	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// appName is the ADK app name library-run sessions are stored under.
// It matches cmd/mast so a session DB written by an embedded runtime
// reads identically through `mast sessions` and mast-web.
const appName = "mast"

// userID is the ADK user ID for library-run sessions (the library
// analogue of cmd/mast's inject user).
const userID = "mast-library"

// Config bundles the common knobs for Run/RunWorkload/ResumeSession.
// The zero value is not runnable: a model (Model or ModelName) is
// required. Everything else defaults sensibly — in-memory sessions,
// no logging, bundle-derived budget.
type Config struct {
	// Model is an explicit ADK model to drive every agent. Takes
	// precedence over ModelName. Use this to inject a custom or fake
	// model.LLM.
	Model model.LLM

	// ModelName constructs a built-in model when Model is nil: "echo"
	// (in-process fake, no credentials — the testing surface) or a
	// "gemini-*" model id (Vertex/Gemini via ADK). It also selects the
	// usage pricing rate; when Model is set and ModelName is empty,
	// pricing falls back to Model.Name().
	ModelName string

	// Sessions is the ADK session service runs execute against. Nil
	// means a fresh in-memory service per call — no durability, and no
	// resume across calls. Pass a shared service (ADK's
	// session/database over SQLite/Postgres, or one InMemoryService
	// reused across calls) to get durable pause/resume and
	// ListSessions.
	Sessions adksession.Service

	// Logger receives construction and turn logs. Nil disables
	// logging.
	Logger *slog.Logger

	// Budget overrides the budget ceilings for the run. Nil derives
	// limits from the workload bundle's budget block (Run has no
	// bundle, so nil means unlimited). A zero RatePer1K is filled from
	// the model's flat spike pricing so Result.Usage.CostUSD is never
	// silently zero-rated.
	Budget *budget.Limits
}

// Usage is the run's cumulative usage snapshot, taken from the
// pkg/budget meter after the turn completes.
type Usage struct {
	// Tokens is the total token count across all model calls.
	Tokens int64
	// CostUSD is the derived cost (flat spike pricing; see pkg/budget).
	CostUSD float64
	// ModelCalls is the number of model calls ("turns" in mast's
	// budget vocabulary).
	ModelCalls int
}

// Result is the outcome of one library-run turn.
type Result struct {
	// Output is the turn's final output: the last node output or model
	// text the runner emitted. When the turn parked on a HITL
	// interrupt, Output holds whatever the run produced before
	// pausing; inspect ListSessions for the pending interrupt.
	Output string

	// SessionID identifies the session the turn ran in. Pass it to
	// ResumeSession (with a durable Config.Sessions) to continue a
	// paused run.
	SessionID string

	// Usage is the session's cumulative usage after the turn.
	Usage Usage
}

// RunWorkload executes one turn of a programmatically-registered
// workload: bundle and specs are plain values (the same types
// pkg/workload and pkg/specialists loaders produce from files — no
// filesystem is touched here), input is the turn's user message.
//
// The dispatch shape is chosen from the roster, mirroring cmd/mast's
// semantics: the supervisor-body planner when bundle.Planner.Enabled;
// the workflow-graph LLM-as-router when the roster carries a
// SingleTurn classifier and a "_fallback" Task specialist; the
// SubAgents coordinator otherwise.
//
// The bundle's budget block (or Config.Budget) is enforced while the
// turn streams; crossing a ceiling aborts the run with an error
// wrapping budget.ErrExceeded. bundle.Budget.MaxWallclockSeconds
// bounds the whole turn.
func RunWorkload(ctx context.Context, cfg Config, bundle workload.Bundle, specs []specialists.Spec, input string) (*Result, error) {
	llm, modelName, err := resolveModel(ctx, cfg)
	if err != nil {
		return nil, err
	}
	root, err := compose.BuildRoot(compose.RootConfig{
		Bundle:        bundle,
		Specs:         specs,
		Model:         llm,
		Dispatch:      compose.DispatchAuto,
		Logger:        cfg.Logger,
		PauseRecorder: pauseRecorder(cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("mast: build workload %q: %w", bundle.Name, err)
	}

	if bundle.Budget.MaxWallclockSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(bundle.Budget.MaxWallclockSeconds)*time.Second)
		defer cancel()
	}

	msg := genai.NewContentFromText(input, genai.RoleUser)
	return runTurn(ctx, cfg, root, &bundle, limits(cfg, &bundle, modelName), newSessionID(), msg)
}

// Run is the single-agent convenience: one Chat-mode agent with the
// given system instruction, one turn on input. No workload, no
// specialists, no dispatch — the "hello world" of embedding mast.
func Run(ctx context.Context, cfg Config, instruction, input string) (*Result, error) {
	llm, modelName, err := resolveModel(ctx, cfg)
	if err != nil {
		return nil, err
	}
	root, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name:        "mast_agent",
		Description: "Single-agent mast run.",
		Instruction: instruction,
		Model:       llm,
	})
	if err != nil {
		return nil, fmt.Errorf("mast: build agent: %w", err)
	}
	msg := genai.NewContentFromText(input, genai.RoleUser)
	return runTurn(ctx, cfg, root, nil, limits(cfg, nil, modelName), newSessionID(), msg)
}

// ListSessions returns the operator projections (pkg/transcript) for
// every session in Config.Sessions under mast's app name: state
// (paused/aborted/idle), pending interrupt IDs, last-event times.
// Thin delegation to pkg/transcript's Store; use that package directly
// for detail views and abort markers.
func ListSessions(ctx context.Context, cfg Config) ([]transcript.Summary, error) {
	if cfg.Sessions == nil {
		return nil, errors.New("mast: ListSessions requires Config.Sessions (sessions from a nil service are per-call and never listable)")
	}
	return transcript.NewStore(cfg.Sessions, appName).List(ctx, "")
}

// ResumeSession feeds an operator verdict back into a session that
// parked on a HITL interrupt (a durable RequestInput; see
// transcript.Detail.Pending for pending interrupt IDs and response
// schemas). bundle and specs must describe the same workload the
// session was started with — the resume turn is executed by a runner
// over the same root shape — and Config.Sessions must be the service
// (or database) holding the session.
//
// The wire shape is the spike-2-verified resume contract: a user turn
// carrying a FunctionResponse whose ID equals the pending interrupt
// ID, with response under the "response" key. Sessions carrying an
// operator abort marker are refused.
func ResumeSession(ctx context.Context, cfg Config, bundle workload.Bundle, specs []specialists.Spec, sessionID, interruptID string, response any) (*Result, error) {
	if cfg.Sessions == nil {
		return nil, errors.New("mast: ResumeSession requires Config.Sessions (the service holding the paused session)")
	}
	store := transcript.NewStore(cfg.Sessions, appName)
	if d, err := store.Get(ctx, "", sessionID); err != nil {
		return nil, fmt.Errorf("mast: resume session %q: %w", sessionID, err)
	} else if d.State == transcript.StateAborted {
		return nil, fmt.Errorf("mast: session %q is aborted (%s); refusing resume", sessionID, d.AbortReason)
	} else if d.GatePause.Active() {
		return nil, fmt.Errorf("mast: session %q is gate-paused (%s: %s); resume it with ResumeByToken first",
			sessionID, d.PauseReason, d.PauseMessage)
	}

	llm, modelName, err := resolveModel(ctx, cfg)
	if err != nil {
		return nil, err
	}
	root, err := compose.BuildRoot(compose.RootConfig{
		Bundle:        bundle,
		Specs:         specs,
		Model:         llm,
		Dispatch:      compose.DispatchAuto,
		Logger:        cfg.Logger,
		PauseRecorder: pauseRecorder(cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("mast: build workload %q: %w", bundle.Name, err)
	}

	msg := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromFunctionResponse("adk_request_input", map[string]any{
			"response": response,
		})},
	}
	msg.Parts[0].FunctionResponse.ID = interruptID
	return runTurn(ctx, cfg, root, &bundle, limits(cfg, &bundle, modelName), sessionID, msg)
}

// AckEffects records the operator's acknowledgement of ambiguous prior
// effects on a session: dangling mutating tool calls from an
// interrupted turn — persisted up to now — stop tripping the
// recorded-effect outbox's fail-closed refusal on subsequent turns
// (docs/durable-execution-design.md, "Recorded-effect outbox"). The
// caller asserts they checked whether those calls took effect
// externally. The library twin of `mast sessions resume --ack-effects`.
func AckEffects(ctx context.Context, cfg Config, sessionID, reason string) error {
	if cfg.Sessions == nil {
		return errors.New("mast: AckEffects requires Config.Sessions (acks on a nil service could never be read back)")
	}
	return transcript.NewStore(cfg.Sessions, appName).AckEffects(ctx, "", sessionID, reason)
}

// pauseRecorder returns the pause_session record sink for library
// roots: the transcript store when sessions are durable, nil (tool
// unregistered) otherwise — an in-memory pause record would die with
// the process.
func pauseRecorder(cfg Config) planner.PauseRecorder {
	if cfg.Sessions == nil {
		return nil
	}
	return transcript.NewStore(cfg.Sessions, appName)
}

// Pause gate-pauses a session (plane B of the v0.2 pause/abort
// surface, docs/durable-execution-design.md "The v0.2 pause/abort
// mechanics"): every subsequent turn on the session — Run, RunWorkload
// continuations, ResumeSession — refuses until the pause is resumed
// with the returned handle's token via ResumeByToken. The pause takes
// effect at the turn boundary; a turn this process has in flight
// completes (library embedders own their turn contexts — cancel yours
// for a hard pause; PauseSpec.Interrupt is daemon machinery and is
// ignored here). The library twin of `mast sessions pause` /
// POST /pause.
func Pause(ctx context.Context, cfg Config, sessionID string, spec transcript.PauseSpec) (transcript.PauseHandle, error) {
	if cfg.Sessions == nil {
		return transcript.PauseHandle{}, errors.New("mast: Pause requires Config.Sessions (a pause on a nil service could never be resumed)")
	}
	return transcript.NewStore(cfg.Sessions, appName).PauseGate(ctx, "", sessionID, spec)
}

// ResumeByToken resumes a pause by its resume token (minted by Pause,
// by the planner's pause_session tool, or by a graph RequestInput
// helper). A gate-pause resume clears the gate and runs no turn —
// nothing was parked; the returned Result carries only the session ID.
// An interrupt-pause resume drives the normal resume turn (response
// nil defaults to {"resumed_by": "operator"}), and the token is
// consumed once the resume FunctionResponse is durably appended — a
// turn that fails before the append leaves the token live for retry.
// Expired tokens refuse with transcript.ErrTokenExpired (the pause
// remains); replays refuse with transcript.ErrAlreadyResumed. The
// library twin of `mast sessions resume --token`.
func ResumeByToken(ctx context.Context, cfg Config, bundle workload.Bundle, specs []specialists.Spec, token string, response any) (*Result, error) {
	if cfg.Sessions == nil {
		return nil, errors.New("mast: ResumeByToken requires Config.Sessions (the service holding the paused session)")
	}
	store := transcript.NewStore(cfg.Sessions, appName)
	rec, err := store.FindToken(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("mast: %w", err)
	}
	if !rec.ConsumedAt.IsZero() {
		return nil, fmt.Errorf("mast: token consumed %s by %s: %w",
			rec.ConsumedAt.Format(time.RFC3339), rec.ConsumedBy, transcript.ErrAlreadyResumed)
	}
	if rec.Expired(time.Now().UTC()) {
		return nil, fmt.Errorf("mast: token expired %s (the pause remains; ExtendToken via the transcript store is the recovery): %w",
			rec.ExpiresAt.Format(time.RFC3339), transcript.ErrTokenExpired)
	}
	if rec.Plane == transcript.PlaneGate {
		if _, err := store.ConsumeToken(ctx, token, "library ResumeByToken"); err != nil {
			return nil, fmt.Errorf("mast: %w", err)
		}
		return &Result{SessionID: rec.SessionID}, nil
	}
	if response == nil {
		response = map[string]any{"resumed_by": "operator"}
	}
	res, rerr := ResumeSession(ctx, cfg, bundle, specs, rec.SessionID, rec.InterruptID, response)
	// Consumption keys on the durable append of the resume
	// FunctionResponse, not on turn success (the effects-ack
	// precedent): if the interrupt is no longer pending, the pause is
	// over regardless of how the turn ended.
	if d, gerr := store.Get(ctx, "", rec.SessionID); gerr == nil {
		pending := false
		for _, id := range d.PendingInterruptIDs {
			if id == rec.InterruptID {
				pending = true
				break
			}
		}
		if !pending {
			if _, cerr := store.ConsumeToken(ctx, rec.Token, "library ResumeByToken"); cerr != nil &&
				!errors.Is(cerr, transcript.ErrAlreadyResumed) && !errors.Is(cerr, transcript.ErrTokenNotFound) && cfg.Logger != nil {
				cfg.Logger.Error("mast: failed to consume resume token after answered interrupt",
					"session", rec.SessionID, "error", cerr.Error())
			}
		}
	}
	return res, rerr
}

// resolveModel returns the model to run with plus the name used for
// pricing: Config.Model verbatim when set, otherwise a built-in model
// constructed from Config.ModelName.
func resolveModel(ctx context.Context, cfg Config) (model.LLM, string, error) {
	if cfg.Model != nil {
		name := cfg.ModelName
		if name == "" {
			name = cfg.Model.Name()
		}
		return cfg.Model, name, nil
	}
	if cfg.ModelName == "" {
		return nil, "", errors.New("mast: Config.Model or Config.ModelName is required")
	}
	// No provider alias on the library surface: claude-* backend
	// selection is env-driven here (ANTHROPIC_API_KEY vs Vertex
	// project). Consumers who need to force a backend construct the
	// model via pkg/providers/anthropic and set Config.Model.
	llm, err := compose.BuildModel(ctx, "", cfg.ModelName)
	if err != nil {
		return nil, "", fmt.Errorf("mast: %w", err)
	}
	return llm, cfg.ModelName, nil
}

// limits derives the meter ceilings: Config.Budget verbatim when set
// (with a zero rate filled from the model pricing), otherwise the
// bundle's budget block over the model's flat rate.
func limits(cfg Config, bundle *workload.Bundle, modelName string) budget.Limits {
	if cfg.Budget != nil {
		l := *cfg.Budget
		if l.RatePer1K == 0 {
			l.RatePer1K = compose.RatePer1K(modelName)
		}
		return l
	}
	l := budget.Limits{RatePer1K: compose.RatePer1K(modelName)}
	if bundle != nil {
		l.MaxCostUSD = bundle.Budget.MaxCostUSD
		l.MaxTurns = bundle.Budget.MaxTurns
	}
	return l
}

// runTurn drives one turn through an ADK runner over cfg.Sessions,
// metering usage against limits and collecting the final output (the
// last node output or model text on the event stream — the same
// projection examples/deploy/slim uses).
func runTurn(ctx context.Context, cfg Config, root adkagent.Agent, bundle *workload.Bundle, lim budget.Limits, sessionID string, msg *genai.Content) (*Result, error) {
	svc := cfg.Sessions
	if svc == nil {
		svc = adksession.InMemoryService()
	}
	// Recorded-effect outbox (docs/durable-execution-design.md): same
	// guard as cmd/mast — every runner construction path attaches it.
	// The ack watermark reads through the same store AckEffects writes.
	ackStore := transcript.NewStore(svc, appName)
	var policies []effects.ToolPolicy
	if bundle != nil {
		for _, p := range bundle.ToolCatalog.Tools {
			policies = append(policies, effects.ToolPolicy{Name: p.Name, Mutating: p.Mutating})
		}
	}
	outboxPlugin, err := effects.New(effects.Config{
		Predicate:     effects.NewPredicate(effects.Overrides(cfg.Logger, policies)),
		SubAgentNames: effects.SubAgentNames(root),
		AckedAt: func(ctx context.Context, sid string) (time.Time, bool) {
			return ackStore.EffectsAckedAt(ctx, "", sid)
		},
		Logger: cfg.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("mast: construct effects outbox: %w", err)
	}
	r, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{outboxPlugin}},
	})
	if err != nil {
		return nil, fmt.Errorf("mast: construct runner: %w", err)
	}

	// Budget enforcement point (same shape as cmd/mast): the meter
	// folds UsageMetadata from each streamed event; crossing a ceiling
	// cancels the run context, aborting in-flight model/tool work.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	meter := budget.NewMeter(lim)

	res := &Result{SessionID: sessionID}
	for event, err := range r.Run(ctx, userID, sessionID, msg, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}) {
		if err != nil {
			return nil, fmt.Errorf("mast: session %q: %w", sessionID, err)
		}
		if berr := meter.Observe(event); berr != nil {
			cancel()
			res.Usage.Tokens, res.Usage.CostUSD, res.Usage.ModelCalls = meter.Snapshot()
			return nil, fmt.Errorf("mast: session %q: %w", sessionID, berr)
		}
		if event == nil {
			continue
		}
		if cfg.Logger != nil {
			cfg.Logger.Debug("runner event", "session", sessionID, "author", event.Author)
		}
		if event.Output != nil {
			res.Output = fmt.Sprintf("%v", event.Output)
			continue
		}
		if event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part != nil && part.Text != "" {
				res.Output = part.Text
			}
		}
	}
	res.Usage.Tokens, res.Usage.CostUSD, res.Usage.ModelCalls = meter.Snapshot()
	return res, nil
}

// newSessionID mints a fresh library-run session ID. Random rather
// than derived: the library has no inject envelope to key on, and the
// ID is returned in Result for callers who need to resume.
func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is effectively unreachable; fall back to
		// a time-based ID rather than propagating an error nobody can
		// handle.
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(b[:])
}
