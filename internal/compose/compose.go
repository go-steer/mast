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
	"strings"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/tool"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/graph"
	"github.com/go-steer/mast/pkg/planner"
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

	// DispatchAuto picks the shape from the roster: graph when a
	// SingleTurn classifier and a graph.FallbackName Task specialist
	// are both present (the pair graph dispatch needs), coordinator
	// otherwise. This is the library default — programmatic callers
	// declare a roster, not a flag.
	DispatchAuto Dispatch = "auto"
)

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

	// Model drives every built specialist (specs with a model override
	// already bound it upstream — the spike binds one model per
	// process).
	Model model.LLM

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
}

// BuildRoot builds the roster and assembles the dispatch shape:
//
//   - bundle.Planner.Enabled → the supervisor-body planner root
//     (pkg/planner); Dispatch is ignored.
//   - DispatchGraph → the workflow graph (pkg/graph); errors without
//     a SingleTurn classifier.
//   - DispatchCoordinator → the SubAgents coordinator (pkg/router).
//   - DispatchAuto/empty → graph when the roster has both a SingleTurn
//     classifier and a graph.FallbackName specialist, else coordinator.
func BuildRoot(cfg RootConfig) (adkagent.Agent, error) {
	dispatch := cfg.Dispatch
	if dispatch == "" {
		dispatch = DispatchAuto
	}
	switch dispatch {
	case DispatchCoordinator, DispatchGraph, DispatchAuto:
	default:
		return nil, fmt.Errorf("compose: unknown dispatch %q (want coordinator, graph, or auto)", dispatch)
	}

	byName := make(map[string]adkagent.Agent, len(cfg.Specs))
	taskOnly := make(map[string]graph.Specialist, len(cfg.Specs))
	var classifier adkagent.Agent
	for _, spec := range cfg.Specs {
		opts := specialists.BuildOptions{Model: cfg.Model}
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
			return nil, fmt.Errorf("build specialist %q: %w", spec.Name, err)
		}
		byName[spec.Name] = a
		if isTask {
			// The spec's budget rides along so graph.Build can map
			// max_wallclock_seconds onto the node's Timeout.
			taskOnly[spec.Name] = graph.Specialist{Agent: a, Budget: spec.Budget}
		} else if classifier == nil {
			classifier = a
		}
	}

	// Planner dispatch (docs/orchestration-design.md "The planner",
	// v0.1 scaffold): when the bundle enables the planner, the root is
	// the supervisor-body planner with the bundle's specialists as its
	// invoke_specialist roster, and the requested dispatch is ignored.
	// Budget is unchanged — the planner's model calls stream past the
	// caller's meter like any other agent's.
	if cfg.Bundle.Planner.Enabled {
		if cfg.Logger != nil {
			cfg.Logger.Info("planner enabled; --dispatch ignored", "dispatch_flag", string(cfg.Dispatch))
		}
		return planner.NewRoot(planner.Config{
			Name:        cfg.Bundle.Name,
			Description: cfg.Bundle.Description,
			Model:       cfg.Model,
			Specialists: byName,
			Order:       cfg.Bundle.Specialists,
		})
	}

	if dispatch == DispatchAuto {
		_, hasFallback := taskOnly[graph.FallbackName]
		if classifier != nil && hasFallback {
			dispatch = DispatchGraph
		} else {
			dispatch = DispatchCoordinator
		}
	}

	if dispatch == DispatchGraph {
		if classifier == nil {
			return nil, fmt.Errorf("--dispatch=graph requires a SingleTurn classifier specialist in the roster")
		}
		return graph.Build(graph.Config{
			Bundle:      cfg.Bundle,
			Classifier:  classifier,
			Specialists: taskOnly,
		})
	}

	return router.Build(router.Config{
		Bundle:      cfg.Bundle,
		Specialists: byName,
		Model:       cfg.Model,
	})
}

// BuildModel constructs the model.LLM for the given name. "echo"
// builds a fake in-process echo model (no credentials required);
// anything starting with "gemini-" builds a Vertex/Gemini model via
// ADK.
func BuildModel(ctx context.Context, name string) (model.LLM, error) {
	switch {
	case name == "echo":
		return mastagent.NewEchoModel("mast-echo"), nil
	case strings.HasPrefix(name, "gemini-"):
		return gemini.NewModel(ctx, name, &genai.ClientConfig{})
	default:
		return nil, fmt.Errorf("unknown model %q (want `echo` or a `gemini-*` model id)", name)
	}
}

// RatePer1K is the spike's flat pricing table: enough structure to
// prove cost derivation from UsageMetadata, not a real price list.
func RatePer1K(modelName string) float64 {
	switch {
	case modelName == "echo":
		return 0.05 // inflated so offline smoke tests can trip small caps
	case strings.HasPrefix(modelName, "gemini-"):
		return 0.0006
	default:
		return 0.001
	}
}
