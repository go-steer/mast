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
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"

	mastagent "github.com/go-steer/mast/internal/agent"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// Toolsets a custom main.go supplies (cli.WithToolset) are reached the way
// an MCP server is, so the startup checks that guard MCP allowlists have to
// count them — or a workload whose tools all come from the host would pass
// every check by declaring nothing.

func TestAHostToolsetNameIsADeclaredServer(t *testing.T) {
	if err := CheckMCPServerNames(workload.Bundle{}, []specialists.Spec{allows("w", "tickets")}, "tickets"); err != nil {
		t.Fatalf("an allowlist naming a host toolset was refused: %v", err)
	}
}

func TestAMistypedHostToolsetNameIsRefused(t *testing.T) {
	err := CheckMCPServerNames(workload.Bundle{}, []specialists.Spec{allows("w", "tickts")}, "tickets")
	if err == nil {
		t.Fatal("a typo'd host toolset name was accepted; with no MCP catalog the bundle used to be exempt, and the host toolset is what ends that")
	}
	for _, want := range []string{`"tickts"`, `this binary supplies toolsets "tickets"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q:\n%s", want, err)
		}
	}
}

func TestHostToolsetNamesThatCannotBeToldApartAreRefused(t *testing.T) {
	cases := []struct {
		name   string
		bundle workload.Bundle
		hosts  []string
		want   string
	}{
		{"an empty name", workload.Bundle{}, []string{""}, "empty Name()"},
		{"two with one name", workload.Bundle{}, []string{"tickets", "tickets"}, `both named "tickets"`},
		{"a name an MCP server has", catalog("gke"), []string{"gke"}, `already declares as an MCP server`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckHostToolsets(tc.bundle, tc.hosts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckHostToolsets = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if err := CheckHostToolsets(catalog("gke"), []string{"tickets", "pager"}); err != nil {
		t.Fatalf("distinct host names beside an MCP server were refused: %v", err)
	}
}

func TestAReadOnlySpecialistCannotInheritHostToolsets(t *testing.T) {
	// No tools.mcp key at all: in a workload with tools to inherit, that
	// grants every one of them. Before host toolsets counted, a bundle
	// with no MCP catalog had nothing to inherit as far as this check
	// knew, and a read-only diagnoser would have been handed the host's
	// mutating tools without a word.
	ro := specialists.Spec{Name: "diagnoser", Mode: specialists.ModeTask}
	pred := MutationPredicate(workload.Bundle{}, nil)
	if err := CheckCapabilitySplit(workload.Bundle{}, []specialists.Spec{ro}, pred, nil); err != nil {
		t.Fatalf("with nothing to inherit the roster was refused: %v", err)
	}
	err := CheckCapabilitySplit(workload.Bundle{}, []specialists.Spec{ro}, pred, nil, "tickets")
	if err == nil || !strings.Contains(err.Error(), "declares no tools.mcp allowlist") {
		t.Fatalf("CheckCapabilitySplit = %v, want the inherit-all refusal", err)
	}
}

func TestTheBinarysModelLookupIsAskedAndRemembered(t *testing.T) {
	root := mastagent.NewEchoModel("gemini-3.5-flash")
	acme := mastagent.NewEchoModel("acme-large")
	asked := 0
	lookup := func(name string) (model.LLM, bool) {
		asked++
		if name == "acme-large" {
			return acme, true
		}
		return nil, false
	}
	resolve := NewModelResolver(context.Background(), "", "gemini-3.5-flash", root, workload.BuiltinTools{}, nil, lookup)
	for i := 0; i < 2; i++ {
		got, err := resolve("acme-large")
		if err != nil || got != acme {
			t.Fatalf("resolve(acme-large) = %v, %v; want the binary's model", got, err)
		}
	}
	if asked != 1 {
		t.Errorf("lookup asked %d times for one name, want 1 (memoized like any model)", asked)
	}
	if got, _ := resolve("gemini-3.5-flash"); got != root {
		t.Error("the root's own name went past the root to the lookup")
	}
}

func TestAnOfflineFakeRootOutranksTheBinarysModels(t *testing.T) {
	root := mastagent.NewEchoModel("echo")
	lookup := func(string) (model.LLM, bool) { return mastagent.NewEchoModel("acme"), true }
	resolve := NewModelResolver(context.Background(), "", "echo", root, workload.BuiltinTools{}, nil, lookup)
	if got, _ := resolve("acme-large"); got != root {
		t.Error("under --model=echo a specialist's model went to the binary instead of collapsing to the fake")
	}
}
