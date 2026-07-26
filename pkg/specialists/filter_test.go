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
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

type staticToolset struct {
	name  string
	tools []tool.Tool
}

func (s *staticToolset) Name() string                                          { return s.name }
func (s *staticToolset) Tools(_ adkagent.ReadonlyContext) ([]tool.Tool, error) { return s.tools, nil }

func mkTool(t *testing.T, name string) tool.Tool {
	t.Helper()
	ft, err := functiontool.New(functiontool.Config{Name: name, Description: name},
		func(_ adkagent.Context, _ struct{}) (string, error) { return "", nil })
	if err != nil {
		t.Fatalf("functiontool.New(%q): %v", name, err)
	}
	return ft
}

func TestFilterToolsets(t *testing.T) {
	gke := &staticToolset{name: "gke", tools: []tool.Tool{
		mkTool(t, "get_pod"), mkTool(t, "rollout_undo"),
	}}
	slack := &staticToolset{name: "slack", tools: []tool.Tool{mkTool(t, "post_message")}}
	offered := []tool.Toolset{gke, slack}

	// Empty allowlist inherits everything.
	if got := filterToolsets(Spec{Name: "s"}, offered); len(got) != 2 {
		t.Fatalf("empty allowlist: got %d toolsets, want 2", len(got))
	}

	// Non-empty allowlist drops unlisted servers and narrows listed ones.
	spec := Spec{Name: "s", Tools: ToolAllowlist{MCP: []MCPAllowlist{
		{Server: "gke", Tools: []string{"get_pod"}},
	}}}
	got := filterToolsets(spec, offered)
	if len(got) != 1 || got[0].Name() != "gke" {
		t.Fatalf("allowlist: got %v toolsets, want just gke", len(got))
	}
	tools, err := got[0].Tools(nil)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name() != "get_pod" {
		names := make([]string, 0, len(tools))
		for _, tl := range tools {
			names = append(names, tl.Name())
		}
		t.Fatalf("narrowed toolset exposes %v, want [get_pod]", names)
	}
}
