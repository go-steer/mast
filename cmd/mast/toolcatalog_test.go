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
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// catalogTool is the minimum tool.Tool the catalog reads.
type catalogTool struct {
	name string
	desc string
}

func (c catalogTool) Name() string        { return c.name }
func (c catalogTool) Description() string { return c.desc }
func (c catalogTool) IsLongRunning() bool { return false }

// catalogToolset is a tool.Toolset that can be made to fail, and that
// counts how many times it was listed (so caching is observable).
type catalogToolset struct {
	name  string
	tools []tool.Tool
	err   error
	calls int
}

func (c *catalogToolset) Name() string { return c.name }

func (c *catalogToolset) Tools(adkagent.ReadonlyContext) ([]tool.Tool, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return c.tools, nil
}

// mutatingNames is a predicate over a fixed set, standing in for the
// real effects.Predicate without pulling in the override machinery.
func mutatingNames(names ...string) effects.Predicate {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return func(name string) effects.Class {
		if set[name] {
			return effects.ClassMutating
		}
		return effects.ClassReadOnly
	}
}

func testCatalog(t *testing.T, onMutation workload.OnMutation, sets ...*catalogToolset) *toolCatalog {
	t.Helper()
	toolsets := make([]tool.Toolset, 0, len(sets))
	for _, s := range sets {
		toolsets = append(toolsets, s)
	}
	return &toolCatalog{
		toolsets:   toolsets,
		mutating:   mutatingNames("scale_deployment", "delete_pod"),
		onMutation: onMutation,
		logger:     discardLogger(),
		ttl:        toolCatalogTTL,
		timeout:    toolCatalogTimeout,
	}
}

// The endpoint's whole point is attribution: which server a tool came
// from, and what the write gate would do to it. core-agent's own
// implementation reports every MCP tool as source "other" with no
// server (their #767); mast reads the toolsets at the wiring site
// precisely so it doesn't have to.
func TestToolCatalogAttributesServerAndGateState(t *testing.T) {
	gke := &catalogToolset{name: "gke", tools: []tool.Tool{
		catalogTool{name: "scale_deployment", desc: "scale a deployment"},
		catalogTool{name: "get_pods", desc: "list pods"},
	}}
	slack := &catalogToolset{name: "slack", tools: []tool.Tool{
		catalogTool{name: "post_message", desc: "post to a channel"},
	}}

	got := testCatalog(t, workload.OnMutationRequireApproval, gke, slack).snapshot(context.Background())

	// Sorted by (server, name) — a polling client should not see the
	// list reshuffle between identical refreshes.
	want := []attach.ToolInfo{
		{Name: "get_pods", Description: "list pods", Source: attach.ToolSourceMCP, Server: "gke", GateState: attach.ToolGateAllowed},
		{Name: "scale_deployment", Description: "scale a deployment", Source: attach.ToolSourceMCP, Server: "gke", GateState: attach.ToolGatePrompted},
		{Name: "post_message", Description: "post to a channel", Source: attach.ToolSourceMCP, Server: "slack", GateState: attach.ToolGateAllowed},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// gate_state is a projection of the workload's write-gate policy, so
// the same tool has to read differently under a different on_mutation.
// A catalog that reported "prompted" for a dry-run daemon would tell
// an operator a call is available for approval when nothing will run.
func TestToolCatalogGateStateFollowsOnMutation(t *testing.T) {
	cases := []struct {
		policy       workload.OnMutation
		wantMutating string
	}{
		{workload.OnMutationRequireApproval, attach.ToolGatePrompted},
		{"", attach.ToolGatePrompted}, // empty resolves to the safe default
		{workload.OnMutationApply, attach.ToolGateAllowed},
		{workload.OnMutationDryRun, attach.ToolGateDenied},
	}
	for _, tc := range cases {
		t.Run(string(tc.policy), func(t *testing.T) {
			gke := &catalogToolset{name: "gke", tools: []tool.Tool{
				catalogTool{name: "delete_pod"},
				catalogTool{name: "get_pods"},
			}}
			got := testCatalog(t, tc.policy, gke).snapshot(context.Background())
			if len(got) != 2 {
				t.Fatalf("snapshot = %+v, want 2 tools", got)
			}
			if got[0].Name != "delete_pod" || got[0].GateState != tc.wantMutating {
				t.Errorf("delete_pod gate_state = %q, want %q", got[0].GateState, tc.wantMutating)
			}
			// A read-only tool is allowed under every policy: the write
			// gate is the only thing being projected here.
			if got[1].GateState != attach.ToolGateAllowed {
				t.Errorf("get_pods gate_state = %q, want %q under %q", got[1].GateState, attach.ToolGateAllowed, tc.policy)
			}
		})
	}
}

// No predicate wired (a library-shaped daemon with no effect policy)
// means no claim: the field stays empty rather than guessing
// "allowed", which is the value that would let a client render a write
// tool as safe.
func TestToolCatalogOmitsGateStateWithoutPredicate(t *testing.T) {
	tc := testCatalog(t, workload.OnMutationRequireApproval,
		&catalogToolset{name: "gke", tools: []tool.Tool{catalogTool{name: "delete_pod"}}})
	tc.mutating = nil

	got := tc.snapshot(context.Background())
	if len(got) != 1 || got[0].GateState != "" {
		t.Errorf("snapshot = %+v, want one entry with an empty gate_state", got)
	}
}

// Every /tools request is a round trip to every MCP server, and an
// operator live-tailing a session polls. Within the TTL the answer
// comes from memory.
func TestToolCatalogCachesWithinTTL(t *testing.T) {
	gke := &catalogToolset{name: "gke", tools: []tool.Tool{catalogTool{name: "get_pods"}}}
	tc := testCatalog(t, workload.OnMutationApply, gke)

	now := time.Unix(1_700_000_000, 0)
	tc.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if got := tc.snapshot(context.Background()); len(got) != 1 {
			t.Fatalf("call %d: snapshot = %+v, want 1 tool", i, got)
		}
	}
	if gke.calls != 1 {
		t.Errorf("listed the server %d times within the TTL, want 1", gke.calls)
	}

	// Past the TTL the catalog re-reads, so a server that gained a tool
	// (or went away) shows up without a daemon restart.
	now = now.Add(tc.ttl + time.Second)
	gke.tools = append(gke.tools, catalogTool{name: "get_events"})
	if got := tc.snapshot(context.Background()); len(got) != 2 {
		t.Errorf("after the TTL: snapshot = %+v, want the refreshed 2-tool list", got)
	}
	if gke.calls != 2 {
		t.Errorf("listed the server %d times across the TTL boundary, want 2", gke.calls)
	}
}

// One wedged server must not blank the catalog for the ones that
// answered — the failure mode this endpoint exists to end is a read
// that looks like an answer.
func TestToolCatalogPartialFailureKeepsHealthyServers(t *testing.T) {
	broken := &catalogToolset{name: "broken", err: errors.New("transport closed")}
	gke := &catalogToolset{name: "gke", tools: []tool.Tool{catalogTool{name: "get_pods"}}}

	got := testCatalog(t, workload.OnMutationApply, broken, gke).snapshot(context.Background())
	if len(got) != 1 || got[0].Server != "gke" {
		t.Errorf("snapshot = %+v, want only gke's tool", got)
	}
}

// A blip that takes down every server must not be cached: caching it
// would hold "this daemon has no tools" for a full TTL after recovery,
// and the last good answer is closer to true than the empty one.
func TestToolCatalogDoesNotCacheATotalFailure(t *testing.T) {
	gke := &catalogToolset{name: "gke", tools: []tool.Tool{catalogTool{name: "get_pods"}}}
	tc := testCatalog(t, workload.OnMutationApply, gke)

	now := time.Unix(1_700_000_000, 0)
	tc.now = func() time.Time { return now }
	if got := tc.snapshot(context.Background()); len(got) != 1 {
		t.Fatalf("priming call: snapshot = %+v, want 1 tool", got)
	}

	now = now.Add(tc.ttl + time.Second)
	gke.err = errors.New("transport closed")
	if got := tc.snapshot(context.Background()); len(got) != 1 || got[0].Name != "get_pods" {
		t.Errorf("during the outage: snapshot = %+v, want the last good answer", got)
	}

	// And the next call retries rather than serving a cached outage.
	before := gke.calls
	gke.err = nil
	if got := tc.snapshot(context.Background()); len(got) != 1 {
		t.Errorf("after recovery: snapshot = %+v, want 1 tool", got)
	}
	if gke.calls != before+1 {
		t.Errorf("server listed %d times after the outage, want a fresh attempt", gke.calls-before)
	}
}

// A daemon with no MCP servers and no builtins reports an empty catalog
// rather than panicking on a nil receiver — and it does so without a
// refresh loop.
func TestToolCatalogEmptyIsStable(t *testing.T) {
	if got := (*toolCatalog)(nil).snapshot(context.Background()); got != nil {
		t.Errorf("nil catalog snapshot = %+v, want nil", got)
	}
	tc := testCatalog(t, workload.OnMutationApply)
	if got := tc.snapshot(context.Background()); len(got) != 0 {
		t.Errorf("snapshot = %+v, want empty", got)
	}
}

// #137: a planner-dispatch daemon with no MCP servers used to answer
// with an empty catalog. That was true of its MCP tools and wrong as an
// answer to "what can this daemon do" — the planner's control plane is
// the whole dispatch surface such a daemon has.
func TestToolCatalogListsTheDaemonsOwnTools(t *testing.T) {
	tc := testCatalog(t, workload.OnMutationRequireApproval)
	tc.builtin = []tool.Tool{
		catalogTool{name: "invoke_specialist", desc: "dispatch to a roster specialist"},
		catalogTool{name: "request_operator_input", desc: "ask an operator"},
	}

	got := tc.snapshot(context.Background())
	want := []attach.ToolInfo{
		{Name: "invoke_specialist", Description: "dispatch to a roster specialist", Source: attach.ToolSourceBuiltin, GateState: attach.ToolGateAllowed},
		{Name: "request_operator_input", Description: "ask an operator", Source: attach.ToolSourceBuiltin, GateState: attach.ToolGateAllowed},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// Builtins carry no server, so they group ahead of every MCP tool. The
// order matters only in that it is stable — a polling client must not
// see the list reshuffle — but reading the daemon's own control plane
// first and then each server's catalog is also the order an operator
// wants.
func TestToolCatalogSortsBuiltinsAheadOfServers(t *testing.T) {
	tc := testCatalog(t, workload.OnMutationApply,
		&catalogToolset{name: "aaa-sorts-first", tools: []tool.Tool{catalogTool{name: "get_pods"}}})
	tc.builtin = []tool.Tool{catalogTool{name: "invoke_specialist"}}

	got := tc.snapshot(context.Background())
	if len(got) != 2 || got[0].Name != "invoke_specialist" || got[1].Name != "get_pods" {
		t.Fatalf("snapshot = %+v, want the builtin first even though its server name would sort last", got)
	}
	if got[0].Server != "" {
		t.Errorf("builtin reported server %q; a tool mast wired itself came from no server", got[0].Server)
	}
}

// The gate projection is the same predicate for both sources. A builtin
// that mutates must not read as allowed just because mast registered it
// — and the planner's spawning tools, which are neither read-only nor
// mutating, must not read as prompted.
func TestToolCatalogGatesBuiltinsLikeAnythingElse(t *testing.T) {
	tc := testCatalog(t, workload.OnMutationRequireApproval)
	tc.builtin = []tool.Tool{
		catalogTool{name: "invoke_specialist"},
		catalogTool{name: "scale_deployment"}, // mutatingNames covers this one
	}

	got := tc.snapshot(context.Background())
	byName := map[string]string{}
	for _, ti := range got {
		byName[ti.Name] = ti.GateState
	}
	if byName["invoke_specialist"] != attach.ToolGateAllowed {
		t.Errorf("invoke_specialist gate = %q, want %q", byName["invoke_specialist"], attach.ToolGateAllowed)
	}
	if byName["scale_deployment"] != attach.ToolGatePrompted {
		t.Errorf("a mutating builtin gate = %q, want %q", byName["scale_deployment"], attach.ToolGatePrompted)
	}
}

// Both halves have to survive the trip from buildRoot to the endpoint.
// #133 was not a broken projection but an argument serve never passed,
// and #137 adds a second one that could go the same way — silently,
// because a catalog missing its builtins still answers 200 with a
// plausible list.
func TestRootBuildCatalogCarriesBothSources(t *testing.T) {
	b := rootBuild{
		toolsets: []tool.Toolset{&catalogToolset{name: "gke", tools: []tool.Tool{catalogTool{name: "get_pods"}}}},
		builtin:  []tool.Tool{catalogTool{name: "invoke_specialist"}},
		bundle:   &workload.Bundle{},
	}

	got := b.catalog(discardLogger(), mutatingNames()).snapshot(context.Background())
	names := make([]string, 0, len(got))
	for _, ti := range got {
		names = append(names, ti.Name)
	}
	if !slices.Equal(names, []string{"invoke_specialist", "get_pods"}) {
		t.Fatalf("catalog = %v, want the daemon's own tools and the server's", names)
	}
}

// An MCP outage says nothing about the daemon's own tools, so it must
// not blank them. The pre-#137 shape of this bug is the reverse — an
// empty answer that looked like "no tools" — and reporting nothing
// during a blip would recreate it for a planner daemon.
func TestToolCatalogBuiltinsSurviveATotalMCPFailure(t *testing.T) {
	broken := &catalogToolset{name: "gke", err: errors.New("transport closed")}
	tc := testCatalog(t, workload.OnMutationApply, broken)
	tc.builtin = []tool.Tool{catalogTool{name: "invoke_specialist"}}

	got := tc.snapshot(context.Background())
	if len(got) != 1 || got[0].Name != "invoke_specialist" {
		t.Fatalf("snapshot during an outage = %+v, want the daemon's own tools", got)
	}
	// And the outage is still not cached: the next call retries.
	before := broken.calls
	tc.snapshot(context.Background())
	if broken.calls != before+1 {
		t.Errorf("server listed %d more times, want a fresh attempt", broken.calls-before)
	}
}

// The bug behind #133 was not a broken projection — it was a Config
// field cmd/mast never assigned, so GET /tools answered 200 with an
// empty list on every mast daemon while the code that would have
// filled it sat unreferenced. Asserting on the rendered Config, by
// reflection, is what makes the next such field fail here instead of
// in an operator's terminal: a new func field on attachadapter.Config
// breaks this test until serve either wires it or records why it
// can't.
func TestAttachWiringLeavesNoCapabilityUnwired(t *testing.T) {
	// Capabilities the daemon genuinely cannot serve yet. Empty today;
	// an entry here is a claim that mast has nothing to put in the
	// field, and needs the issue that tracks it.
	unwired := map[string]string{}

	w := attachWiring{
		appName:     appName,
		userID:      defaultUserID,
		baseContext: context.Background(),
		modelName:   "echo",
		description: "test",
		tools:       testCatalog(t, workload.OnMutationApply),
		subagents:   subagentCatalog(nil, []specialists.Spec{spec("log-analyst", specialists.ModeTask)}, workload.DispatchCoordinator),
		usage:       func(string) attach.UsageInfo { return attach.UsageInfo{} },
		runTurn:     func(context.Context, string, string) error { return nil },
	}

	cfg := reflect.ValueOf(w.config("sess-1"))
	for i := 0; i < cfg.NumField(); i++ {
		field := cfg.Type().Field(i)
		if field.Type.Kind() != reflect.Func {
			continue
		}
		if reason, ok := unwired[field.Name]; ok {
			if !cfg.Field(i).IsNil() {
				t.Errorf("%s is listed as unwired (%q) but serve set it — drop the exemption", field.Name, reason)
			}
			continue
		}
		if cfg.Field(i).IsNil() {
			t.Errorf("attachadapter.Config.%s is nil: serve never wired it, so its endpoint answers with an empty body "+
				"that reads like an answer (#133). Wire it in attachWiring.config, or add it to this test's unwired map with the issue that tracks it.", field.Name)
		}
	}

	// The scalar identity fields matter too — a session registered
	// under the wrong app name is unreachable over attach.
	if got := cfg.FieldByName("SessionID").String(); got != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", got)
	}
	if got := cfg.FieldByName("AppName").String(); got != appName {
		t.Errorf("AppName = %q, want %q", got, appName)
	}
}

// And the projection actually reaches the endpoint: an adapter built
// from the wiring answers AttachTools with the catalog's contents,
// not with the empty list a nil ToolsFn produces.
func TestAttachAdapterServesTheToolCatalog(t *testing.T) {
	w := attachWiring{
		appName:     appName,
		userID:      defaultUserID,
		baseContext: context.Background(),
		tools: testCatalog(t, workload.OnMutationDryRun, &catalogToolset{
			name:  "gke",
			tools: []tool.Tool{catalogTool{name: "delete_pod", desc: "delete a pod"}},
		}),
		usage:   func(string) attach.UsageInfo { return attach.UsageInfo{} },
		runTurn: func(context.Context, string, string) error { return nil },
	}

	got := w.config("sess-1").ToolsFn()
	if len(got) != 1 {
		t.Fatalf("ToolsFn = %+v, want the catalog's single tool", got)
	}
	if got[0].Server != "gke" || got[0].Source != attach.ToolSourceMCP || got[0].GateState != attach.ToolGateDenied {
		t.Errorf("ToolsFn entry = %+v, want gke/mcp/denied", got[0])
	}
}
