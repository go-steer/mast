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

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/genai"

	"gorm.io/gorm"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/internal/a2a"
	"github.com/go-steer/mast/internal/agui"
	"github.com/go-steer/mast/internal/auth"
	"github.com/go-steer/mast/internal/compose"
	"github.com/go-steer/mast/internal/effects"
	"github.com/go-steer/mast/internal/envelope"
	"github.com/go-steer/mast/internal/eventlog"
	"github.com/go-steer/mast/internal/inject"
	"github.com/go-steer/mast/internal/observability"
	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// daemon is serve()'s state: one field per value that crosses from one
// startup phase to the next. serve() was a single function of about 1,250 lines
// (#293); each phase is now a method, run in the order the function ran
// its sections, reading what earlier phases built and setting what later
// ones need.
//
// Closures handed to servers capture d rather than locals, so a field
// assigned after its closure is built (parkNotices, resumeForPerms) is
// read at call time exactly as the captured variable used to be.
type daemon struct {
	// Main's context; the signal context's parent.
	parent       context.Context
	logger       *slog.Logger
	wl           workloadOpts
	mdl          modelOpts
	listeners    listenOpts
	sessions     sessionOpts
	resumes      resumeOpts
	watchdogFlag string
	mcpDigest    bool

	// Set by checkIngress.
	bearer       string
	injectAuthn  auth.Authenticator
	notifyClient notifySender

	// Set by startLifetimes.
	ctx         context.Context
	stop        context.CancelFunc
	turnCtx     context.Context
	cancelTurns context.CancelFunc

	// Set by buildModel.
	roster *loadedWorkload
	llm    model.LLM

	// Set by openSessions.
	sessionSvc session.Service
	elHandle   *eventlog.Handle
	durableDB  *gorm.DB
	store      *transcript.Store
	pauseRec   *daemonPauseRecorder
	subObs     *daemonSubRunObserver

	// Set by buildRoot.
	built        rootBuild
	root         adkagent.Agent
	bundle       *workload.Bundle
	specs        []specialists.Spec
	dispatchMode string
	declared     workload.Bundle

	// Set by buildGovernance.
	effPred      effects.Predicate
	effSubAgents map[string]bool
	subIntents   compose.SubRunIntentStore
	wdRes        watchdogResolution
	wds          *watchdogPool
	toolSchemas  *toolSchemas
	parkNotices  *parkNotifier
	writeGate    compose.WriteGateResult
	r            *runner.Runner

	// Set by buildMetering.
	meters       *meterPool
	obs          *observability.Registry
	workloadName string
	tracker      *turnTracker
	turnLocks    *sessionTurnLocks
	deps         turnDeps

	// Set by buildOperatorSurfaces.
	att            *attachDeps
	resumeForPerms func(context.Context, inject.ResumeRequest) error
	a2aSrv         *a2a.Server
	a2aLn          net.Listener
	aguiSrv        *agui.Server
	aguiLn         net.Listener

	// Set by buildRequestHandlers.
	drain             time.Duration
	handler           func(context.Context, envelope.InjectPayload) error
	resumeByInterrupt func(context.Context, inject.ResumeRequest) error
	resumeHandler     func(context.Context, inject.ResumeRequest) error
	abortHandler      func(context.Context, inject.AbortRequest) error
	ackHandler        func(context.Context, inject.AckEffectsRequest) error

	// Set by startScheduling.
	schedLease        *schedulingLease
	sched             *pauseScheduler
	bootDone          chan struct{}
	schedDone         chan struct{}
	stopScheduled     func()
	monitorAckHandler inject.MonitorAckHandler

	// Set by buildInjectServer.
	srv *inject.Server

	// teardown holds what serve() used to defer, in registration order;
	// runTeardown unwinds it last-in first-out.
	teardown []func()
	// disarmTeardown is the teardown watchdog's disarm; serveUntilShutdown
	// arms the watchdog and runTeardown calls this after every step.
	disarmTeardown func()
}

// serve runs the daemon: inject endpoint + runner + session store.
// Fatal startup errors are logged in place and returned (not
// os.Exit'd) so the teardown runs.
func serve(parent context.Context, logger *slog.Logger, wl workloadOpts, mdl modelOpts, listeners listenOpts, sessions sessionOpts, resumes resumeOpts, watchdogFlag string, mcpDigest bool) error {
	d := &daemon{
		parent: parent, logger: logger, wl: wl, mdl: mdl, listeners: listeners,
		sessions: sessions, resumes: resumes, watchdogFlag: watchdogFlag, mcpDigest: mcpDigest,
		disarmTeardown: func() {},
	}
	// Deferred once, before anything can register a step, so it runs
	// whatever happens below — and disarms the teardown watchdog only
	// after every Close and flush has returned. A teardown that wedges
	// never reaches the disarm, which is the case the watchdog is for.
	defer d.runTeardown()
	for _, phase := range []func() error{
		d.checkIngress,
		d.startLifetimes,
		d.buildModel,
		d.openSessions,
		d.buildRoot,
		d.buildGovernance,
		d.buildMetering,
		d.buildOperatorSurfaces,
		d.buildRequestHandlers,
		d.startScheduling,
		d.buildInjectServer,
	} {
		if err := phase(); err != nil {
			return err
		}
	}
	return d.serveUntilShutdown()
}

// onTeardown registers f to run when serve() returns, before everything
// registered earlier — the order a defer at the same point would have run
// it in.
func (d *daemon) onTeardown(f func()) { d.teardown = append(d.teardown, f) }

// runTeardown unwinds onTeardown's registrations last-in first-out, then
// disarms the teardown watchdog. Each step is deferred in turn, so one
// that panics does not skip those registered before it: the guarantee
// the defers it replaced gave.
func (d *daemon) runTeardown() {
	defer func() { d.disarmTeardown() }()
	unwind(d.teardown)
}

func unwind(fs []func()) {
	if len(fs) == 0 {
		return
	}
	defer unwind(fs[:len(fs)-1])
	fs[len(fs)-1]()
}

// checkIngress checks the inject bind policy and builds the inject
// listener's user table and the chat ingress — configuration errors an
// operator should hear about before anything expensive runs.
func (d *daemon) checkIngress() error {
	var err error
	d.bearer = os.Getenv("MAST_INJECT_TOKEN")
	// Checked here rather than left to inject.New, which does not run
	// until provider detection, MCP loading and workload resolution are
	// all behind us. A bind that is going to be refused should be
	// refused before the operator pays for a boot that cannot finish.
	if err := inject.CheckBindPolicy(d.listeners.inject, d.bearer != ""); err != nil {
		d.logger.Error(err.Error())
		return err
	}
	if d.bearer == "" {
		d.logger.Warn("MAST_INJECT_TOKEN not set; inject endpoint is unauthenticated (loopback only)")
	}
	// Who an approval names (#198). Nil unless the operator configured a
	// user table, in which case a resume records the person rather than
	// the shared credential.
	d.injectAuthn, err = injectAuthenticator(d.logger, d.bearer)
	if err != nil {
		d.logger.Error("failed to build the inject listener's user table", "error", err.Error())
		return err
	}

	// Where a monitoring cycle speaks (v0.5 W4.5). Built here, before
	// anything expensive, because the checks it makes are configuration
	// errors an operator should hear about at startup rather than on the
	// first cycle that had something to report — including the one that
	// matters: the outbound token must not be an inbound one.
	d.notifyClient, err = buildNotifyClient(d.logger, d.listeners.notify, map[string]string{
		"MAST_INJECT_TOKEN": d.bearer,
		"MAST_ATTACH_TOKEN": os.Getenv("MAST_ATTACH_TOKEN"),
		"MAST_A2A_TOKEN":    os.Getenv("MAST_A2A_TOKEN"),
		"MAST_AGUI_TOKEN":   os.Getenv("MAST_AGUI_TOKEN"),
	})
	if err != nil {
		d.logger.Error("failed to configure the chat ingress", "error", err.Error())
		return err
	}
	return nil
}

// startLifetimes starts the two lifetimes — the signal context that
// starts the drain, and the turn context that outlives it — and OTel
// export.
func (d *daemon) startLifetimes() error {
	var err error
	// Two lifetimes (docs/durable-execution-design.md, "Shutdown
	// contract"): ctx ends when a shutdown SIGNAL arrives and triggers
	// the drain; turnCtx is what turns, toolsets, and the eventlog
	// actually live on, and ends only when the drain window elapses —
	// so an in-flight turn keeps its tools and its context for up to
	// its own budget ceiling after SIGTERM instead of dying instantly.
	d.ctx, d.stop = signal.NotifyContext(d.parent, syscall.SIGINT, syscall.SIGTERM)
	d.onTeardown(func() { d.stop() })
	d.turnCtx, d.cancelTurns = context.WithCancel(context.Background())
	d.onTeardown(func() { d.cancelTurns() })

	// Env-gated OTel trace export: a no-op unless OTEL_EXPORTER_OTLP_*
	// endpoints are set. mast opens no spans of its own in v0.1 — ADK
	// v2's runner emits the span tree; this only exports it.
	otelShutdown, otelEnabled, err := observability.SetupOTel(d.turnCtx)
	if err != nil {
		d.logger.Error("failed to configure OTel trace export", "error", err.Error())
		return err
	}
	d.onTeardown(func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = otelShutdown(flushCtx)
	})
	if otelEnabled {
		d.logger.Info("OTel trace export enabled", "endpoint_source", "OTEL_EXPORTER_OTLP_* env")
	}
	return nil
}

// buildModel resolves the workload roster, then builds the model, which
// reads the roster's builtin_tools block.
func (d *daemon) buildModel() error {
	var err error
	// The roster is resolved here rather than inside buildRoot because
	// the model is constructed first and the bundle's `builtin_tools:`
	// block is what gates the provider's server-side tools (#324).

	if d.wl.arg != "" {
		bundle, specs, cfgDir, err := resolveWorkload(d.logger, d.wl.arg)
		if err != nil {
			d.logger.Error("failed to load workload", "workload", d.wl.arg, "error", err.Error())
			return err
		}
		d.roster = &loadedWorkload{bundle: bundle, specs: specs, cfgDir: cfgDir}
	}

	d.llm, err = buildModel(d.turnCtx, d.mdl.provider, d.mdl.name, d.roster.builtinTools())
	if err != nil {
		d.logger.Error("failed to construct model", "model", d.mdl.name, "error", err.Error())
		return err
	}
	// The server-side built-ins are named here or nowhere: they never
	// become a tool call, so no permission prompt, no write-gate park
	// and no catalog line mentions them. Read off the constructed model
	// rather than off the bundle, which is what makes this line able to
	// answer "did my key take" — a misspelled YAML key is discarded in
	// silence. Absent for a backend with no such concept.
	if bt := compose.BuiltinToolsSummary(d.llm); bt != "" {
		d.logger.Info("model constructed", "name", d.llm.Name(), "builtin_tools", bt)
	} else {
		d.logger.Info("model constructed", "name", d.llm.Name())
	}
	return nil
}

// openSessions opens the session backend and the transcript store over
// it, and builds the two sinks the root agent needs at construction.
func (d *daemon) openSessions() error {
	// Session backend, built BEFORE the root agent: the planner's
	// pause_session tool needs the transcript store at construction
	// time (v0.2 pause/abort). With --attach-listen the store opens
	// through internal/eventlog instead of raw session/database: same ADK
	// tables, plus the seq-overlay the attach broadcaster live-tails.
	// Without attach the plain service keeps the pre-P1.3c shape
	// (including in-memory sessions when --session-db is empty).

	// The connection the durable stores go on, from whichever
	// backend opened one. Nil only when sessions are in-memory,
	// which is the one configuration with nothing to persist to.

	if d.listeners.attach != "" {
		// No "requires --session-db" gate here any more: resolveSessionDB
		// implies one for attach mode before serve is entered (#329), and
		// refuses by name the two shapes it cannot imply — an explicitly
		// empty --session-db, and a Postgres driver with no DSN. This
		// branch is unreachable with an empty sessions.db, which is why
		// the checks below are on elHandle rather than on the string.
		if d.sessions.implied {
			// Said before the file is created, not after: a database
			// appearing under a home directory the operator never named
			// is the surprise, and the line that prevents it has to name
			// both the cause and the way out.
			d.logger.Info("session db implied by --attach-listen (attach live-tail pumps from the eventlog overlay)",
				"path", d.sessions.db, "relocate_with", "--session-db=PATH")
		}
		dial, err := sessionDialector(d.sessions.driver, d.sessions.db)
		if err != nil {
			if d.sessions.implied {
				// The bare "create session-db directory ...: permission
				// denied" is true and useless to someone who never asked
				// for a session db. The fix has to be in the error.
				d.logger.Error("attach mode needs a durable session db and the implied location is not usable",
					"path", d.sessions.db, "error", err.Error(),
					"fix", "pass --session-db=/some/writable/path/sessions.db")
				return err
			}
			d.logger.Error("failed to construct session service", "error", err.Error())
			return err
		}
		d.elHandle, err = eventlog.Open(d.turnCtx, dial)
		if err != nil {
			d.logger.Error("failed to open eventlog-backed session store", "error", err.Error())
			return err
		}
		d.onTeardown(func() { _ = d.elHandle.Close() })
		d.sessionSvc = d.elHandle.Service
		d.durableDB = d.elHandle.DB
		d.logger.Info("session db opened (eventlog overlay for attach)", "driver", d.sessions.driver)
	} else {
		var err error
		d.sessionSvc, d.durableDB, err = buildSessionService(d.turnCtx, d.sessions.driver, d.sessions.db, d.logger)
		if err != nil {
			d.logger.Error("failed to construct session service", "error", err.Error())
			return err
		}
	}
	// Operator surface over the same session service the runner writes
	// through (docs/durable-execution-design.md, "Operator-facing
	// surface"): /abort appends the durable abort marker, and /resume
	// refuses sessions that carry one. Built before the runner because
	// the outbox plugin reads the effects-ack watermark through it.
	d.store = transcript.NewStore(d.sessionSvc, appName)

	// pause_session's record sink: the store, plus a timer push into
	// the scheduler once it exists (attached below — the scheduler
	// needs the runner, which needs the root).
	d.pauseRec = &daemonPauseRecorder{store: d.store}

	// Where a planner dispatch's private sub-run reports its spend.
	// Also built empty and attached below: the meter pool and the
	// metric registry both need the bundle this call is about to load.
	d.subObs = &daemonSubRunObserver{}
	return nil
}

// buildRoot builds the root agent and makes the startup refusals that
// need the loaded bundle.
func (d *daemon) buildRoot() error {
	var err error
	d.built, err = buildRoot(d.turnCtx, d.logger, d.llm, d.mdl, d.wl, d.roster,
		hostSeams{pause: d.pauseRec, subRun: d.subObs, digest: newDigestOptions(d.logger, d.mcpDigest)})
	if err != nil {
		d.logger.Error("failed to construct root agent", "error", err.Error())
		return err
	}
	d.root, d.bundle, d.specs, d.dispatchMode = d.built.agent, d.built.bundle, d.built.specs, d.built.dispatch
	// declared is what the workload declares, for the reads below that
	// only want a block's settings: the bundle when there is one, and
	// the zero bundle when the daemon was started without --workload to
	// serve the trivial coordinator. Every zero block means "declares
	// none" — no monitor, no cadence, the default HITL policy — which is
	// the honest answer for a daemon with no workload. Reading the nil
	// bundle directly crashed that documented mode at boot. Reads that
	// must tell "no workload" from "a workload that declares nothing"
	// (the workload name, its budget) still test bundle itself.

	if d.bundle != nil {
		d.declared = *d.bundle
	}
	d.logger.Info("root agent constructed",
		"name", d.root.Name(),
		"sub_agents", len(d.root.SubAgents()),
	)

	// A ConfigMap edit rewrites the mounted files under a running pod
	// and changes nothing about the daemon, which keeps serving what it
	// parsed at boot. Nothing reloads that; this says so out loud
	// instead of leaving the operator with a change that had no effect
	// and no line to grep for (#289).
	go watchConfig(d.turnCtx, d.logger, d.built.config, configWatchInterval)

	// Refused as early as the bundle is readable (v0.5 W4.5): a
	// workload whose entire output is a chat message, started against a
	// daemon with nowhere to send it, is a monitor an operator believes
	// is reporting. The scheduled trigger checks this again when it arms
	// — this one is here so the answer arrives before the listeners bind
	// rather than after.
	if d.declared.Monitor.Notify != nil && d.notifyClient == nil {
		err := fmt.Errorf("workload %q posts monitoring notices to %q but no chat ingress is configured; set --notify-url and %s",
			d.bundle.Name, d.bundle.Monitor.NotifyTarget(), notifyTokenEnv)
		d.logger.Error("refusing to start", "error", err.Error())
		return err
	}

	// Same refusal, same reason, for the park egress (#451): an operator
	// who named a conversation for approval parks has said they are not
	// watching a console, and starting anyway with a warning on that
	// console hands them the silence they configured against. Checked
	// here rather than where the notifier is built, which is after the
	// metric registry it needs — and startup errors belong before the
	// listeners bind.
	if err := parkNotifyConfigError(d.listeners.parkNotify, d.notifyClient); err != nil {
		d.logger.Error("refusing to start", "error", err.Error())
		return err
	}
	return nil
}

// buildGovernance builds the effect outbox, resolves the watchdog
// posture, builds the write gate, and builds the runner they plug into.
func (d *daemon) buildGovernance() error {
	var err error
	// Recorded-effect outbox (docs/durable-execution-design.md): the
	// runner plugin that refuses mutating tool calls while a session
	// carries unacknowledged dangling intents from an interrupted turn,
	// and replays recorded completions instead of re-executing. Every
	// runner construction path attaches it (#53's lesson).
	// Built once and shared with the boot-time auto-resume pass so its
	// eligibility gate classifies dangling calls exactly as the outbox does.
	d.effPred = effects.NewPredicateWithHints(effects.Overrides(d.logger, toolPolicies(d.bundle)), d.built.mcpAnnotations.ReadOnly)
	d.effSubAgents = effects.SubAgentNames(d.root)
	// A sub-agent name that also names a mutating tool is ambiguous in the
	// session log and makes a genuine effect invisible to the outbox (gate
	// finding N2). Refuse to start rather than run with the fail-open hole;
	// the operator renames one side.
	if hits := effects.CheckNameCollisions(d.effSubAgents, d.effPred, toolPolicies(d.bundle)); len(hits) > 0 {
		d.logger.Error("sub-agent/tool name collision", "names", hits)
		return fmt.Errorf("composition names both a sub-agent and a mutating tool %q: a mutating tool sharing a specialist's name is invisible to the effect outbox — rename the specialist or the tool", strings.Join(hits, ", "))
	}
	// Where a planner dispatch's mutating calls are recorded, since the
	// outbox plugin cannot see them (#235, v0.6 W9.3). The user ID is
	// bound here rather than resolved per write: the store can find one
	// by scanning the app's session list, but that scan would sit in
	// front of every dispatched mutating call.
	d.subIntents = compose.SubRunIntentStore{Store: d.store, UserID: defaultUserID}
	d.subObs.attachRecording(d.subIntents, d.effPred, d.effSubAgents)

	outboxPlugin, err := effects.New(effects.Config{
		Predicate:     d.effPred,
		SubAgentNames: d.effSubAgents,
		// A dispatched specialist's mutating calls are in neither this
		// session's log nor any log this process will ever scan, so the
		// outbox is told about them out of band — the same way its spend
		// crosses the boundary (#226). Merged before the ack filter, so
		// `mast sessions ack-effects` clears these too.
		ExternalDangling: d.subIntents.Dangling,
		AckedAt: func(ctx context.Context, sid string) (time.Time, bool) {
			return d.store.EffectsAckedAt(ctx, "", sid)
		},
		Logger: d.logger,
	})
	if err != nil {
		d.logger.Error("failed to construct effects outbox", "error", err.Error())
		return err
	}

	// Resolved here rather than at flag time because the bundle is a
	// source: --watchdog > safety.watchdog > mast's default. Logged at
	// Info with its source, because a posture nobody can see is a
	// posture nobody audits — and enforce, the one an operator most
	// needs to know is armed, is the one that only announces itself by
	// refusing a turn.
	//
	// Built before the write gate rather than after it, because the gate
	// holds a handle into the pool: the two halves of #449 are the gate
	// suppressing a refused call and the watchdog forgetting the call
	// the gate disposed of, and a gate constructed before the pool
	// exists can only reach it through a variable assigned later.
	d.wdRes, err = resolveWatchdog(watchdogInputs{Flag: d.watchdogFlag, Bundle: bundleWatchdog(d.bundle)})
	if err != nil {
		d.logger.Error("invalid watchdog posture", "error", err.Error())
		return err
	}
	d.logger.Info("watchdog posture resolved", "mode", string(d.wdRes.Mode), "source", d.wdRes.Source)
	d.wds = newWatchdogPool(d.wdRes.Mode)

	// Pre-call write gate (docs/v0.3-plan.md W2). Registered *after*
	// the outbox: a replayed result performs no new effect and needs no
	// fresh approval (resolved-decision row 144).
	plugins := []*plugin.Plugin{outboxPlugin}
	// Name → input schema over the same wired toolsets /tools reports
	// from, so the producer contract checks a proposed change against
	// the tool that would actually run it (v0.4 W7.0).
	d.toolSchemas = newToolSchemas(d.logger, d.built.toolsets)
	// Assigned once, below, as soon as the metric registry exists. The
	// gate reads it through the closure it is handed, never here.

	gateCfg := compose.WriteGateConfig{
		Bundle:      d.bundle,
		Predicate:   d.effPred,
		Specs:       d.specs,
		ToolSchemas: d.toolSchemas.lookup,
		ToolRead:    d.toolSchemas.read,
		// The gate's cut has to scrub the watchdog's evidence, or the
		// calls it already disposed of stay on the books and the next
		// identical one is the fifth in a row (#449).
		ForgetToolRun: d.wds.forgetToolRun,
		Logger:        d.logger,
	}
	// Installed only when an operator asked for it, so an unconfigured
	// daemon spends no goroutine and no context per park — and resolved
	// through the variable rather than bound to its value, because
	// parkNotices is not built until the metric registry exists a few
	// hundred lines below. Reading it at wiring time is how core-agent's
	// port of this shipped a nil dereference at startup with the feature
	// switched off (their #647 follow-up); a getter cannot have that bug.
	if d.listeners.parkNotify != "" {
		gateCfg.NotifyPark = func(ctx context.Context, n approval.ParkNotice) {
			d.parkNotices.announce(ctx, n)
		}
	}
	d.writeGate, err = compose.WriteGate(gateCfg)
	if err != nil {
		d.logger.Error("failed to construct write gate", "error", err.Error())
		return err
	}
	if d.writeGate.Plugin != nil {
		plugins = append(plugins, d.writeGate.Plugin)
		d.logger.Info("write gate registered", "on_mutation", d.declared.HITL.EffectiveOnMutation())
	}

	d.r, err = runner.New(runner.Config{
		AppName:           appName,
		Agent:             d.root,
		SessionService:    d.sessionSvc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: plugins},
	})
	if err != nil {
		d.logger.Error("failed to construct runner", "error", err.Error())
		return err
	}
	return nil
}

// buildMetering builds the budget meters and their durable stores, the
// metric registry, the park notifier, the turn tracker, and the turn
// dependencies every surface shares.
func (d *daemon) buildMetering() error {
	var err error
	d.meters = newMeterPool(d.bundle, d.specs, d.mdl.provider, d.mdl.name)

	// Both durable stores live on whichever connection the session
	// backend opened — the eventlog overlay's under --attach-listen, ADK's
	// own otherwise. They are tables mast owns either way; what differs is
	// only who else is on the connection (internal/attach's session ACL store,
	// on the overlay side).
	//
	// The two are wired on DIFFERENT conditions, and #274 is what happens
	// when they share one.
	var gstore *eventlog.GuardrailStore
	if d.durableDB != nil {
		gstore, err = eventlog.NewGuardrailStore(d.turnCtx, d.durableDB)
		if err != nil {
			d.logger.Error("failed to open the durable guardrail store", "error", err.Error())
			return err
		}
	}

	// A halt that a restart clears is not a halt, and mast's restarts are
	// automatic — but the watchdog's durability stays attach-gated, and
	// that is a correctness condition rather than a convenience: POST
	// /guardrails/reset is attach-only, so persisting a halt on a daemon
	// with no attach surface would leave an operator no way to clear it
	// short of deleting a row. Don't durably latch what nobody can
	// unlatch. --attach-listen implies --session-db (#329), so the store
	// exists exactly when the reset that clears it does.
	if d.elHandle != nil {
		d.wds.durable(gstore, d.logger)
	} else if d.wdRes.Mode.Enforces() {
		// Said once, at startup, rather than discovered after a restart
		// silently disarmed the backstop.
		d.logger.Warn("watchdog is in enforce mode without --attach-listen: a halt will not survive a restart, and there is no reset endpoint to clear one")
	}

	// The ledger (#175) needs a database and nothing else. It is not a
	// latch: there is no state an operator has to be able to clear, so the
	// argument above does not reach it, and #274 was it inheriting that
	// argument anyway by riding the same handle. A max_cost_usd ceiling
	// only bounds what a workload spends *per process* until the spend is
	// durable, and mast's restarts are automatic: a crash loop can spend
	// the cap once per restart. A crash loop is an unattended failure
	// mode, and an unattended daemon is the least likely to have bound an
	// operator socket — so gating this on attach denied the ledger to
	// exactly the deployment that needed it.
	//
	// gstore is passed for the grants half of restore(): a ceiling an
	// operator raised through the attach surface must still be replayed on
	// a later run that has no attach surface, or the restored spend wedges
	// a session they already rescued. With no attach listener there are
	// simply no grants to fold, and Fold says so cheaply.
	if d.durableDB != nil {
		sstore, serr := eventlog.NewSpendStore(d.turnCtx, d.durableDB)
		if serr != nil {
			d.logger.Error("failed to open the durable budget spend ledger", "error", serr.Error())
			return serr
		}
		d.meters.durable(sstore, gstore, d.logger)
	} else if d.bundle != nil && (d.bundle.Budget.MaxCostUSD > 0 || d.bundle.Budget.MaxTurns > 0) {
		// Two ways to arrive here, and they are not the same operator
		// mistake. Naming the wrong one is what #274 did.
		if d.sessions.db == "" {
			d.logger.Warn("budget ceilings without --session-db: sessions are in-memory, so spend is not persisted and a restart hands this workload its full budget back")
		} else {
			d.logger.Warn("budget ceilings without a durable ledger: the session backend opened no connection to write one to, so a restart hands this workload its full budget back",
				"driver", d.sessions.driver)
		}
	}

	// Fixed metric registry (internal/observability owns every family name;
	// nothing here can mint new ones). Single-workload process in v0.1,
	// so the workload label is resolved once. Built before the tracker
	// so the shutdown-drain marker-failure counter can flow through it.
	d.obs = observability.New()
	d.workloadName = "(none)"
	if d.bundle != nil {
		d.workloadName = d.bundle.Name
	}
	d.obs.Prime(d.workloadName)

	// Now that the workload has a name and a registry, the retry every
	// runtime model already carries gets somewhere to report (#452).
	reportProviderRetries(d.obs, d.workloadName, d.logger)

	// The push half of a park (#451). Built here because it needs the
	// registry above; the write gate already holds a getter for it. The
	// error is the "configured with nowhere to send" one serve refused
	// long before this, kept because a constructor that can fail should
	// say so rather than rely on a caller having checked.
	d.parkNotices, err = buildParkNotifier(d.logger, d.obs, d.workloadName, d.listeners.parkNotify, d.notifyClient)
	if err != nil {
		d.logger.Error("failed to configure park announcements", "error", err.Error())
		return err
	}

	// Shutdown bookkeeping: which sessions have a turn in flight, and
	// the pre-mark/clear ordering for their interruption markers. The
	// tracker owns the drain-time marker writes, so it emits the
	// marker-failure and planned-stop gate-pause counters (#50).
	d.tracker = newTurnTracker(d.store, d.logger, d.obs, d.workloadName)

	// Every sink now exists, so a planner dispatch's sub-run has
	// somewhere to report — and, through the tracker, a way to cancel
	// the turn it is running inside when the watchdog halts the session
	// (#226). Before this line no turn has started, so no dispatch can
	// be in flight to miss it.
	d.subObs.attach(d.meters, d.obs, d.wds, d.tracker, d.workloadName, d.logger)

	// One turn per session at a time (#62): a second runner turn on
	// the same session row dies on ADK's stale-session check, so
	// same-session injects/resumes queue behind the in-flight turn
	// (bounded by the workload wallclock budget) instead of losing it.
	d.turnLocks = newSessionTurnLocks()

	// Everything a turn needs that is fixed for the daemon's lifetime,
	// assembled once. Every surface that starts a turn takes this, so
	// "which objects does a turn run against" has one answer rather
	// than six threaded argument lists.
	d.deps = turnDeps{
		r:            d.r,
		logger:       d.logger,
		store:        d.store,
		meters:       d.meters,
		wds:          d.wds,
		obs:          d.obs,
		tracker:      d.tracker,
		turnLocks:    d.turnLocks,
		workloadName: d.workloadName,
	}
	return nil
}

// buildOperatorSurfaces builds and binds the attach, A2A and AG-UI
// surfaces; serveUntilShutdown serves them once the inject server is up.
func (d *daemon) buildOperatorSurfaces() error {
	var err error
	// Operator attach surface (--attach-listen): registry + resumer +
	// per-session adapters over the same runTurn path the inject
	// endpoint drives. Bound here (fail-fast), served after the inject
	// server is up.

	// resumeForPerms is assigned once the resume path below exists.
	//
	// The indirection is a real cycle, not an ordering accident: the
	// attach surface's /perms routes answer a park by resuming it
	// (#364), and the resume path registers the resumed session with
	// the attach surface (att.ensure). One variable breaks it; the
	// alternative is a second copy of one side's job. Nothing reaches
	// it before the assignment — it is called from a per-session
	// adapter, and no session registers until serve is listening — but
	// the nil check is kept because that ordering is a fact about a
	// hundred lines of this function rather than about this line.

	if d.listeners.attach != "" {
		grView := &guardrailView{meters: d.meters, wds: d.wds, logger: d.logger}
		wiring := attachWiring{
			appName:     appName,
			userID:      defaultUserID,
			eventLog:    d.elHandle,
			baseContext: d.turnCtx,
			modelName:   d.llm.Name(),
			description: attachDescription(d.bundle),
			// GET /sessions/{sid}/tools, off what buildRoot wired — the
			// only place a tool's attribution still exists (#133, #137).
			tools: d.built.catalog(d.logger, d.effPred),
			// GET /sessions/{sid}/subagents: the roster the daemon
			// loaded, which is what "what can this thing do" asks for —
			// /agents answers "what is running", and that is empty most
			// of the time (#134).
			subagents: subagentCatalog(d.bundle, d.specs, d.built.dispatch),
			// The turn_state projection: a session parked on the write
			// gate reported `idle` — the string a finished turn gets —
			// for every release the attach surface has existed (#313).
			store:  d.store,
			logger: d.logger,
			// GET /perms, which answered 200 with the zero value — a
			// daemon that gates nothing — on every release since the
			// route was ported (#375). The gate comes back from
			// compose.WriteGate rather than being rebuilt here, so what
			// an operator reads is the object the write gate consults;
			// it is nil exactly when the policy builds none, and the
			// projection omits mode rather than inventing one.
			gate:       d.writeGate.Gate,
			onMutation: string(d.declared.HITL.EffectiveOnMutation()),
			// GET /perms/stream + POST /perms/respond, answered from
			// the durable park instead of 501 (#364). Same resume path
			// POST /resume takes, so the approver is the authenticated
			// caller and the audit row is the one an operator gets from
			// the CLI.
			resume: func(ctx context.Context, req inject.ResumeRequest) error {
				if d.resumeForPerms == nil {
					return errors.New("resume path is not wired yet")
				}
				return d.resumeForPerms(ctx, req)
			},
			// GET /usage, which declared an eight-field token breakdown
			// and filled in two of them on every release the route has
			// existed (#356). The counts come off the same OnSpend hook
			// the durable ledger does, so what an operator reads is the
			// split the money was computed from.
			usage: d.meters.usageInfo,
			// GET /guardrails + POST /guardrails/reset: which backstop
			// stopped this session, and the only thing that unsticks
			// it. A budget trip is otherwise permanent — the meter
			// re-derives it every turn — so without the reset the
			// recovery is a daemon restart, which takes every other
			// session's in-flight turn with it (#135).
			guardrails:     grView.info,
			resetGuardrail: grView.reset,
			runTurn: func(turnCtx context.Context, sid, message string) error {
				// The attach surface stays up through the shutdown
				// drain so operators can live-tail finishing turns —
				// but NEW work is refused once draining (#48), or an
				// operator could burn the whole grace period.
				if d.tracker.isDraining() {
					return errors.New("daemon is shutting down; not accepting new turns")
				}
				// Same wallclock ceiling as the inject dispatch
				// path — operator turns are not budget-exempt.
				if d.bundle != nil && d.bundle.Budget.MaxWallclockSeconds > 0 {
					var cancel context.CancelFunc
					turnCtx, cancel = context.WithTimeout(turnCtx, time.Duration(d.bundle.Budget.MaxWallclockSeconds)*time.Second)
					defer cancel()
				}
				msg := genai.NewContentFromText(message, genai.RoleUser)
				return runTurn(turnCtx, d.deps, sid, msg, "attach:inject")
			},
		}
		var err error
		d.att, err = buildAttach(d.logger, d.listeners.attach, os.Getenv("MAST_ATTACH_TOKEN"), d.store, wiring.adapterFor)
		if err != nil {
			d.logger.Error("failed to construct attach surface", "error", err.Error())
			return err
		}
		d.onTeardown(func() { _ = d.att.srv.Close() })
	}

	// A2A server surface (--a2a-listen): agent card + JSON-RPC endpoint
	// for workloads that opt in via the bundle's a2a.expose. Bound here
	// (fail-fast), served after the inject server is up. The Backend
	// drives task verbs through the transcript store's state projection
	// (GetTask) and the same abort machinery the /abort door uses
	// (CancelTask); message/send turn execution through runTurnPre is
	// Stage B (docs/a2a-design.md).

	if d.listeners.a2a != "" {
		backend := &a2aBackend{turnDeps: d.deps, bundle: d.bundle, reg: newTaskRegistry()}
		d.a2aSrv, err = buildA2AServer(d.logger, d.listeners.a2a, d.bundle, backend, d.obs, d.turnCtx)
		if err != nil {
			d.logger.Error("failed to construct A2A server", "error", err.Error())
			return err
		}
		if d.a2aSrv != nil {
			d.a2aLn, err = a2aListener(d.listeners.a2a)
			if err != nil {
				d.logger.Error("failed to bind A2A listener", "addr", d.listeners.a2a, "error", err.Error())
				return err
			}
			d.onTeardown(func() { _ = d.a2aSrv.Close() })
		}
	}

	// AG-UI server surface (--agui-listen): an HTTP+SSE run endpoint plus a
	// discovery doc for workloads that opt in via the bundle's agui.expose.
	// Bound here (fail-fast), served after the inject server is up. The Backend
	// drives each run through the same runTurnPre chokepoint as inject/a2a,
	// translating mast events into AG-UI frames (internal/cli/agui.go).

	if d.listeners.agui != "" {
		backend := &aguiBackend{turnDeps: d.deps, bundle: d.bundle}
		d.aguiSrv, err = buildAGUIServer(d.logger, d.listeners.agui, d.bundle, backend, d.obs, d.turnCtx)
		if err != nil {
			d.logger.Error("failed to construct AG-UI server", "error", err.Error())
			return err
		}
		if d.aguiSrv != nil {
			d.aguiLn, err = aguiListener(d.listeners.agui)
			if err != nil {
				d.logger.Error("failed to bind AG-UI listener", "addr", d.listeners.agui, "error", err.Error())
				return err
			}
			d.onTeardown(func() { _ = d.aguiSrv.Close() })
		}
	}
	return nil
}

// buildRequestHandlers builds the inject, resume, abort and ack
// handlers.
func (d *daemon) buildRequestHandlers() error {
	// Drain bound, needed by the stop handler's response before the
	// shutdown goroutine exists.
	d.drain = drainBound(d.bundle)

	d.handler = func(reqCtx context.Context, p envelope.InjectPayload) error {
		// Drain gate (#58): a request that made it past accept before
		// the listener closed must not start a fresh turn mid-drain.
		if d.tracker.isDraining() {
			return inject.ErrUnavailable
		}
		if err := reservedPayloadErr(p); err != nil {
			return err
		}
		d.att.ensure(sessionIDFor(p))
		return dispatch(reqCtx, d.deps, d.bundle, p)
	}
	// resumeByInterrupt is the shared inner resume path (operator
	// interrupt keying, token keying, and the timed-pause scheduler all
	// land here).
	d.resumeByInterrupt = func(reqCtx context.Context, req inject.ResumeRequest) error {
		// Companion ops rows are marker storage, not sessions (#56):
		// resuming one would drive a runner turn into the marker row.
		if transcript.IsReservedSessionID(req.SessionID) {
			return fmt.Errorf("session ID %q uses the reserved ops-row suffix; not a resumable session: %w", req.SessionID, inject.ErrBadPayload)
		}
		// Fast-path refusal for a clearer message; the runTurnPre
		// chokepoint is the authoritative check (under the turn lock).
		if det, err := d.store.Get(reqCtx, "", req.SessionID); err == nil && det.State == transcript.StateAborted {
			return fmt.Errorf("session %q is aborted (%s); refusing resume: %w", req.SessionID, det.AbortReason, inject.ErrConflict)
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
				if err := d.store.AckEffects(ctx, "", req.SessionID, "operator resume --ack-effects"); err != nil {
					return fmt.Errorf("record effects acknowledgement for session %q: %w", req.SessionID, err)
				}
				return nil
			}
		}
		d.att.ensure(req.SessionID)
		return resume(reqCtx, d.deps, d.bundle, req, preTurn)
	}
	resumeByToken := newResumeByToken(d.store, d.logger, d.resumeByInterrupt)
	d.resumeHandler = func(reqCtx context.Context, req inject.ResumeRequest) error {
		if d.tracker.isDraining() {
			return inject.ErrUnavailable
		}
		if req.Token != "" {
			return resumeByToken(reqCtx, req)
		}
		return d.resumeByInterrupt(reqCtx, req)
	}
	// Close the cycle declared above: the attach /perms routes now have
	// the same resume path POST /resume uses, draining check included.
	d.resumeForPerms = d.resumeHandler

	d.abortHandler = func(reqCtx context.Context, req inject.AbortRequest) error {
		if transcript.IsReservedSessionID(req.SessionID) {
			return fmt.Errorf("session ID %q uses the reserved ops-row suffix; not an abortable session: %w", req.SessionID, inject.ErrBadPayload)
		}
		// Terminal abort (v0.2): marker first — the durable truth —
		// then sweep the in-flight turn's cancel handle. Deliberately
		// no turn lock here: abort must not queue behind the very turn
		// it cancels (the register-before-check handshake in runTurnPre
		// closes the ordering window instead).
		if err := recordAbort(reqCtx, d.store, d.obs, d.workloadName, req.SessionID, req.Reason); err != nil {
			// A second abort of an already-terminal session is a state
			// conflict, not a daemon fault — map to 409, mirroring /pause
			// (and the idempotent durable marker keeps the counter at 1).
			// The engine-level A2A tasks/cancel path chose idempotent
			// success instead; the operator door reports the conflict.
			if errors.Is(err, transcript.ErrAlreadyAborted) {
				return fmt.Errorf("%v: %w", err, inject.ErrConflict)
			}
			return err
		}
		if d.tracker.cancelSession(req.SessionID) {
			d.logger.Info("abort cancelled in-flight turn", "session", req.SessionID)
		}
		return nil
	}
	// Standalone ack surface: the outbox's primary scenario — a process
	// killed mid-mutating-tool — leaves a dangling intent and NO
	// pending interrupt, so resume --ack-effects (which requires one)
	// cannot reach it. Same daemon-routed shape as abort (single
	// writer). Serialized against in-flight turns via the turn lock so
	// the watermark cannot cover intents still being persisted.
	d.ackHandler = func(reqCtx context.Context, req inject.AckEffectsRequest) error {
		// Same drain contract as inject/resume (#58/#65): refuse new
		// work while shutting down, and map a drain-cancelled lock wait
		// to 503 rather than a bare 500.
		if d.tracker.isDraining() {
			return inject.ErrUnavailable
		}
		if transcript.IsReservedSessionID(req.SessionID) {
			return fmt.Errorf("session ID %q uses the reserved ops-row suffix; not a session: %w", req.SessionID, inject.ErrBadPayload)
		}
		unlock, err := d.turnLocks.lock(reqCtx, req.SessionID)
		if err != nil {
			if d.tracker.isDraining() {
				return fmt.Errorf("%w (queued ack cancelled: %v)", inject.ErrUnavailable, err)
			}
			return err
		}
		defer unlock()
		if err := d.store.AckEffects(reqCtx, "", req.SessionID, req.Reason); err != nil {
			// An operator typo is a client error, not a daemon fault.
			if errors.Is(err, transcript.ErrNotFound) {
				return fmt.Errorf("%v: %w", err, inject.ErrBadPayload)
			}
			return err
		}
		return nil
	}
	return nil
}

// startScheduling acquires the scheduling lease and starts the loops it
// governs — timed pauses, boot-time auto-resume, the scheduled trigger —
// and arms the monitor ack leg.
func (d *daemon) startScheduling() error {
	// One lease over every loop that starts a turn nobody asked for
	// (#345). See schedlease.go for what it governs and what it
	// deliberately is not. schedCtx cancels those loops without
	// cancelling the request-driven daemon around them, which is what a
	// lease lost mid-life has to do: stop acting on our own, keep
	// serving.
	d.schedLease = acquireSchedulingLease(d.turnCtx, d.durableDB, d.workloadName, d.logger)
	d.onTeardown(func() { _ = d.schedLease.release() })
	schedCtx, stopSchedulingWork := context.WithCancel(d.turnCtx)
	d.onTeardown(func() { stopSchedulingWork() })
	go func() {
		select {
		case <-schedCtx.Done():
		case <-d.schedLease.lost():
			// Another instance took the lease because our heartbeat
			// lapsed past the staleness window. It is now also firing,
			// so the safe move is to stop rather than to race it.
			d.logger.Error("scheduling lease lost to another instance; this one stops firing scheduled triggers, timed-pause resumes and auto-resume, and will not take them back without a restart",
				"workload", d.workloadName)
			stopSchedulingWork()
		}
	}()

	// Timed-pause scheduler (v0.2 pause/abort design): fires through
	// the same doors an operator would use — no privileged side path.
	//
	// nil on a passive replica, and the two push sites check for it. A
	// scheduler whose loop never runs is not a harmless spare: pushes
	// would pile up in a map nothing drains, and — worse for whoever
	// reads this next — the code would look like the timer was armed.

	if d.schedLease.drivesScheduledWork() {
		d.sched = newPauseScheduler(d.store, d.logger,
			newTimedFireCallback(d.store, d.tracker, d.obs, d.workloadName, d.resumeByInterrupt, d.logger))
		go d.sched.run(schedCtx)
		go func() {
			// Boot scan: seeds timers minted before this process started —
			// including ones that expired while the daemon was down.
			if err := d.sched.seed(schedCtx); err != nil {
				d.logger.Error("timed-pause boot scan failed; pre-existing timers will not fire until restart", "error", err.Error())
			}
		}()
		// And a rescan on a cadence, because the boot scan only covers
		// what predates this process. A pause minted with a resume_at on
		// a *passive* replica is written durably and pushed into that
		// replica's scheduler, which never runs — so without this the
		// timer would wait for the leader to restart. The rescan is
		// idempotent by construction: entries is keyed by token and
		// fireDue re-fetches, so re-arming a consumed token is a silent
		// drop rather than a second fire.
		go d.sched.runRescan(schedCtx)
		// pause_session records minted mid-serve push their timers
		// straight in.
		d.pauseRec.attach(d.sched)
	}

	// Boot-time auto-resume (#41): scan sessions a prior shutdown cut
	// short and drive a continuation for each eligible one. On schedCtx
	// (drain-cancellable, and cancelled if the scheduling lease is lost)
	// and only with a durable store — in-memory sessions never survive a
	// restart, so there is nothing to resume.
	// bootDone lets the drain path await this goroutine so it cannot
	// start a fresh turn after the drain has sampled "all turns finished"
	// (closed immediately when the pass never launches).
	//
	// Gated on the scheduling lease for the same reason the two
	// schedulers are, and it is the path where duplication costs most:
	// two replicas booting together would each drive a continuation for
	// every cut-short session, so every one of them gets two turns
	// (#345). The issue names the scheduler and the timed-pause path;
	// this is the third loop of the same shape and leaving it out would
	// have made the headline false.
	d.bootDone = make(chan struct{})
	if d.resumes.auto && d.sessions.db != "" && d.schedLease.drivesScheduledWork() {
		ar := &autoResumer{
			turnDeps:     d.deps,
			bundle:       d.bundle,
			dispatchMode: d.dispatchMode,
			pred:         d.effPred,
			subAgents:    d.effSubAgents,
			external:     d.subIntents.Dangling,
			window:       d.resumes.window,
		}
		go func() {
			defer close(d.bootDone)
			ar.run(schedCtx)
		}()
	} else {
		close(d.bootDone)
		// Only the store explains itself here. A lease refusal has
		// already said, at ERROR, everything it stops — repeating one of
		// the three at INFO would read like a different cause.
		if d.resumes.auto && d.sessions.db == "" {
			d.logger.Info("auto-resume enabled but --session-db is empty (in-memory sessions); nothing to resume")
		}
	}

	// Scheduled trigger (v0.4 W4.1): a workload that declares a cadence
	// wakes itself, with no inbound POST. Same drain discipline as the
	// boot pass — schedDone lets the shutdown path stop the loop and
	// wait for it, so a tick cannot start a turn after the drain has
	// sampled "all turns finished" (closed immediately when the
	// workload declares no cadence).
	d.schedDone = make(chan struct{})
	d.stopScheduled = func() {}
	if sched := d.declared.EdgeTrigger.Scheduled; sched != nil && d.schedLease.drivesScheduledWork() {
		// Both already validated at load; re-resolved here because the
		// daemon reads the cadence from the bundle, not from its own
		// durable record — editing the bundle is how an operator
		// changes the schedule.
		interval, ierr := sched.EffectiveInterval()
		jitter, jerr := sched.EffectiveJitter()
		if ierr != nil || jerr != nil {
			close(d.schedDone)
			d.logger.Error("scheduled trigger not armed; its cadence does not parse",
				"error", errors.Join(ierr, jerr).Error())
		} else {
			// The collection leg rides the same seam the write gate's
			// precondition read does — the same wired toolsets, the
			// same direct-run assertion — because there is exactly one
			// door for a tool call no model asked for (v0.5 W4.2).
			collector := newMonitorCollector(d.logger, d.bundle.Monitor, d.toolSchemas.collect, appName, defaultUserID)
			if collector.enabled() {
				// transitions_from is on the line because it changes what
				// a failed cycle means: the named result is parsed, and a
				// classifier that answers badly stops the fire rather than
				// reaching the model as an absence (v0.5 W4.4).
				args := []any{"workload", d.workloadName, "collect", d.bundle.Monitor.CollectTools()}
				if key := d.bundle.Monitor.TransitionsKey(); key != "" {
					args = append(args, "transitions_from", key)
				}
				d.logger.Info("monitoring cycle armed; these calls run before the model is woken", args...)
			}
			// The egress leg (v0.5 W4.5). A bundle that declares where to
			// speak and a daemon with no ingress to speak through is a
			// refusal, not a warning: the workload's whole output is the
			// message it was going to send.
			nf, nerr := newNotifier(d.logger, d.obs, d.workloadName, d.bundle.Monitor, d.notifyClient)
			if nerr != nil {
				close(d.schedDone)
				d.logger.Error("monitoring notifications not armed", "workload", d.workloadName, "error", nerr.Error())
				return nerr
			}
			if nf.enabled() {
				args := []any{"workload", d.workloadName, "conversation", nf.conv}
				if nf.digest > 0 {
					args = append(args, "digest_after", nf.digest.String())
				}
				if d.bundle.Monitor.TransitionsKey() == "" {
					// Worth saying out loud, because the operator who wrote
					// the notify block probably wanted the other thing.
					args = append(args, "speaks_every_cycle", true)
				}
				d.logger.Info("monitoring notifications armed; a cycle that changes nothing will not wake the model", args...)
			}
			st := newScheduledTrigger(d.store, d.logger, d.obs, d.tracker, d.workloadName, defaultUserID, interval, jitter,
				newScheduledFireCallback(d.deps, d.bundle, collector, nf, d.att.ensure))
			if d.sessions.db == "" {
				// The anchor lands in an in-memory store that dies with
				// the process, so the cadence re-phases on every restart.
				// Worth saying out loud: "the schedule survives a restart"
				// is the claim W4.1 makes, and without --session-db it
				// does not hold.
				d.logger.Warn("scheduled trigger has no durable store (--session-db is empty); its cadence will re-anchor on every restart",
					"workload", d.workloadName, "interval", interval.String())
			}
			if err := st.seed(d.turnCtx); err != nil {
				d.logger.Error("scheduled trigger could not persist its anchor; the cadence runs but will re-phase if this process restarts",
					"workload", d.workloadName, "error", err.Error())
			}
			d.stopScheduled = st.stop
			go func() {
				defer close(d.schedDone)
				st.run(schedCtx)
			}()
		}
	} else {
		close(d.schedDone)
	}

	// The ack leg (v0.5 W4.6), armed outside the scheduled-trigger branch
	// on purpose. An ack is inbound: it arrives when an operator reads
	// their chat, and a workload whose monitoring is driven by inbound
	// POSTs rather than a cadence takes acks exactly like one that fires
	// on a clock. Tying it to the cadence would make "can this be
	// acknowledged?" depend on how the cycle happens to be triggered.
	acker := newMonitorAcker(d.logger, d.obs, d.store, d.declared.Monitor, d.toolSchemas.ack, d.workloadName, appName, defaultUserID)

	if acker.enabled() {
		d.monitorAckHandler = acker.forward
		d.logger.Info("operator acknowledgements armed; they are attributed here and suppressed by the producer",
			"workload", d.workloadName, "tool", acker.tool)
		if d.sessions.db == "" {
			// The attribution is the half mast alone holds, and without a
			// durable store it dies with the process — leaving the
			// producer's suppression in place with no record of who asked
			// for it. Worth refusing? No: the forward still works, and a
			// daemon run without --session-db has already accepted that
			// nothing it writes survives. Worth saying, loudly.
			d.logger.Warn("operator acknowledgements have no durable record (--session-db is empty); the suppression will outlive mast's note of who asked for it",
				"workload", d.workloadName)
		}
	}
	return nil
}

// buildInjectServer builds the pause, extend and stop handlers, the
// readiness checks, and the inject server itself.
func (d *daemon) buildInjectServer() error {
	var err error
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
		h, err := openGatePause(reqCtx, d.store, d.obs, d.workloadName, req.SessionID, spec)
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
			if d.tracker.cancelSession(req.SessionID) {
				d.logger.Info("hard pause cancelled in-flight turn", "session", req.SessionID)
			}
		}
		if !spec.ResumeAt.IsZero() && d.sched != nil {
			// nil on a passive replica (#345). The record is durable
			// either way, so the leader's rescan arms it within a minute
			// — that is the whole reason the rescan exists.
			d.sched.push(h.Token, spec.ResumeAt)
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
		rec, err := d.store.ExtendToken(reqCtx, req.Token, ttl)
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
		d.tracker.planStop(reason, req.PauseSessions)
		d.logger.Info("planned stop initiated",
			"reason", reason, "pause_sessions", req.PauseSessions, "drain_bound", d.drain.String())
		d.stop() // cancels the signal context; the shutdown goroutine drains
		return inject.StopResult{DrainBound: d.drain.String()}, nil
	}

	// Readiness, as opposed to "the process is accepting connections",
	// which is what GET / says and all a liveness probe should ask
	// (#326). An in-memory daemon has no durable store to vouch for, so
	// it reports no checks rather than a check that cannot fail.
	healthChecks := map[string]inject.HealthCheck{}
	if d.durableDB != nil {
		db := d.durableDB
		healthChecks["session_db"] = func(ctx context.Context) error {
			return eventlog.CheckSessionDB(ctx, db)
		}
	}

	d.srv, err = inject.New(inject.Config{
		Listen:             d.listeners.inject,
		HealthChecks:       healthChecks,
		BearerToken:        d.bearer,
		Authenticator:      d.injectAuthn,
		Handler:            d.handler,
		ResumeHandler:      d.resumeHandler,
		AbortHandler:       d.abortHandler,
		AckEffectsHandler:  d.ackHandler,
		MonitorAckHandler:  d.monitorAckHandler,
		PauseHandler:       pauseHandler,
		ExtendTokenHandler: extendHandler,
		StopHandler:        stopHandler,
		ParksHandler:       parksHandler(d.store, d.logger),
		Logger:             d.logger,
		Metrics:            d.obs.Handler(),
		// Request contexts derive from the turn lifetime, so when the
		// drain window elapses the surviving handler turns are
		// cancelled (and unwind) rather than dying at process exit.
		BaseContext: d.turnCtx,
	})
	if err != nil {
		d.logger.Error("failed to construct inject server", "error", err.Error())
		return err
	}
	return nil
}

// serveUntilShutdown serves every bound surface, waits for the drain the
// signal context starts, and returns the status the drain earned.
func (d *daemon) serveUntilShutdown() error {
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
		<-d.ctx.Done()
		d.logger.Info("shutdown signal received; draining in-flight turns", "drain_bound", d.drain.String())
		// Detached from parent on purpose: parent being cancelled is what
		// started the drain, and the drain must still get its full bound.
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(d.parent), d.drain)
		defer cancel()
		// Close the inject listener FIRST (#58): Shutdown stops
		// accepting immediately and then waits for handlers, so
		// launching it before the pre-mark pass means no new turn can
		// arrive while markers are being written. The drain-gate in
		// the handlers covers requests already past accept.
		shutdownErr := make(chan error, 1)
		go func() { shutdownErr <- d.srv.Shutdown(drainCtx) }()
		// Pre-mark BEFORE waiting: a SIGKILL mid-drain must find the
		// interruption markers already on disk.
		d.tracker.beginDrain(drainCtx)
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
		case <-d.bootDone:
		case <-drainCtx.Done():
		}
		// And the scheduled trigger, for the same reason and with the
		// same bound. Its loop can be parked on a timer measured in
		// hours, so it is told to stop rather than merely asked to
		// notice: the fire path's drain check would not run until the
		// tick came due, which may be long after the process is gone.
		d.stopScheduled()
		select {
		case <-d.schedDone:
		case <-drainCtx.Done():
		}
		remaining := d.tracker.wait(drainCtx)
		if len(remaining) == 0 {
			if errShutdown != nil {
				// No turns in flight — the listener just has lingering
				// non-turn connections (an SSE scrape, a slow client).
				d.logger.Warn("inject server still draining connections at the deadline; no turns were in flight", "error", errShutdown.Error())
				return
			}
			d.logger.Info("drain complete; all in-flight turns finished")
			return
		}
		// Freeze before cancelling: the surviving turns ARE interrupted,
		// and their unwinding must not clear the markers that say so.
		d.tracker.freeze()
		d.cancelTurns()
		// Give the cancelled turns a short beat to unwind before the
		// deferred teardown (attach close, eventlog close) yanks their
		// dependencies — cancellation is useless if the process exits
		// before the cancelled goroutines observe it (#48).
		graceCtx, graceCancel := context.WithTimeout(context.WithoutCancel(d.parent), 3*time.Second)
		defer graceCancel()
		d.tracker.wait(graceCtx)
		// Honest split (#58/#63): marked survivors truly carry durable
		// markers; unmarked survivors are turns whose mark write failed
		// or never ran — the log must not assert durability for them.
		markedSurvivors, unmarkedSurvivors := d.tracker.survivors()
		drainExpired = true
		d.logger.Warn("drain window elapsed; sessions cut short",
			"sessions_with_durable_marker", markedSurvivors,
			"sessions_without_marker", unmarkedSurvivors,
			"drain_bound", d.drain.String())
	}()

	if d.att != nil {
		go func() {
			// The listener is already bound (buildAttach); Serve only
			// returns on Close or a hard accept failure. A hard failure
			// takes the daemon down — a half-alive daemon whose operator
			// surface silently died is worse than a restart.
			if err := d.att.srv.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) && d.ctx.Err() == nil {
				d.logger.Error("attach server terminated", "error", err.Error())
				d.stop()
			}
		}()
	}

	if d.a2aSrv != nil {
		go func() {
			// The listener is already bound (a2aListener); Serve only
			// returns on Close or a hard accept failure. As with attach, a
			// hard failure takes the daemon down — a half-alive daemon
			// whose A2A surface silently died is worse than a restart.
			if err := d.a2aSrv.Serve(d.a2aLn); err != nil && !errors.Is(err, http.ErrServerClosed) && d.ctx.Err() == nil {
				d.logger.Error("a2a server terminated", "error", err.Error())
				d.stop()
			}
		}()
	}

	if d.aguiSrv != nil {
		go func() {
			// The listener is already bound (aguiListener); Serve only
			// returns on Close or a hard accept failure. As with attach/a2a, a
			// hard failure takes the daemon down — a half-alive daemon whose
			// AG-UI surface silently died is worse than a restart.
			if err := d.aguiSrv.Serve(d.aguiLn); err != nil && !errors.Is(err, http.ErrServerClosed) && d.ctx.Err() == nil {
				d.logger.Error("agui server terminated", "error", err.Error())
				d.stop()
			}
		}()
	}

	if err := d.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// Startup/hard failure: the shutdown goroutine is still parked
		// on ctx.Done, so return without waiting on it.
		d.logger.Error("inject server terminated", "error", err.Error())
		return err
	}
	// ListenAndServe returns ErrServerClosed the moment Shutdown BEGINS;
	// the drain (and its marker bookkeeping) completes in the shutdown
	// goroutine. Returning before it finishes was #38.
	<-shutdownDone
	// The drain is done; only the teardown (OTel flush, eventlog and
	// attach Close, context cancels) remains as serve() unwinds. Arm
	// a watchdog so a wedged Close or an unkillable goroutine surfaces a
	// stack dump and a distinct exit code instead of hanging until the
	// supervisor SIGKILLs the process with no diagnostic. A healthy
	// teardown disarms it: runTeardown calls the disarm after its last
	// step has returned.
	d.disarmTeardown = armTeardownWatchdog(teardownWatchdogTimeout, dumpGoroutines, os.Exit, d.logger)
	if drainExpired {
		// Exit-code contract (issue #42): 3 = the drain window expired
		// with interrupted survivors — work was cut short, whoever
		// initiated the stop. Restart=on-failure supervision revives
		// the daemon exactly when the boot pass has repair work (#41);
		// a clean drain exits 0 and such a unit stays down.
		d.logger.Warn("shutdown complete; drain expired with interrupted sessions (exit 3)")
		return errDrainExpired
	}
	d.logger.Info("shutdown complete")
	return nil
}

// errDrainExpired maps to exit code 3 in run(): the shutdown drain
// window expired with turns still in flight (their sessions carry
// interruption markers where the write landed).
var errDrainExpired = errors.New("drain window expired with interrupted sessions")
