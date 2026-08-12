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
	"testing"

	mastagent "github.com/go-steer/mast/pkg/agent"
)

// TestNewModelResolver_OfflineFakeRootCollapses is the guard on the
// offline test tiers: a bundle that declares real per-specialist tiers
// must still run credential-free under --model=echo (and the scripted /
// toolactor doubles the UAT and eval harnesses drive). Without the
// collapse, tiering a bundle would make `scripts/demo-spike2.sh` and
// every U/E-tier scenario reach for an Anthropic key.
func TestNewModelResolver_OfflineFakeRootCollapses(t *testing.T) {
	for _, rootName := range []string{"echo", "mast-echo", "toolactor", "mast-toolactor", "scripted"} {
		t.Run(rootName, func(t *testing.T) {
			root := mastagent.NewEchoModel(rootName)
			resolve := NewModelResolver(context.Background(), "", rootName, root, nil)
			got, err := resolve("claude-haiku-4-5")
			if err != nil {
				t.Fatalf("resolve under offline root: %v", err)
			}
			if got != root {
				t.Errorf("override resolved to %v, want the root model back", got)
			}
		})
	}
}

// TestNewModelResolver_MatchingNameReusesRoot avoids constructing a
// second client for the tier the parent already runs on.
func TestNewModelResolver_MatchingNameReusesRoot(t *testing.T) {
	root := mastagent.NewEchoModel("gemini-3.5-flash")
	resolve := NewModelResolver(context.Background(), "", "gemini-3.5-flash", root, nil)
	got, err := resolve("gemini-3.5-flash")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != root {
		t.Error("an override naming the root model built a second model")
	}
}

// TestNewModelResolver_Memoizes pins the one-client-per-tier property:
// a roster of eight analysts on one tier must not open eight clients.
func TestNewModelResolver_Memoizes(t *testing.T) {
	// A non-fake root name so the collapse path is not what's under
	// test; "echo" as the override keeps the resolution credential-free.
	resolve := NewModelResolver(context.Background(), "", "gemini-3.5-flash", mastagent.NewEchoModel("root"), nil)
	first, err := resolve("echo")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	second, err := resolve("echo")
	if err != nil {
		t.Fatalf("resolve (second): %v", err)
	}
	if first != second {
		t.Error("resolver built a second model for the same model id")
	}
}

// TestNewModelResolver_UnknownModel keeps a typo in frontmatter loud at
// construction rather than at first call mid-incident.
func TestNewModelResolver_UnknownModel(t *testing.T) {
	resolve := NewModelResolver(context.Background(), "", "gemini-3.5-flash", mastagent.NewEchoModel("root"), nil)
	if _, err := resolve("clod-hakiu-4-5"); err == nil {
		t.Fatal("expected an error for an unknown model id, got nil")
	}
}

func TestIsOfflineFake(t *testing.T) {
	for _, name := range []string{"echo", "mast-echo", "toolactor", "mast-toolactor", "scripted"} {
		if !IsOfflineFake(name) {
			t.Errorf("IsOfflineFake(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "gemini-3.5-flash", "claude-haiku-4-5", "echoes"} {
		if IsOfflineFake(name) {
			t.Errorf("IsOfflineFake(%q) = true, want false", name)
		}
	}
}
