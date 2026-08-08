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
// endpoint, description, input-schema hint, and required scopes, so a client
// (or a directory service) can enumerate what this daemon serves. Mirrors the
// A2A agent card — a descriptor, not a capability, hence public.

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
	Name            string         `json:"name"`
	Endpoint        string         `json:"endpoint"`
	Description     string         `json:"description,omitempty"`
	InputSchema     map[string]any `json:"input_schema,omitempty"`
	ProtocolVersion string         `json:"protocol_version"`
	Auth            DescriptorAuth `json:"auth"`
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
// Public (unauthenticated) — the descriptor advertises capability, it is not
// itself a capability.
func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	out := make([]AgentDescriptor, 0, len(s.byPath))
	for _, ew := range s.byPath {
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
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Endpoint < out[j].Endpoint })

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
