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

// Package compose wires a workload bundle plus its specialist specs
// into a runnable root agent. It is the shared core behind the two
// entry points that construct dispatch shapes: cmd/mast (flag-driven)
// and the top-level mast convenience package (programmatic). Both MUST
// go through BuildRoot so the dispatch semantics — planner override,
// graph vs. coordinator, per-mode toolset offering — cannot drift
// between the binary and the library.
//
// This is runtime glue, not public API (docs/library-api-design.md
// marks internal/ packages churnable); library consumers reach it via
// the root mast package or compose the pkg/ subsystems directly.
package compose

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/tool"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/graph"
	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/pricing"
	"github.com/go-steer/mast/pkg/providers/anthropic"
	geminiprov "github.com/go-steer/mast/pkg/providers/gemini"
	"github.com/go-steer/mast/pkg/providers/mock"
	"github.com/go-steer/mast/pkg/router"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// Dispatch selects the root shape BuildRoot assembles.
type Dispatch string

const (
	// DispatchCoordinator is the spike-1 SubAgents pattern: a
	// Chat-mode coordinator with the roster as SubAgents (pkg/router).
	DispatchCoordinator Dispatch = "coordinator"

	// DispatchGraph is the spike-2 workflow-graph LLM-as-router shape
	// (pkg/graph). Requires a SingleTurn classifier in the roster.
	DispatchGraph Dispatch = "graph"

	// DispatchFanout is the W3 fan-out shape (pkg/graph.BuildFanout):
	// the roster's Task specialists run concurrently as read-only
	// analysts and a graph.SynthesisName specialist merges what they
	// return. Requires that specialist, and refuses to build an analyst
	// that can mutate.
	DispatchFanout Dispatch = "fanout"

	// DispatchBounded is the W4.3 one-call shape (see bounded.go): a
	// roster of exactly one SingleTurn specialist that declares an
	// output_schema, run as a single workflow node. No router, no tool
	// loop, no second specialist to choose between — so a cycle costs
	// one model call, and that is a number rather than a hope.
	DispatchBounded Dispatch = "bounded"

	// DispatchAuto picks the shape from the roster: fanout when a
	// graph.SynthesisName specialist is present, graph when a
	// SingleTurn classifier and a graph.FallbackName Task specialist
	// are both present (the pair graph dispatch needs), coordinator
	// otherwise. This is the library default — programmatic callers
	// declare a roster, not a flag.
	//
	// It never picks bounded; see RosterShape.
	DispatchAuto Dispatch = "auto"
)

// Provider aliases for the Gemini family. The Anthropic pair lives in
// pkg/providers/anthropic (ProviderName / VertexProviderName); these
// two have no provider package of their own because both backends are
// the same genai client under different configuration.
//
// The names match pkg/taskclass.Providers(), which has carried a
// "vertex" family since the port — the tier table could always resolve
// against it, there was just no way to ask for it. They also match
// core-agent's config.ProviderVertex, which is where the pattern comes
// from.
const (
	// ProviderGemini serves gemini-* models through whichever backend
	// genai detects from the environment.
	ProviderGemini = "gemini"

	// ProviderVertex serves gemini-* models against Vertex AI, named
	// outright rather than inferred from GOOGLE_GENAI_USE_VERTEXAI.
	ProviderVertex = "vertex"
)

// DefaultVertexLocation is the location a Vertex client falls back to
// when neither GOOGLE_CLOUD_LOCATION nor GOOGLE_CLOUD_REGION is set.
// It is genai's own default for the backend, restated here only so
// that the value mast passes is explicit and loggable rather than
// filled in somewhere inside the SDK.
const DefaultVertexLocation = "global"

// Resolve returns the dispatch shape to build, given the caller's
// choice and the bundle's own declaration.
//
// A shape is a property of the roster — fan-out needs read-only
// analysts and a synthesis specialist, graph needs a SingleTurn
// classifier and a `_fallback` — so a bundle that declares one is
// stating a fact about itself, not a preference. It therefore wins over
// an unspecified caller, and loses to a caller that named a shape
// explicitly (an operator overriding one run).
func (d Dispatch) Resolve(b workload.Bundle) Dispatch {
	if d != "" && d != DispatchAuto {
		return d
	}
	if b.Dispatch != "" {
		return Dispatch(b.Dispatch)
	}
	if d == "" {
		return DispatchAuto
	}
	return d
}

// RosterShape reads the dispatch shape out of a roster: fan-out when it
// has a synthesis merger, graph when it has both a SingleTurn
// classifier and a graph.FallbackName Task specialist, coordinator
// otherwise.
//
// Exported because BuildRoot is not the only caller that has to know
// the shape — cmd/mast's boot-time auto-resume pass runs only under
// coordinator dispatch, and a second copy of this rule living there is
// a copy that drifts. It reads specs rather than built agents so a
// caller can ask before paying for construction.
//
// # Why bounded is never inferred
//
// A roster of one SingleTurn specialist that declares an output_schema
// is exactly the bounded shape's roster, and it falls through to
// coordinator here on purpose. Every other shape this function infers
// is one a roster cannot have arrived at by accident — you do not add a
// `_synthesis` specialist without meaning fan-out. A one-specialist
// roster is what every workload looks like on its first day, and
// inferring bounded from it would silently take the coordinator away
// from bundles that have been running on it since v0.1: the coordinator
// answers in its own voice, the bounded shape returns the specialist's
// JSON report, and no error is raised on the way. `dispatch: bounded`
// is therefore a declaration, never a deduction — the same reasoning
// that keeps cmd/mast's terminal default at coordinator rather than
// auto (see resolveDispatch).
func RosterShape(specs []specialists.Spec) Dispatch {
	var hasSynthesis, hasFallback, hasClassifier bool
	for _, s := range specs {
		if s.Mode == specialists.ModeSingleTurn {
			hasClassifier = true
			continue
		}
		switch s.Name {
		case graph.SynthesisName:
			hasSynthesis = true
		case graph.FallbackName:
			hasFallback = true
		}
	}
	switch {
	case hasSynthesis:
		return DispatchFanout
	case hasClassifier && hasFallback:
		return DispatchGraph
	default:
		return DispatchCoordinator
	}
}

// RootConfig carries everything BuildRoot needs to turn a loaded
// bundle + specs into a root agent. Bundle and Specs use the existing
// pkg/workload and pkg/specialists vocabulary — file-loaded and
// programmatic values are indistinguishable here by design
// (docs/library-api-design.md, "Embeddable config vs. file-loaded
// config").
type RootConfig struct {
	// Bundle is the workload definition (naming, roster order,
	// planner/HITL policy).
	Bundle workload.Bundle

	// Specs is the loaded specialist roster. Specs with an empty Mode
	// build as Task-mode (the same default pkg/specialists applies).
	Specs []specialists.Spec

	// Model is the root model: the one the coordinator/planner runs on
	// and the default for every specialist that declares no `model:`
	// override.
	Model model.LLM

	// ModelName and Provider are the strings Model was built from (the
	// --model / --provider values). They are what per-specialist
	// `model:` overrides resolve against — see NewModelResolver.
	//
	// Leaving ModelName empty is legal (a library caller may hand over
	// a model.LLM it constructed itself); overrides then resolve on
	// their own model id, with provider selection falling back to the
	// env-driven detection in BuildModel.
	ModelName string
	Provider  string

	// Toolsets are offered to Task-mode specialists (and filtered
	// through each spec's allowlist by specialists.Build). SingleTurn
	// classifiers never receive toolsets — they run one shot with no
	// tool loop.
	Toolsets []tool.Toolset

	// Dispatch selects the root shape. Empty means DispatchAuto.
	Dispatch Dispatch

	// Logger, when non-nil, receives the same construction-time notes
	// cmd/mast has always logged (e.g. planner overriding dispatch).
	Logger *slog.Logger

	// PauseRecorder enables the planner's pause_session tool (v0.2
	// plane-A self-pause) by giving it a durable record sink —
	// *transcript.Store, or the daemon's scheduler-aware wrapper. Nil
	// (no durable store) leaves the tool unregistered.
	PauseRecorder planner.PauseRecorder

	// SubRunObserver folds the events of a planner dispatch's private
	// sub-runner back into the caller's accounting (#226). A host that
	// meters, records metrics or watches for loops on the root runner's
	// event stream MUST set this, or a planner-dispatched specialist's
	// spend reaches none of them. Only the planner shape has the seam;
	// coordinator, graph and fan-out dispatch funnel sub-agent events up
	// the root stream already.
	SubRunObserver planner.SubRunObserver
}

// BuildRoot builds the roster and assembles the dispatch shape. Every
// shape is refused up front if the roster's read/write split does not
// hold — see CheckCapabilitySplit.
//
//   - bundle.Planner.Enabled → the supervisor-body planner root
//     (pkg/planner); Dispatch is ignored.
//   - DispatchGraph → the workflow graph (pkg/graph); errors without
//     a SingleTurn classifier.
//   - DispatchFanout → the concurrent-analysts fan-out shape
//     (pkg/graph.BuildFanout); errors without a graph.SynthesisName
//     specialist, or if any analyst can reach a mutating tool.
//   - DispatchBounded → the one-call shape (bounded.go); errors unless
//     the roster is exactly one SingleTurn specialist declaring an
//     output_schema.
//   - DispatchCoordinator → the SubAgents coordinator (pkg/router).
//   - DispatchAuto/empty → the bundle's own `dispatch:` when it names
//     one (see Dispatch.Resolve); otherwise fanout when the roster has
//     a synthesis specialist, graph when it has both a SingleTurn
//     classifier and a graph.FallbackName specialist, else coordinator.
//
// The second return is the non-MCP tools this call wired onto the root,
// for an operator catalog to report (#137). It is what compose itself
// registered and nothing more: the planner's control-plane vocabulary
// under planner dispatch, and empty under every other shape. Tools ADK
// installs on its own — finish_task, a coordinator's per-sub-agent
// delegation tools — are not in it, because compose never sees them and
// there is no accessor for a built agent's tools (#51 / adk-go#1229).
// A caller with no catalog to feed discards it.
func BuildRoot(ctx context.Context, cfg RootConfig) (adkagent.Agent, []tool.Tool, error) {
	dispatch := cfg.Dispatch.Resolve(cfg.Bundle)
	switch dispatch {
	case DispatchCoordinator, DispatchGraph, DispatchFanout, DispatchBounded, DispatchAuto:
	default:
		return nil, nil, fmt.Errorf("compose: unknown dispatch %q (want coordinator, graph, fanout, bounded, or auto)", dispatch)
	}

	// The read/write split is checked before anything is built, on every
	// dispatch shape: a roster whose diagnosers can remediate is a
	// roster mast refuses to run, not one it runs carefully. The
	// predicate takes a nil logger here because the fan-out path below
	// builds the same one with cfg.Logger, and the audited-override
	// lines are worth exactly one appearance in a startup log.
	if err := CheckCapabilitySplit(cfg.Bundle, cfg.Specs, MutationPredicate(cfg.Bundle, nil), cfg.Logger); err != nil {
		return nil, nil, err
	}

	// The bounded roster is checked before anything is constructed, not
	// at the terminal arm below, so a roster that can never be bounded
	// is refused without first resolving a tier, opening a provider
	// client or building a specialist mast is about to throw away.
	var bounded specialists.Spec
	if dispatch == DispatchBounded {
		var err error
		if bounded, err = boundedSpec(cfg.Bundle, cfg.Specs); err != nil {
			return nil, nil, err
		}
	}

	resolve := NewModelResolver(ctx, cfg.Provider, cfg.ModelName, cfg.Model, cfg.Logger)
	resolveTier := NewTierResolver(ctx, cfg.Provider, cfg.ModelName, cfg.Model, resolve, cfg.Logger)
	logTierResolution(cfg.Specs, cfg.Provider, cfg.ModelName, cfg.Logger)

	byName := make(map[string]adkagent.Agent, len(cfg.Specs))
	taskOnly := make(map[string]graph.Specialist, len(cfg.Specs))
	// analysts are every Task specialist that is neither the synthesis
	// merger nor the graph-dispatch fallback: the fan-out branch set,
	// in roster order. Collected on every path so DispatchAuto can ask
	// whether the roster is a fan-out roster.
	var analysts []graph.Analyst
	var classifier adkagent.Agent
	for _, spec := range cfg.Specs {
		opts := specialists.BuildOptions{Model: cfg.Model, Resolve: resolve, ResolveTier: resolveTier}
		// Task-mode specialists get the toolsets; SingleTurn
		// classifiers don't (they run in one shot with no tool loop).
		// An empty Mode is Task — the same default specialists.Build
		// applies — so programmatic Specs behave like loader-normalized
		// ones.
		isTask := spec.Mode != specialists.ModeSingleTurn
		if isTask {
			opts.Toolsets = cfg.Toolsets
		}
		a, err := specialists.Build(spec, opts)
		if err != nil {
			return nil, nil, fmt.Errorf("build specialist %q: %w", spec.Name, err)
		}
		byName[spec.Name] = a
		if isTask {
			// The spec's budget rides along so graph.Build can map
			// max_wallclock_seconds onto the node's Timeout.
			taskOnly[spec.Name] = graph.Specialist{
				Agent:  a,
				Budget: spec.Budget,
				// And the capability, which is how graph.Build finds
				// the node an approved change is routed to.
				Capability: spec.Capability,
			}
			if spec.Name != graph.SynthesisName && spec.Name != graph.FallbackName {
				// The allowlist rides along too: BuildFanout checks it,
				// and the built agent no longer carries it.
				analysts = append(analysts, graph.Analyst{
					Name:   spec.Name,
					Agent:  a,
					Budget: spec.Budget,
					Tools:  spec.Tools,
				})
			}
		} else if classifier == nil {
			classifier = a
		}
	}

	// Planner dispatch (docs/orchestration-design.md "The planner",
	// v0.1 scaffold): when the bundle enables the planner, the root is
	// the supervisor-body planner with the bundle's specialists as its
	// invoke_specialist roster, and the requested dispatch is ignored.
	// The planner's own model calls stream past the caller's meter like
	// any other agent's; a dispatched specialist's do not, and reach the
	// caller through SubRunObserver instead (#226).
	if cfg.Bundle.Planner.Enabled {
		if cfg.Logger != nil {
			cfg.Logger.Info("planner enabled; --dispatch ignored", "dispatch_flag", string(cfg.Dispatch))
		}
		pcfg := planner.Config{
			Name:           cfg.Bundle.Name,
			Description:    cfg.Bundle.Description,
			Model:          cfg.Model,
			Specialists:    byName,
			Order:          cfg.Bundle.Specialists,
			PauseRecorder:  cfg.PauseRecorder,
			SubRunObserver: cfg.SubRunObserver,
		}
		root, err := planner.NewRoot(pcfg)
		if err != nil {
			return nil, nil, err
		}
		// Rebuilt rather than plucked off the agent, because ADK gives
		// no way to pluck. planner.Vocabulary is the function New itself
		// calls, so the same config yields the same set — the catalog
		// cannot list a tool this planner does not have.
		builtin, err := planner.Vocabulary(pcfg)
		if err != nil {
			return nil, nil, fmt.Errorf("compose: enumerate the planner's tools: %w", err)
		}
		return root, builtin, nil
	}

	if dispatch == DispatchAuto {
		dispatch = RosterShape(cfg.Specs)
	}

	if dispatch == DispatchFanout {
		root, err := graph.BuildFanout(graph.FanoutConfig{
			Bundle:    cfg.Bundle,
			Analysts:  analysts,
			Synthesis: taskOnly[graph.SynthesisName],
			Mutating:  MutationPredicate(cfg.Bundle, cfg.Logger),
		})
		return root, nil, err
	}

	if dispatch == DispatchBounded {
		root, err := buildBounded(cfg.Bundle, bounded, byName[bounded.Name])
		return root, nil, err
	}

	if dispatch == DispatchGraph {
		if classifier == nil {
			return nil, nil, fmt.Errorf("--dispatch=graph requires a SingleTurn classifier specialist in the roster")
		}
		root, err := graph.Build(graph.Config{
			Bundle:            cfg.Bundle,
			Classifier:        classifier,
			Specialists:       taskOnly,
			ApprovedChangeSet: ApprovedChangeSet(cfg.Logger),
			Logger:            cfg.Logger,
		})
		return root, nil, err
	}

	root, err := router.Build(router.Config{
		Bundle:      cfg.Bundle,
		Specialists: byName,
		Model:       cfg.Model,
	})
	return root, nil, err
}

// MutationPredicate builds the tool mutation classifier for a bundle:
// mast's default-deny-unknown stance, narrowed by the workload's
// audited tool_catalog.tools overrides.
//
// The conversion exists because pkg/effects deliberately does not
// import pkg/workload (that would drag the YAML loader into every
// library embed that only wants the guard), so somebody who imports
// both has to bridge the two ToolPolicy types. compose imports both.
func MutationPredicate(b workload.Bundle, logger *slog.Logger) effects.Predicate {
	policies := make([]effects.ToolPolicy, 0, len(b.ToolCatalog.Tools))
	for _, p := range b.ToolCatalog.Tools {
		policies = append(policies, effects.ToolPolicy{Name: p.Name, Mutating: p.Mutating})
	}
	return effects.NewPredicate(effects.Overrides(logger, policies))
}

// BuildModel constructs the model.LLM for the given provider alias
// and model name. The provider alias is only consulted where the
// model id alone is ambiguous (claude-* serves against
// api.anthropic.com or Vertex); everything else dispatches on the
// name.
//
//   - "echo": fake in-process echo model (no credentials required).
//   - "toolactor": request-driven offline fake that drives registered
//     tool calls deterministically (pkg/agent/toolactor.go); the v0.2
//     UAT harness uses it to exercise the crash/drain/abort legs against
//     a real blocking MCP tool. No credentials required.
//   - "scripted": JSONL recorded-turn replay via pkg/providers/mock;
//     the recording path comes from MAST_SCRIPT, and
//     MAST_SCRIPT_STRICT=1 enables strict Contents matching.
//   - "gemini-*": ADK's Gemini model wrapped in pkg/providers/gemini's
//     builtin-tool layer (GoogleSearch + URLContext on — core-agent's
//     defaults). `--provider=vertex` names the Vertex backend outright;
//     with no alias it stays genai's env-driven selection. See
//     geminiClientConfig.
//   - "claude-*": pkg/providers/anthropic; see anthropicProvider for
//     backend selection.
func BuildModel(ctx context.Context, provider, name string) (model.LLM, error) {
	switch {
	case name == "echo":
		return mastagent.NewEchoModel("mast-echo"), nil
	case name == "toolactor":
		return mastagent.NewToolActorModel("mast-toolactor"), nil
	case name == "scripted":
		path := os.Getenv("MAST_SCRIPT")
		if path == "" {
			return nil, fmt.Errorf("model %q requires MAST_SCRIPT (path to a recorded-turns JSONL)", name)
		}
		return mock.NewScripted(path, os.Getenv("MAST_SCRIPT_STRICT") == "1")
	case strings.HasPrefix(name, "gemini-"):
		cfg, err := geminiClientConfig(provider)
		if err != nil {
			return nil, err
		}
		base, err := gemini.NewModel(ctx, name, cfg)
		if err != nil {
			return nil, err
		}
		return geminiprov.Wrap(base, geminiprov.Options{
			BuiltinTools:        geminiprov.DefaultBuiltinTools(),
			TolerateEmptyChunks: geminiOnVertex(provider),
		}), nil
	case strings.HasPrefix(name, "claude-"):
		p, err := anthropicProvider(ctx, provider)
		if err != nil {
			return nil, err
		}
		return p.Model(ctx, name)
	default:
		return nil, fmt.Errorf("unknown model %q (want `echo`, `toolactor`, `scripted`, a `gemini-*` or a `claude-*` model id)", name)
	}
}

// offlineFakes are the model names that need no credentials and reach
// no network: the CLI/library spellings BuildModel accepts, plus the
// instance names it stamps on them (a library caller that constructs
// the fake itself and passes it as Config.Model surfaces the latter
// through Model.Name()).
var offlineFakes = map[string]bool{
	"echo": true, "mast-echo": true,
	"toolactor": true, "mast-toolactor": true,
	"scripted": true,
}

// IsOfflineFake reports whether name is one of mast's offline test
// doubles.
func IsOfflineFake(name string) bool { return offlineFakes[name] }

// NewModelResolver returns the specialists.ModelResolver that binds a
// specialist's `model:` override to a concrete model.LLM. Resolution
// goes through BuildModel, so an override is dispatched exactly like a
// --model value: by model id, with provider only disambiguating the
// Anthropic backend.
//
// Two rules shape it, and both are load-bearing:
//
// Cross-provider overrides are allowed (specialists-design open Q#4,
// resolved 2026-08-12). BuildModel already dispatches on the model id,
// so a gemini-* specialist under a claude-* parent needs no new
// machinery; refusing it would mean inventing a provider-family
// classifier as a second source of truth beside BuildModel's own
// dispatch. The price is that credentials for every distinct provider
// in the roster must resolve, and they must resolve at construction —
// which is where the error lands, not mid-incident on first call.
//
// When the root model is an offline fake, every override collapses back
// to it. A bundle that declares real model tiers must still run under
// `--model=echo` / `scripted` / `toolactor`, or tiering a bundle would
// silently break the offline S/U/E test tiers (docs/v0.3-plan.md §2)
// and scripts/demo-spike2.sh — the whole process is a test double, and
// there is nothing to tier.
//
// Resolution is memoized per model id: eight analysts on one tier share
// one provider client.
func NewModelResolver(ctx context.Context, provider, rootName string, root model.LLM, logger *slog.Logger) specialists.ModelResolver {
	var (
		mu       sync.Mutex
		cache    = map[string]model.LLM{}
		warnOnce sync.Once
	)
	return func(name string) (model.LLM, error) {
		if root != nil && (name == rootName || IsOfflineFake(rootName)) {
			if name != rootName && logger != nil {
				warnOnce.Do(func() {
					logger.Info("specialist model overrides collapsed to the root model: root is an offline fake",
						"root_model", rootName)
				})
			}
			return root, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if m, ok := cache[name]; ok {
			return m, nil
		}
		m, err := BuildModel(ctx, provider, name)
		if err != nil {
			return nil, err
		}
		cache[name] = m
		return m, nil
	}
}

// geminiOnVertex reports whether a gemini-* model will run against
// Vertex: either the --provider=vertex alias says so outright, or
// GOOGLE_GENAI_USE_VERTEXAI is truthy and genai's own env-driven
// selection will pick Vertex. Vertex streaming interleaves
// candidate-less heartbeat chunks, so the builtins wrapper's
// empty-chunk tolerance follows the backend actually in use — which is
// why this asks about the alias too, and not only the environment.
func geminiOnVertex(provider string) bool {
	if provider == ProviderVertex {
		return true
	}
	v := strings.ToLower(os.Getenv("GOOGLE_GENAI_USE_VERTEXAI"))
	return v == "true" || v == "1"
}

// geminiClientConfig builds the genai client config for a gemini-*
// model under the given --provider alias.
//
// Two paths, and the split is deliberate:
//
// With no alias (or any alias that is not `vertex`) the config stays
// empty and genai decides everything from the environment, exactly as
// it did before the alias existed — GOOGLE_GENAI_USE_VERTEXAI picks the
// backend, GOOGLE_API_KEY / GEMINI_API_KEY or GOOGLE_CLOUD_* supply the
// credentials. Nothing about that path changes here.
//
// `--provider=vertex` names the backend outright, which is the whole
// point of the alias: a deployment whose credential is the service
// account's ADC should not have to also set an env var whose absence
// surfaces as an API-key error from the *other* backend. Project and
// location still come from the environment (there is no config file for
// them), but a missing project fails here, naming the variable, rather
// than inside genai as a struct dump.
//
// Location defaults to genai's own default rather than mast inventing
// one, and is passed explicitly so the resolved value is deterministic.
func geminiClientConfig(provider string) (*genai.ClientConfig, error) {
	if provider != ProviderVertex {
		return &genai.ClientConfig{}, nil
	}
	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		return nil, fmt.Errorf("--provider=%s needs a project: set GOOGLE_CLOUD_PROJECT (and authenticate with Application Default Credentials, e.g. `gcloud auth application-default login`)", ProviderVertex)
	}
	location := os.Getenv("GOOGLE_CLOUD_LOCATION")
	if location == "" {
		location = os.Getenv("GOOGLE_CLOUD_REGION")
	}
	if location == "" {
		location = DefaultVertexLocation
	}
	return &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  project,
		Location: location,
	}, nil
}

// anthropicProvider picks the Anthropic backend for claude-* models.
// An explicit --provider alias wins; with no alias, ANTHROPIC_API_KEY
// selects the first-party API and a resolvable Vertex project selects
// Anthropic-on-Vertex — the same detection order core-agent's registry
// used, scoped to the two Anthropic backends. CacheSystem stays off,
// matching core-agent's default (no non-test caller ever enabled it).
func anthropicProvider(ctx context.Context, provider string) (*anthropic.Provider, error) {
	switch provider {
	case anthropic.ProviderName:
		return anthropic.New(anthropic.Options{})
	case anthropic.VertexProviderName:
		return anthropic.NewVertex(ctx, anthropic.VertexOptions{})
	case ProviderGemini, ProviderVertex:
		// A Gemini-family alias picks a Gemini backend; it says nothing
		// about Anthropic's. A roster may still name a claude-*
		// specialist under a gemini root — cross-provider overrides are
		// allowed (see NewModelResolver) — so detect the backend the
		// same way the no-alias path does rather than refusing a model
		// the alias was never about.
		return anthropicProvider(ctx, "")
	case "":
		if os.Getenv(anthropic.EnvAPIKey) != "" {
			return anthropic.New(anthropic.Options{})
		}
		if os.Getenv(anthropic.EnvVertexProject) != "" || os.Getenv("GOOGLE_CLOUD_PROJECT") != "" {
			return anthropic.NewVertex(ctx, anthropic.VertexOptions{})
		}
		return nil, fmt.Errorf("claude-* models need ANTHROPIC_API_KEY (first-party) or a Vertex project (ANTHROPIC_VERTEX_PROJECT_ID / GOOGLE_CLOUD_PROJECT), or an explicit --provider=anthropic|anthropic-vertex")
	default:
		return nil, fmt.Errorf("provider %q cannot serve claude-* models (want `anthropic` or `anthropic-vertex`)", provider)
	}
}

// builtinCatalog is the compiled-in pricing catalog (pkg/pricing's
// builtin layer only — no config overrides or pricing.json files are
// wired into the daemon yet). Built once; catalog construction with
// empty Options cannot fail.
var builtinCatalog = sync.OnceValue(func() *pricing.Catalog {
	c, err := pricing.NewCatalog(pricing.Options{})
	if err != nil {
		// Unreachable with empty Options (no files are read), kept
		// explicit so a future wiring of file layers can't silently
		// swallow a load error here.
		panic(fmt.Sprintf("compose: builtin pricing catalog: %v", err))
	}
	return c
})

// RatePer1K derives pkg/budget's flat USD-per-1K-total-tokens rate
// for a model name (budget.Limits.RatePer1K — API unchanged).
//
// Gemini and Claude rates come from pkg/pricing's builtin catalog
// (longest-prefix lookup, so dated/suffixed IDs land). The catalog prices
// input and output tokens separately, but the budget meter only sees
// UsageMetadata.TotalTokenCount, so the flat rate is the plain
// average of the two per-MTok rates scaled to per-1K — a deliberate
// v0.1 approximation that overcharges input-heavy sessions and
// undercharges output-heavy ones rather than complicating the budget
// API. Gemini IDs the catalog doesn't know keep the old flat spike
// rate so cost metering never silently drops to zero.
//
// The echo fake keeps its inflated rate: offline smoke tests
// (scripts/demo-spike2.sh scenario 3) trip small caps with it. The
// scripted replay and toolactor share it — all offline test doubles.
func RatePer1K(modelName string) float64 {
	switch {
	case modelName == "echo", modelName == "scripted", modelName == "toolactor":
		return 0.05 // inflated so offline smoke tests can trip small caps
	case strings.HasPrefix(modelName, "claude-"):
		if r, ok := builtinCatalog().Lookup(modelName); ok && !r.IsZero() {
			return (r.InputPerMTok + r.OutputPerMTok) / 2 / 1000
		}
		return 0.003 // catalog miss: haiku-class flat fallback, never zero
	case strings.HasPrefix(modelName, "gemini-"):
		if r, ok := builtinCatalog().Lookup(modelName); ok && !r.IsZero() {
			return (r.InputPerMTok + r.OutputPerMTok) / 2 / 1000
		}
		return 0.0006 // catalog miss: pre-catalog flat spike rate
	default:
		return 0.001
	}
}

// MeterScopes derives the per-specialist budget scopes for a roster:
// the ceilings a spec declares (`max_turns`, `max_cost_usd`) plus, when
// it declares a `model:` override or a `tier:`, that model's price.
// Specialists that declare neither get no scope — they are metered into
// the session totals and nothing else, which is what an un-tiered,
// un-capped roster wants.
//
// The price follows SpecModelName, so a `tier:` is priced at the model
// the tier resolved to and not at the root's rate; that is why the
// provider the run was started with is a parameter here.
//
// max_wallclock_seconds is deliberately absent: it is a node-level knob
// (pkg/graph maps it onto workflow.NodeConfig.Timeout), not something a
// usage meter can see.
//
// Pricing collapses under an offline fake, on the same condition
// NewModelResolver collapses the models themselves: if the root model
// is echo/scripted/toolactor then every override resolved back to it,
// so every token was produced by the fake and pricing a specialist at
// its declared tier would report a cost that provably did not happen.
// The consequence is worth stating plainly — per-model cost attribution
// cannot be demonstrated end-to-end in a credential-free test tier,
// because the condition that makes the tier credential-free is exactly
// the condition that collapses the tiers. The derivation is unit-
// testable here; the end-to-end claim needs real models.
func MeterScopes(specs []specialists.Spec, provider, rootModelName string) map[string]budget.Limits {
	fake := IsOfflineFake(rootModelName)
	var scopes map[string]budget.Limits
	for _, s := range specs {
		l := budget.Limits{
			MaxTurns:   s.Budget.MaxTurns,
			MaxCostUSD: s.Budget.MaxCostUSD,
		}
		if name := SpecModelName(s, provider, rootModelName); name != "" && !fake {
			l.RatePer1K = RatePer1K(name)
		}
		if l == (budget.Limits{}) {
			continue
		}
		if scopes == nil {
			scopes = make(map[string]budget.Limits, len(specs))
		}
		scopes[s.Name] = l
	}
	return scopes
}
