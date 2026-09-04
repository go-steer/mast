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

package specialists

import (
	"log/slog"
	"sort"
	"strings"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
)

// reportingToolset is a filtering toolset that says what its filter did
// not find (#278).
//
// tool.AllowedToolsPredicate answers yes or no per offered tool, so a
// name in the allowlist that the server does not serve is simply never
// matched: no error, no warning, no count. That is how eleven names —
// `list_datasets` for `list_dataset_ids`, `query` for
// `execute_sql_readonly`, `list_events` for `list_group_stats`, and
// five networking tools on a server with no networking tools at all —
// sat in a deployed agent's allowlists reading like granted
// capabilities. It took a `tools/list` probe against every server to
// find them, which is not a thing an operator should have to write.
//
// The check has to happen here rather than at startup because it needs
// that `tools/list`. mast's toolsets are lazy on purpose: a bundle
// should load when a server is down, and validating every allowlist
// name at startup would mean connecting to all of them before the first
// incident. So the diff is taken the first time the toolset actually
// lists, and reported once per specialist per server — a warning, not a
// refusal, because by then a turn is in flight and killing it over a
// name that grants nothing would be the larger harm.
//
// The server half of the same allowlist is refused at startup, where it
// costs nothing: internal/compose.CheckMCPServerNames.
type reportingToolset struct {
	inner tool.Toolset

	specialist string
	allowed    []string
	logger     *slog.Logger

	once sync.Once
}

// Name is the server key the allowlist matched on, unchanged: pkg/mcp's
// digest wrap and pkg/graph's fan-out check both match toolsets by it.
func (r *reportingToolset) Name() string { return r.inner.Name() }

// Tools lists the inner toolset, reports whatever the allowlist named
// and the server did not offer, and returns the allowed subset.
//
// The filtering is done here rather than by wrapping tool.FilterToolset
// because both need the same listing, and doing it once means a
// specialist's first model call still costs one `tools/list`.
func (r *reportingToolset) Tools(ctx adkagent.ReadonlyContext) ([]tool.Tool, error) {
	offered, err := r.inner.Tools(ctx)
	if err != nil {
		// No listing, nothing to diff against. The error is the
		// caller's to report; a warning here would blame the allowlist
		// for a server that is down.
		return nil, err
	}
	allow := make(map[string]bool, len(r.allowed))
	for _, n := range r.allowed {
		allow[n] = true
	}
	var kept []tool.Tool
	served := make(map[string]bool, len(offered))
	for _, t := range offered {
		served[t.Name()] = true
		if allow[t.Name()] {
			kept = append(kept, t)
		}
	}
	r.once.Do(func() { r.report(served) })
	return kept, nil
}

// report logs the allowlist entries the server did not offer. Once,
// because a toolset lists on every model call and a diagnoser that runs
// twelve turns would otherwise print the same line twelve times.
func (r *reportingToolset) report(served map[string]bool) {
	if r.logger == nil {
		return
	}
	var missing []string
	for _, n := range r.allowed {
		if !served[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	// WARN rather than ERROR: the specialist still runs, with fewer
	// tools than its file claims. WARN rather than INFO: what it lost
	// is a capability its own prompt may be counting on.
	r.logger.Warn("ALLOWLIST NAMES A TOOL THE SERVER DOES NOT SERVE — the specialist holds fewer tools than its file says",
		"specialist", r.specialist,
		"mcp_server", r.inner.Name(),
		"unmatched", strings.Join(missing, ","),
		"granted", len(r.allowed)-len(missing),
		"note", "a name that matches nothing is dropped in silence; check it against the server's tools/list")
}

// narrow applies an allowlist's tools: list to one toolset, reporting
// what it does not find. It replaces a bare tool.FilterToolset +
// tool.AllowedToolsPredicate, which filters identically and reports
// nothing.
func narrow(spec Spec, ts tool.Toolset, allowed []string, logger *slog.Logger) tool.Toolset {
	return &reportingToolset{
		inner:      ts,
		specialist: spec.Name,
		allowed:    allowed,
		logger:     logger,
	}
}
