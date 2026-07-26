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
	"os"
	"path/filepath"
	"testing"

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

	model := mastagent.NewEchoModel("test-echo")
	agents, err := specialists.BuildAll(specs, specialists.BuildOptions{Model: model})
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
