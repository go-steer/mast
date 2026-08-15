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

package specialists_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/specialists"
)

const taskSpec = `---
description: |
  Diagnoses ImagePullBackOff pod failures. Invoke when a pod reports
  image pull errors in its events.
budget:
  max_turns: 5
  max_wallclock_seconds: 60
tools:
  mcp:
    - server: gke
      tools:
        - get_k8s_resource
        - describe_k8s_resource
---

You are a specialist for diagnosing ImagePullBackOff pod failures.

OBJECTIVE: Identify the root cause and return a 3-bullet digest.
`

const classifierSpec = `---
name: triage-classifier
description: Routes an incoming k8s event to the correct per-failure-mode specialist.
mode: SingleTurn
model: gemini-2.5-flash
---
Given the InjectPayload's reason field, output one of the following
tokens verbatim: ImagePullBackOff, _fallback.

Output only the token; no explanation.
`

const missingDescSpec = `---
name: broken
---
body
`

func writeTempTmpl(t *testing.T, dir, name, contents string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	writeTempTmpl(t, dir, "ImagePullBackOff.tmpl", taskSpec)
	writeTempTmpl(t, dir, "triage-classifier.tmpl", classifierSpec)

	specs, err := specialists.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if got, want := len(specs), 2; got != want {
		t.Fatalf("got %d specs, want %d", got, want)
	}

	// Sorted by Name; ImagePullBackOff < triage-classifier ('I' < 't').
	if got, want := specs[0].Name, "ImagePullBackOff"; got != want {
		t.Errorf("specs[0].Name = %q, want %q", got, want)
	}
	if got, want := specs[0].Mode, specialists.ModeTask; got != want {
		t.Errorf("specs[0].Mode = %q, want %q (default)", got, want)
	}
	if got, want := specs[0].Budget.MaxTurns, 5; got != want {
		t.Errorf("specs[0].Budget.MaxTurns = %d, want %d", got, want)
	}
	if len(specs[0].Tools.MCP) != 1 || specs[0].Tools.MCP[0].Server != "gke" {
		t.Errorf("specs[0].Tools.MCP unexpected: %+v", specs[0].Tools.MCP)
	}
	if specs[0].Instruction == "" || specs[0].Instruction[:8] != "You are " {
		t.Errorf("specs[0].Instruction not as expected: %q", specs[0].Instruction[:min(30, len(specs[0].Instruction))])
	}

	if got, want := specs[1].Name, "triage-classifier"; got != want {
		t.Errorf("specs[1].Name = %q, want %q", got, want)
	}
	if got, want := specs[1].Mode, specialists.ModeSingleTurn; got != want {
		t.Errorf("specs[1].Mode = %q, want %q", got, want)
	}
	if got, want := specs[1].Model, "gemini-2.5-flash"; got != want {
		t.Errorf("specs[1].Model = %q, want %q", got, want)
	}
}

// TestLoadDir_ExampleWorkload pins the shipped GKE-triage roster to
// the full shape in docs/triage-demo-plan.md: eleven per-failure-mode
// Task specialists + the SingleTurn triage-classifier + _fallback, plus
// the one change-executor W2.4 split the write surface out into.
func TestLoadDir_ExampleWorkload(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "workloads", "gke-triage", "specialists")
	specs, err := specialists.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir(%s): %v", dir, err)
	}
	if got, want := len(specs), 14; got != want {
		t.Fatalf("got %d specs, want %d", got, want)
	}

	want := map[string]specialists.Mode{
		"BackOff":           specialists.ModeTask,
		"CrashLoopBackOff":  specialists.ModeTask,
		"ErrImagePull":      specialists.ModeTask,
		"Evicted":           specialists.ModeTask,
		"FailedMount":       specialists.ModeTask,
		"FailedScheduling":  specialists.ModeTask,
		"ImagePullBackOff":  specialists.ModeTask,
		"NetworkNotReady":   specialists.ModeTask,
		"NodeNotReady":      specialists.ModeTask,
		"OOMKilled":         specialists.ModeTask,
		"Unhealthy":         specialists.ModeTask,
		"_fallback":         specialists.ModeTask,
		"change-executor":   specialists.ModeTask,
		"triage-classifier": specialists.ModeSingleTurn,
	}
	// W2.4: exactly one specialist in this roster may change the
	// cluster, and it is the one named for it. Counted rather than
	// spot-checked — the regression to catch is a second specialist
	// quietly acquiring the declaration, which no per-file assertion
	// would see.
	var executors []string
	// The report contract is one file, shared. Collecting the resolved
	// paths and asserting there is exactly one is the assertion that
	// matters: twelve specialists each with their own valid schema would
	// satisfy "every diagnoser is typed" and still be the drift the
	// single asset exists to prevent.
	schemaPaths := map[string]int{}

	for _, s := range specs {
		mode, ok := want[s.Name]
		if !ok {
			t.Errorf("unexpected specialist %q in example roster", s.Name)
			continue
		}
		if s.Mode != mode {
			t.Errorf("%s: Mode = %q, want %q", s.Name, s.Mode, mode)
		}
		if s.Description == "" {
			t.Errorf("%s: empty description", s.Name)
		}
		if s.Capability == specialists.CapabilityChangeExecutor {
			executors = append(executors, s.Name)
		}
		switch s.Name {
		case "triage-classifier":
			// The router emits a bare token, not a report. Holding it
			// to the finding contract would make every routing decision
			// a validation failure.
			if s.OutputSchema != nil {
				t.Errorf("%s: has an output schema; the router emits a token, not a finding", s.Name)
			}
		case "change-executor":
			// A change report is not a finding: it says what was applied
			// and how to undo it, so it is a separate contract on
			// purpose. Sharing finding.json here is the drift that would
			// make "proposed" and "applied" indistinguishable.
			if s.OutputSchema == nil {
				t.Errorf("%s: no output schema", s.Name)
				break
			}
			if _, ok := s.OutputSchema.Properties["applied"]; !ok {
				t.Errorf("%s: schema has no applied property, got %v", s.Name, s.OutputSchema.Properties)
			}
		default:
			if s.OutputSchema == nil {
				t.Errorf("%s: no output schema — a diagnoser returns a finding, and untyped is how free text creeps back", s.Name)
				break
			}
			if _, ok := s.OutputSchema.Properties["severity"]; !ok {
				t.Errorf("%s: schema has no severity property, got %v", s.Name, s.OutputSchema.Properties)
			}
			schemaPaths[s.OutputSchemaPath]++
		}
		delete(want, s.Name)
	}
	for name := range want {
		t.Errorf("roster is missing specialist %q", name)
	}
	if len(schemaPaths) != 1 {
		t.Errorf("diagnosers reference %d distinct schema files, want 1: %v", len(schemaPaths), schemaPaths)
	}
	shared := filepath.Join("..", "..", "examples", "workloads", "gke-triage", "schemas", "finding.json")
	if n := schemaPaths[shared]; n != 12 {
		t.Errorf("%d diagnosers reference %s, want 12 (paths seen: %v)", n, shared, schemaPaths)
	}
	if len(executors) != 1 || executors[0] != "change-executor" {
		t.Errorf("specialists declaring capability: change_executor = %v, want [change-executor] — this roster's write surface is one specialist", executors)
	}
}

// TestLoadFile_Capability covers the W2.4 declaration: absent defaults
// to read_only, change_executor parses, and anything else is refused at
// load time.
//
// The refusal is the point. A misspelled `capability: change-executor`
// defaulted to read_only would be a write declaration that silently did
// not take, and the roster would fail later — at the capability check,
// naming a tool rather than the typo, or worse, not at all if the
// specialist happens to hold no write tools yet.
func TestLoadFile_Capability(t *testing.T) {
	const body = "---\ndescription: d\n%s---\nbody\n"
	tests := []struct {
		name    string
		line    string
		want    specialists.Capability
		wantErr bool
	}{
		{"absent defaults to read_only", "", specialists.CapabilityReadOnly, false},
		{"explicit read_only", "capability: read_only\n", specialists.CapabilityReadOnly, false},
		{"change_executor", "capability: change_executor\n", specialists.CapabilityChangeExecutor, false},
		{"a near miss is refused", "capability: change-executor\n", "", true},
		{"an invented value is refused", "capability: admin\n", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTempTmpl(t, dir, "s.tmpl", fmt.Sprintf(body, tc.line))
			spec, err := specialists.LoadFile(filepath.Join(dir, "s.tmpl"))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LoadFile(%q) = nil error, want a refusal — an unrecognized capability is a declaration that did not take", tc.line)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			if spec.Capability != tc.want {
				t.Fatalf("Capability = %q, want %q", spec.Capability, tc.want)
			}
		})
	}
}

// A tier is refused on the same grounds a capability is: `tier: cheap`
// defaulted to the parent's model would run the whole roster on the
// frontier model and show up on the bill, not in a log.
//
// `model:` and `tier:` together are refused too. They are two answers
// to one question and there is no reading of the file that tells you
// which one lost.
func TestLoadFile_Tier(t *testing.T) {
	const body = "---\ndescription: d\n%s---\nbody\n"
	tests := []struct {
		name    string
		line    string
		want    string
		wantErr bool
	}{
		{"absent", "", "", false},
		{"small", "tier: small\n", "small", false},
		{"mid", "tier: mid\n", "mid", false},
		{"frontier", "tier: frontier\n", "frontier", false},
		{"a near miss is refused", "tier: cheap\n", "", true},
		{"case matters", "tier: Small\n", "", true},
		{"a model id is not a tier", "tier: claude-haiku-4-5\n", "", true},
		{"model and tier together are refused", "model: claude-haiku-4-5\ntier: small\n", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTempTmpl(t, dir, "s.tmpl", fmt.Sprintf(body, tc.line))
			spec, err := specialists.LoadFile(filepath.Join(dir, "s.tmpl"))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LoadFile(%q) = nil error, want a refusal", tc.line)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			if spec.Tier != tc.want {
				t.Fatalf("Tier = %q, want %q", spec.Tier, tc.want)
			}
		})
	}
}

// TestLoadFile_EmptyMCPListIsNotAbsent pins the YAML decode the whole
// deny-all spelling rests on: `mcp: []` must reach the Spec as a
// present-but-empty slice, distinct from a missing `mcp:` key.
//
// This is a test of gopkg.in/yaml.v3's behaviour more than of mast's,
// which is exactly why it is here — filterToolsets and
// CheckCapabilitySplit both branch on ToolAllowlist.InheritsAllMCP, so a
// decoder that normalized the empty sequence to nil would turn every
// `mcp: []` in the corpus from "no tools" into "every tool" with no
// other test noticing.
func TestLoadFile_EmptyMCPListIsNotAbsent(t *testing.T) {
	dir := t.TempDir()
	writeTempTmpl(t, dir, "absent.tmpl", "---\ndescription: d\n---\nbody\n")
	writeTempTmpl(t, dir, "empty.tmpl", "---\ndescription: d\ntools:\n  mcp: []\n---\nbody\n")

	absent, err := specialists.LoadFile(filepath.Join(dir, "absent.tmpl"))
	if err != nil {
		t.Fatalf("LoadFile(absent): %v", err)
	}
	if !absent.Tools.InheritsAllMCP() {
		t.Error("a spec with no tools: block does not read as inherit-all")
	}

	empty, err := specialists.LoadFile(filepath.Join(dir, "empty.tmpl"))
	if err != nil {
		t.Fatalf("LoadFile(empty): %v", err)
	}
	if empty.Tools.InheritsAllMCP() {
		t.Error("`mcp: []` reads as inherit-all; the documented deny-all spelling grants every MCP tool instead of none")
	}
	if len(empty.Tools.MCP) != 0 {
		t.Errorf("`mcp: []` decoded to %d entries, want 0", len(empty.Tools.MCP))
	}
}

func TestLoadFile_MissingDescription(t *testing.T) {
	dir := t.TempDir()
	writeTempTmpl(t, dir, "broken.tmpl", missingDescSpec)
	if _, err := specialists.LoadFile(filepath.Join(dir, "broken.tmpl")); err == nil {
		t.Fatal("expected error for missing description, got nil")
	}
}

func TestBuild(t *testing.T) {
	dir := t.TempDir()
	writeTempTmpl(t, dir, "ImagePullBackOff.tmpl", taskSpec)
	writeTempTmpl(t, dir, "triage-classifier.tmpl", classifierSpec)

	specs, err := specialists.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	// classifierSpec declares `model: gemini-2.5-flash`, so BuildAll
	// needs a resolver — a declared override with none is a build
	// error, not a silent fallback (see register_test.go).
	echo := mastagent.NewEchoModel("test-echo")
	resolve := func(string) (adkmodel.LLM, error) { return mastagent.NewEchoModel("test-echo-override"), nil }
	agents, err := specialists.BuildAll(specs, specialists.BuildOptions{Model: echo, Resolve: resolve})
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	if got, want := len(agents), 2; got != want {
		t.Fatalf("got %d agents, want %d", got, want)
	}
	for _, a := range agents {
		if a == nil {
			t.Fatal("nil agent in BuildAll result")
		}
		if a.Name() == "" {
			t.Errorf("agent has empty name")
		}
	}
}

func TestBuild_RequiresModel(t *testing.T) {
	spec := specialists.Spec{Name: "no-model", Mode: specialists.ModeTask, Description: "x", Instruction: "y"}
	if _, err := specialists.Build(spec, specialists.BuildOptions{}); err == nil {
		t.Fatal("expected error when Model is nil, got nil")
	}
}
