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
	"sort"
	"strings"

	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// CheckMonitorCollectSurface keeps the collection leg out of the model's
// hands (v0.5 W4.2).
//
// # What the exception is
//
// A tool named in `monitor.collect` is run by mast on its own behalf, at
// the top of a scheduled cycle, before the model is woken. It is not
// gated, and it is not gated on purpose: a run-to-run finding diff
// declares itself mutating because it advances persisted state, and
// under the default `hitl.on_mutation: require_approval` a model that
// held it would park the cycle for a human every single fire.
//
// # Why the exception needs a fence
//
// Every other narrow exception in mast is bounded by the direction it
// runs in. The precondition read (v0.4 W7) is bounded by classification
// — internal/compose refuses to start if the declared read is mutating,
// so the exception can only ever widen towards *safer* calls. This one
// inverts that: it permits a mutating call precisely because it is
// mast's own. Classification cannot fence it, so reachability does.
//
// The rule is that a collect tool must be reachable by NOBODY else. A
// tool that mast runs ungated at the top of the cycle and that a
// specialist can also call mid-turn is the worst of both: the audited
// answer to "was this write approved?" becomes "it depends which door it
// came through", and W4.6's "there is no ack tool the model can call"
// stops being a structural fact and becomes a convention someone has to
// remember when they next edit a roster.
//
// # Three ways a roster reaches a tool, and all three are refused
//
// The first is the obvious one; the other two are the un-enumerated
// grants CheckCapabilitySplit found, and they matter more here because
// CheckCapabilitySplit *exempts* a declared change executor from them.
// A `capability: change_executor` specialist with `mcp: [{server: k8s}]`
// and no tools list holds every tool on that server — including the
// diff — and nothing before this check would say so.
//
//  1. the specialist names a collect tool outright, on an MCP server or
//     among its built-ins;
//  2. it names an MCP server with no `tools:` list, which grants it
//     every tool on that server, present and future;
//  3. it declares no `tools.mcp` key at all while the workload declares
//     a tool catalog, which grants it the whole catalog.
//
// Cases 2 and 3 are refused whether or not the collect tool happens to
// live on that server today, because mast cannot tell without connecting
// to it — and "refuse rather than guess" is the property this seam
// inherits from the read it generalizes.
//
// SingleTurn specialists are exempt, as they are in CheckCapabilitySplit
// and for the same reason: BuildRoot builds them without toolsets, so
// they cannot reach a tool of any class. That exemption is what lets the
// clearest demonstration of the whole idea exist — a `dispatch: bounded`
// monitoring workload whose model holds no tools at all and still gets
// the transitions.
//
// The boundary worth knowing is the one CheckCapabilitySplit names: this
// checks *declarations*. A library embed that composes its own Specs and
// passes Toolsets directly can hand a specialist a collect tool without
// saying so, and nothing here will see it. Unlike the write gate, there
// is no runtime backstop for that path, because the whole point of the
// collection leg is that it is ungated — an embed that wants the
// property has to declare the roster it is claiming.
func CheckMonitorCollectSurface(b workload.Bundle, specs []specialists.Spec) error {
	if !b.Monitor.Enabled() {
		return nil
	}
	collect := b.Monitor.CollectTools()
	if len(collect) == 0 {
		return nil
	}
	isCollect := make(map[string]bool, len(collect))
	for _, n := range collect {
		isCollect[n] = true
	}
	hasCatalog := len(b.ToolCatalog.MCP) > 0
	named := strings.Join(collect, ", ")

	for _, s := range specs {
		if s.Mode == specialists.ModeSingleTurn {
			continue
		}
		if hasCatalog && s.Tools.InheritsAllMCP() {
			return fmt.Errorf("compose: workload %q collects %s on its own behalf, but specialist %q declares no tools.mcp allowlist, which grants it the workload's whole tool catalog including those tools; a tool mast runs ungated at the top of a cycle must be reachable by nothing else. Enumerate the tools the specialist needs, or write `mcp: []` if it needs none",
				b.Name, named, s.Name)
		}
		for _, al := range s.Tools.MCP {
			if len(al.Tools) == 0 {
				return fmt.Errorf("compose: workload %q collects %s on its own behalf, but specialist %q allows MCP server %q with no tools: list, which grants it every tool on that server; mast cannot tell whether a collect tool is among them without connecting to it, so this is refused rather than guessed. Enumerate the tools the specialist needs",
					b.Name, named, s.Name, al.Server)
			}
			if bad := collectNames(al.Tools, isCollect); len(bad) > 0 {
				return fmt.Errorf("compose: workload %q collects %s on its own behalf, but specialist %q allows %s on MCP server %q; the collection leg is ungated precisely because no model holds it, and a tool reachable through both doors makes \"was this approved?\" depend on which one it came through. Drop it from the allowlist, or drop it from monitor.collect and let the model call it under the write gate",
					b.Name, named, s.Name, strings.Join(bad, ", "), al.Server)
			}
		}
		if bad := collectNames(s.Tools.Builtin, isCollect); len(bad) > 0 {
			return fmt.Errorf("compose: workload %q collects %s on its own behalf, but specialist %q allows built-in tool(s) %s; drop them from the allowlist, or drop them from monitor.collect",
				b.Name, named, s.Name, strings.Join(bad, ", "))
		}
	}
	return nil
}

// collectNames returns the sorted subset of names that the collection
// leg claims.
func collectNames(names []string, isCollect map[string]bool) []string {
	var bad []string
	for _, n := range names {
		if isCollect[n] {
			bad = append(bad, n)
		}
	}
	sort.Strings(bad)
	return bad
}
