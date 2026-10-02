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

// Discovery for the AG-UI server (docs/ag-ui-design.md open question 8): an
// unauthenticated GET /agui/agents.json listing every exposed workload's
// endpoint, description, input-schema hint, required scopes, and the
// optional frame families a run on it will actually contain, so a client
// (or a directory service) can enumerate what this daemon serves. Mirrors
// the A2A agent card.
//
// The document stays public, and #377 made that a decision rather than an
// inherited default. The rule it settled on: this descriptor may state what
// a client permitted to run would observe in the stream anyway, and may not
// state a property of the governance around the stream. Every key under
// capabilities passes that test — a permitted client learns reasoning from
// the first ReasoningStart and learns the state keys from the paths in the
// first StateDelta. A HITL bit would not, which is why there isn't one: an
// unauthenticated "this workload does not park mutations for a human" is a
// statement about what fires unattended here, and a caller who never gets a
// run to park never observes it.

package agui

import (
	"encoding/json"
	"net/http"
	"sort"
)

// AgentDescriptor is one workload's entry in the discovery document. Field
// names follow the AG-UI discovery convention (snake_case for the compound
// keys, matching the bundle's agui: config surface).
type AgentDescriptor struct {
	Name            string            `json:"name"`
	Endpoint        string            `json:"endpoint"`
	Description     string            `json:"description,omitempty"`
	InputSchema     map[string]any    `json:"input_schema,omitempty"`
	ProtocolVersion string            `json:"protocol_version"`
	Auth            DescriptorAuth    `json:"auth"`
	Capabilities    AgentCapabilities `json:"capabilities"`
}

// AgentCapabilities states which optional frame families a run on this
// workload will actually contain. It answers a question a client cannot
// answer from the protocol version, because these are per-workload
// publication decisions rather than server-wide ones: two workloads on the
// same daemon and the same protocol version can differ on every field here.
//
// Without it a client has one move — start a run, wait for the whole stream,
// and infer from what didn't arrive — which is indistinguishable from a
// model that simply didn't think or a run that wrote no state. That is a
// render decision it needed before the run, not after (#377).
//
// The bool fields are deliberately NOT omitempty. An absent key and an
// explicit false must be distinguishable: the first means a server that
// predates this document's capabilities object, the second is a live
// promise that the frame family will not appear. Eliding false would
// collapse those into the same JSON, which is the ambiguity the object
// exists to remove.
type AgentCapabilities struct {
	// StateDelta reports whether a run can emit StateDelta frames beyond
	// the opening StateSnapshot. False means the client will never see a
	// patch and can skip wiring a reducer at all.
	StateDelta bool `json:"state_delta"`

	// StateKeys names the top-level state keys StateDelta patches may
	// touch, in the order the operator declared them. Omitted when
	// StateDelta is false, and never a claim that every key will be
	// written — a key whose value the run never sets produces no patch.
	// It is here so a client can lay out its panels before the first
	// patch rather than growing them as keys arrive.
	StateKeys []string `json:"state_keys,omitempty"`

	// Reasoning reports whether a run can emit the REASONING_* phase
	// bracket. False means no reasoning frame of any kind, including the
	// bracket — a client showing a "thinking" affordance should not
	// render one at all.
	Reasoning bool `json:"reasoning"`
}

// CapabilityReporter is the optional interface a Backend implements to state
// what a run on a given workload will publish. A Backend that does not
// implement it advertises the zero AgentCapabilities: every frame family
// off, which is what a server with nothing to say should promise.
//
// The interface is on the Backend and not a field on ExposedWorkload for one
// reason, and it is the whole point of #377. The Backend is the object that
// builds the emitter, so the capability bits are read from the same value
// the run is driven by. A field on ExposedWorkload would be filled by
// whoever assembles the config — a second reader of the same source, free to
// drift from the first without anything failing. That is the shape of #364
// and #375 both: a capability claim that restates a config rather than
// reading the thing it describes.
type CapabilityReporter interface {
	// AGUICapabilities reports the frame families a run on workloadName
	// can contain. An unknown workload name gets the zero value.
	AGUICapabilities(workloadName string) AgentCapabilities
}

// DescriptorAuth describes the auth a workload's endpoint requires. Required
// is true whenever the server has a validator configured; Scopes lists the
// per-workload scopes a caller additionally needs.
type DescriptorAuth struct {
	Required bool     `json:"required"`
	Scopes   []string `json:"scopes,omitempty"`
}

// handleDiscovery serves GET /agui/agents.json: the JSON array of exposed
// workload descriptors, sorted by endpoint for a deterministic document.
// Public (unauthenticated) — see the file comment for the rule that keeps it
// public now that it carries capabilities.
func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	// Resolved once for the whole document rather than per entry: a Backend
	// either reports capabilities or it does not, and the type assertion is
	// not the measurement — AGUICapabilities is.
	reporter, _ := s.cfg.Backend.(CapabilityReporter)
	out := make([]AgentDescriptor, 0, len(s.byPath))
	for _, ew := range s.byPath {
		var caps AgentCapabilities
		if reporter != nil {
			caps = reporter.AGUICapabilities(ew.WorkloadName)
		}
		if !caps.StateDelta {
			// Keys without patches would describe a channel that cannot
			// open. Cleared here rather than trusted from the reporter so
			// the document cannot contradict itself whatever it returns.
			caps.StateKeys = nil
		}
		out = append(out, AgentDescriptor{
			Name:            ew.WorkloadName,
			Endpoint:        ew.EndpointPath,
			Description:     ew.Description,
			InputSchema:     ew.InputSchema,
			ProtocolVersion: ProtocolVersion,
			Auth: DescriptorAuth{
				Required: s.authOn,
				Scopes:   ew.Scopes,
			},
			Capabilities: caps,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Endpoint < out[j].Endpoint })

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
