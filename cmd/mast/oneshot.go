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

// mast one-shot mode: `mast --task=<class> [--model=...] [--provider=...]
// [--session-db=...] "<prompt>"`. A positional prompt switches the
// binary from serving to running exactly one turn to completion and
// printing the result on stdout (logs stay on stderr). Serve remains
// the no-prompt default — scripts/demo-spike2.sh's flag-only
// invocations are untouched.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/internal/compose"
	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/eventlog"
	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/taskclass"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/watchdog"
)

// oneShotUserID owns one-shot sessions in the session store,
// distinguishing CLI turns from the inject daemon's defaultUserID.
const oneShotUserID = "mast-cli"

// oneShotSessionID derives the session a one-shot turn appends to.
// Deterministic per class so repeated invocations against the same
// --session-db continue one conversation per task class; an
// operator-chosen session ID is future surface (v0.2's `--session`),
// not silently invented here.
func oneShotSessionID(class string) string { return "task-" + class }

// oneShotOptions carries the resolved flag surface into runOneShot,
// separated from flag parsing so the e2e test can drive it in-process.
type oneShotOptions struct {
	Class      string // validated public task class
	Provider   string // --provider alias (backend hint for claude-*)
	Model      string // resolved model name (post --provider validation)
	SessionDB  string // empty = in-memory
	SessionDrv string // sqlite | postgres
	Prompt     string
	Timeout    time.Duration // whole-turn deadline; 0 = none
}

// runOneShot runs one turn of the class-shaped agent to completion and
// writes the final result to out. The session persists through the
// same session service the daemon uses: with --session-db set, the
// turn's events are durable and inspectable via `mast sessions`.
func runOneShot(ctx context.Context, logger *slog.Logger, opts oneShotOptions, out io.Writer) error {
	// Whole-turn deadline (--timeout, default 5m): a one-shot against
	// an unresponsive backend must fail loudly, not hang a script
	// forever — genai's silent retry-with-backoff on quota errors
	// looks exactly like a hang from the outside (observed live
	// 2026-07-29). Serve mode is not covered here; workload budgets
	// own its wallclock ceilings.
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	llm, err := buildModel(ctx, opts.Provider, opts.Model)
	if err != nil {
		return fmt.Errorf("construct model %q: %w", opts.Model, err)
	}
	sessionSvc, err := buildOneShotSessionService(opts.SessionDrv, opts.SessionDB, logger)
	if err != nil {
		return err
	}
	// Recorded-effect outbox on the one-shot path too — one-shot
	// sessions are durable and continuable when --session-db is set, so
	// a class session interrupted mid-mutation must get the same
	// fail-closed treatment on its next invocation (#53's every-path
	// lesson). No workload bundle here, so no per-tool overrides; the
	// ack surface for a wedged task session is offline —
	// `mast sessions ack-effects <id> --session-db=<this DB>` — since
	// no daemon serves task-class DBs.
	oneShotStore := transcript.NewStore(sessionSvc, appName)
	// pause_session (plane A) is offered only with a durable store: an
	// in-memory pause record would die with the process. Daemonless
	// semantics (v0.2 pause/abort design): the park and token are
	// durable; recovery is `mast sessions show/resume --session-db` —
	// no token index, no timers, no extend-token without a daemon.
	var pauseRec planner.PauseRecorder
	if opts.SessionDB != "" {
		pauseRec = oneShotStore
	}
	root, err := compose.BuildClassRoot(opts.Class, llm, pauseRec)
	if err != nil {
		return err
	}
	outboxPlugin, err := effects.New(effects.Config{
		Predicate:     effects.NewPredicate(nil),
		SubAgentNames: effects.SubAgentNames(root),
		AckedAt: func(ctx context.Context, sid string) (time.Time, bool) {
			return oneShotStore.EffectsAckedAt(ctx, "", sid)
		},
		Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("construct effects outbox: %w", err)
	}
	r, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    sessionSvc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{outboxPlugin}},
	})
	if err != nil {
		return fmt.Errorf("construct runner: %w", err)
	}

	sessionID := oneShotSessionID(opts.Class)
	// The CLI validates Class against the closed taskclass set, but
	// in-process callers (the e2e test, future library surface) can
	// pass anything — the reserved-suffix discipline applies to every
	// session-minting path (#61).
	if transcript.IsReservedSessionID(sessionID) {
		return fmt.Errorf("task class %q derives reserved session ID %q; rejected", opts.Class, sessionID)
	}
	logger.Info("one-shot turn starting",
		"task", opts.Class, "model", opts.Model, "session", sessionID)

	// One turn to completion: iterate the full event stream, keeping
	// the last structured output (Task-mode finish_task value) and the
	// last text part as the printable result.
	// Watchdog tap (pkg/watchdog): one turn, so a fresh watchdog per
	// invocation; alerts are logged on stderr like every other log line
	// (the model-context routing is bucket-3 per docs/fork-design.md).
	wd := watchdog.NewDefaultWatchdog()
	onAlert := func(a watchdog.Alert) {
		logger.Warn("watchdog alert", "task", opts.Class, "session", sessionID,
			"signal", a.Signal, "severity", string(a.Severity), "reason", a.Reason)
	}

	var lastOutput any
	var lastText string
	events := 0
	msg := genai.NewContentFromText(opts.Prompt, genai.RoleUser)
	for event, err := range watchdog.Tap(r.Run(ctx, oneShotUserID, sessionID, msg, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}), wd, onAlert) {
		if err != nil {
			if opts.Timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("turn exceeded --timeout %s after %d events (raise --timeout or pass --timeout=0 to disable): %w", opts.Timeout, events, err)
			}
			return fmt.Errorf("turn failed after %d events: %w", events, err)
		}
		events++
		logEvent(logger, event, sessionID)
		if event == nil {
			continue
		}
		if event.Output != nil {
			lastOutput = event.Output
		}
		if event.Content != nil {
			for _, part := range event.Content.Parts {
				if part != nil && part.Text != "" {
					lastText = part.Text
				}
			}
		}
	}
	logger.Info("one-shot turn complete", "task", opts.Class, "session", sessionID, "events", events)

	switch {
	case lastOutput != nil:
		fmt.Fprintln(out, formatOutput(lastOutput))
	case lastText != "":
		fmt.Fprintln(out, lastText)
	default:
		fmt.Fprintln(out, "(no output)")
	}
	return nil
}

// buildOneShotSessionService mirrors buildSessionService with GORM's
// trace logger silenced: ADK's service probes state rows that
// legitimately may not exist, and GORM's default logger prints those
// "record not found" traces to stdout — where the one-shot result
// goes. Same treatment as pkg/session.Open gives the sessions CLI.
func buildOneShotSessionService(driver, dsn string, logger *slog.Logger) (session.Service, error) {
	if dsn == "" {
		if driver != "sqlite" {
			return nil, fmt.Errorf("--session-db-driver=%s requires --session-db (a DSN); empty --session-db means in-memory sessions", driver)
		}
		logger.Warn("no --session-db; the one-shot session is in-memory and will NOT survive the process")
		return session.InMemoryService(), nil
	}
	dial, err := sessionDialector(driver, dsn)
	if err != nil {
		return nil, err
	}
	// Shared hardening (#64): a one-shot pointed at a daemon's live DB
	// is a concurrent cross-process writer — without busy_timeout it
	// hits immediate SQLITE_BUSY. OpenSessionService also keeps GORM's
	// trace logger silent, which this path needs for clean stdout.
	svc, err := eventlog.OpenSessionService(context.Background(), dial)
	if err != nil {
		return nil, fmt.Errorf("open session db (driver %s): %w", driver, err)
	}
	return svc, nil
}

// formatOutput renders a turn's structured output for stdout. Task-
// mode agents finish via finish_task, whose value arrives as
// {"result": <text>} — unwrap that common shape; render anything else
// structured as JSON rather than Go's map syntax.
func formatOutput(v any) string {
	if m, ok := v.(map[string]any); ok && len(m) == 1 {
		if r, ok := m["result"]; ok {
			return formatOutput(r)
		}
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// resolveModelSelection applies the --provider alias against --model
// (exit criterion 2's literal flag): --provider validates an explicit
// --model, and supplies a default when --model was left unset — the
// mock's own name for echo/scripted, and the --task profile's tier
// default via taskclass.ModelForTier for gemini/anthropic (mid tier
// when no class is declared). modelSet reports whether the operator
// set --model explicitly (flag.Visit), since the flag's default is
// "echo". anthropic and anthropic-vertex resolve the same model ids —
// the alias's backend choice is consumed later by compose.BuildModel.
func resolveModelSelection(provider, model string, modelSet bool, class string) (string, error) {
	tierDefault := func(providerKey string) string {
		tier := taskclass.TierMid
		if p, ok := taskclass.Resolve(class); ok && p.Tier != "" {
			tier = p.Tier
		}
		return taskclass.ModelForTier(providerKey, tier)
	}
	switch provider {
	case "":
		return model, nil
	case "echo", "scripted":
		if modelSet && model != provider {
			return "", fmt.Errorf("--provider=%s conflicts with --model=%s", provider, model)
		}
		return provider, nil
	case "gemini":
		if modelSet {
			if !strings.HasPrefix(model, "gemini-") {
				return "", fmt.Errorf("--provider=gemini conflicts with --model=%s (want a gemini-* model id)", model)
			}
			return model, nil
		}
		return tierDefault("gemini"), nil
	case "anthropic", "anthropic-vertex":
		if modelSet {
			if !strings.HasPrefix(model, "claude-") {
				return "", fmt.Errorf("--provider=%s conflicts with --model=%s (want a claude-* model id)", provider, model)
			}
			return model, nil
		}
		return tierDefault("anthropic"), nil
	default:
		return "", fmt.Errorf("unknown --provider %q (want `gemini`, `anthropic`, `anthropic-vertex`, `echo`, or `scripted`)", provider)
	}
}
