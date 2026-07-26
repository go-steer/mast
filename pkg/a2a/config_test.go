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

package a2a

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeYAML(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "external-triage.yaml", `
name: external-triage
agent_card_url: https://triage.example.com/.well-known/agent-card.json
skills: [investigate-incident]
auth:
  type: bearer
  token_env: EXTERNAL_TRIAGE_TOKEN
timeout_seconds: 300
`)
	writeYAML(t, dir, "scanner.yml", `
name: scanner
endpoint: https://scanner.example.com/a2a
`)
	writeYAML(t, dir, "notes.txt", "not yaml, ignored")

	cfgs, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("loaded %d configs, want 2", len(cfgs))
	}
	byName := map[string]AgentConfig{}
	for _, c := range cfgs {
		byName[c.Name] = c
	}
	et := byName["external-triage"]
	if et.AgentCardURL == "" || et.Auth == nil || et.Auth.TokenEnv != "EXTERNAL_TRIAGE_TOKEN" {
		t.Errorf("external-triage = %+v", et)
	}
	if et.Timeout() != 300*time.Second {
		t.Errorf("Timeout() = %v, want 300s", et.Timeout())
	}
	if len(et.Skills) != 1 || et.Skills[0] != "investigate-incident" {
		t.Errorf("Skills = %v", et.Skills)
	}
	if !strings.HasSuffix(et.Filename, "external-triage.yaml") {
		t.Errorf("Filename = %q", et.Filename)
	}
	if byName["scanner"].Timeout() != DefaultTimeout {
		t.Errorf("scanner Timeout() = %v, want DefaultTimeout", byName["scanner"].Timeout())
	}
}

func TestLoadDirMissingIsEmpty(t *testing.T) {
	cfgs, err := LoadDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil || len(cfgs) != 0 {
		t.Fatalf("LoadDir(missing) = (%v, %v), want (empty, nil)", cfgs, err)
	}
}

func TestLoadDirValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // substring of the fatal error
	}{
		{"missing name", "agent_card_url: https://x.example.com\n", "missing required field name"},
		{"uppercase name", "name: Not-Lowercase\nendpoint: https://x.example.com/a2a\n", "must be lowercase"},
		{"no url or endpoint", "name: x\n", "one of agent_card_url or endpoint"},
		{"unsupported auth type", "name: x\nendpoint: https://x/a2a\nauth: {type: google-iam}\n", "not supported in v0.1"},
		{"bearer without token_env", "name: x\nendpoint: https://x/a2a\nauth: {type: bearer}\n", "token_env is required"},
		{"unknown key rejected", "name: x\nendpoint: https://x/a2a\nagent_card: oops\n", "field agent_card not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeYAML(t, dir, "bad.yaml", tc.body)
			_, err := LoadDir(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadDir err = %v, want substring %q", err, tc.want)
			}
		})
	}
}
