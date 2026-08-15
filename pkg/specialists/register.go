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
	"google.golang.org/adk/v2/agent/llmagent"
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

// TierResolver is the same seam for a specialist's `tier:` frontmatter:
// it turns "small" into whatever the running provider's small model is.
// The mapping lives outside this package for the same reason
// ModelResolver's does, and for one more — the answer depends on which
// provider the operator started mast with, which is a composition fact,
// not a roster fact. internal/compose.BuildRoot supplies the one mast
// ships; it goes through pkg/taskclass.ModelForTier and then through
// the same memoized ModelResolver, so a roster of twelve small-tier
// diagnosers still opens one client.
type TierResolver func(tier string) (model.LLM, error)

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

	// ResolveTier resolves per-specialist `tier:` declarations. Nil is
	// legal only when no Spec in the roster declares one; a declared
	// tier with no resolver is a build error for the same reason a
	// declared `model:` with no Resolve is.
	ResolveTier TierResolver

	Tools    []tool.Tool
	Toolsets []tool.Toolset

	// OnStall, when non-nil, installs mastagent.FinishOnStall on every
	// Task-mode specialist Build creates, using the payload this function
	// returns for that spec. Returning nil selects
	// mastagent.DefaultStallPayload.
	//
	// It is opt-in and it is per-spec, in that order of importance.
	//
	// Opt-in because the guard fabricates a tool call. Every other default in
	// this package is a property of the wiring — which model, which toolsets,
	// whether transfer is offered — and can be argued for without knowing what
	// the roster does. This one puts words in a specialist's mouth on its
	// behalf, and a roster that would rather see the delegation die should not
	// have to discover the field to get that.
	//
	// Per-spec because the payload has to satisfy the spec's own
	// output_schema, and only the roster knows what an empty value means in
	// its own contract. Requesting the guard for a spec that declares a schema
	// without returning a payload for it is a build error rather than a
	// fallback, for the same reason a declared-but-unresolvable model override
	// is: the fallback would produce a finish_task call the runtime refuses,
	// leaving exactly the unresolved delegation the guard exists to prevent,
	// and it would do it at run time on the one turn nobody is watching.
	OnStall func(spec Spec) mastagent.StallPayload
}

// modelFor picks the model a Spec builds with: the resolved `model:`
// override or `tier:` when the spec declares one, the parent's model
// otherwise.
//
// A declared-but-unresolvable override is a build error, never a
// fallback. Falling back would reproduce exactly the failure mode W1.1
// exists to remove — a bundle that reads as "Haiku analysts, Sonnet
// synthesis" while every specialist quietly runs on the parent's model
// and the cost story is a fiction.
func (o BuildOptions) modelFor(spec Spec) (model.LLM, error) {
	if spec.Model != "" && spec.Tier != "" {
		// LoadFile refuses this shape; a Spec built in code can still
		// reach here, and guessing which one wins is the fiction above.
		return nil, fmt.Errorf("specialists: build %q: declares both model %q and tier %q (use one)", spec.Name, spec.Model, spec.Tier)
	}
	if spec.Tier != "" {
		return o.modelForTier(spec)
	}
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

// modelForTier is modelFor's `tier:` half, split out only for length.
func (o BuildOptions) modelForTier(spec Spec) (model.LLM, error) {
	if o.ResolveTier == nil {
		return nil, fmt.Errorf("specialists: build %q: tier %q declared but BuildOptions.ResolveTier is nil", spec.Name, spec.Tier)
	}
	m, err := o.ResolveTier(spec.Tier)
	if err != nil {
		return nil, fmt.Errorf("specialists: build %q: resolve tier %q: %w", spec.Name, spec.Tier, err)
	}
	if m == nil {
		return nil, fmt.Errorf("specialists: build %q: tier resolver returned nil for tier %q", spec.Name, spec.Tier)
	}
	return m, nil
}

// stallGuard resolves BuildOptions.OnStall into the AfterModelCallbacks a
// Task-mode spec is built with. Nil OnStall means no guard and no callbacks.
//
// The error case is the whole reason this is a method and not an inline
// closure: a spec with an output_schema and no payload builder cannot be given
// mastagent.DefaultStallPayload, whose `{"result": string}` shape ADK would
// refuse against that schema. Refusing at build time turns a silent run-time
// regression — the delegation stays unresolved, which is precisely the failure
// the guard exists to prevent — into a message naming the spec.
func (o BuildOptions) stallGuard(spec Spec) ([]llmagent.AfterModelCallback, error) {
	if o.OnStall == nil {
		return nil, nil
	}
	payload := o.OnStall(spec)
	if payload == nil && spec.OutputSchema != nil {
		return nil, fmt.Errorf("specialists: build %q: BuildOptions.OnStall returned no payload for a spec that declares an output_schema; "+
			"the default {\"result\": string} payload would fail finish_task validation, which is the unresolved delegation the guard exists to prevent", spec.Name)
	}
	return []llmagent.AfterModelCallback{mastagent.FinishOnStall(spec.Name, payload)}, nil
}

// filterToolsets applies the specialist's MCP allowlist to the offered
// toolsets, matching allowlist entries to toolsets by server name.
//
// Semantics are the normative table in docs/specialists-design.md, read
// per axis on *presence*: an absent tools.mcp inherits every offered
// toolset; a present-but-empty one (`mcp: []`) denies them all; a
// non-empty one is a whitelist — unlisted servers are dropped, a listed
// server with no tools[] passes whole, a listed server with tools[] is
// narrowed via tool.FilterToolset + AllowedToolsPredicate (both stock
// ADK — per-tool MCP filtering needs no mast machinery, resolving
// specialists-design open question #3).
//
// Corrected 2026-08-14, in W2.4: this used to treat empty as absent, so
// the documented deny-all spelling granted the whole catalog instead.
// Harmless while nothing read the declaration; not harmless once
// CheckCapabilitySplit accepts `mcp: []` as proof a diagnoser holds no
// write tools. A declaration that reads as deny must deny.
func filterToolsets(spec Spec, offered []tool.Toolset) []tool.Toolset {
	if spec.Tools.InheritsAllMCP() {
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
// spec.Model or spec.Tier when the spec declares one, opts.Model
// otherwise — see modelFor.
//
// A spec's OutputSchema reaches both modes. The two enforce it
// differently — Task mode through the finish_task declaration, which
// rejects a malformed call back to the model, SingleTurn through
// validation of the reply, which fails the run — but in neither case
// can output that violates the contract become the specialist's result.
//
// Task-mode specialists are always built unable to transfer, and are
// built with the stall guard when opts.OnStall asks for it. The two are
// the same hazard from opposite ends — a specialist that hands the
// question back, and one that never hands anything back — and only the
// first is safe to default, because only the first is a pure
// subtraction. See BuildOptions.OnStall.
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
		after, err := opts.stallGuard(spec)
		if err != nil {
			return nil, err
		}
		return mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
			Name:         spec.Name,
			Description:  spec.Description,
			Instruction:  spec.Instruction,
			Model:        llm,
			Tools:        opts.Tools,
			Toolsets:     filterToolsets(spec, opts.Toolsets),
			OutputSchema: spec.OutputSchema,
			// A specialist reports through finish_task and never by handing
			// the question back. Peers are already unreachable — ADK's
			// transferTargets skips Task-mode agents — so the only transfer a
			// specialist can make is to the coordinator that delegated to it,
			// and under pkg/router's Chat coordinator that transfer aborts the
			// run. See TaskAgentConfig for the mechanism.
			DisallowTransferToParent: true,
			DisallowTransferToPeers:  true,
			AfterModelCallbacks:      after,
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
