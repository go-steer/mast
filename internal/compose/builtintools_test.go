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
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// offlineGeminiCreds gives the genai client something to construct
// against. No request is ever issued by these tests — the wrapper
// mutates the request and returns a lazy iterator, and nothing here
// iterates it — so the key never has to be real. It does have to be
// present: without it genai refuses at construction and the test would
// pass for the wrong reason.
func offlineGeminiCreds(t *testing.T) {
	t.Helper()
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "offline-not-a-real-key")
}

// sentTools returns the tools m will actually put on the wire for a
// request that starts with none.
//
// This is the assertion #324 asks for and the reason it is written this
// way: a server-side built-in never becomes a tool call, so the only
// observable that answers "can this model reach the public internet" is
// the request the wrapper hands to the backend. Reading the config back
// out of the bundle would answer a different question.
//
// The iterator is deliberately discarded. gemini's wrapper appends its
// built-ins to req.Config.Tools in the body of GenerateContent and only
// calls the inner model when the returned sequence is ranged over, so
// dropping it captures the constructed request without a network call.
func sentTools(ctx context.Context, m adkmodel.LLM) []*genai.Tool {
	req := &adkmodel.LLMRequest{Config: &genai.GenerateContentConfig{}}
	_ = m.GenerateContent(ctx, req, false)
	return req.Config.Tools
}

func hasGoogleSearch(tools []*genai.Tool) bool {
	return slices.ContainsFunc(tools, func(t *genai.Tool) bool { return t != nil && t.GoogleSearch != nil })
}

// readOnlyRoster writes a one-specialist workload whose specialist is
// declared read_only and runs on a model of its own, with the given
// `builtin_tools:` block (pass "" for none at all).
func readOnlyRoster(t *testing.T, builtinTools string) (workload.Bundle, []specialists.Spec) {
	t.Helper()
	bundle, err := loadRoster(t, builtinTools)
	if err != nil {
		t.Fatalf("load workload: %v", err)
	}
	specs, err := specialists.LoadDir(filepath.Join(filepath.Dir(bundle.Filename), "specialists"))
	if err != nil {
		t.Fatalf("load specialists: %v", err)
	}
	return bundle, specs
}

// loadRoster is readOnlyRoster's first half, handing back the load error
// instead of failing on it, so a case can assert on the refusal itself.
func loadRoster(t *testing.T, builtinTools string) (workload.Bundle, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "specialists"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("workload.yaml", `name: grounding
description: Fixture for the built-in tool gate.
mode: single_session
dispatch: coordinator
`+builtinTools+`
tool_catalog:
  tools:
    - name: read_status
      mutating: false

specialists:
  - analyst
`)
	write(filepath.Join("specialists", "analyst.specialist.md"), `---
name: analyst
description: Reads the cluster and reports. Cannot change anything.
mode: Task
capability: read_only
model: gemini-3.5-flash
tools:
  mcp: []
---

Report what you were given.
`)
	return workload.Load(filepath.Join(dir, "workload.yaml"))
}

// resolveAnalyst walks the composed path: the bundle's built-in block
// goes into the same resolver BuildRoot builds, and the specialist's
// `model:` override is resolved through it.
func resolveAnalyst(t *testing.T, bundle workload.Bundle, specs []specialists.Spec) adkmodel.LLM {
	t.Helper()
	if len(specs) != 1 || specs[0].Capability != specialists.CapabilityReadOnly {
		t.Fatalf("fixture is not discriminating: want one read_only specialist, got %+v", specs)
	}
	// A nil root is what makes the override build its own model rather
	// than collapse back (see NewModelResolver); the root's name is a
	// different tier so the reuse shortcut cannot fire either.
	resolve := NewModelResolver(context.Background(), "", "gemini-3.7-flash", nil, bundle.BuiltinTools, nil)
	m, err := resolve(specs[0].Model)
	if err != nil {
		t.Fatalf("resolve the specialist's model: %v", err)
	}
	return m
}

// TestReadOnlySpecialistGetsNoGoogleSearch is #324's acceptance case. A
// specialist mast has verified cannot write must not be handed a tool
// that reads the public internet underneath the permissions gate — and
// the disable has to be observable in the request, because a server-side
// built-in leaves no other trace.
//
// The false is written out rather than left absent so the test fails if
// an explicit disable is ever dropped on the floor; the absent case is
// TestBuiltinToolsDefaultOff below.
func TestReadOnlySpecialistGetsNoGoogleSearch(t *testing.T) {
	offlineGeminiCreds(t)
	bundle, specs := readOnlyRoster(t, "\nbuiltin_tools:\n  web_search: false\n")
	if bundle.BuiltinTools.WebSearch == nil || *bundle.BuiltinTools.WebSearch {
		t.Fatalf("fixture did not parse: BuiltinTools = %+v", bundle.BuiltinTools)
	}
	if tools := sentTools(context.Background(), resolveAnalyst(t, bundle, specs)); hasGoogleSearch(tools) {
		t.Errorf("a read_only specialist under `web_search: false` still sends google_search: %+v", tools)
	}
}

// TestBuiltinToolsDefaultOff is the half that would have caught the bug:
// before #324 a bundle said nothing about built-ins and mast passed
// gemini.DefaultBuiltinTools() anyway, so every Gemini specialist in
// every workload ever shipped had search and URL fetch on. Silence now
// means off.
func TestBuiltinToolsDefaultOff(t *testing.T) {
	offlineGeminiCreds(t)
	bundle, specs := readOnlyRoster(t, "")
	if (bundle.BuiltinTools != workload.BuiltinTools{}) {
		t.Fatalf("a bundle with no builtin_tools block parsed as %+v, want the zero value", bundle.BuiltinTools)
	}
	if tools := sentTools(context.Background(), resolveAnalyst(t, bundle, specs)); len(tools) != 0 {
		t.Errorf("a bundle that names no built-ins sends %+v, want none", tools)
	}
}

// TestBuiltinToolsOptIn is the other direction, and it is what keeps the
// gate a gate rather than a removal: a workload that has decided
// grounding is acceptable says so and gets it.
func TestBuiltinToolsOptIn(t *testing.T) {
	offlineGeminiCreds(t)
	bundle, specs := readOnlyRoster(t, "\nbuiltin_tools:\n  web_search: true\n  url_context: true\n")
	tools := sentTools(context.Background(), resolveAnalyst(t, bundle, specs))
	if !hasGoogleSearch(tools) {
		t.Errorf("`web_search: true` did not reach the request: %+v", tools)
	}
	if !slices.ContainsFunc(tools, func(t *genai.Tool) bool { return t != nil && t.URLContext != nil }) {
		t.Errorf("`url_context: true` did not reach the request: %+v", tools)
	}
	if slices.ContainsFunc(tools, func(t *genai.Tool) bool { return t != nil && t.CodeExecution != nil }) {
		t.Errorf("code_execution was never asked for and arrived anyway: %+v", tools)
	}
}

// TestBuildModelDefaultsOffOnBothProviders pins the cross-provider
// property the neutral block exists for: the same zero value means the
// same reach on either backend. Gemini's own baseline is search-on and
// Anthropic's is search-off, and neither of those is what mast passes.
func TestBuildModelDefaultsOffOnBothProviders(t *testing.T) {
	offlineGeminiCreds(t)
	ctx := context.Background()

	g, err := BuildModel(ctx, "", "gemini-3.5-flash", workload.BuiltinTools{})
	if err != nil {
		t.Fatalf("BuildModel(gemini): %v", err)
	}
	if got := BuiltinToolsSummary(g); got != "none" {
		t.Errorf("gemini built-ins with an empty bundle = %q, want %q", got, "none")
	}

	t.Setenv("ANTHROPIC_API_KEY", "offline-not-a-real-key")
	a, err := BuildModel(ctx, "anthropic", "claude-sonnet-4-6", workload.BuiltinTools{})
	if err != nil {
		t.Fatalf("BuildModel(anthropic): %v", err)
	}
	if got := BuiltinToolsSummary(a); got != "none" {
		t.Errorf("anthropic built-ins with an empty bundle = %q, want %q", got, "none")
	}
}

// TestBuiltinToolsSummaryReportsWhatEachProviderCanSend covers the gap
// the neutral names paper over. url_context has no Anthropic equivalent,
// so a bundle that asks for both gets both on Gemini and only web search
// on Claude — and the summary line is where an operator finds that out,
// rather than assuming the key took because the daemon started.
func TestBuiltinToolsSummaryReportsWhatEachProviderCanSend(t *testing.T) {
	offlineGeminiCreds(t)
	ctx := context.Background()
	yes := true
	bt := workload.BuiltinTools{WebSearch: &yes, URLContext: &yes}

	g, err := BuildModel(ctx, "", "gemini-3.5-flash", bt)
	if err != nil {
		t.Fatalf("BuildModel(gemini): %v", err)
	}
	if got := BuiltinToolsSummary(g); got != "web_search,url_context" {
		t.Errorf("gemini summary = %q, want %q", got, "web_search,url_context")
	}

	t.Setenv("ANTHROPIC_API_KEY", "offline-not-a-real-key")
	a, err := BuildModel(ctx, "anthropic", "claude-sonnet-4-6", bt)
	if err != nil {
		t.Fatalf("BuildModel(anthropic): %v", err)
	}
	if got := BuiltinToolsSummary(a); got != "web_search" {
		t.Errorf("anthropic summary = %q, want %q (url_context has no equivalent there)", got, "web_search")
	}
}

// TestBuiltinToolsSummaryEmptyForOfflineFakes keeps the log line honest
// about the difference between "off" and "not a thing here". echo has no
// server-side built-in concept, and reporting `builtin_tools=none` for
// it would assert an absence that means nothing.
func TestBuiltinToolsSummaryEmptyForOfflineFakes(t *testing.T) {
	for _, m := range []adkmodel.LLM{
		mastagent.NewEchoModel("echo"),
		mastagent.NewToolActorModel("toolactor"),
	} {
		if got := BuiltinToolsSummary(m); got != "" {
			t.Errorf("BuiltinToolsSummary(%s) = %q, want the empty string", m.Name(), got)
		}
	}
}

// TestUnknownBuiltinKeyIsRefused replaces an assertion this file used to
// make in the opposite direction.
//
// When the built-in gate shipped (#324) the loader ignored unrecognised
// keys, so `web_serch: true` was discarded without a word and the only
// way an operator could catch it was the startup summary — which is why
// that summary reads the constructed model rather than the parsed
// config. #302 made an unknown key a load error, so the typo is now
// caught a step earlier, by name, before anything is constructed.
//
// Both halves are still worth pinning. The refusal must name the key, or
// it is not better than the silence it replaced; and the summary must
// keep reading the model, because it also covers the case no strictness
// can — a key that is spelled correctly, parses, and is unmet by the
// provider actually in use.
func TestUnknownBuiltinKeyIsRefused(t *testing.T) {
	_, err := loadRoster(t, "\nbuiltin_tools:\n  web_serch: true\n")
	if err == nil {
		t.Fatal("a misspelled built-in key loaded cleanly; #302 makes it a load error")
	}
	if !strings.Contains(err.Error(), "web_serch") {
		t.Errorf("refusal does not name the offending key: %v", err)
	}
}

// TestSummaryStillCoversWhatStrictnessCannot is the other half. A bundle
// can ask for url_context on Anthropic with every key spelled correctly;
// nothing in the loader can refuse that, because the same bundle is
// meant to run on Gemini where the key is real. What tells the operator
// is the startup line, read off the provider that was constructed.
func TestSummaryStillCoversWhatStrictnessCannot(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "offline-not-a-real-key")
	yes := true
	bt := workload.BuiltinTools{URLContext: &yes}
	m, err := BuildModel(t.Context(), "anthropic", "claude-haiku-4-5", bt)
	if err != nil {
		t.Fatalf("BuildModel(anthropic): %v", err)
	}
	if got := BuiltinToolsSummary(m); got != "none" {
		t.Errorf("summary = %q, want %q — a correctly spelled key the provider has no tool for is unmet, and the log line is the only place that shows", got, "none")
	}
}

// TestBuiltinToolNamesUseTheNeutralVocabulary keeps the two provider
// packages reporting in the same words the bundle is written in. They
// satisfy BuiltinToolsReporter structurally and cannot see each other,
// so nothing but a test holds the vocabulary together.
func TestBuiltinToolNamesUseTheNeutralVocabulary(t *testing.T) {
	yes := true
	all := workload.BuiltinTools{WebSearch: &yes, URLContext: &yes, CodeExecution: &yes}
	if got := strings.Join(geminiBuiltins(all).Names(), ","); got != "web_search,url_context,code_execution" {
		t.Errorf("gemini names = %q", got)
	}
	if got := strings.Join(anthropicBuiltins(all).Names(), ","); got != "web_search" {
		t.Errorf("anthropic names = %q", got)
	}
	if got := geminiBuiltins(workload.BuiltinTools{}).Names(); len(got) != 0 {
		t.Errorf("gemini names off = %v, want none", got)
	}
	if got := anthropicBuiltins(workload.BuiltinTools{}).Names(); len(got) != 0 {
		t.Errorf("anthropic names off = %v, want none", got)
	}
}
