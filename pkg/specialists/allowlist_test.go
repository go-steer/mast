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
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
)

// capture is a logger that keeps what was written, so a test can assert
// on the one line an operator would have to notice.
func capture(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &buf
}

// countingToolset records how many times it was listed: the report has
// to survive a specialist that runs a dozen turns without printing a
// dozen times.
type countingToolset struct {
	staticToolset
	lists int
	err   error
}

func (c *countingToolset) Tools(ctx adkagent.ReadonlyContext) ([]tool.Tool, error) {
	c.lists++
	if c.err != nil {
		return nil, c.err
	}
	return c.staticToolset.Tools(ctx)
}

func TestAToolTheServerDoesNotServeIsNamed(t *testing.T) {
	logger, buf := capture(t)
	gke := &staticToolset{name: "gke", tools: []tool.Tool{
		mkTool(t, "get_k8s_resource"), mkTool(t, "list_dataset_ids"),
	}}
	// The shape found in the wild: the real name is list_dataset_ids,
	// the allowlist says list_datasets, and nothing said so.
	spec := Spec{Name: "scale-outage", Tools: ToolAllowlist{MCP: []MCPAllowlist{
		{Server: "gke", Tools: []string{"get_k8s_resource", "list_datasets", "query"}},
	}}}

	got := filterToolsets(spec, []tool.Toolset{gke}, logger)
	if len(got) != 1 {
		t.Fatalf("got %d toolsets, want 1", len(got))
	}
	tools, err := got[0].Tools(nil)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name() != "get_k8s_resource" {
		t.Fatalf("granted %v, want just get_k8s_resource", names(tools))
	}

	line := buf.String()
	for _, want := range []string{"scale-outage", "gke", "list_datasets", "query", "granted=1"} {
		if !strings.Contains(line, want) {
			t.Errorf("warning does not mention %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "get_k8s_resource") {
		t.Errorf("warning names a tool that WAS served:\n%s", line)
	}
}

func TestAnAllowlistThatMatchesEverythingIsSilent(t *testing.T) {
	logger, buf := capture(t)
	gke := &staticToolset{name: "gke", tools: []tool.Tool{
		mkTool(t, "get_k8s_resource"), mkTool(t, "rollout_undo"),
	}}
	spec := Spec{Name: "s", Tools: ToolAllowlist{MCP: []MCPAllowlist{
		{Server: "gke", Tools: []string{"get_k8s_resource"}},
	}}}

	got := filterToolsets(spec, []tool.Toolset{gke}, logger)
	if _, err := got[0].Tools(nil); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	// Naming fewer tools than the server offers is the *point* of an
	// allowlist. Only the reverse — naming more — is the mistake.
	if buf.Len() != 0 {
		t.Fatalf("a correct allowlist warned:\n%s", buf.String())
	}
}

func TestTheReportIsMadeOncePerToolset(t *testing.T) {
	logger, buf := capture(t)
	gke := &countingToolset{staticToolset: staticToolset{
		name: "gke", tools: []tool.Tool{mkTool(t, "get_k8s_resource")},
	}}
	spec := Spec{Name: "s", Tools: ToolAllowlist{MCP: []MCPAllowlist{
		{Server: "gke", Tools: []string{"get_k8s_resource", "get_router"}},
	}}}

	got := filterToolsets(spec, []tool.Toolset{gke}, logger)
	for range 12 {
		if _, err := got[0].Tools(nil); err != nil {
			t.Fatalf("Tools: %v", err)
		}
	}
	if gke.lists != 12 {
		t.Fatalf("inner toolset listed %d times, want 12 — the wrap must not cache the listing", gke.lists)
	}
	if n := strings.Count(buf.String(), "get_router"); n != 1 {
		t.Fatalf("warned %d times over twelve turns, want once", n)
	}
}

func TestAServerThatWillNotListIsNotBlamedOnTheAllowlist(t *testing.T) {
	logger, buf := capture(t)
	down := &countingToolset{
		staticToolset: staticToolset{name: "gke"},
		err:           errors.New("dial tcp: connection refused"),
	}
	spec := Spec{Name: "s", Tools: ToolAllowlist{MCP: []MCPAllowlist{
		{Server: "gke", Tools: []string{"get_k8s_resource"}},
	}}}

	got := filterToolsets(spec, []tool.Toolset{down}, logger)
	if _, err := got[0].Tools(nil); err == nil {
		t.Fatal("a toolset that cannot list returned no error")
	}
	// Every name is unmatched when nothing was offered. Reporting them
	// would point an operator at their bundle for a server outage.
	if buf.Len() != 0 {
		t.Fatalf("a failed listing produced an allowlist warning:\n%s", buf.String())
	}
}

func TestANilLoggerFiltersTheSameAndSaysNothing(t *testing.T) {
	gke := &staticToolset{name: "gke", tools: []tool.Tool{
		mkTool(t, "get_k8s_resource"), mkTool(t, "rollout_undo"),
	}}
	spec := Spec{Name: "s", Tools: ToolAllowlist{MCP: []MCPAllowlist{
		{Server: "gke", Tools: []string{"get_k8s_resource", "nope"}},
	}}}

	got := filterToolsets(spec, []tool.Toolset{gke}, nil)
	tools, err := got[0].Tools(nil)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name() != "get_k8s_resource" {
		t.Fatalf("granted %v, want just get_k8s_resource", names(tools))
	}
}

func TestTheServerKeySurvivesTheWrap(t *testing.T) {
	// pkg/mcp's digest wrap and pkg/graph's fan-out check both match
	// toolsets by Name(). A wrap that renamed one would silently unmatch
	// them — see pkg/mcp/toolset.go's note on the same hazard.
	gke := &staticToolset{name: "gke", tools: []tool.Tool{mkTool(t, "get_k8s_resource")}}
	spec := Spec{Name: "s", Tools: ToolAllowlist{MCP: []MCPAllowlist{
		{Server: "gke", Tools: []string{"get_k8s_resource"}},
	}}}

	got := filterToolsets(spec, []tool.Toolset{gke}, nil)
	if got[0].Name() != "gke" {
		t.Fatalf("wrapped toolset is named %q, want %q", got[0].Name(), "gke")
	}
}

func names(tools []tool.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name())
	}
	return out
}
