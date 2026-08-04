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

// Command mast is the entry point for the mast agent runtime.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/genai"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/mast/internal/compose"
	buildversion "github.com/go-steer/mast/internal/version"
	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/attachadapter"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/config"
	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/envelope"
	"github.com/go-steer/mast/pkg/eventlog"
	"github.com/go-steer/mast/pkg/inject"
	mastmcp "github.com/go-steer/mast/pkg/mcp"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/taskclass"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/watchdog"
	"github.com/go-steer/mast/pkg/workload"
)

// Release identity, stamped by GoReleaser via -ldflags (see
// .goreleaser.yaml). "dev" for local builds. The version string
// itself lives in internal/version so library surfaces (attach
// capabilities frames, agent cards) can report it too.
var (
	commit = ""
	date   = ""
)

const (
	appName          = "mast"
	defaultUserID    = "mast-inject"
	defaultSessionID = "gke-triage-default"
)

func main() {
	// Subcommand dispatch happens before flag parsing so the flag-only
	// serve invocation (`mast --workload=... --listen=...`) keeps
	// working exactly as before — scripts/demo-spike2.sh depends on it.
	if len(os.Args) > 1 && os.Args[1] == "sessions" {
		os.Exit(runSessions(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "stop" {
		os.Exit(runStop(os.Args[2:]))
	}
	run()
}

// run parses the flag surface shared by serve and one-shot modes and
// dispatches: a positional prompt runs one turn to completion and
// prints the result (oneshot.go); no prompt serves the daemon —
// exactly the pre-one-shot behavior, so scripts/demo-spike2.sh's
// flag-only invocations pass unchanged.
func run() {
	var (
		workloadFlag     = flag.String("workload", "", "workload to run: a name resolved via .agents/ discovery (see pkg/config), or a path to a workload directory (containing workload.yaml + specialists/)")
		dispatchMode     = flag.String("dispatch", "coordinator", "dispatch shape: `coordinator` (spike-1 SubAgents pattern) or `graph` (workflow-graph LLM-as-router)")
		modelName        = flag.String("model", "echo", "model to use: `echo` (fake, for smoke), `scripted` (JSONL replay; path via MAST_SCRIPT), a Gemini model id like `gemini-2.5-flash`, or a Claude model id like `claude-sonnet-4-6`")
		providerFlag     = flag.String("provider", "", "model provider alias: `echo`, `scripted`, `gemini`, `anthropic`, or `anthropic-vertex`. Validates against --model when both are set; picks the provider's default model (the --task profile's tier via pkg/taskclass) when --model is unset. For claude-* models the alias also picks the backend (first-party vs Vertex)")
		taskFlag         = flag.String("task", "", "one-shot task class: `chat`, `debug`, `implement`, `research`, `review`, or `orchestrate` (requires a positional prompt; defaults to chat when a prompt is given without --task)")
		listen           = flag.String("listen", ":7777", "HTTP inject endpoint bind address")
		attachListen     = flag.String("attach-listen", "", "operator attach surface bind address: a TCP address (e.g. `127.0.0.1:8484`) or a Unix socket path prefixed `unix:`; empty disables the surface. Requires --session-db (live-tail pumps from the eventlog). Non-loopback TCP binds are refused without auth — set MAST_ATTACH_TOKEN")
		sessionDB        = flag.String("session-db", "", "session store location: a SQLite file path (default driver) or a Postgres DSN/URL with --session-db-driver=postgres; empty = in-memory sessions (no durability)")
		sessionDrv       = flag.String("session-db-driver", "sqlite", "session DB driver: `sqlite` (--session-db is a file path) or `postgres` (--session-db is a DSN or postgres:// URL)")
		timeoutFlag      = flag.Duration("timeout", 5*time.Minute, "one-shot turn deadline (e.g. 2m, 90s); 0 disables. One-shot only — serve-mode ceilings come from workload budgets")
		logLevel         = flag.String("log-level", "info", "log level: debug|info|warn|error")
		autoResume       = flag.Bool("auto-resume", true, "serve mode: on boot, scan for sessions a prior shutdown interrupted and drive a continuation turn for each eligible one (coordinator dispatch only in v0.2). --auto-resume=false disables")
		autoResumeWindow = flag.Duration("auto-resume-window", time.Hour, "serve mode: only auto-resume sessions interrupted within this window; older interruptions are left for an operator (0 disables the freshness gate)")
		showVersion      = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("mast %s", buildversion.Version)
		if commit != "" {
			fmt.Printf(" (%s %s)", commit, date)
		}
		fmt.Println()
		return
	}

	logger := newLogger(*logLevel)
	slog.SetDefault(logger)

	// --provider is an alias over --model; "explicitly set" matters
	// because --model's default is "echo", not empty.
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	resolvedModel, err := resolveModelSelection(*providerFlag, *modelName, explicit["model"], *taskFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mast:", err)
		os.Exit(2)
	}
	*modelName = resolvedModel

	// One-shot mode: a positional prompt runs a single turn instead of
	// serving. --task shapes the agent (default chat); --workload is a
	// serve-mode flag and combining the two would silently pick one
	// semantics, so it errors instead.
	if flag.NArg() > 0 {
		// Go's flag package stops parsing at the first positional
		// argument, so a flag placed AFTER the prompt silently
		// becomes prompt text — `mast --task=x "prompt" --session-db=y`
		// ran with in-memory sessions and sent the flag to the model
		// (observed live 2026-07-29, twice). Refuse the ambiguity when
		// the trailing token names a flag we actually define; prompts
		// that legitimately mention flag-like words survive by quoting
		// (one argument, not starting with `-`).
		if misplaced := misplacedFlag(flag.Args(), func(name string) bool {
			return flag.Lookup(name) != nil
		}); misplaced != "" {
			fmt.Fprintf(os.Stderr, "mast: %q looks like a flag but appears after the positional prompt — Go flag parsing stops at the first positional argument, so it would be sent to the model as prompt text. Put flags before the prompt.\n", misplaced)
			os.Exit(2)
		}
		class := *taskFlag
		if class == "" {
			class = taskclass.Chat
		}
		if _, ok := taskclass.Resolve(class); !ok {
			fmt.Fprintf(os.Stderr, "mast: unknown --task %q (want one of: %s)\n",
				class, strings.Join(taskclass.Classes(), ", "))
			os.Exit(2)
		}
		if *workloadFlag != "" {
			fmt.Fprintln(os.Stderr, "mast: --workload is a serve-mode flag; one-shot mode runs a single --task-class agent")
			os.Exit(2)
		}
		if *attachListen != "" {
			fmt.Fprintln(os.Stderr, "mast: --attach-listen is a serve-mode flag; one-shot mode has no operator surface to attach to")
			os.Exit(2)
		}
		if explicit["dispatch"] {
			logger.Warn("--dispatch is a serve-mode flag; ignored in one-shot mode")
		}
		if explicit["auto-resume"] || explicit["auto-resume-window"] {
			logger.Warn("--auto-resume / --auto-resume-window are serve-mode flags; ignored in one-shot mode")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		opts := oneShotOptions{
			Class:      class,
			Provider:   *providerFlag,
			Model:      *modelName,
			SessionDB:  *sessionDB,
			SessionDrv: *sessionDrv,
			Prompt:     strings.Join(flag.Args(), " "),
			Timeout:    *timeoutFlag,
		}
		err := runOneShot(ctx, logger, opts, os.Stdout)
		stop()
		if err != nil {
			logger.Error("one-shot turn failed", "task", class, "error", err.Error())
			os.Exit(1)
		}
		return
	}
	if *taskFlag != "" {
		fmt.Fprintln(os.Stderr, "mast: --task requires a positional prompt (one-shot mode); serve mode takes --workload")
		os.Exit(2)
	}
	if explicit["timeout"] {
		logger.Warn("--timeout is a one-shot flag; ignored in serve mode (workload budgets own serve-mode ceilings)")
	}

	if err := serve(logger, *workloadFlag, *dispatchMode, *providerFlag, *modelName, *listen, *attachListen, *sessionDB, *sessionDrv, *autoResume, *autoResumeWindow); err != nil {
		// serve already logged the failure with context; the error
		// return only carries the exit status (and lets serve's defers
		// — signal stop, OTel flush — run before the process dies).
		// Exit 3 = drain expired with interrupted survivors (issue
		// #42's contract); everything else is exit 1.
		if errors.Is(err, errDrainExpired) {
			os.Exit(3)
		}
		os.Exit(1)
	}
}

// serve runs the daemon: inject endpoint + runner + session store.
// Fatal startup errors are logged in place and returned (not
// os.Exit'd) so the deferred cleanups run.
func serve(logger *slog.Logger, workloadArg, dispatchMode, providerName, modelName, listen, attachListen, sessionDB, sessionDrv string, autoResume bool, autoResumeWindow time.Duration) error {

	bearer := os.Getenv("MAST_INJECT_TOKEN")
	if bearer == "" {
		logger.Warn("MAST_INJECT_TOKEN not set; inject endpoint is unauthenticated (dev only)")
	}

	// Two lifetimes (docs/durable-execution-design.md, "Shutdown
	// contract"): ctx ends when a shutdown SIGNAL arrives and triggers
	// the drain; turnCtx is what turns, toolsets, and the eventlog
	// actually live on, and ends only when the drain window elapses —
	// so an in-flight turn keeps its tools and its context for up to
	// its own budget ceiling after SIGTERM instead of dying instantly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	turnCtx, cancelTurns := context.WithCancel(context.Background())
	defer cancelTurns()

	// Env-gated OTel trace export: a no-op unless OTEL_EXPORTER_OTLP_*
	// endpoints are set. mast opens no spans of its own in v0.1 — ADK
	// v2's runner emits the span tree; this only exports it.
	otelShutdown, otelEnabled, err := observability.SetupOTel(turnCtx)
	if err != nil {
		logger.Error("failed to configure OTel trace export", "error", err.Error())
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = otelShutdown(flushCtx)
	}()
	if otelEnabled {
		logger.Info("OTel trace export enabled", "endpoint_source", "OTEL_EXPORTER_OTLP_* env")
	}

	llm, err := buildModel(turnCtx, providerName, modelName)
	if err != nil {
		logger.Error("failed to construct model", "model", modelName, "error", err.Error())
		return err
	}
	logger.Info("model constructed", "name", llm.Name())

	// Session backend, built BEFORE the root agent: the planner's
	// pause_session tool needs the transcript store at construction
	// time (v0.2 pause/abort). With --attach-listen the store opens
	// through pkg/eventlog instead of raw session/database: same ADK
	// tables, plus the seq-overlay the attach broadcaster live-tails.
	// Without attach the plain service keeps the pre-P1.3c shape
	// (including in-memory sessions when --session-db is empty).
	var (
		sessionSvc session.Service
		elHandle   *eventlog.Handle
	)
	if attachListen != "" {
		if sessionDB == "" {
			logger.Error(errAttachNeedsSessionDB.Error())
			return errAttachNeedsSessionDB
		}
		dial, err := sessionDialector(sessionDrv, sessionDB)
		if err != nil {
			logger.Error("failed to construct session service", "error", err.Error())
			return err
		}
		elHandle, err = eventlog.Open(turnCtx, dial)
		if err != nil {
			logger.Error("failed to open eventlog-backed session store", "error", err.Error())
			return err
		}
		defer func() { _ = elHandle.Close() }()
		sessionSvc = elHandle.Service
		logger.Info("session db opened (eventlog overlay for attach)", "driver", sessionDrv)
	} else {
		var err error
		sessionSvc, err = buildSessionService(turnCtx, sessionDrv, sessionDB, logger)
		if err != nil {
			logger.Error("failed to construct session service", "error", err.Error())
			return err
		}
	}
	// Operator surface over the same session service the runner writes
	// through (docs/durable-execution-design.md, "Operator-facing
	// surface"): /abort appends the durable abort marker, and /resume
	// refuses sessions that carry one. Built before the runner because
	// the outbox plugin reads the effects-ack watermark through it.
	store := transcript.NewStore(sessionSvc, appName)

	// pause_session's record sink: the store, plus a timer push into
	// the scheduler once it exists (attached below — the scheduler
	// needs the runner, which needs the root).
	pauseRec := &daemonPauseRecorder{store: store}

	root, bundle, err := buildRoot(turnCtx, logger, llm, modelName, workloadArg, dispatchMode, pauseRec)
	if err != nil {
		logger.Error("failed to construct root agent", "error", err.Error())
		return err
	}
	logger.Info("root agent constructed",
		"name", root.Name(),
		"sub_agents", len(root.SubAgents()),
	)

	// Recorded-effect outbox (docs/durable-execution-design.md): the
	// runner plugin that refuses mutating tool calls while a session
	// carries unacknowledged dangling intents from an interrupted turn,
	// and replays recorded completions instead of re-executing. Every
	// runner construction path attaches it (#53's lesson).
	// Built once and shared with the boot-time auto-resume pass so its
	// eligibility gate classifies dangling calls exactly as the outbox does.
	effPred := effects.NewPredicate(effects.Overrides(logger, toolPolicies(bundle)))
	effSubAgents := effects.SubAgentNames(root)
	outboxPlugin, err := effects.New(effects.Config{
		Predicate:     effPred,
		SubAgentNames: effSubAgents,
		AckedAt: func(ctx context.Context, sid string) (time.Time, bool) {
			return store.EffectsAckedAt(ctx, "", sid)
		},
		Logger: logger,
	})
	if err != nil {
		logger.Error("failed to construct effects outbox", "error", err.Error())
		return err
	}

	r, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    sessionSvc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{outboxPlugin}},
	})
	if err != nil {
		logger.Error("failed to construct runner", "error", err.Error())
		return err
	}

	meters := newMeterPool(bundle, modelName)
	wds := newWatchdogPool()

	// Shutdown bookkeeping: which sessions have a turn in flight, and
	// the pre-mark/clear ordering for their interruption markers.
	tracker := newTurnTracker(store, logger)

	// One turn per session at a time (#62): a second runner turn on
	// the same session row dies on ADK's stale-session check, so
	// same-session injects/resumes queue behind the in-flight turn
	// (bounded by the workload wallclock budget) instead of losing it.
	turnLocks := newSessionTurnLocks()

	// Fixed metric registry (pkg/observability owns every family name;
	// nothing here can mint new ones). Single-workload process in v0.1,
	// so the workload label is resolved once.
	obs := observability.New()
	workloadName := "(none)"
	if bundle != nil {
		workloadName = bundle.Name
	}
	obs.Prime(workloadName)

	// Operator attach surface (--attach-listen): registry + resumer +
	// per-session adapters over the same runTurn path the inject
	// endpoint drives. Bound here (fail-fast), served after the inject
	// server is up.
	var att *attachDeps
	if attachListen != "" {
		adapterFor := func(sid string) (attach.Registrant, error) {
			return attachadapter.New(attachadapter.Config{
				AppName:     appName,
				UserID:      defaultUserID,
				SessionID:   sid,
				EventLog:    elHandle,
				BaseContext: turnCtx,
				ModelName:   llm.Name(),
				Description: attachDescription(bundle),
				RunTurn: func(turnCtx context.Context, message string) (attachadapter.TurnResult, error) {
					// The attach surface stays up through the shutdown
					// drain so operators can live-tail finishing turns —
					// but NEW work is refused once draining (#48), or an
					// operator could burn the whole grace period.
					if tracker.isDraining() {
						return attachadapter.TurnResult{}, errors.New("daemon is shutting down; not accepting new turns")
					}
					// Same wallclock ceiling as the inject dispatch
					// path — operator turns are not budget-exempt.
					if bundle != nil && bundle.Budget.MaxWallclockSeconds > 0 {
						var cancel context.CancelFunc
						turnCtx, cancel = context.WithTimeout(turnCtx, time.Duration(bundle.Budget.MaxWallclockSeconds)*time.Second)
						defer cancel()
					}
					msg := genai.NewContentFromText(message, genai.RoleUser)
					err := runTurn(turnCtx, r, logger, store, meters, wds, obs, tracker, turnLocks, workloadName, sid, msg, "attach:inject")
					// Token split is unknown at this layer (the meter
					// folds totals only); cost rides the usage snapshot.
					return attachadapter.TurnResult{}, err
				},
				UsageFn: func() attach.UsageInfo {
					_, cost, calls := meters.meter(sid).Snapshot()
					return attach.UsageInfo{Overall: attach.UsageTotals{Turns: calls, CostUSD: cost}}
				},
			})
		}
		var err error
		att, err = buildAttach(logger, attachListen, os.Getenv("MAST_ATTACH_TOKEN"), store, adapterFor)
		if err != nil {
			logger.Error("failed to construct attach surface", "error", err.Error())
			return err
		}
		defer func() { _ = att.srv.Close() }()
	}

	// Drain bound, needed by the stop handler's response before the
	// shutdown goroutine exists.
	drain := drainBound(bundle)

	handler := func(reqCtx context.Context, p envelope.InjectPayload) error {
		// Drain gate (#58): a request that made it past accept before
		// the listener closed must not start a fresh turn mid-drain.
		if tracker.isDraining() {
			return inject.ErrUnavailable
		}
		if err := reservedPayloadErr(p); err != nil {
			return err
		}
		att.ensure(sessionIDFor(p))
		return dispatch(reqCtx, r, logger, store, meters, wds, obs, tracker, turnLocks, workloadName, bundle, p)
	}
	// resumeByInterrupt is the shared inner resume path (operator
	// interrupt keying, token keying, and the timed-pause scheduler all
	// land here).
	resumeByInterrupt := func(reqCtx context.Context, req inject.ResumeRequest) error {
		// Companion ops rows are marker storage, not sessions (#56):
		// resuming one would drive a runner turn into the marker row.
		if transcript.IsReservedSessionID(req.SessionID) {
			return fmt.Errorf("session ID %q uses the reserved ops-row suffix; not a resumable session: %w", req.SessionID, inject.ErrBadPayload)
		}
		// Fast-path refusal for a clearer message; the runTurnPre
		// chokepoint is the authoritative check (under the turn lock).
		if d, err := store.Get(reqCtx, "", req.SessionID); err == nil && d.State == transcript.StateAborted {
			return fmt.Errorf("session %q is aborted (%s); refusing resume: %w", req.SessionID, d.AbortReason, inject.ErrConflict)
		}
		// The ack watermark is written under the session's turn lock
		// (runTurn's preTurn hook), AFTER any in-flight turn on the
		// session has finished: a watermark stamped while a turn is
		// still persisting mutating intents would silently cover
		// intents the operator never saw. It is still durable before
		// the resume turn's outbox scan runs. If the ack lands and the
		// turn itself then fails, the watermark stays — it acknowledges
		// the PRIOR intents, not the new turn; a retried resume does
		// not need (and is not harmed by) re-acking.
		var preTurn func(context.Context) error
		if req.AckEffects {
			preTurn = func(ctx context.Context) error {
				if err := store.AckEffects(ctx, "", req.SessionID, "operator resume --ack-effects"); err != nil {
					return fmt.Errorf("record effects acknowledgement for session %q: %w", req.SessionID, err)
				}
				return nil
			}
		}
		att.ensure(req.SessionID)
		return resume(reqCtx, r, logger, store, meters, wds, obs, tracker, turnLocks, workloadName, bundle, req, preTurn)
	}
	// resumeByToken resolves a resume token to its pause and resumes it
	// (v0.2 pause/abort design, "Resume tokens"): gate pause → consume
	// IS the resume (no turn runs — nothing was parked); interrupt
	// pause → the normal resume path, with consumption keyed on the
	// durable append of the resume FunctionResponse.
	resumeByToken := func(reqCtx context.Context, req inject.ResumeRequest) error {
		rec, err := store.FindToken(reqCtx, req.Token)
		if err != nil {
			return fmt.Errorf("%v: %w", err, inject.ErrBadPayload)
		}
		if !rec.ConsumedAt.IsZero() {
			// already_resumed is a structured no-op, not an error: the
			// resume the token asked for has happened.
			logger.Info("resume token already consumed; no-op",
				"session", rec.SessionID, "consumed_at", rec.ConsumedAt.Format(time.RFC3339), "consumed_by", rec.ConsumedBy)
			return nil
		}
		if rec.Expired(time.Now().UTC()) {
			return fmt.Errorf("resume token expired %s (the pause remains; `mast sessions extend-token` is the recovery): %w",
				rec.ExpiresAt.Format(time.RFC3339), inject.ErrConflict)
		}
		if rec.Plane == transcript.PlaneGate {
			_, err := store.ConsumeToken(reqCtx, req.Token, "operator resume --token")
			if errors.Is(err, transcript.ErrAlreadyResumed) {
				return nil
			}
			return err
		}
		response := req.Response
		if response == nil {
			response = map[string]any{"resumed_by": "operator"}
		}
		inner := inject.ResumeRequest{
			SessionID:   rec.SessionID,
			InterruptID: rec.InterruptID,
			Response:    response,
			AckEffects:  req.AckEffects,
		}
		rerr := resumeByInterrupt(reqCtx, inner)
		// Consumption keys on the durable append, not turn success: use
		// a fresh short-lived context so a request-scope cancellation
		// cannot strand an answered interrupt with a live token.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), storeWriteTimeout)
		defer cancel()
		consumeIfAnswered(cctx, store, logger, rec, "operator resume --token")
		return rerr
	}
	resumeHandler := func(reqCtx context.Context, req inject.ResumeRequest) error {
		if tracker.isDraining() {
			return inject.ErrUnavailable
		}
		if req.Token != "" {
			return resumeByToken(reqCtx, req)
		}
		return resumeByInterrupt(reqCtx, req)
	}
	abortHandler := func(reqCtx context.Context, req inject.AbortRequest) error {
		if transcript.IsReservedSessionID(req.SessionID) {
			return fmt.Errorf("session ID %q uses the reserved ops-row suffix; not an abortable session: %w", req.SessionID, inject.ErrBadPayload)
		}
		// Terminal abort (v0.2): marker first — the durable truth —
		// then sweep the in-flight turn's cancel handle. Deliberately
		// no turn lock here: abort must not queue behind the very turn
		// it cancels (the register-before-check handshake in runTurnPre
		// closes the ordering window instead).
		if err := store.Abort(reqCtx, "", req.SessionID, req.Reason); err != nil {
			return err
		}
		if tracker.cancelSession(req.SessionID) {
			logger.Info("abort cancelled in-flight turn", "session", req.SessionID)
		}
		return nil
	}
	// Standalone ack surface: the outbox's primary scenario — a process
	// killed mid-mutating-tool — leaves a dangling intent and NO
	// pending interrupt, so resume --ack-effects (which requires one)
	// cannot reach it. Same daemon-routed shape as abort (single
	// writer). Serialized against in-flight turns via the turn lock so
	// the watermark cannot cover intents still being persisted.
	ackHandler := func(reqCtx context.Context, req inject.AckEffectsRequest) error {
		// Same drain contract as inject/resume (#58/#65): refuse new
		// work while shutting down, and map a drain-cancelled lock wait
		// to 503 rather than a bare 500.
		if tracker.isDraining() {
			return inject.ErrUnavailable
		}
		if transcript.IsReservedSessionID(req.SessionID) {
			return fmt.Errorf("session ID %q uses the reserved ops-row suffix; not a session: %w", req.SessionID, inject.ErrBadPayload)
		}
		unlock, err := turnLocks.lock(reqCtx, req.SessionID)
		if err != nil {
			if tracker.isDraining() {
				return fmt.Errorf("%w (queued ack cancelled: %v)", inject.ErrUnavailable, err)
			}
			return err
		}
		defer unlock()
		if err := store.AckEffects(reqCtx, "", req.SessionID, req.Reason); err != nil {
			// An operator typo is a client error, not a daemon fault.
			if errors.Is(err, transcript.ErrNotFound) {
				return fmt.Errorf("%v: %w", err, inject.ErrBadPayload)
			}
			return err
		}
		return nil
	}

	// Timed-pause scheduler (v0.2 pause/abort design): fires through
	// the same doors an operator would use — no privileged side path.
	sched := newPauseScheduler(store, logger, func(fireCtx context.Context, rec *transcript.PauseRecord) error {
		if tracker.isDraining() {
			return errors.New("daemon draining")
		}
		if rec.Plane == transcript.PlaneGate {
			// ConsumeScheduled, not ConsumeToken: the timer is the daemon's
			// own commitment and is not vetoed by the operator-facing token
			// TTL — a resume_at beyond the token's life would otherwise
			// livelock this fire against an expired token forever.
			_, err := store.ConsumeScheduled(fireCtx, rec.Token, "timer")
			if errors.Is(err, transcript.ErrAlreadyResumed) {
				return nil // an operator resumed earlier and won benignly
			}
			return err
		}
		req := inject.ResumeRequest{
			SessionID:   rec.SessionID,
			InterruptID: rec.InterruptID,
			Response:    map[string]any{"resumed_by": "timer", "resume_at": rec.ResumeAt.Format(time.RFC3339)},
		}
		rerr := resumeByInterrupt(fireCtx, req)
		cctx, cancel := context.WithTimeout(context.WithoutCancel(fireCtx), storeWriteTimeout)
		defer cancel()
		consumeIfAnswered(cctx, store, logger, rec, "timer")
		return rerr
	})
	go sched.run(turnCtx)
	go func() {
		// Boot scan: seeds timers minted before this process started —
		// including ones that expired while the daemon was down.
		if err := sched.seed(turnCtx); err != nil {
			logger.Error("timed-pause boot scan failed; pre-existing timers will not fire until restart", "error", err.Error())
		}
	}()
	// pause_session records minted mid-serve now push their timers too.
	pauseRec.attach(sched)

	// Boot-time auto-resume (#41): scan sessions a prior shutdown cut
	// short and drive a continuation for each eligible one. On turnCtx
	// (drain-cancellable) and only with a durable store — in-memory
	// sessions never survive a restart, so there is nothing to resume.
	// bootDone lets the drain path await this goroutine so it cannot
	// start a fresh turn after the drain has sampled "all turns finished"
	// (closed immediately when the pass never launches).
	bootDone := make(chan struct{})
	if autoResume && sessionDB != "" {
		ar := &autoResumer{
			runner:       r,
			logger:       logger,
			store:        store,
			meters:       meters,
			wds:          wds,
			obs:          obs,
			tracker:      tracker,
			turnLocks:    turnLocks,
			workloadName: workloadName,
			bundle:       bundle,
			dispatchMode: dispatchMode,
			pred:         effPred,
			subAgents:    effSubAgents,
			window:       autoResumeWindow,
		}
		go func() {
			defer close(bootDone)
			ar.run(turnCtx)
		}()
	} else {
		close(bootDone)
		if autoResume {
			logger.Info("auto-resume enabled but --session-db is empty (in-memory sessions); nothing to resume")
		}
	}

	pauseHandler := func(reqCtx context.Context, req inject.PauseRequest) (inject.PauseResult, error) {
		// No drain gate, like abort: a gate pause is a marker write,
		// and pausing during a drain is a legitimate operator move.
		if transcript.IsReservedSessionID(req.SessionID) {
			return inject.PauseResult{}, fmt.Errorf("session ID %q uses the reserved ops-row suffix; not a session: %w", req.SessionID, inject.ErrBadPayload)
		}
		spec := transcript.PauseSpec{
			Reason:   transcript.Reason(req.Reason),
			Message:  req.Message,
			Metadata: req.Metadata,
		}
		if req.ResumeAt != "" {
			at, err := time.Parse(time.RFC3339, req.ResumeAt)
			if err != nil {
				return inject.PauseResult{}, fmt.Errorf("resume_at %q is not RFC3339: %w", req.ResumeAt, inject.ErrBadPayload)
			}
			spec.ResumeAt = at.UTC()
		}
		if req.TTL != "" {
			ttl, err := time.ParseDuration(req.TTL)
			if err != nil {
				return inject.PauseResult{}, fmt.Errorf("ttl %q is not a duration: %w", req.TTL, inject.ErrBadPayload)
			}
			spec.TokenTTL = ttl
		}
		h, err := store.PauseGate(reqCtx, "", req.SessionID, spec)
		if err != nil {
			switch {
			case errors.Is(err, transcript.ErrAlreadyAborted):
				return inject.PauseResult{}, fmt.Errorf("%v: %w", err, inject.ErrConflict)
			case errors.Is(err, transcript.ErrNotFound):
				return inject.PauseResult{}, fmt.Errorf("%v: %w", err, inject.ErrBadPayload)
			default:
				// Spec validation (unknown reason, over-long TTL) is an
				// operator error too.
				return inject.PauseResult{}, fmt.Errorf("%v: %w", err, inject.ErrBadPayload)
			}
		}
		if req.Interrupt {
			// Hard pause: marker durably landed (PauseGate returned), now
			// sweep — the same mark-then-sweep handshake abort uses.
			if tracker.cancelSession(req.SessionID) {
				logger.Info("hard pause cancelled in-flight turn", "session", req.SessionID)
			}
		}
		if !spec.ResumeAt.IsZero() {
			sched.push(h.Token, spec.ResumeAt)
		}
		return inject.PauseResult{
			Token:     h.Token,
			SessionID: h.SessionID,
			ExpiresAt: h.ExpiresAt.Format(time.RFC3339),
		}, nil
	}
	extendHandler := func(reqCtx context.Context, req inject.ExtendTokenRequest) (inject.ExtendTokenResult, error) {
		ttl, err := time.ParseDuration(req.TTL)
		if err != nil {
			return inject.ExtendTokenResult{}, fmt.Errorf("ttl %q is not a duration: %w", req.TTL, inject.ErrBadPayload)
		}
		rec, err := store.ExtendToken(reqCtx, req.Token, ttl)
		if err != nil {
			switch {
			case errors.Is(err, transcript.ErrAlreadyResumed):
				return inject.ExtendTokenResult{}, fmt.Errorf("%v: %w", err, inject.ErrConflict)
			case errors.Is(err, transcript.ErrTokenNotFound):
				return inject.ExtendTokenResult{}, fmt.Errorf("%v: %w", err, inject.ErrBadPayload)
			default:
				return inject.ExtendTokenResult{}, err
			}
		}
		return inject.ExtendTokenResult{Token: rec.Token, ExpiresAt: rec.ExpiresAt.Format(time.RFC3339)}, nil
	}
	stopHandler := func(reqCtx context.Context, req inject.StopRequest) (inject.StopResult, error) {
		// Planned stop (issue #42): classify, then run EXACTLY the
		// SIGTERM drain path. Exit codes encode work-cut-short, not
		// initiator (0 clean drain / 3 drain expired with survivors).
		reason := "operator stop"
		if req.Reason != "" {
			reason += ": " + req.Reason
		}
		tracker.planStop(reason, req.PauseSessions)
		logger.Info("planned stop initiated",
			"reason", reason, "pause_sessions", req.PauseSessions, "drain_bound", drain.String())
		stop() // cancels the signal context; the shutdown goroutine drains
		return inject.StopResult{DrainBound: drain.String()}, nil
	}

	srv, err := inject.New(inject.Config{
		Listen:             listen,
		BearerToken:        bearer,
		Handler:            handler,
		ResumeHandler:      resumeHandler,
		AbortHandler:       abortHandler,
		AckEffectsHandler:  ackHandler,
		PauseHandler:       pauseHandler,
		ExtendTokenHandler: extendHandler,
		StopHandler:        stopHandler,
		Logger:             logger,
		Metrics:            obs.Handler(),
		// Request contexts derive from the turn lifetime, so when the
		// drain window elapses the surviving handler turns are
		// cancelled (and unwind) rather than dying at process exit.
		BaseContext: turnCtx,
	})
	if err != nil {
		logger.Error("failed to construct inject server", "error", err.Error())
		return err
	}

	// Shutdown sequence (#38/#39): pre-mark in-flight sessions durably,
	// then drain up to the bound, then cancel survivors. The attach
	// surface deliberately stays up through the drain — operators
	// live-tailing a finishing turn see its final events; the deferred
	// att.srv.Close() runs after serve returns. drainExpired feeds the
	// exit-code contract (issue #42): work cut short exits 3.
	var drainExpired bool
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		logger.Info("shutdown signal received; draining in-flight turns", "drain_bound", drain.String())
		drainCtx, cancel := context.WithTimeout(context.Background(), drain)
		defer cancel()
		// Close the inject listener FIRST (#58): Shutdown stops
		// accepting immediately and then waits for handlers, so
		// launching it before the pre-mark pass means no new turn can
		// arrive while markers are being written. The drain-gate in
		// the handlers covers requests already past accept.
		shutdownErr := make(chan error, 1)
		go func() { shutdownErr <- srv.Shutdown(drainCtx) }()
		// Pre-mark BEFORE waiting: a SIGKILL mid-drain must find the
		// interruption markers already on disk.
		tracker.beginDrain(drainCtx)
		// Shutdown waits for inject handlers; tracker.wait additionally
		// covers attach-driven turns, which run outside HTTP handlers.
		// Both share the one deadline.
		errShutdown := <-shutdownErr
		// Await the boot-time auto-resume pass before sampling in-flight
		// turns: it gates on isDraining() so it returns promptly, but a
		// boot turn it already started is tracked and must be counted by
		// wait() below — awaiting here guarantees the pass cannot begin a
		// new turn after wait() has concluded the daemon is idle. Bounded
		// by the shared drain deadline; a mid-flight boot turn past the
		// deadline is handled as a survivor like any other.
		select {
		case <-bootDone:
		case <-drainCtx.Done():
		}
		remaining := tracker.wait(drainCtx)
		if len(remaining) == 0 {
			if errShutdown != nil {
				// No turns in flight — the listener just has lingering
				// non-turn connections (an SSE scrape, a slow client).
				logger.Warn("inject server still draining connections at the deadline; no turns were in flight", "error", errShutdown.Error())
				return
			}
			logger.Info("drain complete; all in-flight turns finished")
			return
		}
		// Freeze before cancelling: the surviving turns ARE interrupted,
		// and their unwinding must not clear the markers that say so.
		tracker.freeze()
		cancelTurns()
		// Give the cancelled turns a short beat to unwind before the
		// deferred teardown (attach close, eventlog close) yanks their
		// dependencies — cancellation is useless if the process exits
		// before the cancelled goroutines observe it (#48).
		graceCtx, graceCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer graceCancel()
		tracker.wait(graceCtx)
		// Honest split (#58/#63): marked survivors truly carry durable
		// markers; unmarked survivors are turns whose mark write failed
		// or never ran — the log must not assert durability for them.
		markedSurvivors, unmarkedSurvivors := tracker.survivors()
		drainExpired = true
		logger.Warn("drain window elapsed; sessions cut short",
			"sessions_with_durable_marker", markedSurvivors,
			"sessions_without_marker", unmarkedSurvivors,
			"drain_bound", drain.String())
	}()

	if att != nil {
		go func() {
			// The listener is already bound (buildAttach); Serve only
			// returns on Close or a hard accept failure. A hard failure
			// takes the daemon down — a half-alive daemon whose operator
			// surface silently died is worse than a restart.
			if err := att.srv.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
				logger.Error("attach server terminated", "error", err.Error())
				stop()
			}
		}()
	}

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// Startup/hard failure: the shutdown goroutine is still parked
		// on ctx.Done, so return without waiting on it.
		logger.Error("inject server terminated", "error", err.Error())
		return err
	}
	// ListenAndServe returns ErrServerClosed the moment Shutdown BEGINS;
	// the drain (and its marker bookkeeping) completes in the shutdown
	// goroutine. Returning before it finishes was #38.
	<-shutdownDone
	if drainExpired {
		// Exit-code contract (issue #42): 3 = the drain window expired
		// with interrupted survivors — work was cut short, whoever
		// initiated the stop. Restart=on-failure supervision revives
		// the daemon exactly when the boot pass has repair work (#41);
		// a clean drain exits 0 and such a unit stays down.
		logger.Warn("shutdown complete; drain expired with interrupted sessions (exit 3)")
		return errDrainExpired
	}
	logger.Info("shutdown complete")
	return nil
}

// errDrainExpired maps to exit code 3 in run(): the shutdown drain
// window expired with turns still in flight (their sessions carry
// interruption markers where the write landed).
var errDrainExpired = errors.New("drain window expired with interrupted sessions")

// misplacedFlag returns the first positional argument that names a
// defined flag (leading dashes stripped, =value ignored), or "" when
// none do. defined reports whether a flag name exists — injected so
// tests don't depend on package-level flag registration order.
func misplacedFlag(args []string, defined func(string) bool) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if name != "" && defined(name) {
			return a
		}
	}
	return ""
}

// buildModel constructs the model.LLM for the given provider alias
// and name. Thin alias over the shared core (internal/compose) so the
// flag surface and the library surface can't drift.
func buildModel(ctx context.Context, provider, name string) (model.LLM, error) {
	return compose.BuildModel(ctx, provider, name)
}

// resolveWorkload turns the --workload flag value into a loaded bundle
// plus its specialist specs. Two modes:
//
//   - Path mode: the value is an existing directory → legacy spike
//     layout (workload.yaml + specialists/ inside that directory).
//     scripts/demo-spike2.sh depends on this shape; unchanged.
//   - Name mode: anything else is a workload name resolved via the
//     .agents/ discovery rules in pkg/config (exclusive
//     single-location; see docs/config-layout-design.md).
func resolveWorkload(logger *slog.Logger, arg string) (workload.Bundle, []specialists.Spec, error) {
	if fi, err := os.Stat(arg); err == nil && fi.IsDir() {
		bundle, err := workload.Load(filepath.Join(arg, "workload.yaml"))
		if err != nil {
			return workload.Bundle{}, nil, fmt.Errorf("load workload: %w", err)
		}
		loaded, err := specialists.LoadDir(filepath.Join(arg, "specialists"))
		if err != nil {
			return workload.Bundle{}, nil, fmt.Errorf("load specialists: %w", err)
		}
		return bundle, loaded, nil
	}

	cfg, err := config.Load(logger)
	if err != nil {
		return workload.Bundle{}, nil, err
	}
	bundle, ok := cfg.Workloads[arg]
	if !ok {
		return workload.Bundle{}, nil, fmt.Errorf(
			"workload %q not found in config root %s (source %s; available: %v) and it is not a directory path",
			arg, cfg.Root.Dir, cfg.Root.Source, workloadNames(cfg))
	}
	// Name mode builds only the bundle's roster (the root's
	// specialists/ dir may serve many workloads). LoadRoot already
	// validated every roster reference resolves.
	loaded := make([]specialists.Spec, 0, len(bundle.Specialists))
	for _, name := range bundle.Specialists {
		loaded = append(loaded, cfg.Specialists[name])
	}
	return bundle, loaded, nil
}

func workloadNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Workloads))
	for name := range cfg.Workloads {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// buildRoot wires the top-level agent. With --workload set it loads
// the workload bundle + specialists + tool catalog and hands the
// loaded roster to the shared core (internal/compose.BuildRoot — the
// same code the library-facing mast.RunWorkload uses) to construct
// the dispatch shape selected by --dispatch: the spike-1 SubAgents
// coordinator (pkg/router) or the spike-2 workflow graph (pkg/graph).
// Without --workload it constructs a trivial single-agent coordinator
// (useful for pure inject-endpoint smoke).
func buildRoot(ctx context.Context, logger *slog.Logger, llm model.LLM, modelName, workloadArg, dispatch string, pauseRec planner.PauseRecorder) (adkagent.Agent, *workload.Bundle, error) {
	if dispatch != "coordinator" && dispatch != "graph" {
		return nil, nil, fmt.Errorf("unknown --dispatch %q (want `coordinator` or `graph`)", dispatch)
	}
	if workloadArg == "" {
		logger.Warn("no --workload supplied; running trivial single-agent coordinator")
		a, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
			Name:        "trivial_coordinator",
			Description: "Trivial coordinator (no workload loaded).",
			Instruction: "Acknowledge the incident briefly.",
			Model:       llm,
		})
		return a, nil, err
	}

	bundle, loaded, err := resolveWorkload(logger, workloadArg)
	if err != nil {
		return nil, nil, err
	}
	logger.Info("workload loaded",
		"name", bundle.Name,
		"specialists", len(bundle.Specialists),
		"mcp_servers", len(bundle.ToolCatalog.MCP),
	)
	logger.Info("specialists loaded", "count", len(loaded))

	// Attach MCP toolsets to specialists only when a real model is in
	// use. The echo model doesn't call tools; wiring MCP under echo
	// would only add startup latency (and mask ADC-availability issues
	// as workload-load failures rather than "no real LLM"). MCP wiring
	// stays daemon-side: the library path (mast.RunWorkload) is
	// filesystem- and network-config-free in v0.1.
	var toolsets []tool.Toolset
	if modelName != "echo" {
		for _, ref := range bundle.ToolCatalog.MCP {
			if ref.Server != "gke" {
				logger.Warn("skipping unknown MCP server (spike supports gke only)", "server", ref.Server)
				continue
			}
			ts, err := mastmcp.NewGKEToolset(ctx, mastmcp.GKEConfig{})
			if err != nil {
				return nil, nil, fmt.Errorf("wire MCP server %q: %w", ref.Server, err)
			}
			toolsets = append(toolsets, ts)
			logger.Info("MCP toolset wired", "server", ref.Server)
		}
	}

	a, err := compose.BuildRoot(compose.RootConfig{
		Bundle:        bundle,
		Specs:         loaded,
		Model:         llm,
		Toolsets:      toolsets,
		Dispatch:      compose.Dispatch(dispatch),
		Logger:        logger,
		PauseRecorder: pauseRec,
	})
	if err != nil {
		return nil, nil, err
	}
	return a, &bundle, nil
}

// reservedPayloadErr rejects payloads whose derived session ID uses
// the reserved ops-row suffix (#61): the UID is untrusted and mints
// the session ID, so it is a session-ID surface like any other — a
// reserved ID would create a session the marker machinery corrupts
// and the operator surface hides. Wraps inject.ErrBadPayload so the
// server answers 400, not 500 (emitters must not retry it).
func reservedPayloadErr(p envelope.InjectPayload) error {
	if sid := sessionIDFor(p); transcript.IsReservedSessionID(sid) {
		return fmt.Errorf("payload uid %q derives reserved session ID %q: %w", p.UID, sid, inject.ErrBadPayload)
	}
	return nil
}

// sessionIDFor derives a per-incident session ID from the payload so
// each incident's history, pauses, and resumes are isolated (spike-2
// change; spike 1 funneled every inject into one shared session).
func sessionIDFor(p envelope.InjectPayload) string {
	if p.UID != "" {
		return "incident-" + p.UID
	}
	return defaultSessionID
}

// meterPool hands out one budget.Meter per session, sized from the
// workload bundle's budget block.
type meterPool struct {
	mu     sync.Mutex
	limits budget.Limits
	byID   map[string]*budget.Meter
}

func newMeterPool(bundle *workload.Bundle, modelName string) *meterPool {
	// Pricing lives in the shared core (internal/compose.RatePer1K)
	// so the daemon and mast.RunWorkload derive identical costs.
	limits := budget.Limits{RatePer1K: compose.RatePer1K(modelName)}
	if bundle != nil {
		limits.MaxCostUSD = bundle.Budget.MaxCostUSD
		// Workload turn ceiling: one "turn" = one model call (see
		// budget.Limits.MaxTurns for the vocabulary).
		limits.MaxTurns = bundle.Budget.MaxTurns
	}
	return &meterPool{limits: limits, byID: map[string]*budget.Meter{}}
}

func (mp *meterPool) meter(sessionID string) *budget.Meter {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	m, ok := mp.byID[sessionID]
	if !ok {
		m = budget.NewMeter(mp.limits)
		mp.byID[sessionID] = m
	}
	return m
}

// watchdogPool hands out one watchdog per session, mirroring
// meterPool: the repeated-tool-call signal counts consecutive
// identical calls across turns, so its state must live with the
// session, not the turn — and a daemon-global watchdog would blend
// unrelated sessions' tool streams into false positives.
type watchdogPool struct {
	mu   sync.Mutex
	byID map[string]*watchdog.DefaultWatchdog
}

func newWatchdogPool() *watchdogPool {
	return &watchdogPool{byID: map[string]*watchdog.DefaultWatchdog{}}
}

func (wp *watchdogPool) watchdog(sessionID string) *watchdog.DefaultWatchdog {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	w, ok := wp.byID[sessionID]
	if !ok {
		w = watchdog.NewDefaultWatchdog()
		wp.byID[sessionID] = w
	}
	return w
}

// toolPolicies converts the bundle's tool_catalog per-tool overrides
// into the shape pkg/effects consumes (nil bundle = no overrides).
func toolPolicies(bundle *workload.Bundle) []effects.ToolPolicy {
	if bundle == nil {
		return nil
	}
	out := make([]effects.ToolPolicy, 0, len(bundle.ToolCatalog.Tools))
	for _, p := range bundle.ToolCatalog.Tools {
		out = append(out, effects.ToolPolicy{Name: p.Name, Mutating: p.Mutating})
	}
	return out
}

func dispatch(ctx context.Context, r *runner.Runner, logger *slog.Logger, store *transcript.Store, meters *meterPool, wds *watchdogPool, obs *observability.Registry, tracker *turnTracker, turnLocks *sessionTurnLocks, workloadName string, bundle *workload.Bundle, p envelope.InjectPayload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal inject payload: %w", err)
	}
	// Workload wallclock ceiling: bound the whole turn.
	if bundle != nil && bundle.Budget.MaxWallclockSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(bundle.Budget.MaxWallclockSeconds)*time.Second)
		defer cancel()
	}
	msg := genai.NewContentFromText(fmt.Sprintf("INJECT %s", string(body)), genai.RoleUser)
	return runTurn(ctx, r, logger, store, meters, wds, obs, tracker, turnLocks, workloadName, sessionIDFor(p), msg, "inject:"+p.Reason)
}

// resume feeds an operator's approval verdict back into a paused
// session. The runner treats a user turn carrying a FunctionResponse
// whose ID matches a pending InterruptID as a resume (see adk/v2
// runner buildResumeResponses).
func resume(ctx context.Context, r *runner.Runner, logger *slog.Logger, store *transcript.Store, meters *meterPool, wds *watchdogPool, obs *observability.Registry, tracker *turnTracker, turnLocks *sessionTurnLocks, workloadName string, bundle *workload.Bundle, req inject.ResumeRequest, preTurn func(context.Context) error) error {
	// Same wallclock ceiling as the inject and attach paths — resume
	// turns are not budget-exempt either (#47).
	if bundle != nil && bundle.Budget.MaxWallclockSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(bundle.Budget.MaxWallclockSeconds)*time.Second)
		defer cancel()
	}
	// The FunctionResponse name stays adk_request_input REGARDLESS of
	// the parked call's own name: workflowagent roots (planner, graph)
	// filter resume responses by that name before matching the ID —
	// any other name silently forks a fresh turn instead of resuming
	// (v0.2 pause/abort design, fact 4).
	msg := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromFunctionResponse("adk_request_input", map[string]any{
			"response": req.Response,
		})},
	}
	msg.Parts[0].FunctionResponse.ID = req.InterruptID
	obs.HITLResume(workloadName)
	return runTurnPre(ctx, r, logger, store, meters, wds, obs, tracker, turnLocks, workloadName, req.SessionID, msg, "resume:"+req.InterruptID, preTurn)
}

// consumeIfAnswered consumes a plane-A pause token iff the resume
// FunctionResponse durably landed — pinned by the design (gate finding
// M5): consumption keys on the append, not on turn completion. A
// resume turn that failed BEFORE the append leaves the interrupt
// pending and the token live (retry works); one that failed after has
// still legitimately ended the pause.
func consumeIfAnswered(ctx context.Context, store *transcript.Store, logger *slog.Logger, rec *transcript.PauseRecord, by string) {
	d, err := store.Get(ctx, "", rec.SessionID)
	if err != nil {
		return
	}
	for _, id := range d.PendingInterruptIDs {
		if id == rec.InterruptID {
			return // still pending: the resume never appended
		}
	}
	// ConsumeScheduled, not ConsumeToken: this runs AFTER the resume has
	// durably appended (M5 — consumption keys on the append, not on
	// gating). The pause has legitimately ended; the operator-facing
	// token TTL must not veto the bookkeeping consume and strand an
	// answered interrupt with a live-looking record.
	if _, err := store.ConsumeScheduled(ctx, rec.Token, by); err != nil &&
		!errors.Is(err, transcript.ErrAlreadyResumed) && !errors.Is(err, transcript.ErrTokenNotFound) {
		logger.Error("failed to consume resume token after answered interrupt",
			"session", rec.SessionID, "error", err.Error())
	}
}

func runTurn(ctx context.Context, r *runner.Runner, logger *slog.Logger, store *transcript.Store, meters *meterPool, wds *watchdogPool, obs *observability.Registry, tracker *turnTracker, turnLocks *sessionTurnLocks, workloadName, sessionID string, msg *genai.Content, label string) error {
	return runTurnPre(ctx, r, logger, store, meters, wds, obs, tracker, turnLocks, workloadName, sessionID, msg, label, nil)
}

// runTurnPre is runTurn with an optional hook that runs under the
// session's turn lock, before the turn starts. The resume path uses it
// to write the effects-ack watermark: under the lock, no in-flight
// turn can still be persisting mutating intents the watermark would
// silently cover; before the turn, the outbox's turn-start scan sees
// the watermark durably.
//
// It is also the v0.2 pause/abort CHOKEPOINT: every turn kind — inject,
// attach, resume, timer — passes here, so aborted sessions (terminal;
// ADK has no engine state to delegate to) and gate-paused sessions
// refuse here, under the turn lock. The cancel handle registers BEFORE
// the marker check (the register-before-check half of the abort/hard-
// pause handshake): a sweep after a marker write either finds this
// turn registered and cancels it, or this check sees the marker first.
func runTurnPre(ctx context.Context, r *runner.Runner, logger *slog.Logger, store *transcript.Store, meters *meterPool, wds *watchdogPool, obs *observability.Registry, tracker *turnTracker, turnLocks *sessionTurnLocks, workloadName, sessionID string, msg *genai.Content, label string, preTurn func(context.Context) error) error {
	// One turn per session (#62): ADK's stale-session check makes a
	// second concurrent runner turn on the same row fatal to one of
	// them, so same-session turns queue here. The wait genuinely
	// honors ctx (channel semaphore) — bounded by the wallclock
	// budget, the request lifetime, and drain-expiry cancellation.
	unlock, err := turnLocks.lock(ctx, sessionID)
	if err != nil {
		if tracker.isDraining() {
			// The wait was cut by drain-expiry cancellation: this is
			// the daemon refusing work, not a dispatch failure — 503,
			// same contract as the drain gate (#65).
			return fmt.Errorf("%w (queued turn cancelled: %v)", inject.ErrUnavailable, err)
		}
		return err
	}
	defer unlock()

	// The cancel handle doubles as the budget-trip cancel below and the
	// abort / hard-pause sweep target.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tracker.registerCancel(sessionID, cancel)
	defer tracker.unregisterCancel(sessionID)

	// Chokepoint check, after registration. A read failure skips the
	// check (fail-open): the refusals are availability guards, and an
	// unreadable ops overlay must not wedge every session — the
	// fail-closed safety guard is the effects outbox. ErrNotFound is
	// the normal fresh-session case (the runner auto-creates).
	if d, derr := store.Get(ctx, "", sessionID); derr == nil {
		if d.State == transcript.StateAborted {
			return fmt.Errorf("session %q is aborted (%s); session_aborted: %w", sessionID, d.AbortReason, inject.ErrConflict)
		}
		if d.GatePause.Active() {
			return fmt.Errorf("session %q is gate-paused (%s: %s); session_paused — resume with the pause token: %w",
				sessionID, d.PauseReason, d.PauseMessage, inject.ErrConflict)
		}
	}

	if preTurn != nil {
		if err := preTurn(ctx); err != nil {
			return err
		}
	}

	// Shutdown bookkeeping brackets the whole turn (see turnTracker).
	tracker.begin(sessionID)
	defer tracker.end(sessionID)

	// Budget enforcement point: the meter folds UsageMetadata from
	// each streamed event; crossing a ceiling cancels the run context,
	// aborting any in-flight model/tool work.
	meter := meters.meter(sessionID)

	// Export the turn's cost delta whichever way the turn ends. The
	// meter's session-cumulative cost is authoritative (pricing lives
	// in pkg/budget); the counter only ever sees per-turn deltas.
	_, costBefore, _ := meter.Snapshot()
	defer func() {
		_, costAfter, _ := meter.Snapshot()
		obs.AddCost(workloadName, costAfter-costBefore)
	}()

	// Watchdog tap (pkg/watchdog): per-session accumulation across
	// turns, per-turn dedup of aggregator re-emissions (core-agent
	// #363). Alerts are logged, not routed into model context — the
	// #159-style routing is bucket-3 work per docs/fork-design.md.
	onAlert := func(a watchdog.Alert) {
		logger.Warn("watchdog alert",
			"turn", label, "session", sessionID,
			"signal", a.Signal, "severity", string(a.Severity), "reason", a.Reason)
	}

	events := 0
	for event, err := range watchdog.Tap(r.Run(ctx, defaultUserID, sessionID, msg, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}), wds.watchdog(sessionID), onAlert) {
		if err != nil {
			logger.Error("runner emitted error", "turn", label, "session", sessionID, "error", err.Error(), "events_before_error", events)
			obs.TurnComplete(workloadName, observability.OutcomeError)
			return err
		}
		events++
		logEvent(logger, event, sessionID)
		obs.Observe(event, workloadName)
		if berr := meter.Observe(event); berr != nil {
			tokens, cost, calls := meter.Snapshot()
			logger.Error("BUDGET EXCEEDED — aborting session turn",
				"turn", label, "session", sessionID,
				"tokens", tokens, "cost_usd", fmt.Sprintf("%.4f", cost), "model_calls", calls,
				"error", berr.Error(),
			)
			cancel()
			obs.BudgetTrip(workloadName)
			obs.TurnComplete(workloadName, observability.OutcomeBudgetExceeded)
			return berr
		}
	}
	tokens, cost, calls := meter.Snapshot()
	logger.Info("turn complete", "turn", label, "session", sessionID, "events", events,
		"session_tokens", tokens, "session_cost_usd", fmt.Sprintf("%.4f", cost), "session_model_calls", calls)
	obs.TurnComplete(workloadName, observability.OutcomeOK)
	return nil
}

// sessionDialector maps --session-db-driver onto a GORM dialector for
// ADK's session/database service. SQLite and Postgres are the same
// one-call surface (docs/deployment-design.md, 2026-07-25 revision):
// database.NewSessionService takes either dialector identically. For
// sqlite the DSN is a file path; for postgres it is a DSN or
// postgres:// URL (Cloud Run's required shape — no persistent disk).
func sessionDialector(driver, dsn string) (gorm.Dialector, error) {
	switch driver {
	case "sqlite":
		if err := ensureSQLiteDir(dsn); err != nil {
			return nil, err
		}
		return sqlite.Open(dsn), nil
	case "postgres":
		return postgres.Open(dsn), nil
	default:
		return nil, fmt.Errorf("unknown --session-db-driver %q (want `sqlite` or `postgres`)", driver)
	}
}

// ensureSQLiteDir creates the parent directory for a SQLite file DSN.
// SQLite won't create intermediate directories and reports a missing
// parent as the cryptic "unable to open database file: out of memory
// (14)" (SQLITE_CANTOPEN) — hit on the first smoke run with
// --session-db=/tmp/mast/smoke.db before /tmp/mast existed. An
// unattended daemon's first boot must not fail on an empty state
// directory, so create it instead of demanding a clearer error from
// the operator's runbook. file: URIs are unwrapped (query params
// stripped); in-memory forms pass through untouched.
func ensureSQLiteDir(dsn string) error {
	path := dsn
	if rest, ok := strings.CutPrefix(path, "file:"); ok {
		path = strings.TrimPrefix(rest, "//")
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
	}
	if path == "" || path == ":memory:" || strings.HasPrefix(path, ":") {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "." || dir == "/" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create session-db directory %s: %w", dir, err)
	}
	return nil
}

func buildSessionService(ctx context.Context, driver, dsn string, logger *slog.Logger) (session.Service, error) {
	if dsn == "" {
		if driver != "sqlite" {
			return nil, fmt.Errorf("--session-db-driver=%s requires --session-db (a DSN); empty --session-db means in-memory sessions", driver)
		}
		logger.Warn("no --session-db; sessions are in-memory and will NOT survive restart")
		return session.InMemoryService(), nil
	}
	dial, err := sessionDialector(driver, dsn)
	if err != nil {
		return nil, err
	}
	// Same storage hardening as the attach path (write serialization
	// + busy_timeout + WAL for SQLite) minus the seq overlay — the
	// raw service lost markers and transcript events to SQLITE_BUSY
	// under concurrent sessions (#53).
	svc, err := eventlog.OpenSessionService(ctx, dial)
	if err != nil {
		return nil, fmt.Errorf("open session db (driver %s): %w", driver, err)
	}
	// Deliberately not logging the DSN: a Postgres DSN carries
	// credentials. The sqlite path is safe and useful for operators.
	if driver == "sqlite" {
		logger.Info("session db opened", "driver", driver, "path", dsn)
	} else {
		logger.Info("session db opened", "driver", driver)
	}
	return svc, nil
}

func logEvent(logger *slog.Logger, event *session.Event, sessionID string) {
	if event == nil {
		return
	}
	if event.RequestedInput != nil {
		logger.Info("HITL PAUSE",
			"session", sessionID,
			"interrupt_id", event.RequestedInput.InterruptID,
			"message", event.RequestedInput.Message,
		)
	} else if len(event.LongRunningToolIDs) > 0 {
		// Tool-level pause (e.g. the planner's request_operator_input):
		// the pending function-call ID is the interrupt ID an operator
		// passes to POST /resume.
		logger.Info("HITL PAUSE (long-running tool)",
			"session", sessionID,
			"interrupt_ids", event.LongRunningToolIDs,
		)
	}
	summary := "(no text)"
	if event.Content != nil {
		for _, part := range event.Content.Parts {
			if part == nil {
				continue
			}
			if part.Text != "" {
				summary = part.Text
				break
			}
			if part.FunctionCall != nil {
				summary = "function_call:" + part.FunctionCall.Name
				break
			}
			if part.FunctionResponse != nil {
				summary = "function_response:" + part.FunctionResponse.Name
				break
			}
		}
	}
	attrs := []any{
		"session", sessionID,
		"author", event.Author,
		"branch", event.Branch,
		"summary", summary,
	}
	if event.Output != nil {
		attrs = append(attrs, "output", fmt.Sprintf("%v", event.Output))
	}
	logger.Info("runner event", attrs...)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
