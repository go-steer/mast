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

package specialists

import (
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	mastagent "github.com/go-steer/mast/pkg/agent"
)

// ModelResolver turns a specialist's `model:` frontmatter override into
// a concrete model.LLM. It exists so pkg/specialists can honor the
// override without depending on the provider packages: this package
// knows a specialist declared "claude-haiku-4-5", and nothing else about
// what that string means.
//
// Implementations are expected to memoize — a roster of eight analysts
// on the same tier should share one provider client, not open eight.
// internal/compose.NewModelResolver is the one mast ships; it resolves
// through the same BuildModel path the root model came from.
type ModelResolver func(name string) (model.LLM, error)

// BuildOptions carries the runtime bindings a Spec needs to become a
// concrete ADK agent. The model is required. Toolsets are offered to
// every built specialist but filtered through Spec.Tools.MCP first —
// see filterToolsets for the spike allowlist semantics.
type BuildOptions struct {
	// Model is the parent's model — the default every specialist runs
	// on when it declares no `model:` override.
	Model model.LLM

	// Resolve resolves per-specialist `model:` overrides. Nil is legal
	// only when no Spec in the roster declares an override; Build
	// refuses a declared override it cannot resolve rather than
	// silently running the specialist on the parent's model (the bug
	// this field fixes — see docs/v0.3-plan.md W1.1).
	Resolve ModelResolver

	Tools    []tool.Tool
	Toolsets []tool.Toolset
}

// modelFor picks the model a Spec builds with: the resolved `model:`
// override when the spec declares one, the parent's model otherwise.
//
// A declared-but-unresolvable override is a build error, never a
// fallback. Falling back would reproduce exactly the failure mode W1.1
// exists to remove — a bundle that reads as "Haiku analysts, Sonnet
// synthesis" while every specialist quietly runs on the parent's model
// and the cost story is a fiction.
func (o BuildOptions) modelFor(spec Spec) (model.LLM, error) {
	if spec.Model == "" {
		return o.Model, nil
	}
	if o.Resolve == nil {
		return nil, fmt.Errorf("specialists: build %q: model override %q declared but BuildOptions.Resolve is nil", spec.Name, spec.Model)
	}
	m, err := o.Resolve(spec.Model)
	if err != nil {
		return nil, fmt.Errorf("specialists: build %q: resolve model override %q: %w", spec.Name, spec.Model, err)
	}
	if m == nil {
		return nil, fmt.Errorf("specialists: build %q: model resolver returned nil for override %q", spec.Name, spec.Model)
	}
	return m, nil
}

// filterToolsets applies the specialist's MCP allowlist to the offered
// toolsets, matching allowlist entries to toolsets by server name.
//
// Spike semantics (docs/specialists-design.md's allowlist algebra is
// contradictory across docs — this is the interpretation spike 2
// commits to, flagged for the docs pass): an absent/empty tools.mcp
// list inherits every offered toolset; a non-empty list is a
// whitelist — unlisted servers are dropped, a listed server with no
// tools[] passes whole, a listed server with tools[] is narrowed via
// tool.FilterToolset + AllowedToolsPredicate (both stock ADK v2.1.0 —
// per-tool MCP filtering needs no mast machinery, resolving
// specialists-design open question #3).
func filterToolsets(spec Spec, offered []tool.Toolset) []tool.Toolset {
	if len(spec.Tools.MCP) == 0 {
		return offered
	}
	byServer := make(map[string]MCPAllowlist, len(spec.Tools.MCP))
	for _, al := range spec.Tools.MCP {
		byServer[al.Server] = al
	}
	var out []tool.Toolset
	for _, ts := range offered {
		al, ok := byServer[ts.Name()]
		if !ok {
			continue
		}
		if len(al.Tools) == 0 {
			out = append(out, ts)
			continue
		}
		out = append(out, tool.FilterToolset(ts, tool.AllowedToolsPredicate(al.Tools)))
	}
	return out
}

// Build turns a Spec into an ADK agent, dispatching to Task or
// SingleTurn constructors based on Spec.Mode. The agent runs on
// spec.Model when the spec declares one, opts.Model otherwise — see
// modelFor.
//
// A spec's OutputSchema reaches both modes. The two enforce it
// differently — Task mode through the finish_task declaration, which
// rejects a malformed call back to the model, SingleTurn through
// validation of the reply, which fails the run — but in neither case
// can output that violates the contract become the specialist's result.
func Build(spec Spec, opts BuildOptions) (adkagent.Agent, error) {
	if opts.Model == nil {
		return nil, fmt.Errorf("specialists: build %q: BuildOptions.Model is required", spec.Name)
	}
	llm, err := opts.modelFor(spec)
	if err != nil {
		return nil, err
	}
	switch spec.Mode {
	case ModeTask, "":
		return mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
			Name:         spec.Name,
			Description:  spec.Description,
			Instruction:  spec.Instruction,
			Model:        llm,
			Tools:        opts.Tools,
			Toolsets:     filterToolsets(spec, opts.Toolsets),
			OutputSchema: spec.OutputSchema,
		})
	case ModeSingleTurn:
		return mastagent.NewSingleTurnAgent(mastagent.SingleTurnAgentConfig{
			Name:         spec.Name,
			Description:  spec.Description,
			Instruction:  spec.Instruction,
			Model:        llm,
			OutputSchema: spec.OutputSchema,
		})
	default:
		return nil, fmt.Errorf("specialists: build %q: unknown mode %q", spec.Name, spec.Mode)
	}
}

// BuildAll builds every Spec in specs using the same BuildOptions. Any
// error short-circuits and is returned with the offending Spec name.
func BuildAll(specs []Spec, opts BuildOptions) ([]adkagent.Agent, error) {
	agents := make([]adkagent.Agent, 0, len(specs))
	for _, spec := range specs {
		a, err := Build(spec, opts)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, nil
}
