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

package main

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/agui"
	"github.com/go-steer/mast/pkg/workload"
)

// TestAGUICapabilitiesMatchWhatARunPublishes is the whole point of #377 and
// the reason the capability bits are not a second reading of the bundle. For
// each of the four bundle shapes it takes the capabilities a discovery client
// would be given and then measures what a run on that same backend actually
// publishes, and requires the two to agree.
//
// The reasoning axis is measured through a real RunAgent — a live turn with a
// model that thinks — so it covers the whole path from bundle to frame. The
// state axis is measured one layer down, against the emitter RunAgent builds
// from the same publication() value, because no scripted model in this
// harness produces the Actions.StateDelta a runtime state write rides on.
// What that leaves unmeasured is one struct literal, and it is the literal
// directly above the RunAgent call the other axis drives.
//
// Neutralize check, reported honestly: have AGUICapabilities read
// bundle.AGUI.EmitReasoning and len(bundle.AGUI.StateProjection) directly
// instead of publication(), and this test stays green — because at the moment
// of that edit the two readings still agree. That is the point rather than a
// gap: no test can catch a drift that has not happened yet, so the design has
// to make it impossible, and what this test pins is the agreement a later edit
// would have to break in two places at once to keep green.
func TestAGUICapabilitiesMatchWhatARunPublishes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		projection []string
		reasoning  bool
	}{
		{"both off (the default)", nil, false},
		{"state only", []string{"plan", "phase"}, false},
		{"reasoning only", nil, true},
		{"both on", []string{"plan"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := &workload.Bundle{
				Name: "w",
				AGUI: workload.AGUI{
					Expose:          true,
					StateProjection: tc.projection,
					EmitReasoning:   tc.reasoning,
				},
			}
			m := &aguiScriptedModel{script: func(int, *model.LLMRequest) *model.LLMResponse {
				return &model.LLMResponse{Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{Text: "the tag does not exist", Thought: true},
						{Text: "the image tag is wrong"},
					},
				}}
			}}
			h := newTurnHarness(t, m)
			b := &aguiBackend{turnDeps: h.deps(), bundle: bundle}

			// What the discovery document will say about this workload.
			caps := b.AGUICapabilities("w")

			// What a real run publishes: the reasoning axis, end to end.
			emit, got := collectEmit()
			if _, err := b.RunAgent(context.Background(), agui.RunInput{
				ThreadID: "t1", RunID: "r1", Text: "why is it crashlooping?",
			}, emit); err != nil {
				t.Fatalf("RunAgent: %v", err)
			}
			sawReasoning := false
			for _, f := range *got {
				if _, ok := f.(agui.ReasoningStart); ok {
					sawReasoning = true
				}
			}
			if caps.Reasoning != sawReasoning {
				t.Errorf("advertised reasoning = %v but the run emitted %v; frames: %v",
					caps.Reasoning, sawReasoning, aguiFrameTypes(*got))
			}

			// The state axis, against the emitter RunAgent builds, driven with
			// a write to every declared key plus one that is not declared.
			pub := b.publication()
			semit, sgot := collectEmit()
			em := &aguiEmitter{emit: semit, projection: pub.stateSet, reasoning: pub.reasoning}
			write := map[string]any{"undeclared": "x"}
			for _, k := range tc.projection {
				write[k] = "v"
			}
			em.onEvent(mkStateEvent(write))
			sawDelta := len(stateDeltas(t, *sgot)) > 0
			if caps.StateDelta != sawDelta {
				t.Errorf("advertised state_delta = %v but the emitter produced %v; frames: %v",
					caps.StateDelta, sawDelta, aguiFrameTypes(*sgot))
			}

			// Every advertised key is one the emitter will actually patch on.
			// A key it filters out would leave a client waiting forever for a
			// panel that cannot arrive.
			for _, k := range caps.StateKeys {
				if !pub.stateSet[k] {
					t.Errorf("advertised state key %q is filtered out by the emitter", k)
				}
			}
			if len(caps.StateKeys) != len(pub.stateSet) {
				t.Errorf("advertised keys %v do not cover the emitter's set %v", caps.StateKeys, pub.stateSet)
			}
		})
	}
}

// TestAGUICapabilitiesUnknownWorkload pins that the reporter answers about the
// workload it was asked about. The daemon serves one bundle today, so this can
// only fire the day that changes — which is the day a descriptor would
// otherwise begin advertising another workload's publication settings.
func TestAGUICapabilitiesUnknownWorkload(t *testing.T) {
	b := &aguiBackend{bundle: &workload.Bundle{
		Name: "w",
		AGUI: workload.AGUI{Expose: true, StateProjection: []string{"plan"}, EmitReasoning: true},
	}}
	if got := b.AGUICapabilities("someone-else"); got.StateDelta || got.Reasoning || got.StateKeys != nil {
		t.Errorf("AGUICapabilities(other workload) = %+v, want the zero value", got)
	}
	if got := (&aguiBackend{}).AGUICapabilities("w"); got.StateDelta || got.Reasoning || got.StateKeys != nil {
		t.Errorf("AGUICapabilities on a nil bundle = %+v, want the zero value", got)
	}
}

// TestAGUIBackendIsACapabilityReporter pins the wiring the discovery handler
// depends on. It resolves by type assertion at runtime, so nothing in the
// compiler notices if the method is renamed or its signature drifts — the
// descriptor would simply go quiet and advertise nothing, which is the state
// this change exists to leave behind.
func TestAGUIBackendIsACapabilityReporter(t *testing.T) {
	var b agui.Backend = &aguiBackend{}
	if _, ok := b.(agui.CapabilityReporter); !ok {
		t.Fatal("aguiBackend no longer satisfies agui.CapabilityReporter; /agui/agents.json advertises nothing")
	}
}
