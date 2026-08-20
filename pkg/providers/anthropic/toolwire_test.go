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

package anthropic

import (
	"context"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/mast/internal/toolcatalog"
)

// This file is the gating layer of the tool-calling measurement track
// (#168): offline, deterministic, no model, no credentials, and it
// runs in the ordinary `go test ./...` presubmit.
//
// #154 shipped a total tool-calling outage to a release. Every
// mast-authored tool and every MCP tool reached Claude as a bare name
// with an empty input schema, because toolsParam handled only the
// typed genai.Schema spelling and ADK v2 populates
// ParametersJsonSchema. The fix came with a regression test built
// around functiontool.New — which covers the tool that happened to be
// noticed, not the invariant that was broken.
//
// The invariant is: whatever mast dispatches, the model is shown the
// arguments it accepts. This test states it over the whole catalog a
// real turn assembles, so a fourth construction path added next year
// is covered on the day it lands rather than on the day it breaks.
func TestToolWire_EveryDispatchedToolPresentsItsArguments(t *testing.T) {
	t.Parallel()

	endpoint, stop := toolcatalog.StartStubMCP()
	t.Cleanup(stop)

	catalog, err := toolcatalog.Build(context.Background(), toolcatalog.Config{MCPEndpoint: endpoint})
	if err != nil {
		t.Fatalf("build tool catalog: %v", err)
	}
	// A floor, not an equality: the catalog is meant to grow as mast
	// grows tools, and a test that has to be edited for every new tool
	// gets edited without being read. It exists so a rig that quietly
	// stops producing tools fails here instead of passing vacuously.
	// internal/toolcatalog's own tests pin the composition.
	if len(catalog) < 8 {
		t.Fatalf("catalog has only %d tools — too few to be measuring anything:\n%s", len(catalog), toolcatalog.Summary(catalog))
	}

	decls := make([]*genai.FunctionDeclaration, 0, len(catalog))
	for _, e := range catalog {
		decls = append(decls, e.Declaration)
	}

	// Drive the real adapter and read the real request body. Calling
	// toolsParam directly would test the function the bug was in;
	// going through GenerateContent tests what Anthropic receives,
	// which is the thing that was actually wrong.
	l, captured := newOfflineLLM(t, "claude-test", messagesSSEFixture)
	for _, err := range l.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Contents: userText("enumerate the tools"),
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{{FunctionDeclarations: decls}},
		},
	}, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
	}

	wire := anthropicWire(t, captured)
	if len(wire) != len(catalog) {
		t.Errorf("sent %d tools, %d arrived on the wire", len(catalog), len(wire))
	}
	for _, problem := range toolcatalog.Verify(catalog, wire) {
		t.Error(problem)
	}
	if t.Failed() {
		t.Logf("catalog under test:\n%s", toolcatalog.Summary(catalog))
	}
}

// anthropicWire reads the tools array out of the captured Messages
// request body, keyed by name, with each tool's input_schema as the
// emitted argument schema. The traversal is deliberately defensive:
// every shape it refuses is a way the payload could be malformed
// without any individual tool looking wrong.
func anthropicWire(t *testing.T, captured *capturedRequest) toolcatalog.Wire {
	t.Helper()

	raw, ok := captured.body["tools"]
	if !ok {
		t.Fatalf("request body carries no tools field at all: %v", captured.body)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("tools is %T, want a JSON array", raw)
	}

	wire := toolcatalog.Wire{}
	for i, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("tools[%d] is %T, want a JSON object", i, item)
		}
		name, _ := entry["name"].(string)
		if name == "" {
			t.Fatalf("tools[%d] has no name: %v", i, entry)
		}
		schema, ok := entry["input_schema"].(map[string]any)
		if !ok {
			t.Errorf("%s: input_schema is %T, want a JSON object — Anthropic rejects a tool without one", name, entry["input_schema"])
			continue
		}
		if got, _ := schema["type"].(string); got != "object" {
			t.Errorf("%s: input_schema type is %q, want \"object\"", name, got)
		}
		wire[name] = schema
	}
	return wire
}
