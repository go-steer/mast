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
	"errors"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/taskclass"
)

// The portability claim, stated as a test: one roster declaration,
// `tier: small`, resolves to the cheap model of whichever provider the
// operator started mast with. If this table ever resolves a gemini root
// to a claude id (or vice versa), a tiered bundle silently reaches for
// the wrong credentials mid-incident.
func TestTierModelName_FollowsTheRunningProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		root     string
		tier     string
		want     string
	}{
		{"gemini root, no alias", "", "gemini-3.6-flash", taskclass.TierSmall, "gemini-2.5-flash"},
		{"claude root, no alias", "", "claude-opus-4-7", taskclass.TierSmall, "claude-haiku-4-5"},
		{"gemini root, mid", "", "gemini-3.6-flash", taskclass.TierMid, "gemini-3.5-flash"},
		{"claude root, frontier", "", "claude-haiku-4-5", taskclass.TierFrontier, "claude-opus-4-7"},
		{"explicit vertex alias", "vertex", "gemini-3.6-flash", taskclass.TierSmall, "gemini-2.5-flash"},
		{"explicit anthropic-vertex alias", "anthropic-vertex", "claude-opus-4-7", taskclass.TierMid, "claude-sonnet-4-6"},
		// The alias wins over the root's prefix. It is the operator's
		// explicit statement of which provider this run is against.
		{"alias beats the prefix", "anthropic", "gemini-3.6-flash", taskclass.TierSmall, "claude-haiku-4-5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TierModelName(tc.provider, tc.root, tc.tier)
			if err != nil {
				t.Fatalf("TierModelName(%q, %q, %q): %v", tc.provider, tc.root, tc.tier, err)
			}
			if got != tc.want {
				t.Errorf("TierModelName(%q, %q, %q) = %q, want %q", tc.provider, tc.root, tc.tier, got, tc.want)
			}
		})
	}
}

// Every unresolvable shape is an error, never a fall-through to the
// root model: a tier that quietly became "whatever the parent runs on"
// is the fiction W1.1 removed for `model:`.
func TestTierModelName_RefusesWhatItCannotResolve(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		root     string
		tier     string
	}{
		{"unknown tier", "", "gemini-3.6-flash", "cheap"},
		{"empty tier", "", "gemini-3.6-flash", ""},
		{"unrecognizable root and no alias", "", "some-other-model", taskclass.TierSmall},
		{"offline fake root and no alias", "", "echo", taskclass.TierSmall},
		{"provider with no tier table", "openai", "gpt-9", taskclass.TierSmall},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TierModelName(tc.provider, tc.root, tc.tier)
			if err == nil {
				t.Fatalf("TierModelName(%q, %q, %q) = %q, want an error", tc.provider, tc.root, tc.tier, got)
			}
		})
	}
}

// A tiered roster must still run credential-free under --model=echo,
// on the same condition and for the same reason `model:` overrides
// collapse: the whole process is a test double and there is nothing to
// tier. Note that the collapse has to happen *before* the mapping —
// "echo" has no provider family, so a resolver that mapped first would
// fail the offline S/U/E tiers instead of collapsing.
func TestNewTierResolver_OfflineFakeRootCollapses(t *testing.T) {
	for _, rootName := range []string{"echo", "mast-echo", "toolactor", "mast-toolactor", "scripted"} {
		t.Run(rootName, func(t *testing.T) {
			root := mastagent.NewEchoModel(rootName)
			resolveTier := NewTierResolver(context.Background(), "", rootName, root, nil, nil)
			got, err := resolveTier(taskclass.TierSmall)
			if err != nil {
				t.Fatalf("resolve tier under offline root: %v", err)
			}
			if got != root {
				t.Errorf("tier resolved to %v, want the root model back", got)
			}
		})
	}
}

// The tier resolver goes through the memoized model resolver rather
// than building its own client: twelve small-tier diagnosers are one
// provider client, not twelve.
func TestNewTierResolver_GoesThroughTheModelResolver(t *testing.T) {
	var asked []string
	fake := mastagent.NewEchoModel("stand-in")
	resolve := func(name string) (model.LLM, error) {
		asked = append(asked, name)
		return fake, nil
	}
	resolveTier := NewTierResolver(context.Background(), "", "claude-opus-4-7", mastagent.NewEchoModel("root"), resolve, nil)
	for range 12 {
		got, err := resolveTier(taskclass.TierSmall)
		if err != nil {
			t.Fatalf("resolve tier: %v", err)
		}
		if got != fake {
			t.Fatalf("tier resolved to %v, want the model resolver's answer", got)
		}
	}
	if len(asked) != 12 {
		t.Fatalf("model resolver called %d times, want 12", len(asked))
	}
	for _, name := range asked {
		if name != "claude-haiku-4-5" {
			t.Fatalf("model resolver asked for %q, want claude-haiku-4-5 (the small tier for a claude root)", name)
		}
	}
}

// An unresolvable tier surfaces as an error from the resolver, so
// specialists.Build fails the roster instead of running it on the
// parent's model.
func TestNewTierResolver_PropagatesFailures(t *testing.T) {
	root := mastagent.NewEchoModel("claude-opus-4-7")
	sentinel := errors.New("no credentials")
	failing := func(string) (model.LLM, error) { return nil, sentinel }

	if _, err := NewTierResolver(context.Background(), "", "claude-opus-4-7", root, failing, nil)(taskclass.TierSmall); !errors.Is(err, sentinel) {
		t.Errorf("resolver error = %v, want the underlying %v", err, sentinel)
	}
	if _, err := NewTierResolver(context.Background(), "", "claude-opus-4-7", root, nil, nil)(taskclass.TierSmall); err == nil {
		t.Error("a tier with no model resolver behind it should be an error, not the root model")
	}
	_, err := NewTierResolver(context.Background(), "", "claude-opus-4-7", root, failing, nil)("cheap")
	if err == nil || !strings.Contains(err.Error(), "cheap") {
		t.Errorf("unknown tier error = %v, want one naming the tier", err)
	}
}

// SpecModelName is the single answer to "what does this spec run on",
// shared by the build path and the pricing path. The two derived it
// separately once; the tier half of the bill was wrong until they did
// not.
func TestSpecModelName(t *testing.T) {
	tests := []struct {
		name string
		spec specialists.Spec
		want string
	}{
		{"no declaration inherits", specialists.Spec{Name: "plain"}, ""},
		{"model wins as written", specialists.Spec{Name: "pinned", Model: "claude-haiku-4-5"}, "claude-haiku-4-5"},
		{"tier resolves", specialists.Spec{Name: "cheap", Tier: taskclass.TierSmall}, "gemini-2.5-flash"},
		{"unresolvable tier inherits for pricing", specialists.Spec{Name: "bogus", Tier: "cheap"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SpecModelName(tc.spec, "", "gemini-3.6-flash"); got != tc.want {
				t.Errorf("SpecModelName = %q, want %q", got, tc.want)
			}
		})
	}
}
