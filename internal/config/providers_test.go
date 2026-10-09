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

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProvider(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadProviders(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "providers")
	writeProvider(t, dir, "house-vllm.yaml", `
name: house-vllm
extends: vllm
base_url: http://vllm.infra.svc:8000/v1
usage: {cached_tokens: unreliable}
models:
  - {id: Qwen/Qwen3-Coder-Next, tier: mid, context_window: 262144}
tiers: {mid: Qwen/Qwen3-Coder-Next}
`)
	writeProvider(t, dir, "notes.txt", "not a profile")
	got, err := loadProviders(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := got["house-vllm"]
	if len(got) != 1 || !ok || p.Extends != "vllm" || p.Tiers["mid"] != "Qwen/Qwen3-Coder-Next" {
		t.Fatalf("loaded %+v", got)
	}

	missing, err := loadProviders(filepath.Join(t.TempDir(), "absent"))
	if err != nil || len(missing) != 0 {
		t.Errorf("a missing dir = %v, %v; want no profiles and no error", missing, err)
	}
}

func TestLoadProvidersRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"misspelled key": {map[string]string{"a.yaml": "name: a\nextends: ollama\nbase_ulr: http://x\n"}, "base_ulr"},
		"no name":        {map[string]string{"a.yaml": "extends: ollama\n"}, "has no name"},
		"bad shape":      {map[string]string{"a.yaml": "name: a\nextends: vllm\n"}, "base_url is required"},
		"unknown base":   {map[string]string{"a.yaml": "name: a\nextends: vlm\n"}, `extends "vlm"`},
		"collision": {map[string]string{
			"a.yaml": "name: lab\nextends: ollama\n",
			"b.yml":  "name: lab\nextends: ollama\n",
		}, "defined by both"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "providers")
			for f, body := range tc.files {
				writeProvider(t, dir, f, body)
			}
			if _, err := loadProviders(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("loadProviders = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestDiscoverProvidersWithNoRootIsNotAnError(t *testing.T) {
	empty := t.TempDir()
	t.Setenv(EnvConfigDir, "")
	t.Setenv("XDG_CONFIG_HOME", empty)
	t.Setenv("HOME", empty)
	t.Chdir(empty)
	got, err := DiscoverProviders(nil)
	if err != nil || len(got) != 0 {
		t.Errorf("DiscoverProviders with no root = %v, %v", got, err)
	}
}
