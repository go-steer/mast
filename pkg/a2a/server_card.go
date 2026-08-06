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

// Agent-card publication for the A2A server (docs/a2a-design.md,
// "Endpoint layout"): an aggregated card at /.well-known/agent-card.json
// exposing every opted-in workload as a skill, plus optional
// per-workload cards at /.well-known/agent-card/<name>.json for
// registries that require distinct endpoints per agent.

package a2a

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// handleAggregatedCard serves the one-card-per-instance discovery
// document: all exposed workloads as skills within a single card. Public
// (unauthenticated) — registries fetch cards at registration time and
// the card is a descriptor, not a capability.
func (s *Server) handleAggregatedCard(w http.ResponseWriter, r *http.Request) {
	card := s.buildCard(s.cfg.CardName, s.cfg.CardDescription, s.cfg.Skills, requestOrigin(r, s.cfg.ExternalURL))
	writeCard(w, card)
}

// handlePerWorkloadCard serves a single workload's card. The path value
// carries the workload name, tolerating a trailing ".json".
func (s *Server) handlePerWorkloadCard(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(r.PathValue("name"), ".json")
	skill, ok := s.byWork[name]
	if !ok {
		http.Error(w, "no such exposed workload", http.StatusNotFound)
		return
	}
	desc := skill.Description
	if desc == "" {
		desc = s.cfg.CardDescription
	}
	card := s.buildCard(skill.SkillName, desc, []ExposedSkill{skill}, requestOrigin(r, s.cfg.ExternalURL))
	writeCard(w, card)
}

// buildCard projects config + the given skills into the wire card. Pure;
// no I/O. base is the already-resolved request/override origin
// (scheme://host); the card's url points at the JSON-RPC endpoint.
func (s *Server) buildCard(name, description string, skills []ExposedSkill, base string) AgentCard {
	url := base
	if base != "" {
		url = strings.TrimRight(base, "/") + "/a2a"
	}
	card := AgentCard{
		Name:               name,
		Description:        description,
		URL:                url,
		Version:            s.cfg.CardVersion,
		ProtocolVersion:    ProtocolVersion,
		PreferredTransport: TransportJSONRPC,
		// Streaming flips true when message/stream lands (Stage C).
		Capabilities:       Capabilities{Streaming: false},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             cardSkills(skills),
	}
	if s.authOn {
		card.SecuritySchemes = &SecuritySchemes{Bearer: &SecurityScheme{Type: "http", Scheme: "Bearer"}}
		card.Security = []map[string][]string{{"bearer": {}}}
	}
	return card
}

// cardSkills projects exposed skills into wire AgentSkills, sorted by id
// for a deterministic card. Note: input/output JSON Schemas are NOT
// emitted — spec AgentSkill has no schema fields (docs/a2a-design.md);
// the daemon folds any schema hints into Description before it gets here.
func cardSkills(skills []ExposedSkill) []AgentSkill {
	out := make([]AgentSkill, 0, len(skills))
	for _, sk := range skills {
		tags := sk.Tags
		if len(tags) == 0 {
			tags = []string{"mast"}
		}
		out = append(out, AgentSkill{
			ID:          sk.SkillName,
			Name:        sk.SkillName,
			Description: sk.Description,
			Tags:        tags,
			InputModes:  []string{"text/plain"},
			OutputModes: []string{"text/plain"},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// requestOrigin derives the card's origin from the request (override
// wins). X-Forwarded-Proto/Host take precedence for proxied deploys.
// These headers are forgeable, but the card is a public descriptor a
// caller fetched for itself — a bogus url is self-inflicted.
func requestOrigin(r *http.Request, override string) string {
	if override != "" {
		return override
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

// writeCard emits a card as indented JSON.
func writeCard(w http.ResponseWriter, card AgentCard) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(card)
}
