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

package main

import (
	"github.com/go-steer/mast/internal/compose"
	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/graph"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// shapePlanner is the effective shape when the bundle enables the
// planner. It is not a --dispatch value — compose ignores dispatch
// entirely when planner.enabled is set — but the catalog has to name
// what it resolved, and "planner" is that name.
const shapePlanner = "planner"

// effectiveShape is the dispatch shape the root was actually built
// with: the planner when the bundle enables it (which overrides
// --dispatch), the roster-derived shape when the caller said `auto`
// or nothing, the resolved value otherwise.
//
// It re-derives rather than reads a field because it must answer for
// the same inputs compose branched on; a shape recorded separately is
// a shape that can drift from the one that got built.
func effectiveShape(bundle *workload.Bundle, dispatch string, specs []specialists.Spec) string {
	if bundle != nil && bundle.Planner.Enabled {
		return shapePlanner
	}
	if dispatch == "" || dispatch == workload.DispatchAuto {
		return string(compose.RosterShape(specs))
	}
	return dispatch
}

// subagentCatalog projects the loaded roster for
// GET /sessions/{sid}/subagents (#134).
//
// The interesting field is Invocation, and the reason it exists is
// that core-agent's `modes` cannot answer mast's question. Upstream,
// `modes` distinguishes "spawnable by reference" (async) from "carried
// as a parent tool" (sync), because upstream a subagent is one or both
// of those. In mast a specialist is neither by default: it is a
// transfer target, or a graph node, or one branch of a fan-out, and
// which one depends on the shape the daemon composed. So `modes`
// carries only what is true in core-agent's own vocabulary — ["sync"]
// under planner dispatch, where invoke_specialist really is a parent
// tool, and empty otherwise — and Invocation carries the rest.
//
// Emitting ["async"] to make the field look populated is exactly the
// upstream bug this endpoint was warned about (core-agent#741: the
// catalog claims sync anyway). An empty list is a true statement.
//
// A specialist the composed shape cannot reach gets an empty
// Invocation, deliberately. Fanout dispatch builds nothing for a
// `_fallback` spec, graph dispatch uses only the first SingleTurn spec
// as its classifier, and a roster can carry a member that no shape
// routes to — the change-executor orphan the first live GKE run found
// was exactly this. The operator should be able to see it in the
// roster listing rather than by reading compose.
func subagentCatalog(bundle *workload.Bundle, specs []specialists.Spec, dispatch string) []attach.SubagentCatalogInfo {
	shape := effectiveShape(bundle, dispatch, specs)

	// Under graph dispatch only the first SingleTurn spec becomes the
	// classifier; the rest are built and never routed to.
	classifier := ""
	for _, spec := range specs {
		if spec.Mode == specialists.ModeSingleTurn {
			classifier = spec.Name
			break
		}
	}

	out := make([]attach.SubagentCatalogInfo, 0, len(specs))
	for _, spec := range specs {
		modes := []string{}
		if shape == shapePlanner {
			modes = []string{attach.SubagentInvocationModeSync}
		}
		out = append(out, attach.SubagentCatalogInfo{
			Name:        spec.Name,
			Description: spec.Description,
			Model:       spec.Model,
			Root:        spec.Filename,
			Modes:       modes,
			Invocation:  invocationFor(shape, spec, classifier),
			Capability:  string(spec.Capability),
			AgentMode:   string(spec.Mode),
		})
	}
	return out
}

// invocationFor answers how shape reaches spec, or "" when it doesn't.
// The branches mirror internal/compose.BuildRoot's own, in its order.
func invocationFor(shape string, spec specialists.Spec, classifier string) string {
	task := spec.Mode == specialists.ModeTask || spec.Mode == ""
	switch shape {
	case shapePlanner:
		// Every roster member is an invoke_specialist target,
		// SingleTurn ones included.
		return attach.InvocationParentTool
	case workload.DispatchCoordinator:
		// router.Build takes the whole roster as SubAgents.
		return attach.InvocationTransfer
	case workload.DispatchGraph:
		if !task {
			if spec.Name == classifier {
				return attach.InvocationGraphNode
			}
			return "" // a second SingleTurn spec is built and never routed to
		}
		return attach.InvocationGraphNode
	case workload.DispatchFanout:
		switch {
		case !task:
			return "" // fanout has no classifier
		case spec.Name == graph.SynthesisName:
			// The merger, not one of the parallel branches — the
			// distinction an operator reading a fanout roster wants.
			return attach.InvocationGraphNode
		case spec.Name == graph.FallbackName:
			return "" // BuildFanout builds no fallback node
		default:
			return attach.InvocationFanoutBranch
		}
	default:
		return ""
	}
}
