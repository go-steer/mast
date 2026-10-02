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

// CheckMCPServerNames refuses a roster whose specialist names an MCP
// server the workload does not have (#278).
//
// The allowlist is applied by dropping what does not match
// (pkg/specialists.filterToolsets iterates the *offered* toolsets and
// looks each one up in the specialist's map), so an entry that matches
// no toolset is never consulted and never reported. `server: loggging`
// reads like a granted capability and grants nothing. The specialist
// starts clean, runs, and cannot do the thing its own prompt tells it
// to do — the first symptom being a model behaving oddly, at incident
// time, three tool calls into a diagnosis.
//
// This is the same class of mistake as a `model:` whose credentials do
// not resolve, a `tier:` the provider cannot answer, or a malformed
// `output_schema`, all of which already fail the roster at startup with
// the name in the message. It was the one that passed.
//
// # Why only the server half is refused here
//
// The set of servers a specialist may name is `tool_catalog.mcp`,
// already loaded and already validated against mcp.json
// (internal/cli/main.go). Checking a name against it costs nothing and
// reaches no network. The *tool* half of the same allowlist cannot be
// checked here: it needs a live `tools/list`, which is why mast's
// toolsets are lazy — connecting to every server at startup to validate
// a name would make a bundle un-loadable whenever a server is down.
// That half is reported as a warning the first time a toolset lists,
// by pkg/specialists (see allowlist.go).
//
// # Why a bundle with no catalog is exempt
//
// `tool_catalog.mcp` is the authoritative list of what a workload
// wired. A bundle that declares none has not said what exists, so
// nothing here can tell a typo from a server this deployment does not
// happen to carry. A library embed that hands compose its own toolsets
// is in the same position. Both fall through to the runtime warning.
//
// # Toolsets the binary supplies
//
// hostToolsets names the toolsets a custom main.go hands the daemon
// (RootConfig.HostToolsets). An allowlist reaches one by naming it in
// tools.mcp, exactly as it names a server, so the declared set is the
// catalog plus these — and a workload whose only tools come from the
// host is no longer a workload that declared nothing.
func CheckMCPServerNames(b workload.Bundle, specs []specialists.Spec, hostToolsets ...string) error {
	if len(b.ToolCatalog.MCP) == 0 && len(hostToolsets) == 0 {
		return nil
	}
	declared := make(map[string]bool, len(b.ToolCatalog.MCP)+len(hostToolsets))
	for _, ref := range b.ToolCatalog.MCP {
		declared[ref.Server] = true
	}
	for _, n := range hostToolsets {
		declared[n] = true
	}
	for _, s := range specs {
		var unknown []string
		for _, al := range s.Tools.MCP {
			if !declared[al.Server] {
				unknown = append(unknown, al.Server)
			}
		}
		if len(unknown) == 0 {
			continue
		}
		sort.Strings(unknown)
		declares := quoteAll(catalogNames(b))
		if len(hostToolsets) > 0 {
			hosts := append([]string(nil), hostToolsets...)
			sort.Strings(hosts)
			declares += ", and this binary supplies toolsets " + quoteAll(hosts)
		}
		return fmt.Errorf("compose: specialist %q allows MCP %s %s, which the workload's tool_catalog.mcp does not declare (it declares %s); the allowlist is applied by dropping what does not match, so this grants the specialist nothing from %s and would not have been reported at run time. Fix the name, or add the server to tool_catalog.mcp",
			s.Name,
			plural(len(unknown), "server"),
			quoteAll(unknown),
			declares,
			plural(len(unknown), "it"))
	}
	return nil
}

// catalogNames is the workload's declared server list, sorted, for the
// "it declares" half of the message: an operator who mistyped a name
// should not have to open the bundle to find the right one.
func catalogNames(b workload.Bundle) []string {
	out := make([]string, 0, len(b.ToolCatalog.MCP))
	for _, ref := range b.ToolCatalog.MCP {
		out = append(out, ref.Server)
	}
	sort.Strings(out)
	return out
}

func quoteAll(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	switch word {
	case "it":
		return "them"
	default:
		return word + "s"
	}
}

// CheckHostToolsets refuses toolset names a custom main.go supplies that
// could not be told apart in an allowlist: an empty name, a name two of
// them share, or a name the workload's tool_catalog.mcp already uses for
// an MCP server. Each would make "which tools does this specialist reach"
// depend on wiring order rather than on what the bundle says.
func CheckHostToolsets(b workload.Bundle, hostToolsets []string) error {
	seen := make(map[string]bool, len(hostToolsets))
	servers := make(map[string]bool, len(b.ToolCatalog.MCP))
	for _, ref := range b.ToolCatalog.MCP {
		servers[ref.Server] = true
	}
	for _, n := range hostToolsets {
		switch {
		case n == "":
			return fmt.Errorf("compose: a toolset supplied by this binary has an empty Name(); allowlists reach a toolset by name, so an unnamed one could never be granted")
		case seen[n]:
			return fmt.Errorf("compose: two toolsets supplied by this binary are both named %q; an allowlist naming it could not say which it means", n)
		case servers[n]:
			return fmt.Errorf("compose: a toolset supplied by this binary is named %q, which tool_catalog.mcp already declares as an MCP server; rename one — an allowlist naming it would reach both", n)
		}
		seen[n] = true
	}
	return nil
}
