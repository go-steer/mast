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
