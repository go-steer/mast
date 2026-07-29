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
//
// Originally derived from go-steer/core-agent@b8dd225e9ae7fdeb3ff23772cc5be25eed34b818

package anthropic

import (
	"strings"
	"testing"
)

func TestDefaultSmallModel(t *testing.T) {
	// Use a zero-value Provider — DefaultSmallModel doesn't depend on
	// any provider state (client, builtins, etc.), so this is safe and
	// avoids the API-key requirement of the New() constructor.
	p := &Provider{}
	if got, want := p.DefaultSmallModel(), DefaultSmallModelID; got != want {
		t.Errorf("DefaultSmallModel() = %q, want %q", got, want)
	}
	if DefaultSmallModelID != "claude-haiku-4-5" {
		t.Errorf("DefaultSmallModelID = %q; expected the haiku-4-5 alias used elsewhere in the codebase", DefaultSmallModelID)
	}
}

func TestNew_RequiresAPIKey(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	_, err := New(Options{})
	if err == nil || !strings.Contains(err.Error(), "api key is required") {
		t.Fatalf("expected api-key-required error, got %v", err)
	}
}

func TestNew_APIKeyFallsBackToEnv(t *testing.T) {
	t.Setenv(EnvAPIKey, "env-key")
	p, err := New(Options{})
	if err != nil {
		t.Fatalf("New with env key: %v", err)
	}
	if p.Name() != ProviderName {
		t.Errorf("Name() = %q, want %q", p.Name(), ProviderName)
	}
}

func TestNew_CacheSystemOption(t *testing.T) {
	t.Parallel()
	p, err := New(Options{APIKey: "test-key", CacheSystem: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !p.cacheSystem {
		t.Errorf("Options.CacheSystem = true didn't take")
	}
}
