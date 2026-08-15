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
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/mast/pkg/approval"
)

// How long a resolved schema stays good, and how long one refresh may
// take. A tool's arguments change when its MCP server is upgraded,
// which is rare; the TTL is long because re-listing every server on
// every finding would put a tools/list round trip in the path of every
// specialist's report. The timeout is the same bound the /tools catalog
// uses: one wedged stdio server must not hold a specialist's report
// open.
const (
	toolSchemaTTL     = 5 * time.Minute
	toolSchemaTimeout = 5 * time.Second
)

// toolSchemas resolves a wired tool's declared input schema by name, so
// the write gate can check a specialist's proposed change against the
// arguments the tool actually takes (v0.4 W7.0).
//
// Same source and same caveat as toolCatalog: the MCP toolsets
// buildRoot wired, because that is the only place a built agent's tools
// survive as handles. A change naming one of mast's non-MCP tools
// therefore does not resolve and is refused (#137) — which is the safe
// direction, since the non-MCP surface is the planner's control-plane
// five and nothing a diagnoser should be proposing as remediation.
type toolSchemas struct {
	toolsets []tool.Toolset
	logger   *slog.Logger

	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time // nil means time.Now; a seam for tests

	mu       sync.Mutex
	cached   map[string]tool.Tool
	cachedAt time.Time
}

func newToolSchemas(logger *slog.Logger, toolsets []tool.Toolset) *toolSchemas {
	return &toolSchemas{
		toolsets: toolsets,
		logger:   logger,
		ttl:      toolSchemaTTL,
		timeout:  toolSchemaTimeout,
	}
}

func (ts *toolSchemas) clock() time.Time {
	if ts.now != nil {
		return ts.now()
	}
	return time.Now()
}

// lookup is the resolver handed to compose.WriteGate.
//
// A miss forces one refresh before it is reported as a miss: the cache
// can be older than a server that has just come up, and the cost of
// being wrong is refusing a legitimate remediation during an incident.
// A second miss is an answer — this daemon holds no tool by that name.
func (ts *toolSchemas) lookup(name string) (*jsonschema.Schema, error) {
	if ts == nil {
		return nil, fmt.Errorf("this deployment has no tools wired, so tool %q cannot be called", name)
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()

	refreshed := false
	if ts.cached == nil || ts.clock().Sub(ts.cachedAt) >= ts.ttl {
		ts.refresh()
		refreshed = true
	}
	t, ok := ts.cached[name]
	if !ok && !refreshed {
		ts.refresh()
		t, ok = ts.cached[name]
	}
	if !ok {
		return nil, fmt.Errorf("no tool named %q is wired into this deployment; name one from the tool_catalog, or return an empty change set", name)
	}
	return approval.InputSchema(t)
}

// refresh re-lists every wired toolset. Called with ts.mu held, which
// serializes concurrent reports behind one listing.
func (ts *toolSchemas) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), ts.timeout)
	defer cancel()

	out := make(map[string]tool.Tool, len(ts.cached))
	listed := 0
	for _, s := range ts.toolsets {
		tools, err := s.Tools(toolCatalogCtx{Context: ctx})
		if err != nil {
			ts.logger.Warn("producer contract: MCP server did not list its tools; changes naming them will be refused",
				"server", s.Name(), "error", err.Error())
			continue
		}
		listed++
		for _, t := range tools {
			out[t.Name()] = t
		}
	}
	// Don't cache a total wipeout, for the same reason toolCatalog
	// doesn't: a transport blip would otherwise refuse every proposed
	// change for a full TTL past recovery. Keep the last good answer
	// and retry on the next report.
	if len(ts.toolsets) > 0 && listed == 0 && ts.cached != nil {
		return
	}
	ts.cached, ts.cachedAt = out, ts.clock()
}
