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

// Package cli is the mast binary: serve mode, one-shot mode, and the
// `mast sessions` / `mast stop` subcommands. cmd/mast is a one-line
// main over Main, so a custom main.go built on this package is the same
// binary by construction (#301).
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/go-steer/mast/internal/taskclass"
	buildversion "github.com/go-steer/mast/internal/version"
	"github.com/go-steer/mast/pkg/watchdog"
)

const (
	appName          = "mast"
	defaultUserID    = "mast-inject"
	defaultSessionID = "gke-triage-default"
)

// Exit codes. These are a frozen contract (DESIGN.md, "The v1.0
// stability promise"): callers branch on them, and a caller cannot see
// this package's symbols. exitDrainExpired in particular is issue #42's
// contract and is asserted by scripts/uat-v0.2.sh's S4-exit3 leg, which
// is the reason it is worth distinguishing at all — an exit code
// nothing branches on is a comment (#297).
const (
	exitOK = 0
	// exitFailure: the work was attempted and failed.
	exitFailure = 1
	// exitUsage: the invocation was rejected before any work started.
	exitUsage = 2
	// exitDrainExpired: serve mode's shutdown drain expired with
	// sessions still interrupted; their work is durable but unfinished.
	exitDrainExpired = 3
)

// Main runs the mast binary over args — the command line without the
// program name, os.Args[1:] — and returns the process exit status. It
// does not call os.Exit; the caller does, with what Main returns. The
// one exception is the teardown watchdog, which kills a process whose
// shutdown has hung past every deadline, because by then nothing is
// left to return to.
//
// Cancelling ctx shuts serve mode down the way SIGINT or SIGTERM does:
// the same drain, the same exit status.
func Main(ctx context.Context, args []string) int {
	return MainWith(ctx, args, Options{})
}

// MainWith is Main with what a custom main.go adds (see Options). The
// public cli package calls it; cmd/mast calls Main, which is MainWith
// with nothing added.
func MainWith(ctx context.Context, args []string, ext Options) int {
	// Subcommand dispatch happens before flag parsing so the flag-only
	// serve invocation (`mast --workload=... --listen=...`) keeps
	// working exactly as before — scripts/demo-spike2.sh depends on it.
	if len(args) > 0 && args[0] == "sessions" {
		return runSessions(args[1:])
	}
	if len(args) > 0 && args[0] == "stop" {
		return runStop(args[1:])
	}
	return run(ctx, args, ext)
}

// programName is what usage and parse errors call the binary, which
// flag.CommandLine took from os.Args[0]. Kept, so `mast -h` reads the
// same as it did before the binary was a package, and a custom build
// names itself.
func programName() string {
	if len(os.Args) > 0 && os.Args[0] != "" {
		return os.Args[0]
	}
	return appName
}

func run(ctx context.Context, args []string, ext Options) int {
	// A FlagSet of its own rather than flag.CommandLine, so Main reads
	// the args it was handed and a second call in one process starts
	// clean. ExitOnError semantics are kept by hand below: -h is 0, a
	// bad flag is 2, and the flag package has printed the message and
	// usage either way.
	fs := flag.NewFlagSet(programName(), flag.ContinueOnError)
	f := registerRunFlags(fs)
	var (
		workloadFlag     = f.workload
		dispatchMode     = f.dispatch
		modelName        = f.model
		providerFlag     = f.provider
		taskFlag         = f.task
		listen           = f.listen
		attachListen     = f.attachListen
		a2aListen        = f.a2aListen
		aguiListen       = f.aguiListen
		notifyURL        = f.notifyURL
		parkNotify       = f.parkNotify
		sessionDB        = f.sessionDB
		sessionDrv       = f.sessionDrv
		timeoutFlag      = f.timeout
		logLevel         = f.logLevel
		autoResume       = f.autoResume
		autoResumeWindow = f.autoResumeWindow
		watchdogFlag     = f.watchdog
		mcpDigest        = f.mcpDigest
		showVersion      = f.version
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}

	if *showVersion {
		fmt.Printf("mast %s", buildversion.Version)
		if buildversion.Commit != "" {
			fmt.Printf(" (%s %s)", buildversion.Commit, buildversion.Date)
		}
		fmt.Println()
		return exitOK
	}

	logger := newLogger(*logLevel)
	slog.SetDefault(logger)

	// --provider is an alias over --model; "explicitly set" matters
	// because --model's default is "echo", not empty.
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	resolvedModel, err := resolveModelSelection(*providerFlag, *modelName, explicit["model"], *taskFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mast:", err)
		return exitUsage
	}
	*modelName = resolvedModel

	// The flag is validated before the mode split — a typo'd posture
	// should exit 2 as a config error, not surface later as a run
	// failure — but it is not *resolved* here. Resolution needs the
	// workload's safety.watchdog, and the bundle is not loaded until
	// serve; see resolveWatchdog.
	if *watchdogFlag != "" {
		if _, err := watchdog.ParseMode(*watchdogFlag); err != nil {
			fmt.Fprintln(os.Stderr, "mast: --watchdog:", err)
			return exitUsage
		}
	}

	// One-shot mode: a positional prompt runs a single turn instead of
	// serving. --task shapes the agent (default chat); --workload is a
	// serve-mode flag and combining the two would silently pick one
	// semantics, so it errors instead.
	if fs.NArg() > 0 {
		// Go's flag package stops parsing at the first positional
		// argument, so a flag placed AFTER the prompt silently
		// becomes prompt text — `mast --task=x "prompt" --session-db=y`
		// ran with in-memory sessions and sent the flag to the model
		// (observed live 2026-07-29, twice). Refuse the ambiguity when
		// the trailing token names a flag we actually define; prompts
		// that legitimately mention flag-like words survive by quoting
		// (one argument, not starting with `-`).
		if misplaced := misplacedFlag(fs.Args(), func(name string) bool {
			return fs.Lookup(name) != nil
		}); misplaced != "" {
			fmt.Fprintf(os.Stderr, "mast: %q looks like a flag but appears after the positional prompt — Go flag parsing stops at the first positional argument, so it would be sent to the model as prompt text. Put flags before the prompt.\n", misplaced)
			return exitUsage
		}
		class := *taskFlag
		if class == "" {
			class = taskclass.Chat
		}
		if _, ok := taskclass.Resolve(class); !ok {
			fmt.Fprintf(os.Stderr, "mast: unknown --task %q (want one of: %s)\n",
				class, strings.Join(taskclass.Classes(), ", "))
			return exitUsage
		}
		if *workloadFlag != "" {
			fmt.Fprintln(os.Stderr, "mast: --workload is a serve-mode flag; one-shot mode runs a single --task-class agent")
			return exitUsage
		}
		if *attachListen != "" {
			fmt.Fprintln(os.Stderr, "mast: --attach-listen is a serve-mode flag; one-shot mode has no operator surface to attach to")
			return exitUsage
		}
		if *a2aListen != "" {
			fmt.Fprintln(os.Stderr, "mast: --a2a-listen is a serve-mode flag; one-shot mode exposes no A2A surface")
			return exitUsage
		}
		if *aguiListen != "" {
			fmt.Fprintln(os.Stderr, "mast: --agui-listen is a serve-mode flag; one-shot mode exposes no AG-UI surface")
			return exitUsage
		}
		if *notifyURL != "" {
			fmt.Fprintln(os.Stderr, "mast: --notify-url is a serve-mode flag; one-shot mode runs no monitoring cycle")
			return exitUsage
		}
		if *parkNotify != "" {
			fmt.Fprintln(os.Stderr, "mast: --park-notify is a serve-mode flag; one-shot mode builds no write gate, so it raises no parks")
			return exitUsage
		}
		if explicit["dispatch"] {
			logger.Warn("--dispatch is a serve-mode flag; ignored in one-shot mode")
		}
		if explicit["auto-resume"] || explicit["auto-resume-window"] {
			logger.Warn("--auto-resume / --auto-resume-window are serve-mode flags; ignored in one-shot mode")
		}
		// No bundle in one-shot mode, so the chain is flag-or-default.
		// The resolution still runs rather than short-circuiting to the
		// flag: the default is a real decision and the log line has to
		// name it.
		wdRes, err := resolveWatchdog(watchdogInputs{Flag: *watchdogFlag})
		if err != nil {
			fmt.Fprintln(os.Stderr, "mast:", err)
			return exitUsage
		}
		ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
		opts := oneShotOptions{
			Ext:        ext,
			Class:      class,
			Provider:   *providerFlag,
			Model:      *modelName,
			SessionDB:  *sessionDB,
			SessionDrv: *sessionDrv,
			Prompt:     strings.Join(fs.Args(), " "),
			Timeout:    *timeoutFlag,
			Watchdog:   wdRes,
		}
		err = runOneShot(ctx, logger, opts, os.Stdout)
		stop()
		if err != nil {
			logger.Error("one-shot turn failed", "task", class, "error", err.Error())
			return exitFailure
		}
		return exitOK
	}
	if *taskFlag != "" {
		fmt.Fprintln(os.Stderr, "mast: --task requires a positional prompt (one-shot mode); serve mode takes --workload")
		return exitUsage
	}
	if explicit["timeout"] {
		logger.Warn("--timeout is a one-shot flag; ignored in serve mode (workload budgets own serve-mode ceilings)")
	}

	// Attach mode implies a durable store (#329). Resolved here rather
	// than inside serve because "was the flag given?" is a property of
	// the command line, not of the value — mast's --session-db is a
	// string, so an explicit empty one is indistinguishable downstream
	// from an absent one, and that is the case the refusal is for.
	sessions, err := resolveSessionDB(
		sessionOpts{db: *sessionDB, driver: *sessionDrv},
		explicit["session-db"], *attachListen != "", defaultSessionDBPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mast:", err)
		return exitUsage
	}

	d := &daemon{
		parent: ctx,
		logger: logger,
		wl:     workloadOpts{arg: *workloadFlag, dispatch: *dispatchMode},
		mdl:    modelOpts{provider: *providerFlag, name: *modelName},
		listeners: listenOpts{
			inject:     *listen,
			attach:     *attachListen,
			a2a:        *a2aListen,
			agui:       *aguiListen,
			notify:     *notifyURL,
			parkNotify: *parkNotify,
		},
		sessions:     sessions,
		resumes:      resumeOpts{auto: *autoResume, window: *autoResumeWindow},
		watchdogFlag: *watchdogFlag,
		mcpDigest:    *mcpDigest,
		ext:          ext,
	}
	if err := d.serve(); err != nil {
		// serve already logged the failure with context; the error
		// return only carries the exit status (and lets serve's defers
		// — signal stop, OTel flush — run before the process dies).
		// Exit 3 = drain expired with interrupted survivors (issue
		// #42's contract); everything else is exit 1.
		if errors.Is(err, errDrainExpired) {
			return exitDrainExpired
		}
		return exitFailure
	}
	return exitOK
}
