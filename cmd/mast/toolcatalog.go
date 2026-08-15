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
	"log/slog"
	"sort"
	"sync"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/workload"
)

// Defaults for the catalog's two time bounds. The TTL is short enough
// that an operator who restarts an MCP server sees the new catalog
// within a page refresh or two, long enough that live-tailing a busy
// session doesn't re-list every server on every poll. The timeout
// bounds one refresh, not one server: a stdio server wedged on
// tools/list must not hold a /tools request open indefinitely.
const (
	toolCatalogTTL     = 30 * time.Second
	toolCatalogTimeout = 5 * time.Second
)

// toolCatalog answers GET /sessions/{sid}/tools from the MCP toolsets
// the daemon wired at composition time (#133).
//
// The wiring site is the source because ADK exposes no accessor for a
// built agent's tools — llmagent keeps them behind an internal Reveal
// — so by the time an adapter holds an agent.Agent, the attribution
// this endpoint exists to report is already gone. cmd/mast keeps the
// toolsets on rootBuild for exactly this reason, which is also why
// this type lives in package main rather than in pkg/attachadapter.
//
// Scope, stated plainly: MCP tools only. mast's non-MCP tools are the
// planner's control-plane five, and they are registered inside
// internal/compose with no handle surviving the call. Reporting them
// would mean either a second hand-maintained list (which drifts, and a
// wrong catalog is worse than a partial one — see the tool_catalog
// finding in v0.3) or a compose signature change; both are follow-up
// work, tracked separately. A daemon running planner dispatch with no
// MCP servers therefore reports an empty catalog, correctly: it holds
// no MCP tools.
type toolCatalog struct {
	toolsets   []tool.Toolset
	mutating   effects.Predicate
	onMutation workload.OnMutation
	logger     *slog.Logger

	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time // nil means time.Now; a seam for tests

	mu       sync.Mutex
	cached   []attach.ToolInfo
	cachedAt time.Time
}

// newToolCatalog builds the catalog for a wired daemon. A nil bundle
// (no workload) resolves on_mutation the same way the write gate does.
func newToolCatalog(logger *slog.Logger, toolsets []tool.Toolset, pred effects.Predicate, bundle *workload.Bundle) *toolCatalog {
	onMutation := workload.OnMutationRequireApproval
	if bundle != nil {
		onMutation = bundle.HITL.EffectiveOnMutation()
	}
	return &toolCatalog{
		toolsets:   toolsets,
		mutating:   pred,
		onMutation: onMutation,
		logger:     logger,
		ttl:        toolCatalogTTL,
		timeout:    toolCatalogTimeout,
	}
}

func (tc *toolCatalog) clock() time.Time {
	if tc.now != nil {
		return tc.now()
	}
	return time.Now()
}

// snapshot lists every wired MCP tool, sorted by (server, name) so
// polling clients see a stable order rather than map iteration noise.
//
// The lock is held across the tools/list calls. That serializes
// concurrent /tools requests behind one refresh — which is the point:
// ten operators tailing one session should cost the MCP servers one
// listing, not ten. The wait is bounded by tc.timeout.
func (tc *toolCatalog) snapshot(ctx context.Context) []attach.ToolInfo {
	if tc == nil {
		return nil
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()

	if tc.cached != nil && tc.clock().Sub(tc.cachedAt) < tc.ttl {
		return append([]attach.ToolInfo(nil), tc.cached...)
	}

	ctx, cancel := context.WithTimeout(ctx, tc.timeout)
	defer cancel()

	out := make([]attach.ToolInfo, 0, len(tc.toolsets)*8)
	listed := 0
	for _, ts := range tc.toolsets {
		tools, err := ts.Tools(toolCatalogCtx{Context: ctx})
		if err != nil {
			// One unreachable server must not blank the catalog for
			// the servers that did answer; the operator learns which
			// one is down from the log rather than from a hole they
			// can't see. Counted so a total failure can decline to
			// cache — see below.
			tc.logger.Warn("attach: MCP server did not list its tools; omitting it from /tools",
				"server", ts.Name(), "error", err.Error())
			continue
		}
		listed++
		for _, t := range tools {
			out = append(out, attach.ToolInfo{
				Name:        t.Name(),
				Description: t.Description(),
				Source:      attach.ToolSourceMCP,
				Server:      ts.Name(),
				GateState:   tc.gateState(t.Name()),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Name < out[j].Name
	})

	// Don't cache a total wipeout. If every server failed — the shape
	// of a transport blip or a timeout — caching the empty result
	// would hold the lie for a full TTL past recovery, and would let
	// the next poll report "no tools" for a daemon that has them. Serve
	// the last good answer if there is one, and retry on the next call.
	if len(tc.toolsets) > 0 && listed == 0 {
		if tc.cached != nil {
			return append([]attach.ToolInfo(nil), tc.cached...)
		}
		return out
	}

	tc.cached, tc.cachedAt = out, tc.clock()
	return append([]attach.ToolInfo(nil), out...)
}

// gateState projects what the write gate would do to a call of this
// tool, without making one. The mapping is the wire contract in
// pkg/attach/state.go against pkg/workload's on_mutation values:
//
//	read-only (any policy)   → allowed
//	mutating + apply         → allowed
//	mutating + require_approval → prompted
//	mutating + dry_run       → denied
//
// Spawning tools classify as neither read-only nor mutating; the gate
// doesn't park them, so they report allowed. It is a projection, not a
// promise: the gate decides per call, and a session carrying dangling
// intents has its mutating calls refused by the outbox regardless.
func (tc *toolCatalog) gateState(name string) string {
	if tc.mutating == nil {
		return ""
	}
	if tc.mutating(name) != effects.ClassMutating {
		return attach.ToolGateAllowed
	}
	switch tc.onMutation {
	case workload.OnMutationApply:
		return attach.ToolGateAllowed
	case workload.OnMutationDryRun:
		return attach.ToolGateDenied
	default:
		return attach.ToolGatePrompted
	}
}

// toolCatalogCtx is a minimal agent.ReadonlyContext for listing tools
// outside a turn. mcptoolset.Tools uses only the embedded
// context.Context (to reach the transport), so the accessors return
// zero values — there is no invocation here to describe.
type toolCatalogCtx struct {
	context.Context
}

func (toolCatalogCtx) UserContent() *genai.Content          { return nil }
func (toolCatalogCtx) InvocationID() string                 { return "" }
func (toolCatalogCtx) AgentName() string                    { return "" }
func (toolCatalogCtx) ReadonlyState() session.ReadonlyState { return nil }
func (toolCatalogCtx) UserID() string                       { return "" }
func (toolCatalogCtx) AppName() string                      { return "" }
func (toolCatalogCtx) SessionID() string                    { return "" }
func (toolCatalogCtx) Branch() string                       { return "" }

var _ adkagent.ReadonlyContext = toolCatalogCtx{}
