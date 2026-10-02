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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/mast/internal/compose"
	"github.com/go-steer/mast/internal/config"
	"github.com/go-steer/mast/internal/digest"
	"github.com/go-steer/mast/internal/effects"
	mastmcp "github.com/go-steer/mast/internal/mcp"
	"github.com/go-steer/mast/internal/planner"
	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// buildModel constructs the model.LLM for the given provider alias
// and name, with bt gating the provider's server-side built-in tools.
// Thin alias over the shared core (internal/compose) so the flag
// surface and the library surface can't drift.
//
// The runtime constructor, not the bare one: a daemon turn that meets a
// provider's 429 should wait two seconds rather than end short (#452).
func buildModel(ctx context.Context, provider, name string, bt workload.BuiltinTools) (model.LLM, error) {
	return compose.NewRuntimeModel(ctx, provider, name, bt)
}

// loadedWorkload is a roster resolved before the root agent is built.
//
// It exists because of an ordering constraint the daemon did not used
// to have: the bundle's `builtin_tools:` block gates the provider's
// server-side tools, and those are chosen when the MODEL is
// constructed, which happens well before buildRoot. Resolving once and
// handing the result down keeps the bytes #289's config digest
// identifies as the bytes that actually configured the model — a second
// load would be cheap and would also be a second answer.
type loadedWorkload struct {
	bundle workload.Bundle
	specs  []specialists.Spec
	cfgDir string
}

// builtinTools is nil-safe: no --workload means no bundle to read, and
// mast's baseline is every server-side tool off.
func (w *loadedWorkload) builtinTools() workload.BuiltinTools {
	if w == nil {
		return workload.BuiltinTools{}
	}
	return w.bundle.BuiltinTools
}

// resolveWorkload turns the --workload flag value into a loaded bundle
// plus its specialist specs. Two modes:
//
//   - Path mode: the value is an existing directory → legacy spike
//     layout (workload.yaml + specialists/ inside that directory).
//     scripts/demo-spike2.sh depends on this shape; unchanged.
//   - Name mode: anything else is a workload name resolved via the
//     .agents/ discovery rules in internal/config (exclusive
//     single-location; see docs/config-layout-design.md).
//
// resolveWorkload loads a workload bundle + its specialist roster. The
// returned dir is where the MCP catalog (mcp.json) lives: the workload
// directory in path mode, or the config root in name mode.
func resolveWorkload(logger *slog.Logger, arg string) (workload.Bundle, []specialists.Spec, string, error) {
	if fi, err := os.Stat(arg); err == nil && fi.IsDir() {
		bundle, err := workload.Load(filepath.Join(arg, "workload.yaml"))
		if err != nil {
			return workload.Bundle{}, nil, "", fmt.Errorf("load workload: %w", err)
		}
		loaded, err := specialists.LoadDir(filepath.Join(arg, "specialists"))
		if err != nil {
			return workload.Bundle{}, nil, "", fmt.Errorf("load specialists: %w", err)
		}
		return bundle, loaded, arg, nil
	}

	cfg, err := config.Load(logger)
	if err != nil {
		return workload.Bundle{}, nil, "", err
	}
	bundle, ok := cfg.Workloads[arg]
	if !ok {
		return workload.Bundle{}, nil, "", fmt.Errorf(
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
	return bundle, loaded, cfg.Root.Dir, nil
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
// the dispatch shape: the spike-1 SubAgents coordinator (internal/router),
// the spike-2 workflow graph (internal/graph), or the W3 fan-out shape
// (internal/graph.BuildFanout). Without --workload it constructs a trivial
// single-agent coordinator (useful for pure inject-endpoint smoke).
//
// The shape comes from --dispatch when the operator typed one, from the
// workload's own `dispatch:` otherwise, and from the historical
// coordinator default when neither names a shape — see resolveDispatch.
// It is returned alongside the agent because callers act on it too (the
// boot-time auto-resume pass only runs under coordinator dispatch), and
// one resolution shared beats two that can drift.
// hostSeams are the daemon-side sinks compose wires into the root:
// where a plane-A self-pause records itself, and where a planner
// dispatch's private sub-run reports its spend. Both are constructed
// before the root and finish wiring after it — the scheduler needs the
// runner and the meter pool needs the bundle, and both of those need
// the root — so they travel together as one late-bound bundle rather
// than as a growing tail of positional arguments. A zero value is
// legal: it is what a caller with neither seam (a test, a one-shot)
// passes.
type hostSeams struct {
	pause  planner.PauseRecorder
	subRun planner.SubRunObserver

	// digest configures the MCP digest wrap (#221). Nil turns it off,
	// which is what --mcp-digest=false hands over and what a caller
	// with no MCP surface — every test that builds a root without one —
	// passes by leaving the zero value alone.
	digest *mastmcp.DigestOptions
}

func buildRoot(ctx context.Context, logger *slog.Logger, llm model.LLM, mdl modelOpts, wl workloadOpts, pre *loadedWorkload, seams hostSeams) (rootBuild, error) {
	if err := validateDispatch(wl.dispatch); err != nil {
		return rootBuild{}, err
	}
	if wl.arg == "" {
		logger.Warn("no --workload supplied; running trivial single-agent coordinator")
		a, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
			Name:        "trivial_coordinator",
			Description: "Trivial coordinator (no workload loaded).",
			Instruction: "Acknowledge the incident briefly.",
			Model:       llm,
		})
		return rootBuild{agent: a, dispatch: resolveDispatch(wl.dispatch, nil)}, err
	}

	// pre is the roster serve() already resolved, so the bundle that
	// gated the model's server-side built-ins is the bundle the root is
	// built from. A caller that has not resolved one (a test, any path
	// that only has the flag value) passes nil and this loads it.
	if pre == nil {
		bundle, specs, cfgDir, err := resolveWorkload(logger, wl.arg)
		if err != nil {
			return rootBuild{}, err
		}
		pre = &loadedWorkload{bundle: bundle, specs: specs, cfgDir: cfgDir}
	}
	bundle, loaded, cfgDir := pre.bundle, pre.specs, pre.cfgDir
	resolved := resolveDispatch(wl.dispatch, &bundle)
	if resolved == workload.DispatchAuto {
		// Resolve `auto` here rather than handing it downstream: the
		// returned shape is what the caller's own decisions key off
		// (boot-time auto-resume runs only under coordinator dispatch),
		// and "auto" is not a shape.
		resolved = string(compose.RosterShape(loaded))
	}
	logger.Info("workload loaded",
		"name", bundle.Name,
		"specialists", len(bundle.Specialists),
		"mcp_servers", len(bundle.ToolCatalog.MCP),
		"dispatch", resolved,
	)
	logger.Info("specialists loaded", "count", len(loaded))

	// Which configuration this is, as something an operator can compare
	// from outside (#289). Computed here rather than in serve because
	// this is where the paths are still known — the loaders return
	// values, and only the bundle keeps its filename.
	cfgID := identifyConfig(cfgDir, bundle.Filename,
		configPaths(&bundle, loaded, filepath.Join(cfgDir, mastmcp.CatalogFileName)))
	cfgID.log(logger)

	// A declared HTTP trigger is informational — the inject server
	// declares its routes globally and reads nothing from the bundle
	// (pkg/workload's HTTPTrigger says so; internal/inject's fixed route
	// table is where it does not happen). Silence made that
	// indistinguishable, from outside, from a field that works: the
	// declared path answered 405, which reads as a verb mistake, and
	// the operator went debugging their emitter. mast already logs the
	// write gate, the watchdog posture and every mutation-class
	// override at startup; an ignored trigger declaration belongs in
	// that same set (#277).
	//
	// Warned rather than refused: the field has been inert since the
	// spike, and failing a bundle that runs today over a declaration
	// that never did anything would be a break with no safety behind
	// it. The unmatched path now answers 404 naming POST /inject, which
	// is the other half of the same fix.
	if h := bundle.EdgeTrigger.HTTP; h != nil && h.Path != "" {
		logger.Warn("edge_trigger.http IS NOT WIRED — the declared path serves nothing",
			"declared_path", h.Path,
			"declared_auth", h.Auth,
			"endpoint", "POST /inject",
			"note", "the inject server's routes are fixed; per-workload path prefixes are deferred")
	}

	// Refuse a roster the shape can never build BEFORE touching MCP,
	// which reads a privilege-bearing catalog file and, for a hosted
	// server, fetches an OAuth token. Neither is work a doomed startup
	// should do, and the token fetch is also the error the operator
	// would otherwise be shown — an auth failure standing in for "your
	// roster is fourteen specialists and bounded takes one". BuildRoot
	// re-checks; see compose.CheckRoster.
	if err := compose.CheckRoster(bundle, loaded, compose.Dispatch(resolved)); err != nil {
		return rootBuild{}, err
	}

	// Annotations captured off each server's tools/list, so the mutation
	// predicate can classify an MCP tool instead of defaulting it to
	// mutating (#447). Built here because this is where the toolsets are
	// built; read in serve, where the predicate is.
	mcpAnnotations := mastmcp.NewAnnotations(logger)
	toolsets, extraTools, err := wireMCPToolsets(ctx, logger, bundle, cfgDir, mdl.name, seams.digest, mcpAnnotations)
	if err != nil {
		return rootBuild{}, err
	}

	a, builtin, err := compose.BuildRoot(ctx, compose.RootConfig{
		Bundle:          bundle,
		Specs:           loaded,
		Model:           llm,
		ModelName:       mdl.name,
		Provider:        mdl.provider,
		Toolsets:        toolsets,
		SpecialistTools: extraTools,
		Dispatch:        compose.Dispatch(resolved),
		Logger:          logger,
		PauseRecorder:   seams.pause,
		SubRunObserver:  seams.subRun,
	})
	if err != nil {
		return rootBuild{}, err
	}
	return rootBuild{
		agent:    a,
		bundle:   &bundle,
		specs:    loaded,
		toolsets: toolsets,
		// retrieve_raw joins the planner's vocabulary in the catalog:
		// /tools answers "what can this daemon do" (#205), and a tool
		// specialists can actually call is part of that answer whether
		// compose installed it or the MCP wiring did. Concatenated into
		// a fresh slice rather than appended in place — compose's return
		// value is not this function's to extend.
		builtin:        append(append([]tool.Tool(nil), builtin...), extraTools...),
		dispatch:       resolved,
		config:         cfgID,
		mcpAnnotations: mcpAnnotations,
	}, nil
}

// rootBuild is what buildRoot resolved: the composed root agent plus the
// facts the rest of serve acts on. A struct rather than a fifth and sixth
// return value because the operator surfaces need the wired MCP toolsets
// too — GET /sessions/.../tools projects them, with the server each tool
// came from (#133) — and ADK exposes no tool accessor on a built agent,
// so the wiring site is the only place that attribution still exists.
type rootBuild struct {
	agent    adkagent.Agent
	bundle   *workload.Bundle
	specs    []specialists.Spec
	toolsets []tool.Toolset
	// builtin are the non-MCP tools this daemon wired: the planner's
	// control-plane vocabulary from compose (empty under every other
	// dispatch shape, #137) plus retrieve_raw when the MCP digest wrap
	// is active (#221). Same reason as toolsets: the wiring site is the
	// only place the list still exists.
	builtin  []tool.Tool
	dispatch string

	// config is what was loaded, hashed — the startup line is written
	// from it, and serve hands it to the drift watcher. Zero value
	// under --workload="" (no bundle, nothing to watch).
	config configIdentity

	// mcpAnnotations holds the readOnlyHint every wired MCP server
	// published for its own tools. serve feeds it to the mutation
	// predicate; it is populated lazily, on each server's first
	// tools/list, which is the same round trip that shows the model the
	// tool in the first place (#447).
	mcpAnnotations *mastmcp.Annotations
}

// catalog builds the operator tool catalog for this build.
//
// A method rather than an inline call in serve because the bug behind
// #133 was a field serve never assigned — the endpoint answered 200
// with an empty list on every daemon while the code that would have
// filled it sat unreferenced. Two sources of tools means two arguments
// that can silently be left out, so the assembly is somewhere a test
// can call.
func (b rootBuild) catalog(logger *slog.Logger, pred effects.Predicate) *toolCatalog {
	return newToolCatalog(logger, b.toolsets, b.builtin, pred, b.bundle)
}

// validateDispatch rejects a --dispatch value the binary cannot build.
// Empty is legal: it means "the workload decides".
func validateDispatch(dispatch string) error {
	switch dispatch {
	case "", workload.DispatchCoordinator, workload.DispatchGraph, workload.DispatchFanout, workload.DispatchBounded, workload.DispatchAuto:
		return nil
	default:
		return fmt.Errorf("unknown --dispatch %q (want `coordinator`, `graph`, `fanout`, `bounded` or `auto`)", dispatch)
	}
}

// resolveDispatch applies cmd/mast's precedence: an explicit --dispatch
// wins, then the workload's own `dispatch:`, then coordinator.
//
// Coordinator stays the terminal default rather than `auto` so that
// upgrading mast cannot silently re-shape an existing bundle that never
// said anything about dispatch — adding a `_synthesis` specialist to a
// roster should not turn a coordinator into a fan-out behind an
// operator's back. A bundle (or an operator, for one run) opts into
// being auto-shaped by naming `auto`, which the caller then resolves
// against the roster via compose.RosterShape.
func resolveDispatch(flagValue string, bundle *workload.Bundle) string {
	if flagValue != "" {
		return flagValue
	}
	if bundle != nil && bundle.Dispatch != "" {
		return bundle.Dispatch
	}
	return workload.DispatchCoordinator
}

// wireMCPToolsets builds the workload's MCP toolsets from the mcp.json
// catalog. Server definitions live in the catalog file alongside the
// workload (cfgDir); the bundle only references them by name. Each entry
// dispatches by transport kind (streamable HTTP or a local stdio process)
// — no server is special-cased.
//
// It is a no-op under the echo model, which never emits tool
// calls, so wiring MCP there is pure startup cost (and, for credentialed
// HTTP servers, would surface auth failures as workload-load errors rather
// than "no real LLM"). The scripted model and real providers do wire MCP;
// a stdio server needs no credentials, so offline tool-driving works under
// --model scripted. A workload that references a server absent from the
// catalog is a fatal error rather than a silently-dropped tool.
//
// digestOpts, when non-nil, routes every wired server's tool responses
// through internal/digest (#221) and the second return value carries the
// retrieve_raw escape hatch that makes that safe. The two travel
// together deliberately: a digest with no way back to the raw payload
// is a lossy compression the model cannot appeal. A server that set
// `no_digest: true` is wrapped by neither — WithDigest returns it
// unchanged — but retrieve_raw is still registered for the servers that
// were, and a roster where every server opted out gets no tool, because
// nothing will have stored anything for it to fetch.
func wireMCPToolsets(ctx context.Context, logger *slog.Logger, bundle workload.Bundle, cfgDir, modelName string, digestOpts *mastmcp.DigestOptions, annotations *mastmcp.Annotations) ([]tool.Toolset, []tool.Tool, error) {
	if modelName == "echo" || len(bundle.ToolCatalog.MCP) == 0 {
		return nil, nil, nil
	}
	catalogPath := filepath.Join(cfgDir, mastmcp.CatalogFileName)
	catalog, err := mastmcp.LoadCatalog(catalogPath)
	if err != nil {
		return nil, nil, err
	}
	var toolsets []tool.Toolset
	digested := 0
	for _, ref := range bundle.ToolCatalog.MCP {
		scfg, ok := catalog.Servers[ref.Server]
		if !ok {
			return nil, nil, fmt.Errorf(
				"workload references MCP server %q not defined in %s", ref.Server, catalogPath)
		}
		// A stdio server executes a local command — audit-log the
		// resolved command *and* args (the security-relevant payload
		// often lives in the args) so the operator can see what mast will
		// run. mcp.json is a privilege-bearing control-plane file; the
		// launch itself is lazy (on first tool use).
		if scfg.Transport == mastmcp.TransportStdio {
			cmdPath, cmdArgs := scfg.ResolvedCommand()
			logger.Info("wiring stdio MCP server (launched on first tool use)",
				"server", ref.Server, "command", cmdPath, "args", cmdArgs)
		}
		ts, err := mastmcp.NewToolset(ctx, ref.Server, scfg, mastmcp.WithAnnotations(annotations))
		if err != nil {
			return nil, nil, fmt.Errorf("wire MCP server %q: %w", ref.Server, err)
		}
		serverOpts := digestOpts
		if scfg.NoDigest {
			serverOpts = nil
		}
		if wrapped := mastmcp.WithDigest(ts, ref.Server, serverOpts); wrapped != ts {
			ts = wrapped
			digested++
		}
		toolsets = append(toolsets, ts)
		logger.Info("MCP toolset wired",
			"server", ref.Server, "transport", scfg.Transport, "digest", serverOpts != nil)
	}
	if digested == 0 || digestOpts == nil || digestOpts.Store == nil {
		return toolsets, nil, nil
	}
	rawTool, err := mastmcp.NewRetrieveRawTool(digestOpts.Store)
	if err != nil {
		return nil, nil, fmt.Errorf("wire retrieve_raw: %w", err)
	}
	logger.Info("MCP digest wrap active",
		"servers", digested, "threshold_bytes", mastmcp.DefaultDigestThreshold, "escape_hatch", mastmcp.RetrieveRawToolName)
	return toolsets, []tool.Tool{rawTool}, nil
}

// newDigestOptions builds the MCP digest configuration for a serve run,
// or nil when --mcp-digest=false turned the wrap off.
//
// The CCR store is scratch: it holds the raw payload of a tool call only
// so retrieve_raw can hand it back during the same run, and nothing
// reads it after the process exits. So it lives under os.TempDir()
// rather than beside --session-db — house rule #5, and the honest
// lifetime. A store that cannot be created is not fatal: digesting
// still runs, retrieve_raw goes unregistered, and the log says so,
// because losing the escape hatch is worse than losing the compression
// but neither is worth refusing to serve over.
func newDigestOptions(logger *slog.Logger, enabled bool) *mastmcp.DigestOptions {
	if !enabled {
		logger.Info("MCP digest wrap disabled (--mcp-digest=false)")
		return nil
	}
	opts := &mastmcp.DigestOptions{}
	dir := filepath.Join(os.TempDir(), "mast", "digest-raw")
	store, err := digest.NewFilesystemStore(dir)
	if err != nil {
		logger.Warn("MCP digest raw-payload store unavailable; retrieve_raw will not be registered",
			"dir", dir, "error", err.Error())
		return opts
	}
	opts.Store = store
	return opts
}
