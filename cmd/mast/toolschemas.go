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
	adkagent "google.golang.org/adk/v2/agent"
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

// lookup is the schema resolver handed to compose.WriteGate: it
// answers both "does this daemon hold a tool by that name" and "what
// arguments does it take".
func (ts *toolSchemas) lookup(name string) (*jsonschema.Schema, error) {
	t, err := ts.resolve(name)
	if err != nil {
		return nil, err
	}
	return approval.InputSchema(t)
}

// resolve finds a wired tool by name.
//
// A miss forces one refresh before it is reported as a miss: the cache
// can be older than a server that has just come up, and the cost of
// being wrong is refusing a legitimate remediation during an incident.
// A second miss is an answer — this daemon holds no tool by that name.
func (ts *toolSchemas) resolve(name string) (tool.Tool, error) {
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
	return t, nil
}

// runOwnBehalf runs a wired tool that no model asked for and returns
// its result.
//
// There are exactly THREE callers, and the count is the point — this is
// mast's whole surface for calling a tool outside the model's dispatch
// path, and each caller is a named exception with its own fence:
//
//   - read (v0.4 W7): a change-set freshness precondition. Fenced by
//     CLASSIFICATION — the write gate only ever passes a tool the
//     bundle declared as a precondition read, and internal/compose
//     refuses to start if that tool is classified mutating. The
//     exception can only widen towards safer calls.
//   - collect (v0.5 W4.2): a monitoring cycle's collection and
//     state-advance legs. Fenced by REACHABILITY, because
//     classification cannot fence it — the whole reason the call is
//     mast's is that it is mutating and would otherwise park the cycle
//     for a human on every fire. compose.CheckMonitorCollectSurface
//     refuses to start if a collect tool is reachable from any roster.
//   - ack (v0.5 W4.6): an operator's acknowledgement, forwarded to
//     whoever owns the finding state. Fenced by REACHABILITY through the
//     same check, and additionally by ARRIVAL: the only thing that calls
//     it is the daemon's authenticated /monitor-ack route, so every ack
//     carries an identity resolved from a credential. This is the one
//     exception whose argument mast overwrites rather than passes
//     through — subject_key and ack_by go on last, over the bundle's
//     literals, because an operator identity a caller or a YAML file
//     could set is not an identity.
//
// A fourth caller needs its own fence and its own paragraph here, not a
// fourth call site.
//
// Two properties are shared and neither is optional.
//
// ADK's runnable-tool interface is unexported (tool.Tool is name and
// description only), so the handle is asserted rather than imported; a
// tool that does not satisfy it is reported as unrunnable rather than
// treated as an empty answer. Silence and "nothing changed" are the two
// results this seam must never confuse, in both directions: a
// precondition that reads as unmoved approves a stale change set, and a
// collection that reads as empty is a monitor that has stopped
// monitoring.
//
// The handle is unwrapped first. The wired toolsets carry pkg/mcp's
// digesting wrap, which exists to shrink what a *model* reads. Neither
// caller is a model: the precondition read goes into a digest and a
// field comparison, and the collection result goes into a transition
// classification. A digest envelope on either is pure loss — it drops
// the very fields the comparison is made of and stamps a fresh call id
// on every call, which reads as "the cluster moved" forever after.
func (ts *toolSchemas) runOwnBehalf(ctx adkagent.Context, name string, args map[string]any, unrunnable string) (map[string]any, error) {
	t, err := ts.resolve(name)
	if err != nil {
		return nil, err
	}
	t = unwrapTool(t)
	runner, ok := t.(interface {
		Run(ctx adkagent.Context, args any) (map[string]any, error)
	})
	if !ok {
		return nil, fmt.Errorf("tool %q cannot be run by mast directly, so it cannot %s", name, unrunnable)
	}
	if args == nil {
		args = map[string]any{}
	}
	return runner.Run(ctx, args)
}

// read runs a read-only tool on mast's own behalf, for change-set
// freshness preconditions (v0.4 W7).
//
// The result never reaches the transcript — the model is not told what
// mast checked, because the check is about the operator's approval and
// not about the agent's reasoning. See runOwnBehalf for the fence.
func (ts *toolSchemas) read(ctx adkagent.Context, name string, args map[string]any) (map[string]any, error) {
	result, err := ts.runOwnBehalf(ctx, name, args, "serve as a precondition read")
	if err != nil {
		return nil, fmt.Errorf("precondition read %s: %w", name, err)
	}
	return result, nil
}

// collect runs one of a monitoring cycle's declared collection calls on
// mast's own behalf (v0.5 W4.2).
//
// Unlike read, the result IS handed to the model — as the turn's input,
// already gathered. That is the whole trade: the model reasons over the
// transitions and never holds the tool that produced them, so the
// collection leg costs zero model calls by construction rather than by
// measurement. See runOwnBehalf for the fence.
func (ts *toolSchemas) collect(ctx adkagent.Context, name string, args map[string]any) (map[string]any, error) {
	result, err := ts.runOwnBehalf(ctx, name, args, "serve as a monitor.collect call")
	if err != nil {
		return nil, fmt.Errorf("monitor collection %s: %w", name, err)
	}
	return result, nil
}

// ack forwards one operator acknowledgement to the tool the bundle's
// monitor.ack names, on mast's own behalf (v0.5 W4.6).
//
// Unlike both others, this one writes — and to a store mast does not
// own. The result is discarded: the producer's answer is bookkeeping
// about its own suppression state, mast has already recorded the half it
// is the store of record for, and handing a mute-button receipt to
// either the model or the operator's chat would be noise. What matters
// is whether it errored. See runOwnBehalf for the fence.
func (ts *toolSchemas) ack(ctx adkagent.Context, name string, args map[string]any) (map[string]any, error) {
	result, err := ts.runOwnBehalf(ctx, name, args, "forward an operator acknowledgement")
	if err != nil {
		return nil, fmt.Errorf("monitor ack %s: %w", name, err)
	}
	return result, nil
}

// unwrapTool peels the wrappers a caller outside the model's dispatch
// path should see through — today that is pkg/mcp's digesting wrap.
// Bounded rather than unbounded because an Unwrap that returns its own
// receiver would otherwise spin; no real chain is more than one deep.
func unwrapTool(t tool.Tool) tool.Tool {
	for i := 0; i < 8; i++ {
		u, ok := t.(interface{ Unwrap() tool.Tool })
		if !ok {
			return t
		}
		inner := u.Unwrap()
		if inner == nil {
			return t
		}
		t = inner
	}
	return t
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
