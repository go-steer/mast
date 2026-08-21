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

package compose

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// CheckCapabilitySplit is W2.4: a specialist may only reach a mutating
// tool if it says so.
//
// The rule is one line — a roster's write surface must be declared,
// per specialist, in a field rather than in a prompt — and the reason
// is that the alternative was load-bearing until now. The shipped
// gke-triage diagnosers held `patch_resource` and were restrained by
// the sentence "Do NOT mutate anything on your own initiative", which
// is a suggestion to a language model, not a control. Declaring
// `capability: change_executor` is not an approval either: every
// mutating call still goes to the write gate. What it buys is that
// adding a write tool to a diagnoser now fails the roster at startup,
// naming the specialist and the tool, instead of quietly widening what
// an incident can do to a cluster.
//
// Three cases count as reaching a mutating tool, and only the first is
// the obvious one (this mirrors pkg/graph's fan-out branch check, which
// found the other two):
//
//  1. the specialist names a tool the predicate does not classify
//     read-only;
//  2. it names an MCP server with no tools: list, which grants it every
//     tool on that server, present and future;
//  3. it declares no tools.mcp key at all while the workload declares a
//     tool catalog, which grants it the whole catalog.
//
// Under mast's default-deny-unknown predicate an un-enumerated grant is
// a grant of mutating tools whether or not any exist today, so cases 2
// and 3 are refusals rather than warnings. The cost is real — a roster
// has to classify its read tools by name in tool_catalog.tools — and it
// is the intended cost: the alternative is trusting a tool's name.
//
// Case 3 turns on presence, not length: `mcp: []` is the documented
// deny-all spelling and passes, because a specialist that reaches no
// MCP tool at all reaches no mutating one. That is the spelling for a
// pure-reasoning specialist — a synthesis node, a summarizer — in a
// workload that does have a catalog.
//
// SingleTurn specialists are exempt. They are built without toolsets
// (see BuildRoot), so a classifier cannot reach a tool of any class,
// and requiring it to enumerate an allowlist it will never use would be
// ceremony.
//
// The boundary worth knowing: this checks *declarations*. A library
// embed that passes Toolsets directly and composes its own Specs can
// hand a read-only specialist a mutating tool without saying so, and
// nothing here will see it — enumerating a live toolset means
// connecting to every MCP server at construction. The write gate is the
// runtime backstop for that path: an undeclared mutating call still
// parks.
func CheckCapabilitySplit(b workload.Bundle, specs []specialists.Spec, pred effects.Predicate, logger *slog.Logger) error {
	hasCatalog := len(b.ToolCatalog.MCP) > 0
	for _, s := range specs {
		if s.Mode == specialists.ModeSingleTurn {
			continue
		}
		if s.Capability == specialists.CapabilityChangeExecutor {
			// Not a refusal, but the one thing an operator reading a
			// startup log should be able to find: which specialists in
			// this roster can change the world, and with what.
			if logger != nil {
				logger.Info("specialist declares write capability",
					"specialist", s.Name,
					"tools", strings.Join(declaredMutating(s, pred), ","))
			}
			continue
		}
		if hasCatalog && s.Tools.InheritsAllMCP() {
			return fmt.Errorf("compose: specialist %q declares no tools.mcp allowlist, which grants it the workload's whole tool catalog including any mutating tool in it; enumerate the read-only tools it needs, write `mcp: []` if it needs none, or declare capability: change_executor if it is meant to change things", s.Name)
		}
		for _, al := range s.Tools.MCP {
			if len(al.Tools) == 0 {
				return fmt.Errorf("compose: specialist %q allows MCP server %q with no tools: list, which grants it every tool on that server; enumerate the read-only tools it needs, or declare capability: change_executor", s.Name, al.Server)
			}
			if bad := mutatingNames(al.Tools, pred); len(bad) > 0 {
				return fmt.Errorf("compose: specialist %q allows mutating tool(s) %s on MCP server %q but is read_only; a specialist that diagnoses must not be able to remediate on its own initiative. Either drop them, classify them read-only in the workload's tool_catalog.tools, or declare capability: change_executor", s.Name, strings.Join(bad, ", "), al.Server)
			}
		}
		if bad := mutatingNames(s.Tools.Builtin, pred); len(bad) > 0 {
			return fmt.Errorf("compose: specialist %q allows mutating built-in tool(s) %s but is read_only; drop them or declare capability: change_executor", s.Name, strings.Join(bad, ", "))
		}
	}
	return nil
}

// declaredMutating is the write surface a change executor declares, for
// the startup log. Un-enumerated grants are reported as such rather
// than expanded: what the roster says is what an operator can check.
func declaredMutating(s specialists.Spec, pred effects.Predicate) []string {
	var out []string
	out = append(out, mutatingNames(s.Tools.Builtin, pred)...)
	for _, al := range s.Tools.MCP {
		if len(al.Tools) == 0 {
			out = append(out, al.Server+"/*")
			continue
		}
		out = append(out, mutatingNames(al.Tools, pred)...)
	}
	if s.Tools.InheritsAllMCP() && len(s.Tools.Builtin) == 0 {
		out = append(out, "(whole tool catalog)")
	}
	return out
}

// mutatingNames returns the sorted subset of names the predicate does
// not classify read-only. Spawning counts: a sub-run started from
// inside a specialist takes its own tool calls with it, out of sight.
func mutatingNames(names []string, pred effects.Predicate) []string {
	var bad []string
	for _, n := range names {
		if pred(n) != effects.ClassReadOnly {
			bad = append(bad, n)
		}
	}
	sort.Strings(bad)
	return bad
}

// CheckPlannerWriteSurface refuses a planner roster that holds a change
// executor while the bundle asks for the mutation to be gated (#235).
//
// # The hole
//
// The write gate and the effect outbox are runner plugins, and
// `invoke_specialist` runs its specialist on a runner it constructs
// itself — with none. So a mutating call made inside a planner dispatch
// does not park, does not dry-run, and leaves no durable intent record,
// on a bundle whose `hitl.on_mutation` asked for all three. Every other
// dispatch shape runs on the outer runner and is gated normally;
// `examples/workloads/gke-triage` is two commented lines away from the
// combination, which is how close a supported configuration sits to it.
//
// Measured rather than reasoned about: an outer BeforeToolCallback over
// this shape sees invoke_specialist and finish_task, and never the
// mutating call the specialist executed between them.
//
// # Why refuse rather than warn
//
// Same argument as CheckNameCollisions one file over, and the same
// argument the read/write split makes: a fail-open hole in the write
// path is refused at startup, not run carefully. A warning would be a
// line in a log nobody is tailing on a deployment nobody is watching,
// which is the posture this whole product is built against — and the
// operator who wrote `require_approval` would go on believing they had
// it. The refusal is also cheap to escape: the same roster runs under
// `coordinator` or `graph`, where the gate reaches it.
//
// # Why on_mutation: apply is exempt
//
// The refusal is scoped to the promise that is actually broken. Under
// `apply` the gate was never going to stop the call, so nothing about
// the dispatch changes what executes. What is still missing there is
// the outbox record, which costs exactly-once replay after an
// interrupted dispatch — a real gap, named at the call site in
// pkg/planner/dispatch.go and in the write-gate reference page, and not
// one worth refusing a startup over.
//
// This is containment, not the fix. #235 carries the three candidate
// boundaries; whichever lands, this check comes out with it.
func CheckPlannerWriteSurface(b workload.Bundle, specs []specialists.Spec) error {
	if !b.Planner.Enabled {
		return nil
	}
	policy := b.HITL.EffectiveOnMutation()
	if policy == workload.OnMutationApply {
		return nil
	}
	var executors []string
	for _, s := range specs {
		if s.Capability == specialists.CapabilityChangeExecutor {
			executors = append(executors, s.Name)
		}
	}
	if len(executors) == 0 {
		return nil
	}
	sort.Strings(executors)
	return fmt.Errorf("compose: workload %q enables the planner and declares hitl.on_mutation: %s, but its roster holds change executor(s) %s: the write gate and the effect outbox are runner plugins and invoke_specialist runs each specialist on a runner of its own, so a mutating call made inside a dispatch would neither park nor dry-run and would leave no durable record of what it did (go-steer/mast#235). Run this roster under dispatch: coordinator or graph, where the gate reaches it; or set hitl.on_mutation: apply if these writes are genuinely meant to fire unattended and unrecorded",
		b.Name, policy, strings.Join(executors, ", "))
}
