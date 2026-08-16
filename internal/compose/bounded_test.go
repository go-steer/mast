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
	"slices"
	"strings"
	"testing"

	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// boundedSpecFixture is the only roster shape the bounded path accepts:
// one SingleTurn specialist carrying a report contract.
func boundedSpecFixture() specialists.Spec {
	return specialists.Spec{
		Name:        "incident-report",
		Instruction: "Return one finding for the incident envelope.",
		Mode:        specialists.ModeSingleTurn,
		OutputSchema: &genai.Schema{
			Type:       genai.TypeObject,
			Properties: map[string]*genai.Schema{"severity": {Type: genai.TypeString}},
			Required:   []string{"severity"},
		},
	}
}

func boundedBundleFixture() workload.Bundle {
	return workload.Bundle{Name: "bt", Dispatch: workload.DispatchBounded}
}

// The shape builds, and it builds the one thing it promises: a root
// wrapping exactly one agent. The name is asserted because it is what
// tells an operator reading a transcript which shape produced it —
// `bt_bounded` and `bt_fanout` are the same workload spending very
// different money.
func TestBuildRootBounded(t *testing.T) {
	root, err := BuildRoot(context.Background(), RootConfig{
		Bundle: boundedBundleFixture(),
		Specs:  []specialists.Spec{boundedSpecFixture()},
		Model:  mastagent.NewEchoModel("echo"),
	})
	if err != nil {
		t.Fatalf("BuildRoot: %v", err)
	}
	if root.Name() != "bt_bounded" {
		t.Fatalf("root = %q, want bt_bounded — the bundle's dispatch: did not reach the shape", root.Name())
	}
	if got, want := agentNames(root.SubAgents()), []string{"incident-report"}; !slices.Equal(got, want) {
		t.Fatalf("root sub-agents = %v, want %v", got, want)
	}
}

// Each way to miss the contract is a refusal that names what it found,
// not a shape that silently costs more than the bundle claims.
//
// Model is deliberately nil in every case: the roster check runs before
// BuildRoot resolves a tier or opens a client, so a roster that can
// never be bounded is refused without a provider in the room. If a
// future edit moves the check below construction, these fail on a
// missing-model error instead and say so.
func TestBuildRootBoundedRefusals(t *testing.T) {
	taskSpec := boundedSpecFixture()
	taskSpec.Mode = specialists.ModeTask

	unsetMode := boundedSpecFixture()
	unsetMode.Mode = ""

	noSchema := boundedSpecFixture()
	noSchema.OutputSchema = nil

	second := boundedSpecFixture()
	second.Name = "second-opinion"

	plannerBundle := boundedBundleFixture()
	plannerBundle.Planner.Enabled = true

	tests := []struct {
		name   string
		bundle workload.Bundle
		specs  []specialists.Spec
		want   []string
	}{{
		name:   "two specialists need a chooser, and the chooser is the orchestrator",
		bundle: boundedBundleFixture(),
		specs:  []specialists.Spec{boundedSpecFixture(), second},
		want:   []string{"2 specialists", "incident-report, second-opinion", "exactly one"},
	}, {
		name:   "an empty roster is not one specialist either",
		bundle: boundedBundleFixture(),
		specs:  nil,
		want:   []string{"0 specialists", "none"},
	}, {
		name:   "a Task specialist runs a tool loop, so the step count stops being a number",
		bundle: boundedBundleFixture(),
		specs:  []specialists.Spec{taskSpec},
		want:   []string{"incident-report", "mode Task", "mode: SingleTurn"},
	}, {
		// The error has to name the default rather than a `mode:` line
		// the file does not contain, or the operator goes looking for
		// something to change and finds nothing.
		name:   "an unset mode is Task, and the error says so",
		bundle: boundedBundleFixture(),
		specs:  []specialists.Spec{unsetMode},
		want:   []string{"mode Task (the default for a spec with no mode:)"},
	}, {
		name:   "without a schema there is nothing to force and nobody to read prose",
		bundle: boundedBundleFixture(),
		specs:  []specialists.Spec{noSchema},
		want:   []string{"incident-report", "output_schema"},
	}, {
		name:   "a planner is the orchestrator this shape is defined by not having",
		bundle: plannerBundle,
		specs:  []specialists.Spec{boundedSpecFixture()},
		want:   []string{"planner.enabled", "keep one of the two"},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildRoot(context.Background(), RootConfig{Bundle: tc.bundle, Specs: tc.specs})
			if err == nil {
				t.Fatal("BuildRoot accepted the roster; want a refusal")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// RosterShape must never return bounded, and this is the roster where
// that is tempting: one SingleTurn specialist with a declared schema is
// exactly what a bounded bundle holds. See RosterShape's own comment for
// why inferring it would be wrong; this pins the decision so a later
// "helpful" inference has to delete a test that explains itself.
func TestRosterShapeNeverInfersBounded(t *testing.T) {
	specs := []specialists.Spec{boundedSpecFixture()}
	if got := RosterShape(specs); got != DispatchCoordinator {
		t.Fatalf("RosterShape = %q, want %q — a bounded-shaped roster must still be asked for by name", got, DispatchCoordinator)
	}
	// And auto builds that same roster as a coordinator, not a bounded
	// root: the shape inference and the build have to agree, or `auto`
	// means one thing to the resolver and another to the operator.
	root, err := BuildRoot(context.Background(), RootConfig{
		Bundle:   workload.Bundle{Name: "bt"},
		Specs:    specs,
		Model:    mastagent.NewEchoModel("echo"),
		Dispatch: DispatchAuto,
	})
	if err != nil {
		t.Fatalf("BuildRoot: %v", err)
	}
	if root.Name() == "bt_bounded" {
		t.Fatal("auto dispatch built the bounded shape; bounded is opt-in by name only")
	}
}

// The bounded shape is opt-in, but it is not opt-in twice: a caller
// passing DispatchBounded gets it even when the bundle says nothing,
// because that is how `--dispatch=bounded` reaches a bundle an operator
// is trying the shape against.
func TestBuildRootBoundedFromTheCaller(t *testing.T) {
	root, err := BuildRoot(context.Background(), RootConfig{
		Bundle:   workload.Bundle{Name: "bt"},
		Specs:    []specialists.Spec{boundedSpecFixture()},
		Model:    mastagent.NewEchoModel("echo"),
		Dispatch: DispatchBounded,
	})
	if err != nil {
		t.Fatalf("BuildRoot: %v", err)
	}
	if root.Name() != "bt_bounded" {
		t.Fatalf("root = %q, want bt_bounded", root.Name())
	}
}

// CheckRoster is the pre-flight the binary runs before it wires MCP, so
// what matters about it is that it refuses the same rosters BuildRoot
// does and accepts everything else without needing a model — including
// the shapes it does not check at all.
func TestCheckRoster(t *testing.T) {
	tests := []struct {
		name     string
		bundle   workload.Bundle
		specs    []specialists.Spec
		dispatch Dispatch
		wantErr  string
	}{{
		name:     "a bounded roster that keeps the contract",
		bundle:   boundedBundleFixture(),
		specs:    []specialists.Spec{boundedSpecFixture()},
		dispatch: DispatchAuto,
	}, {
		// The refusal has to survive the bundle being the only thing
		// that names the shape: that is how a scheduled workload gets
		// here, with no operator typing a flag.
		name:     "the bundle's own dispatch: is what is checked",
		bundle:   boundedBundleFixture(),
		specs:    []specialists.Spec{boundedSpecFixture(), func() specialists.Spec { s := boundedSpecFixture(); s.Name = "second-opinion"; return s }()},
		dispatch: DispatchAuto,
		wantErr:  "2 specialists",
	}, {
		name:     "an unknown shape is refused before anything is built",
		bundle:   workload.Bundle{Name: "bt"},
		specs:    []specialists.Spec{boundedSpecFixture()},
		dispatch: Dispatch("sideways"),
		wantErr:  `unknown dispatch "sideways"`,
	}, {
		// Not this check's business. A fourteen-specialist coordinator
		// is an ordinary workload, and a pre-flight that grew opinions
		// about the other shapes would start refusing rosters BuildRoot
		// accepts — two answers to one question again.
		name:     "the same roster under another shape is nobody's problem here",
		bundle:   workload.Bundle{Name: "bt"},
		specs:    []specialists.Spec{boundedSpecFixture(), func() specialists.Spec { s := boundedSpecFixture(); s.Name = "second-opinion"; return s }()},
		dispatch: DispatchCoordinator,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckRoster(tc.bundle, tc.specs, tc.dispatch)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckRoster = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("CheckRoster = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
