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

	mastagent "github.com/go-steer/mast/pkg/agent"

	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

func catalog(servers ...string) workload.Bundle {
	refs := make([]workload.MCPServerRef, 0, len(servers))
	for _, s := range servers {
		refs = append(refs, workload.MCPServerRef{Server: s})
	}
	return workload.Bundle{ToolCatalog: workload.ToolCatalog{MCP: refs}}
}

func allows(name string, servers ...string) specialists.Spec {
	al := make([]specialists.MCPAllowlist, 0, len(servers))
	for _, s := range servers {
		al = append(al, specialists.MCPAllowlist{Server: s, Tools: []string{"get_k8s_resource"}})
	}
	return specialists.Spec{Name: name, Tools: specialists.ToolAllowlist{MCP: al}}
}

func TestAMistypedServerNameIsRefusedAtStartup(t *testing.T) {
	b := catalog("gke", "logging", "monitoring")
	err := CheckMCPServerNames(b, []specialists.Spec{allows("stuck-pod", "gke", "loggging")})
	if err == nil {
		t.Fatal("a specialist naming a server the workload does not have was accepted")
	}
	msg := err.Error()
	// An operator reading this at 3am needs three things: which
	// specialist, which name is wrong, and what the right names are.
	for _, want := range []string{`"stuck-pod"`, `"loggging"`, `"gke", "logging", "monitoring"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not contain %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, `"gke", "loggging"`) {
		t.Errorf("error blames a server the specialist named correctly:\n%s", msg)
	}
}

func TestAServerTheWorkloadDeclaresIsFine(t *testing.T) {
	b := catalog("gke", "logging")
	specs := []specialists.Spec{allows("a", "gke"), allows("b", "gke", "logging")}
	if err := CheckMCPServerNames(b, specs); err != nil {
		t.Fatalf("a correct roster was refused: %v", err)
	}
}

func TestASpecialistThatNamesNoServerIsFine(t *testing.T) {
	b := catalog("gke")
	// Both spellings the capability split cares about: inherit-all (no
	// tools.mcp key) and the deny-all `mcp: []`. Neither names a server,
	// so neither can misname one.
	inherit := specialists.Spec{Name: "inherit"}
	denyAll := specialists.Spec{Name: "deny", Tools: specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{}}}
	if err := CheckMCPServerNames(b, []specialists.Spec{inherit, denyAll}); err != nil {
		t.Fatalf("refused a roster that names no server: %v", err)
	}
}

func TestABundleWithNoCatalogIsExempt(t *testing.T) {
	// Without tool_catalog.mcp the bundle has not said what exists, so
	// nothing here can tell a typo from a server this deployment does
	// not carry. A library embed that passes its own toolsets is in the
	// same position. Both fall through to the runtime warning.
	if err := CheckMCPServerNames(workload.Bundle{}, []specialists.Spec{allows("s", "anything")}); err != nil {
		t.Fatalf("refused a specialist in a bundle that declares no catalog: %v", err)
	}
}

func TestEveryUnknownServerOnTheSpecialistIsNamed(t *testing.T) {
	b := catalog("gke")
	err := CheckMCPServerNames(b, []specialists.Spec{allows("s", "netwrk", "gke", "bigqery")})
	if err == nil {
		t.Fatal("accepted two unknown servers")
	}
	msg := err.Error()
	// Reported together and sorted: fixing one typo and rerunning to
	// find the next is the loop this check exists to remove.
	if !strings.Contains(msg, `"bigqery", "netwrk"`) {
		t.Errorf("error does not name both unknown servers together:\n%s", msg)
	}
	if !strings.Contains(msg, "servers") {
		t.Errorf("error is not pluralized for two:\n%s", msg)
	}
}

func TestTheRefusalReachesBuildRoot(t *testing.T) {
	// The check is worth nothing if it is not wired. This is the same
	// assertion the capability split's own test makes, for the same
	// reason: a check called from nowhere passes every test it has.
	spec := readOnlySpec("stuck-pod", "get_k8s_resource")
	spec.Tools.MCP[0].Server = "loggging"
	_, _, err := BuildRoot(context.Background(), RootConfig{
		Bundle:   triageCatalog(),
		Specs:    []specialists.Spec{spec},
		Model:    mastagent.NewEchoModel("echo"),
		Dispatch: DispatchCoordinator,
	})
	if err == nil || !strings.Contains(err.Error(), `"loggging"`) {
		t.Fatalf("BuildRoot did not refuse the mistyped server name: %v", err)
	}
}
