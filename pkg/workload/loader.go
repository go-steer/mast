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

package workload

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads a workload bundle YAML file, parses it, validates required
// fields, and returns the populated Bundle.
func Load(path string) (Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Bundle{}, fmt.Errorf("workload: read %q: %w", path, err)
	}
	var b Bundle
	if err := yaml.Unmarshal(data, &b); err != nil {
		return Bundle{}, fmt.Errorf("workload: parse %q: %w", path, err)
	}
	b.Filename = path
	if err := b.foldHITLPolicy(); err != nil {
		return Bundle{}, fmt.Errorf("workload: parse %q: %w", path, err)
	}
	if err := b.validate(); err != nil {
		return Bundle{}, fmt.Errorf("workload: validate %q: %w", path, err)
	}
	if b.Mode == "" {
		b.Mode = ModeSingleSession
	}
	return b, nil
}

// foldHITLPolicy collapses the documented `hitl_policy:` spelling onto
// the shipped `hitl:` field so everything downstream reads one place.
func (b *Bundle) foldHITLPolicy() error {
	if b.HITLPolicy == (HITL{}) {
		return nil
	}
	if b.HITL != (HITL{}) {
		return fmt.Errorf("both hitl: and hitl_policy: are set; they are the same block — keep one")
	}
	b.HITL = b.HITLPolicy
	b.HITLPolicy = HITL{}
	return nil
}

func (b *Bundle) validate() error {
	if b.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(b.Specialists) == 0 {
		return fmt.Errorf("specialists roster is empty (workload cannot dispatch)")
	}
	if b.Mode != "" && b.Mode != ModeSingleSession && b.Mode != ModeMultiSession {
		return fmt.Errorf("unknown mode %q (want single_session or multi_session)", b.Mode)
	}
	switch b.Dispatch {
	case "", DispatchCoordinator, DispatchGraph, DispatchFanout, DispatchBounded, DispatchAuto:
	default:
		return fmt.Errorf("unknown dispatch %q (want coordinator, graph, fanout, bounded, or auto)", b.Dispatch)
	}
	switch b.HITL.OnMutation {
	case "", OnMutationRequireApproval, OnMutationApply, OnMutationDryRun:
	default:
		return fmt.Errorf("unknown hitl.on_mutation %q (want require_approval, apply, or dry_run)", b.HITL.OnMutation)
	}
	seen := make(map[string]bool, len(b.Specialists))
	for _, name := range b.Specialists {
		if name == "" {
			return fmt.Errorf("specialists roster contains an empty entry")
		}
		if seen[name] {
			return fmt.Errorf("specialists roster contains duplicate %q", name)
		}
		seen[name] = true
	}
	if _, err := b.HITL.EffectiveChangeSetTTL(); err != nil {
		return err
	}
	seenTools := make(map[string]bool, len(b.ToolCatalog.Tools))
	for _, p := range b.ToolCatalog.Tools {
		if p.Name == "" {
			return fmt.Errorf("tool_catalog.tools contains an entry without a name")
		}
		if seenTools[p.Name] {
			return fmt.Errorf("tool_catalog.tools contains duplicate %q", p.Name)
		}
		seenTools[p.Name] = true
		if err := validatePrecondition(p); err != nil {
			return err
		}
	}
	return nil
}

// validatePrecondition checks what the bundle alone can check about a
// change-set freshness declaration (v0.4 W7). Whether the named read is
// classified read-only is checked at composition, where the mutation
// predicate exists; whether it is wired at all is checked at the moment
// it is used, where the toolsets do.
func validatePrecondition(p ToolPolicy) error {
	pre := p.Precondition
	if pre == nil {
		return nil
	}
	if strings.TrimSpace(pre.Read) == "" {
		return fmt.Errorf("tool_catalog.tools[%q].precondition names no read tool", p.Name)
	}
	if pre.Read == p.Name {
		return fmt.Errorf("tool_catalog.tools[%q].precondition reads %q, which is the tool being checked; a change cannot be its own precondition", p.Name, pre.Read)
	}
	for _, f := range pre.Fields {
		if strings.TrimSpace(f) == "" {
			return fmt.Errorf("tool_catalog.tools[%q].precondition.fields contains an empty path", p.Name)
		}
	}
	for readArg, changeArg := range pre.ArgsFrom {
		if strings.TrimSpace(readArg) == "" || strings.TrimSpace(changeArg) == "" {
			return fmt.Errorf("tool_catalog.tools[%q].precondition.args_from maps %q to %q; both sides name an argument", p.Name, readArg, changeArg)
		}
	}
	return nil
}
