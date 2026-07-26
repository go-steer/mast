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

package router_test

import (
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/router"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

func buildSpecialist(t *testing.T, name string, mode specialists.Mode) adkagent.Agent {
	t.Helper()
	spec := specialists.Spec{
		Name:        name,
		Description: "test specialist " + name,
		Mode:        mode,
		Instruction: "you are " + name,
	}
	a, err := specialists.Build(spec, specialists.BuildOptions{Model: mastagent.NewEchoModel("test-echo")})
	if err != nil {
		t.Fatalf("build specialist %q: %v", name, err)
	}
	return a
}

func TestBuild(t *testing.T) {
	bundle := workload.Bundle{
		Name:        "gke-triage",
		Description: "Autonomous triage of GKE cluster incidents.",
		Specialists: []string{"ImagePullBackOff", "_fallback"},
	}
	specs := map[string]adkagent.Agent{
		"ImagePullBackOff": buildSpecialist(t, "ImagePullBackOff", specialists.ModeTask),
		"_fallback":        buildSpecialist(t, "_fallback", specialists.ModeTask),
	}
	root, err := router.Build(router.Config{
		Bundle:      bundle,
		Specialists: specs,
		Model:       mastagent.NewEchoModel("coord-echo"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := root.Name(), "gke-triage_coordinator"; got != want {
		t.Errorf("root.Name() = %q, want %q", got, want)
	}
	if got, want := len(root.SubAgents()), 2; got != want {
		t.Fatalf("SubAgents count = %d, want %d", got, want)
	}
	// Verify the sub-agents are in bundle order (ImagePullBackOff, _fallback).
	names := []string{root.SubAgents()[0].Name(), root.SubAgents()[1].Name()}
	want := []string{"ImagePullBackOff", "_fallback"}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("SubAgents[%d].Name() = %q, want %q", i, names[i], n)
		}
	}
}

func TestBuild_MissingSpecialist(t *testing.T) {
	bundle := workload.Bundle{
		Name:        "gke-triage",
		Specialists: []string{"ImagePullBackOff", "_fallback"},
	}
	specs := map[string]adkagent.Agent{
		"ImagePullBackOff": buildSpecialist(t, "ImagePullBackOff", specialists.ModeTask),
		// _fallback missing.
	}
	_, err := router.Build(router.Config{
		Bundle:      bundle,
		Specialists: specs,
		Model:       mastagent.NewEchoModel("coord-echo"),
	})
	if err == nil {
		t.Fatal("expected error for missing specialist, got nil")
	}
	if !strings.Contains(err.Error(), "_fallback") {
		t.Errorf("error should mention the missing specialist name; got %v", err)
	}
}

func TestBuild_RequiresModel(t *testing.T) {
	bundle := workload.Bundle{Name: "b", Specialists: []string{"x"}}
	specs := map[string]adkagent.Agent{"x": buildSpecialist(t, "x", specialists.ModeTask)}
	if _, err := router.Build(router.Config{Bundle: bundle, Specialists: specs}); err == nil {
		t.Fatal("expected error for missing Model, got nil")
	}
}
