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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// declaringTool is catalogTool plus the one thing the producer contract
// needs and the /tools endpoint does not: the declared arguments.
type declaringTool struct {
	catalogTool
	params *jsonschema.Schema
}

func (d declaringTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: d.name, ParametersJsonSchema: d.params}
}

func scaleTool() declaringTool {
	return declaringTool{
		catalogTool: catalogTool{name: "scale_deployment", desc: "scale a deployment"},
		params: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"deployment": {Type: "string"},
				"replicas":   {Type: "integer"},
			},
			Required: []string{"deployment", "replicas"},
		},
	}
}

func testSchemas(sets ...*catalogToolset) *toolSchemas {
	toolsets := make([]tool.Toolset, 0, len(sets))
	for _, s := range sets {
		toolsets = append(toolsets, s)
	}
	return &toolSchemas{
		toolsets: toolsets,
		logger:   discardLogger(),
		ttl:      toolSchemaTTL,
		timeout:  toolSchemaTimeout,
	}
}

// TestToolSchemasResolvesAWiredTool is the resolver doing its job: the
// arguments the write gate checks a proposed change against are the
// ones the tool that would run it declares, read off the live toolset
// rather than transcribed into the workload file.
func TestToolSchemasResolvesAWiredTool(t *testing.T) {
	ts := testSchemas(&catalogToolset{name: "gke", tools: []tool.Tool{scaleTool()}})

	got, err := ts.lookup("scale_deployment")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.Properties["replicas"] == nil || got.Properties["replicas"].Type != "integer" {
		t.Fatalf("resolved schema = %+v, want the tool's own declared properties", got)
	}
}

// TestToolSchemasRefusesAToolNothingWired: the resolver is also the
// "does this exist" check, and its error text is what the specialist
// reads, so it has to say what to do instead.
func TestToolSchemasRefusesAToolNothingWired(t *testing.T) {
	ts := testSchemas(&catalogToolset{name: "gke", tools: []tool.Tool{scaleTool()}})

	_, err := ts.lookup("kubectl_scale")
	if err == nil {
		t.Fatal("an unwired tool resolved")
	}
	for _, want := range []string{"kubectl_scale", "empty change set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// TestToolSchemasCachesButRetriesAMiss: a hit costs nothing after the
// first report, and a miss costs exactly one extra listing — the cache
// can predate a server that just came up, and refusing a real
// remediation during an incident because of a stale cache is the
// expensive mistake. One retry, not one per lookup.
func TestToolSchemasCachesButRetriesAMiss(t *testing.T) {
	gke := &catalogToolset{name: "gke", tools: []tool.Tool{scaleTool()}}
	ts := testSchemas(gke)

	if _, err := ts.lookup("scale_deployment"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if gke.calls != 1 {
		t.Fatalf("listings after the first hit = %d, want 1", gke.calls)
	}
	if _, err := ts.lookup("scale_deployment"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if gke.calls != 1 {
		t.Errorf("listings after a second hit = %d, want the cache to have served it", gke.calls)
	}

	if _, err := ts.lookup("kubectl_scale"); err == nil {
		t.Fatal("an unwired tool resolved")
	}
	if gke.calls != 2 {
		t.Errorf("listings after a miss = %d, want exactly one re-list: a miss checks for a server that came up, it does not re-list per call", gke.calls)
	}
}

// TestToolSchemasSeesAServerThatCameUp is the reason a miss re-lists at
// all. The MCP server holding the remediation tools is the one most
// likely to have been restarted during the incident being triaged.
func TestToolSchemasSeesAServerThatCameUp(t *testing.T) {
	gke := &catalogToolset{name: "gke", err: errors.New("connection refused")}
	ts := testSchemas(gke)

	if _, err := ts.lookup("scale_deployment"); err == nil {
		t.Fatal("a tool resolved from a server that never answered")
	}
	gke.err, gke.tools = nil, []tool.Tool{scaleTool()}
	if _, err := ts.lookup("scale_deployment"); err != nil {
		t.Fatalf("the tool did not resolve after its server came back: %v", err)
	}
}

// TestToolSchemasDoesNotCacheATotalOutage: same rule the /tools catalog
// follows. A transport blip must not refuse every proposed change for a
// full TTL past recovery — and the TTL here is minutes, so caching the
// outage would outlast most incidents' remediation window.
func TestToolSchemasDoesNotCacheATotalOutage(t *testing.T) {
	gke := &catalogToolset{name: "gke", tools: []tool.Tool{scaleTool()}}
	ts := testSchemas(gke)

	if _, err := ts.lookup("scale_deployment"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	gke.err = errors.New("connection refused")
	// Force the TTL to have expired without waiting it out.
	now := time.Now().Add(2 * toolSchemaTTL)
	ts.now = func() time.Time { return now }

	if _, err := ts.lookup("scale_deployment"); err != nil {
		t.Errorf("the last good schema was dropped when the server blipped: %v", err)
	}
}

// TestToolSchemasRefusesAToolThatDeclaresNothing: a wired tool with no
// declared arguments cannot have a proposed call checked against it, so
// it is refused rather than waved through. Fail closed — an unchecked
// argument list is the thing an operator would be approving blind.
func TestToolSchemasRefusesAToolThatDeclaresNothing(t *testing.T) {
	ts := testSchemas(&catalogToolset{name: "gke", tools: []tool.Tool{
		catalogTool{name: "scale_deployment", desc: "scale a deployment"},
	}})

	if _, err := ts.lookup("scale_deployment"); err == nil {
		t.Fatal("a tool that declares no arguments resolved, so any arguments at all would have passed")
	}
}

// The precondition read (v0.4 W7): the one place mast calls a tool no
// model asked for.

// runnableTool satisfies ADK's unexported runnable-tool interface, which
// is what mast asserts against because tool.Tool itself is name and
// description only.
type runnableTool struct {
	catalogTool
	sawArgs any
	result  map[string]any
	err     error
}

func (r *runnableTool) Run(_ adkagent.Context, args any) (map[string]any, error) {
	r.sawArgs = args
	return r.result, r.err
}

func TestToolSchemasReadRunsTheTool(t *testing.T) {
	get := &runnableTool{
		catalogTool: catalogTool{name: "get_deployment", desc: "read a deployment"},
		result:      map[string]any{"spec": map[string]any{"replicas": float64(3)}},
	}
	ts := testSchemas(&catalogToolset{name: "gke", tools: []tool.Tool{get}})

	got, err := ts.read(nil, "get_deployment", map[string]any{"name": "api"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if spec, _ := got["spec"].(map[string]any); spec == nil || spec["replicas"] != float64(3) {
		t.Errorf("read = %v, want the tool's own result carried back whole", got)
	}
	args, _ := get.sawArgs.(map[string]any)
	if args == nil || args["name"] != "api" {
		t.Errorf("the tool saw %v, want the arguments the gate passed", get.sawArgs)
	}
}

// TestToolSchemasReadPassesAnEmptyMapNotNil: a read whose arguments all
// come from the bundle's literals — or from nothing at all — still has
// to reach the tool as an object, because an MCP tool unmarshalling nil
// is a failure that would read as "the cluster moved".
func TestToolSchemasReadPassesAnEmptyMapNotNil(t *testing.T) {
	get := &runnableTool{catalogTool: catalogTool{name: "get_nodes"}, result: map[string]any{}}
	ts := testSchemas(&catalogToolset{name: "gke", tools: []tool.Tool{get}})

	if _, err := ts.read(nil, "get_nodes", nil); err != nil {
		t.Fatalf("read: %v", err)
	}
	args, ok := get.sawArgs.(map[string]any)
	if !ok || args == nil {
		t.Errorf("the tool saw %#v, want an empty map", get.sawArgs)
	}
}

// TestToolSchemasReadRefusesAToolItCannotRun: mast asserts its way to a
// handle it can call, and a tool that does not satisfy the interface is
// said to be unrunnable rather than silently returning nothing — which
// the gate would compare against the snapshot and read as unchanged.
func TestToolSchemasReadRefusesAToolItCannotRun(t *testing.T) {
	ts := testSchemas(&catalogToolset{name: "gke", tools: []tool.Tool{
		catalogTool{name: "get_deployment", desc: "read a deployment"},
	}})

	_, err := ts.read(nil, "get_deployment", nil)
	if err == nil {
		t.Fatal("a tool mast cannot run served as a precondition read")
	}
	if !strings.Contains(err.Error(), "cannot be run by mast directly") {
		t.Errorf("error does not say why the read is impossible: %v", err)
	}
}

func TestToolSchemasReadRefusesAnUnwiredTool(t *testing.T) {
	ts := testSchemas(&catalogToolset{name: "gke", tools: []tool.Tool{scaleTool()}})

	_, err := ts.read(nil, "get_deployment", nil)
	if err == nil {
		t.Fatal("a tool nothing wired served as a precondition read")
	}
	if !strings.Contains(err.Error(), "get_deployment") {
		t.Errorf("error does not name the tool: %v", err)
	}
}

// TestToolSchemasReadReportsTheToolsFailure: the gate voids a grant when
// the read fails, so the failure has to arrive as an error naming the
// read — an empty result would compare equal to nothing and let the call
// through on a check that never ran.
func TestToolSchemasReadReportsTheToolsFailure(t *testing.T) {
	get := &runnableTool{
		catalogTool: catalogTool{name: "get_deployment"},
		err:         errors.New("connection refused"),
	}
	ts := testSchemas(&catalogToolset{name: "gke", tools: []tool.Tool{get}})

	got, err := ts.read(nil, "get_deployment", nil)
	if err == nil {
		t.Fatal("a failed read returned no error")
	}
	if got != nil {
		t.Errorf("a failed read returned %v, want nil", got)
	}
	for _, want := range []string{"precondition read", "get_deployment", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
