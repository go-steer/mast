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
	"strings"
	"time"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/internal/compose"
	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/auth"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/watchdog"
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

	// FinalReport grants a specialist stopped by a ceiling one model
	// call to write its report with, with every other tool withdrawn.
	// It is a separate knob from Budget because it is a policy and not
	// a ceiling: a bundle that declares budget.final_report gets it
	// whether or not the caller overrode the ceilings above. See
	// pkg/budget/finalreport.go.
	FinalReport bool
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

	// Exhausted lists the specialists whose own ceilings can no longer
	// admit a call, with the arithmetic behind each.
	//
	// A specialist here did not stop the run: it was refused, the
	// coordinator was handed that refusal as an answer and routed on, and
	// the turn finished normally (v0.6 W10.3). That is the point of a
	// per-specialist cap, and it is also why this field exists — a run
	// that quietly lost half its roster returns the same nil error as one
	// that did not, and an unattended caller has nowhere else to find out.
	// The workload's own ceiling is not reported here; it comes back as an
	// error, because there is nothing left to route to.
	Exhausted []budget.Trip
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
	// Built before the root, not inside runTurn, because the planner's
	// dispatch tool needs it at construction time: a specialist run
	// through invoke_specialist streams past a private runner, so the
	// only way its spend reaches this turn's ceilings is the observer
	// seam compose threads into the planner (#226).
	meter := budget.New(meterConfig(cfg, &bundle, specs, modelName))
	seam := &subRunMeter{m: meter}
	root, _, err := compose.BuildRoot(ctx, compose.RootConfig{
		Bundle:         bundle,
		Specs:          specs,
		Model:          llm,
		ModelName:      modelName,
		Dispatch:       compose.DispatchAuto,
		Logger:         cfg.Logger,
		PauseRecorder:  pauseRecorder(cfg),
		SubRunObserver: seam,
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
	return runTurn(ctx, cfg, root, &bundle, meter, seam, newSessionID(), msg)
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
	return runTurn(ctx, cfg, root, nil, budget.New(meterConfig(cfg, nil, nil, modelName)), nil, newSessionID(), msg)
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
	meter := budget.New(meterConfig(cfg, &bundle, specs, modelName))
	seam := &subRunMeter{m: meter}
	root, _, err := compose.BuildRoot(ctx, compose.RootConfig{
		Bundle:         bundle,
		Specs:          specs,
		Model:          llm,
		ModelName:      modelName,
		Dispatch:       compose.DispatchAuto,
		Logger:         cfg.Logger,
		PauseRecorder:  pauseRecorder(cfg),
		SubRunObserver: seam,
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
	return runTurn(ctx, cfg, root, &bundle, meter, seam, sessionID, msg)
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
	h, _, err := transcript.NewStore(cfg.Sessions, appName).PauseGate(ctx, "", sessionID, spec)
	return h, err
}

// ResumeByToken resumes a pause by its resume token (minted by Pause,
// by the planner's pause_session tool, or by a graph RequestInput
// helper). A gate-pause resume clears the gate and runs no turn —
// nothing was parked; the returned Result carries only the session ID.
// An interrupt-pause resume drives the normal resume turn (response
// nil defaults to {"resumed_by": <the ctx Caller, or "library
// ResumeByToken">}, the same string recorded as the pause record's
// ConsumedBy — put a logged-in user on ctx with auth.WithCaller and the
// audit record names them), and the token is
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
	// Who is spending this token. An embedder that has a logged-in user
	// puts them on ctx with auth.WithCaller and the audit record names
	// them; one that doesn't gets "library ResumeByToken", which names
	// the mechanism truthfully rather than guessing at a human. The
	// daemon's twin resolves the same way from its request context
	// (cmd/mast/main.go, resumeByToken).
	by := auth.Attribution(ctx, "library ResumeByToken")
	if rec.Plane == transcript.PlaneGate {
		if _, err := store.ConsumeToken(ctx, token, by); err != nil {
			return nil, fmt.Errorf("mast: %w", err)
		}
		return &Result{SessionID: rec.SessionID}, nil
	}
	if response == nil {
		response = map[string]any{"resumed_by": by}
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
			// ConsumeScheduled, not ConsumeToken: this runs after the
			// resume has durably appended, so the pause has legitimately
			// ended (M5); the operator-facing token TTL must not veto the
			// bookkeeping consume and strand an answered interrupt with a
			// live-looking record — the daemon twin (consumeIfAnswered)
			// does the same.
			if _, cerr := store.ConsumeScheduled(ctx, rec.Token, by); cerr != nil &&
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

// libraryProvider is the --provider alias the library surface runs
// under: none. resolveModel builds every model with the same empty
// alias, so compose.Backend resolves the same backend the client was
// built against, and the price follows it. Named rather than written as
// a bare "" so the pricing calls read as the deliberate choice they are.
//
// The one case it cannot speak for is a caller-supplied Config.Model
// that forced a backend itself; mast sees only the model's name there,
// and prices it by the environment like any other. A caller that needs
// the price pinned to the backend it chose should set Config.Budget's
// RatePer1K outright.
const libraryProvider = ""

// limits derives the meter ceilings: Config.Budget verbatim when set
// (with any pricing it left unset filled from the model), otherwise the
// bundle's budget block over the model's pricing.
func limits(cfg Config, bundle *workload.Bundle, modelName string) budget.Limits {
	priced := compose.MeterLimits(libraryProvider, modelName)
	if cfg.Budget != nil {
		l := *cfg.Budget
		// Each price knob is filled independently, because a caller may
		// have set one and not the other: a Budget carrying only a flat
		// rate is the pre-catalog shape and still wants the catalog, and
		// one carrying only a catalog still wants a fallback rate.
		if l.RatePer1K == 0 {
			l.RatePer1K = priced.RatePer1K
		}
		if l.Catalog == nil {
			l.Catalog, l.Backend, l.Model = priced.Catalog, priced.Backend, priced.Model
		}
		return l
	}
	l := priced
	if bundle != nil {
		l.MaxCostUSD = bundle.Budget.MaxCostUSD
		l.MaxTurns = bundle.Budget.MaxTurns
	}
	return l
}

// meterConfig composes the session ceilings with the roster's
// per-specialist scopes. Config.Budget overrides the workload's
// ceilings but not a specialist's: a caller-supplied session budget
// says what the whole run may spend, not what each specialist may.
//
// The final-report grant is read from both, and unlike the ceilings it
// is an OR rather than an override: it is a policy about what a stopped
// specialist may still say, so a caller who narrowed the ceilings has
// not thereby retracted the bundle's declaration.
func meterConfig(cfg Config, bundle *workload.Bundle, specs []specialists.Spec, modelName string) budget.Config {
	c := budget.Config{
		Limits:      limits(cfg, bundle, modelName),
		Scopes:      compose.MeterScopes(specs, libraryProvider, modelName),
		FinalReport: cfg.FinalReport,
	}
	if bundle != nil && bundle.Budget.FinalReport {
		c.FinalReport = true
	}
	return c
}

// libraryWatchdogMode resolves the watchdog posture for a library run:
// the bundle's safety.watchdog when it declares one, otherwise
// watchdog.DefaultMode. There is no flag rung here — cmd/mast's
// --watchdog is an invocation-level override and a library consumer's
// invocation is the Go call, which passes the bundle.
//
// An unparseable value is an error rather than a fall-through to the
// default: workload.Load refuses a bad posture naming the file, but a
// bundle built in Go never went through the loader, and quietly
// downgrading a backstop somebody asked for is the failure this whole
// field exists to prevent.
func libraryWatchdogMode(bundle *workload.Bundle) (watchdog.Mode, error) {
	if bundle == nil || bundle.Safety.Watchdog == "" {
		return watchdog.DefaultMode, nil
	}
	m, err := watchdog.ParseMode(bundle.Safety.Watchdog)
	if err != nil {
		return "", fmt.Errorf("mast: workload %q: safety.watchdog: %w", bundle.Name, err)
	}
	return m, nil
}

// runTurn drives one turn through an ADK runner over cfg.Sessions,
// metering usage against limits and collecting the final output (the
// last node output or model text on the event stream — the same
// projection examples/deploy/slim uses).
// subRunMeter adapts one turn's budget meter to
// planner.SubRunObserver, so a specialist dispatched through the
// planner's private sub-runner is billed to the turn that dispatched
// it. The session ID is ignored: a library meter already IS this
// call's session, and there is no pool to pick from. The specialist
// name is ignored too — a meter attributes by the event's own Author,
// which is what makes a per-specialist ceiling bind here.
//
// The library build wires no watchdog — cmd/mast is where that sink has
// a boundary to respect.
//
// # Late binding, and why this is a pointer
//
// The seam has to exist before the root is built, because the planner's
// dispatch tool takes it at construction. The other half of what it does
// — recording a dispatched specialist's mutating calls where the outbox
// can find them (#235, v0.6 W9.3) — needs the effects predicate and the
// session store, and both are derived inside runTurn from the composed
// root. So the meter is set at construction and bindRecorder supplies
// the rest before the turn starts. Same shape as cmd/mast's
// daemonSubRunObserver, and for the same reason.
type subRunMeter struct {
	m       *budget.Meter
	records func(sessionID, specialist string) *effects.SubRunRecorder
}

// bindRecorder gives the seam its recording half. A nil records function
// (no session service, so nothing durable to record against) leaves the
// seam metering-only, which is what it was through v0.5.
func (s *subRunMeter) bindRecorder(f func(sessionID, specialist string) *effects.SubRunRecorder) {
	s.records = f
}

// SubRun opens one dispatch's sink. The session ID is the OUTER
// session's; the specialist name is ignored by the meter — it attributes
// by the event's own Author, which is what makes a per-specialist
// ceiling bind here — and used by the recorder for attribution the log
// cannot supply.
func (s *subRunMeter) SubRun(sessionID, specialist string) planner.SubRunSink {
	sink := &subRunSink{m: s.m}
	if s.records != nil {
		sink.rec = s.records(sessionID, specialist)
	}
	return sink
}

// subRunSink is one dispatch's sink for the library build.
type subRunSink struct {
	m   *budget.Meter
	rec *effects.SubRunRecorder
}

// Observe meters first and records last, the ordering cmd/mast's sink
// documents at length: the recorder writes a durable "this may have
// mutated" claim, and a ceiling that fires on the same event stops the
// call before it is made.
func (s *subRunSink) Observe(ev *adksession.Event) error {
	if err := s.m.Observe(ev); err != nil {
		return err
	}
	if s.rec == nil {
		return nil
	}
	return s.rec.Observe(ev)
}

func (s *subRunSink) Close() {}

func runTurn(ctx context.Context, cfg Config, root adkagent.Agent, bundle *workload.Bundle, meter *budget.Meter, seam *subRunMeter, sessionID string, msg *genai.Content) (*Result, error) {
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
	pred := effects.NewPredicate(effects.Overrides(cfg.Logger, policies))
	subAgents := effects.SubAgentNames(root)
	// A sub-agent name that also names a mutating tool is invisible to the
	// outbox's dangling scan (gate finding N2) — refuse it here too, so the
	// library surface fails closed exactly like the daemon and one-shot
	// paths rather than running with the fail-open hole.
	if hits := effects.CheckNameCollisions(subAgents, pred, policies); len(hits) > 0 {
		return nil, fmt.Errorf("mast: composition names both a sub-agent and a mutating tool %q: a mutating tool sharing a specialist's name is invisible to the effect outbox — rename the specialist or the tool", strings.Join(hits, ", "))
	}
	// Dispatched mutations (#235, v0.6 W9.3). A planner's specialist runs
	// past a private runner, so this plugin never sees its tool calls; the
	// observer seam does, and records them out-of-band on the outer
	// session's companion ops row. Binding is late because the seam is
	// built before the root (the dispatch tool needs it at construction
	// time) and the predicate is only known here.
	//
	// Only with a caller-supplied session service: a nil Sessions means a
	// fresh in-memory service per call, so there is no later turn and no
	// resume for the record to inform, and writing one would be pure cost.
	var externalDangling func(context.Context, string) []effects.DanglingIntent
	if cfg.Sessions != nil {
		subIntents := compose.SubRunIntentStore{Store: ackStore, UserID: userID}
		externalDangling = subIntents.Dangling
		if seam != nil {
			seam.bindRecorder(func(sid, specialist string) *effects.SubRunRecorder {
				rec, err := effects.NewSubRunRecorder(effects.SubRunRecorderConfig{
					Store:         subIntents,
					SessionID:     sid,
					Specialist:    specialist,
					Predicate:     pred,
					SubAgentNames: subAgents,
					Logger:        cfg.Logger,
				})
				if err != nil {
					// Loud, not fatal: a dispatch that cannot be recorded
					// is worse than one that is, but killing the turn over
					// it trades a recording gap for an availability one.
					if cfg.Logger != nil {
						cfg.Logger.Error("dispatched mutations will go UNRECORDED for this sub-run",
							"session", sid, "specialist", specialist, "err", err)
					}
					return nil
				}
				return rec
			})
		}
	}
	outboxPlugin, err := effects.New(effects.Config{
		Predicate:     pred,
		SubAgentNames: subAgents,
		AckedAt: func(ctx context.Context, sid string) (time.Time, bool) {
			return ackStore.EffectsAckedAt(ctx, "", sid)
		},
		// Merged before the ack filter, so `mast ack-effects` clears a
		// dispatched dangling intent on the same terms as an in-band one.
		ExternalDangling: externalDangling,
		Logger:           cfg.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("mast: construct effects outbox: %w", err)
	}
	// Pre-call write gate, registered after the outbox (docs/v0.3-plan.md
	// W2). With no bundle there is no workload policy and no resume
	// surface, so the library default is no gate — see compose.WriteGate.
	plugins := []*plugin.Plugin{outboxPlugin}
	writeGate, err := compose.WriteGate(compose.WriteGateConfig{
		Bundle:    bundle,
		Predicate: pred,
		Logger:    cfg.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("mast: %w", err)
	}
	if writeGate != nil {
		plugins = append(plugins, writeGate)
	}
	r, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: plugins},
	})
	if err != nil {
		return nil, fmt.Errorf("mast: construct runner: %w", err)
	}

	// Budget enforcement point (same shape as cmd/mast): the meter
	// folds UsageMetadata from each streamed event; crossing a ceiling
	// cancels the run context, aborting in-flight model/tool work. The
	// meter is the caller's rather than built here because a planner
	// root has to be handed it at construction time (see subRunMeter).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// And in front of the call as well (W10.2). Observe below stays as
	// the ledger; this is what keeps a workload one call from its cap
	// from spending that call to find out. Same context-borne gate the
	// daemon installs, for the same reason — it is what reaches a
	// planner-dispatched specialist, which on this surface is the
	// common case.
	ctx = mastagent.WithCallGate(ctx, meter)

	// A delta rather than a total: the meter is the caller's, and a
	// caller that reuses one across runs would otherwise have every later
	// run report the first one's refusal.
	//
	// The session's refusals, not every refusal (W10.3). A specialist
	// refused at its own cap hands the coordinator an answer to route
	// around; only the workload's own ceiling ends the run.
	sessionRefusalsBefore, _ := meter.SessionRefusals()
	refused := func() error {
		n, first := meter.SessionRefusals()
		if n <= sessionRefusalsBefore {
			return nil
		}
		return fmt.Errorf("mast: session %q: %w", sessionID, first)
	}

	// Behavioral watchdog (pkg/watchdog). Every other turn-driving path
	// in this repo taps the event stream — the daemon at runTurnPre,
	// the one-shot in runOneShot — and this one did not, so a
	// library-embedded workload was the single mast surface with no
	// runaway backstop at all. "Library-embedded" is one of the three
	// words in mast's own thesis; it should not be the unguarded one.
	//
	// A fresh watchdog and enforcer per call, because this surface has
	// no cross-call state to hang them on: RunWorkload mints a new
	// session per call and holds nothing between them. That bounds what
	// the rungs can mean here. Enforce cancels the runaway turn, which
	// is the half that matters — the "refuse every later turn" half
	// needs a session pool, and the daemon is where that lives.
	// Feedback's next-turn injection has nothing to inject into, the
	// same collapse one-shot mode has.
	wdMode, err := libraryWatchdogMode(bundle)
	if err != nil {
		return nil, err
	}
	wd := watchdog.NewDefaultWatchdog()
	enf := watchdog.NewEnforcer(wdMode, "The turn was abandoned; nothing is left halted — this process holds no cross-call session state.")
	onAlert := func(a watchdog.Alert) {
		if cfg.Logger != nil {
			cfg.Logger.Warn("watchdog alert", "session", sessionID,
				"signal", a.Signal, "severity", string(a.Severity), "reason", a.Reason)
		}
		if enf.Observe(a) && cfg.Logger != nil {
			_, reason := enf.Tripped()
			cfg.Logger.Error("WATCHDOG HALT — abandoning the turn",
				"session", sessionID, "signal", a.Signal, "reason", reason)
		}
	}

	res := &Result{SessionID: sessionID}
	for event, err := range watchdog.Tap(r.Run(ctx, userID, sessionID, msg, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}), wd, onAlert) {
		if err != nil {
			return nil, fmt.Errorf("mast: session %q: %w", sessionID, err)
		}
		// Tap drains alerts before it yields, so a halt raised by this
		// event is already visible. Returning abandons the stream
		// mid-turn — the library's cancel, and the reason the deferred
		// cancel above is not enough on its own.
		if terr := enf.Preflight(); terr != nil {
			cancel()
			return nil, fmt.Errorf("mast: session %q: %w", sessionID, terr)
		}
		// A specialist crossing its own cap does not end the run (W10.3):
		// it used to cancel here only because cancelling was the one way
		// to stop that specialist calling again, and the pre-call gate
		// does that now. The call that crossed is folded and priced either
		// way — Observe did that before it returned.
		if berr := meter.Observe(event); berr != nil {
			if _, isScope := budget.Scope(berr); !isScope {
				cancel()
				res.Usage.Tokens, res.Usage.CostUSD, res.Usage.ModelCalls = meter.Snapshot()
				return nil, fmt.Errorf("mast: session %q: %w", sessionID, berr)
			}
			if cfg.Logger != nil {
				cfg.Logger.Warn("BUDGET CEILING — a specialist crossed its own cap; the run continues",
					"session", sessionID, "error", berr.Error())
			}
		}
		// The pre-call half, stopping the stream for the same reason the
		// fold above does. A refusal costs nothing, so a retry loop above
		// the model — a contract validator handing a report back to be
		// fixed, say — never runs out of anything and spins on free calls.
		// Before W10.2 it burned the ceiling and stopped.
		if rerr := refused(); rerr != nil {
			cancel()
			return nil, rerr
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
	// Scoped entries only: a session ceiling that precludes another call
	// is not a spent *specialist*, and it is already the reason the caller
	// is holding an error rather than this Result on every path where it
	// mattered. What is left is the roster view — which paths this run
	// closed behind it.
	for _, t := range meter.PrecludedAll() {
		if t.Scope != "" {
			res.Exhausted = append(res.Exhausted, t)
		}
	}

	// Backstop for the same check, for a refusal inside a sub-agent whose
	// run surfaces no event here. The stream ended cleanly, so without
	// this the caller gets a Result whose Output is prose saying the
	// budget ran out — readable by a person, invisible to the `if err !=
	// nil` above it. budget.ErrRefused rather than ErrExceeded: the
	// ceiling held, and nothing was spent crossing it. Nil result, matching
	// every other stopped-run path on this function rather than inventing
	// a third convention for the one case that ends tidily.
	if rerr := refused(); rerr != nil {
		return nil, rerr
	}
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
