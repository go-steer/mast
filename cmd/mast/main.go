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
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/mast/internal/compose"
	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/config"
	"github.com/go-steer/mast/pkg/envelope"
	"github.com/go-steer/mast/pkg/inject"
	mastmcp "github.com/go-steer/mast/pkg/mcp"
	"github.com/go-steer/mast/pkg/observability"
	mastsession "github.com/go-steer/mast/pkg/session"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/taskclass"
	"github.com/go-steer/mast/pkg/watchdog"
	"github.com/go-steer/mast/pkg/workload"
)

// Release identity, stamped by GoReleaser via -ldflags (see
// .goreleaser.yaml). "dev" for local builds.
var (
	version = "dev"
	commit  = ""
	date    = ""
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
	run()
}

// run parses the flag surface shared by serve and one-shot modes and
// dispatches: a positional prompt runs one turn to completion and
// prints the result (oneshot.go); no prompt serves the daemon —
// exactly the pre-one-shot behavior, so scripts/demo-spike2.sh's
// flag-only invocations pass unchanged.
func run() {
	var (
		workloadFlag = flag.String("workload", "", "workload to run: a name resolved via .agents/ discovery (see pkg/config), or a path to a workload directory (containing workload.yaml + specialists/)")
		dispatchMode = flag.String("dispatch", "coordinator", "dispatch shape: `coordinator` (spike-1 SubAgents pattern) or `graph` (workflow-graph LLM-as-router)")
		modelName    = flag.String("model", "echo", "model to use: `echo` (fake, for smoke), `scripted` (JSONL replay; path via MAST_SCRIPT), a Gemini model id like `gemini-2.5-flash`, or a Claude model id like `claude-sonnet-4-6`")
		providerFlag = flag.String("provider", "", "model provider alias: `echo`, `scripted`, `gemini`, `anthropic`, or `anthropic-vertex`. Validates against --model when both are set; picks the provider's default model (the --task profile's tier via pkg/taskclass) when --model is unset. For claude-* models the alias also picks the backend (first-party vs Vertex)")
		taskFlag     = flag.String("task", "", "one-shot task class: `chat`, `debug`, `implement`, `research`, `review`, or `orchestrate` (requires a positional prompt; defaults to chat when a prompt is given without --task)")
		listen       = flag.String("listen", ":7777", "HTTP inject endpoint bind address")
		sessionDB    = flag.String("session-db", "", "session store location: a SQLite file path (default driver) or a Postgres DSN/URL with --session-db-driver=postgres; empty = in-memory sessions (no durability)")
		sessionDrv   = flag.String("session-db-driver", "sqlite", "session DB driver: `sqlite` (--session-db is a file path) or `postgres` (--session-db is a DSN or postgres:// URL)")
		logLevel     = flag.String("log-level", "info", "log level: debug|info|warn|error")
		showVersion  = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("mast %s", version)
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
		if explicit["dispatch"] {
			logger.Warn("--dispatch is a serve-mode flag; ignored in one-shot mode")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		opts := oneShotOptions{
			Class:      class,
			Provider:   *providerFlag,
			Model:      *modelName,
			SessionDB:  *sessionDB,
			SessionDrv: *sessionDrv,
			Prompt:     strings.Join(flag.Args(), " "),
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

	if err := serve(logger, *workloadFlag, *dispatchMode, *providerFlag, *modelName, *listen, *sessionDB, *sessionDrv); err != nil {
		// serve already logged the failure with context; the error
		// return only carries the exit status (and lets serve's defers
		// — signal stop, OTel flush — run before the process dies).
		os.Exit(1)
	}
}

// serve runs the daemon: inject endpoint + runner + session store.
// Fatal startup errors are logged in place and returned (not
// os.Exit'd) so the deferred cleanups run.
func serve(logger *slog.Logger, workloadArg, dispatchMode, providerName, modelName, listen, sessionDB, sessionDrv string) error {

	bearer := os.Getenv("MAST_INJECT_TOKEN")
	if bearer == "" {
		logger.Warn("MAST_INJECT_TOKEN not set; inject endpoint is unauthenticated (dev only)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Env-gated OTel trace export: a no-op unless OTEL_EXPORTER_OTLP_*
	// endpoints are set. mast opens no spans of its own in v0.1 — ADK
	// v2's runner emits the span tree; this only exports it.
	otelShutdown, otelEnabled, err := observability.SetupOTel(ctx)
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

	llm, err := buildModel(ctx, providerName, modelName)
	if err != nil {
		logger.Error("failed to construct model", "model", modelName, "error", err.Error())
		return err
	}
	logger.Info("model constructed", "name", llm.Name())

	root, bundle, err := buildRoot(ctx, logger, llm, modelName, workloadArg, dispatchMode)
	if err != nil {
		logger.Error("failed to construct root agent", "error", err.Error())
		return err
	}
	logger.Info("root agent constructed",
		"name", root.Name(),
		"sub_agents", len(root.SubAgents()),
	)

	sessionSvc, err := buildSessionService(sessionDrv, sessionDB, logger)
	if err != nil {
		logger.Error("failed to construct session service", "error", err.Error())
		return err
	}
	r, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    sessionSvc,
		AutoCreateSession: true,
	})
	if err != nil {
		logger.Error("failed to construct runner", "error", err.Error())
		return err
	}

	meters := newMeterPool(bundle, modelName)
	wds := newWatchdogPool()

	// Operator surface over the same session service the runner writes
	// through (docs/durable-execution-design.md, "Operator-facing
	// surface"): /abort appends the durable abort marker, and /resume
	// refuses sessions that carry one.
	store := mastsession.NewStore(sessionSvc, appName)

	// Fixed metric registry (pkg/observability owns every family name;
	// nothing here can mint new ones). Single-workload process in v0.1,
	// so the workload label is resolved once.
	obs := observability.New()
	workloadName := "(none)"
	if bundle != nil {
		workloadName = bundle.Name
	}
	obs.Prime(workloadName)

	handler := func(reqCtx context.Context, p envelope.InjectPayload) error {
		return dispatch(reqCtx, r, logger, meters, wds, obs, workloadName, bundle, p)
	}
	resumeHandler := func(reqCtx context.Context, req inject.ResumeRequest) error {
		if d, err := store.Get(reqCtx, "", req.SessionID); err == nil && d.State == mastsession.StateAborted {
			return fmt.Errorf("session %q is aborted (%s); refusing resume", req.SessionID, d.AbortReason)
		}
		return resume(reqCtx, r, logger, meters, wds, obs, workloadName, req)
	}
	abortHandler := func(reqCtx context.Context, req inject.AbortRequest) error {
		return store.Abort(reqCtx, "", req.SessionID, req.Reason)
	}

	srv, err := inject.New(inject.Config{
		Listen:        listen,
		BearerToken:   bearer,
		Handler:       handler,
		ResumeHandler: resumeHandler,
		AbortHandler:  abortHandler,
		Logger:        logger,
		Metrics:       obs.Handler(),
	})
	if err != nil {
		logger.Error("failed to construct inject server", "error", err.Error())
		return err
	}

	go func() {
		<-ctx.Done()
		logger.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("inject server terminated", "error", err.Error())
		return err
	}
	return nil
}

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
func buildRoot(ctx context.Context, logger *slog.Logger, llm model.LLM, modelName, workloadArg, dispatch string) (adkagent.Agent, *workload.Bundle, error) {
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
		Bundle:   bundle,
		Specs:    loaded,
		Model:    llm,
		Toolsets: toolsets,
		Dispatch: compose.Dispatch(dispatch),
		Logger:   logger,
	})
	if err != nil {
		return nil, nil, err
	}
	return a, &bundle, nil
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

func dispatch(ctx context.Context, r *runner.Runner, logger *slog.Logger, meters *meterPool, wds *watchdogPool, obs *observability.Registry, workloadName string, bundle *workload.Bundle, p envelope.InjectPayload) error {
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
	return runTurn(ctx, r, logger, meters, wds, obs, workloadName, sessionIDFor(p), msg, "inject:"+p.Reason)
}

// resume feeds an operator's approval verdict back into a paused
// session. The runner treats a user turn carrying a FunctionResponse
// whose ID matches a pending InterruptID as a resume (see adk/v2
// runner buildResumeResponses).
func resume(ctx context.Context, r *runner.Runner, logger *slog.Logger, meters *meterPool, wds *watchdogPool, obs *observability.Registry, workloadName string, req inject.ResumeRequest) error {
	msg := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromFunctionResponse("adk_request_input", map[string]any{
			"response": req.Response,
		})},
	}
	msg.Parts[0].FunctionResponse.ID = req.InterruptID
	obs.HITLResume(workloadName)
	return runTurn(ctx, r, logger, meters, wds, obs, workloadName, req.SessionID, msg, "resume:"+req.InterruptID)
}

func runTurn(ctx context.Context, r *runner.Runner, logger *slog.Logger, meters *meterPool, wds *watchdogPool, obs *observability.Registry, workloadName, sessionID string, msg *genai.Content, label string) error {
	// Budget enforcement point: the meter folds UsageMetadata from
	// each streamed event; crossing a ceiling cancels the run context,
	// aborting any in-flight model/tool work.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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

func buildSessionService(driver, dsn string, logger *slog.Logger) (session.Service, error) {
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
	svc, err := database.NewSessionService(dial)
	if err != nil {
		return nil, fmt.Errorf("open session db (driver %s): %w", driver, err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		return nil, fmt.Errorf("migrate session db (driver %s): %w", driver, err)
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
