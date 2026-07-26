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
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/genai"

	"github.com/glebarez/sqlite"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/adk/v2/tool"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/envelope"
	"github.com/go-steer/mast/pkg/graph"
	"github.com/go-steer/mast/pkg/inject"
	mastmcp "github.com/go-steer/mast/pkg/mcp"
	"github.com/go-steer/mast/pkg/router"
	mastsession "github.com/go-steer/mast/pkg/session"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
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
	serve()
}

// serve runs the daemon: inject endpoint + runner + session store.
func serve() {
	var (
		workloadDir  = flag.String("workload", "", "path to workload directory (containing workload.yaml + specialists/)")
		dispatchMode = flag.String("dispatch", "coordinator", "dispatch shape: `coordinator` (spike-1 SubAgents pattern) or `graph` (workflow-graph LLM-as-router)")
		modelName    = flag.String("model", "echo", "model to use: `echo` (fake, for smoke) or a Gemini model id like `gemini-2.5-flash`")
		listen       = flag.String("listen", ":7777", "HTTP inject endpoint bind address")
		sessionDB    = flag.String("session-db", "", "path to a SQLite session DB; empty = in-memory sessions (no durability)")
		logLevel     = flag.String("log-level", "info", "log level: debug|info|warn|error")
	)
	flag.Parse()

	logger := newLogger(*logLevel)
	slog.SetDefault(logger)

	bearer := os.Getenv("MAST_INJECT_TOKEN")
	if bearer == "" {
		logger.Warn("MAST_INJECT_TOKEN not set; inject endpoint is unauthenticated (dev only)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	llm, err := buildModel(ctx, *modelName)
	if err != nil {
		logger.Error("failed to construct model", "model", *modelName, "error", err.Error())
		os.Exit(1)
	}
	logger.Info("model constructed", "name", llm.Name())

	root, bundle, err := buildRoot(ctx, logger, llm, *modelName, *workloadDir, *dispatchMode)
	if err != nil {
		logger.Error("failed to construct root agent", "error", err.Error())
		os.Exit(1)
	}
	logger.Info("root agent constructed",
		"name", root.Name(),
		"sub_agents", len(root.SubAgents()),
	)

	sessionSvc, err := buildSessionService(*sessionDB, logger)
	if err != nil {
		logger.Error("failed to construct session service", "error", err.Error())
		os.Exit(1)
	}
	r, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    sessionSvc,
		AutoCreateSession: true,
	})
	if err != nil {
		logger.Error("failed to construct runner", "error", err.Error())
		os.Exit(1)
	}

	meters := newMeterPool(bundle, *modelName)

	// Operator surface over the same session service the runner writes
	// through (docs/durable-execution-design.md, "Operator-facing
	// surface"): /abort appends the durable abort marker, and /resume
	// refuses sessions that carry one.
	store := mastsession.NewStore(sessionSvc, appName)

	handler := func(reqCtx context.Context, p envelope.InjectPayload) error {
		return dispatch(reqCtx, r, logger, meters, bundle, p)
	}
	resumeHandler := func(reqCtx context.Context, req inject.ResumeRequest) error {
		if d, err := store.Get(reqCtx, "", req.SessionID); err == nil && d.State == mastsession.StateAborted {
			return fmt.Errorf("session %q is aborted (%s); refusing resume", req.SessionID, d.AbortReason)
		}
		return resume(reqCtx, r, logger, meters, req)
	}
	abortHandler := func(reqCtx context.Context, req inject.AbortRequest) error {
		return store.Abort(reqCtx, "", req.SessionID, req.Reason)
	}

	srv, err := inject.New(inject.Config{
		Listen:        *listen,
		BearerToken:   bearer,
		Handler:       handler,
		ResumeHandler: resumeHandler,
		AbortHandler:  abortHandler,
		Logger:        logger,
	})
	if err != nil {
		logger.Error("failed to construct inject server", "error", err.Error())
		os.Exit(1)
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
		os.Exit(1)
	}
}

// buildModel constructs the model.LLM for the given name. "echo" builds
// a fake in-process echo model (no credentials required); anything
// starting with "gemini-" builds a Vertex/Gemini model via ADK.
func buildModel(ctx context.Context, name string) (model.LLM, error) {
	switch {
	case name == "echo":
		return mastagent.NewEchoModel("mast-echo"), nil
	case strings.HasPrefix(name, "gemini-"):
		return gemini.NewModel(ctx, name, &genai.ClientConfig{})
	default:
		return nil, fmt.Errorf("unknown model %q (want `echo` or a `gemini-*` model id)", name)
	}
}

// buildRoot wires the top-level agent. With --workload set it loads
// the workload bundle + specialists + tool catalog and constructs the
// dispatch shape selected by --dispatch: the spike-1 SubAgents
// coordinator (pkg/router) or the spike-2 workflow graph (pkg/graph).
// Without --workload it constructs a trivial single-agent coordinator
// (useful for pure inject-endpoint smoke).
func buildRoot(ctx context.Context, logger *slog.Logger, llm model.LLM, modelName, workloadDir, dispatch string) (adkagent.Agent, *workload.Bundle, error) {
	if dispatch != "coordinator" && dispatch != "graph" {
		return nil, nil, fmt.Errorf("unknown --dispatch %q (want `coordinator` or `graph`)", dispatch)
	}
	if workloadDir == "" {
		logger.Warn("no --workload supplied; running trivial single-agent coordinator")
		a, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
			Name:        "trivial_coordinator",
			Description: "Trivial coordinator (no workload loaded).",
			Instruction: "Acknowledge the incident briefly.",
			Model:       llm,
		})
		return a, nil, err
	}

	bundlePath := filepath.Join(workloadDir, "workload.yaml")
	bundle, err := workload.Load(bundlePath)
	if err != nil {
		return nil, nil, fmt.Errorf("load workload: %w", err)
	}
	logger.Info("workload loaded",
		"name", bundle.Name,
		"specialists", len(bundle.Specialists),
		"mcp_servers", len(bundle.ToolCatalog.MCP),
	)

	specsDir := filepath.Join(workloadDir, "specialists")
	loaded, err := specialists.LoadDir(specsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("load specialists: %w", err)
	}
	logger.Info("specialists loaded", "count", len(loaded))

	// Attach MCP toolsets to specialists only when a real model is in
	// use. The echo model doesn't call tools; wiring MCP under echo
	// would only add startup latency (and mask ADC-availability issues
	// as workload-load failures rather than "no real LLM").
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

	byName := make(map[string]adkagent.Agent, len(loaded))
	taskOnly := make(map[string]adkagent.Agent, len(loaded))
	var classifier adkagent.Agent
	for _, spec := range loaded {
		opts := specialists.BuildOptions{Model: llm}
		// Task-mode specialists get the MCP toolsets; SingleTurn
		// classifiers don't (they run in one shot with no tool loop).
		if spec.Mode == specialists.ModeTask {
			opts.Toolsets = toolsets
		}
		a, err := specialists.Build(spec, opts)
		if err != nil {
			return nil, nil, fmt.Errorf("build specialist %q: %w", spec.Name, err)
		}
		byName[spec.Name] = a
		if spec.Mode == specialists.ModeTask {
			taskOnly[spec.Name] = a
		} else if classifier == nil {
			classifier = a
		}
	}

	if dispatch == "graph" {
		if classifier == nil {
			return nil, nil, fmt.Errorf("--dispatch=graph requires a SingleTurn classifier specialist in the roster")
		}
		a, err := graph.Build(graph.Config{
			Bundle:      bundle,
			Classifier:  classifier,
			Specialists: taskOnly,
		})
		return a, &bundle, err
	}

	a, err := router.Build(router.Config{
		Bundle:      bundle,
		Specialists: byName,
		Model:       llm,
	})
	return a, &bundle, err
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

// ratePer1K is the spike's flat pricing table: enough structure to
// prove cost derivation from UsageMetadata, not a real price list.
func ratePer1K(modelName string) float64 {
	switch {
	case modelName == "echo":
		return 0.05 // inflated so offline smoke tests can trip small caps
	case strings.HasPrefix(modelName, "gemini-"):
		return 0.0006
	default:
		return 0.001
	}
}

func newMeterPool(bundle *workload.Bundle, modelName string) *meterPool {
	limits := budget.Limits{RatePer1K: ratePer1K(modelName)}
	if bundle != nil {
		limits.MaxCostUSD = bundle.Budget.MaxCostUSD
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

func dispatch(ctx context.Context, r *runner.Runner, logger *slog.Logger, meters *meterPool, bundle *workload.Bundle, p envelope.InjectPayload) error {
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
	return runTurn(ctx, r, logger, meters, sessionIDFor(p), msg, "inject:"+p.Reason)
}

// resume feeds an operator's approval verdict back into a paused
// session. The runner treats a user turn carrying a FunctionResponse
// whose ID matches a pending InterruptID as a resume (see adk/v2
// runner buildResumeResponses).
func resume(ctx context.Context, r *runner.Runner, logger *slog.Logger, meters *meterPool, req inject.ResumeRequest) error {
	msg := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromFunctionResponse("adk_request_input", map[string]any{
			"response": req.Response,
		})},
	}
	msg.Parts[0].FunctionResponse.ID = req.InterruptID
	return runTurn(ctx, r, logger, meters, req.SessionID, msg, "resume:"+req.InterruptID)
}

func runTurn(ctx context.Context, r *runner.Runner, logger *slog.Logger, meters *meterPool, sessionID string, msg *genai.Content, label string) error {
	// Budget enforcement point: the meter folds UsageMetadata from
	// each streamed event; crossing a ceiling cancels the run context,
	// aborting any in-flight model/tool work.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	meter := meters.meter(sessionID)

	events := 0
	for event, err := range r.Run(ctx, defaultUserID, sessionID, msg, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}) {
		if err != nil {
			logger.Error("runner emitted error", "turn", label, "session", sessionID, "error", err.Error(), "events_before_error", events)
			return err
		}
		events++
		logEvent(logger, event, sessionID)
		if berr := meter.Observe(event); berr != nil {
			tokens, cost, calls := meter.Snapshot()
			logger.Error("BUDGET EXCEEDED — aborting session turn",
				"turn", label, "session", sessionID,
				"tokens", tokens, "cost_usd", fmt.Sprintf("%.4f", cost), "model_calls", calls,
				"error", berr.Error(),
			)
			cancel()
			return berr
		}
	}
	tokens, cost, calls := meter.Snapshot()
	logger.Info("turn complete", "turn", label, "session", sessionID, "events", events,
		"session_tokens", tokens, "session_cost_usd", fmt.Sprintf("%.4f", cost), "session_model_calls", calls)
	return nil
}

func buildSessionService(path string, logger *slog.Logger) (session.Service, error) {
	if path == "" {
		logger.Warn("no --session-db; sessions are in-memory and will NOT survive restart")
		return session.InMemoryService(), nil
	}
	svc, err := database.NewSessionService(sqlite.Open(path))
	if err != nil {
		return nil, fmt.Errorf("open session db %q: %w", path, err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		return nil, fmt.Errorf("migrate session db %q: %w", path, err)
	}
	logger.Info("session db opened", "path", path)
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
