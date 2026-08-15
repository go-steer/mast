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
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/jsonschema-go/jsonschema"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/plugin"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/graph"
	"github.com/go-steer/mast/pkg/permissions"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// WriteGateConfig configures WriteGate.
type WriteGateConfig struct {
	// Bundle is the loaded workload, or nil when there is none.
	Bundle *workload.Bundle

	// Predicate classifies tools. Optional: WriteGate derives one from
	// the bundle when it is nil. Pass the one the effects outbox is
	// using so the two plugins agree about what a mutation is —
	// disagreement means either a call that is recorded but ungated or
	// one that is gated but unrecorded.
	Predicate effects.Predicate

	// Gate decides policy for a parked call. Optional: WriteGate builds
	// a default gate when the policy needs one and this is nil.
	Gate *permissions.Gate

	// Specs is the loaded roster, used to work out which tools a change
	// executor in it can actually run. Optional: without it the
	// producer contract still checks that a proposed change names a
	// real tool with valid arguments, it just cannot narrow that to one
	// specialist's allowlist.
	Specs []specialists.Spec

	// ToolSchemas resolves a wired tool's declared input schema by
	// name, for the producer contract (v0.4 W7.0). It answers two
	// questions at once: whether this daemon holds a tool by that name
	// at all, and what arguments it takes.
	//
	// Optional, and its absence is fail-closed rather than fail-open:
	// with a bundle and no resolver, a specialist that proposes a
	// change gets it refused, because nothing here can tell an
	// executable call from an invented one. A roster whose report
	// schema has no proposed_change field is unaffected either way.
	ToolSchemas func(toolName string) (*jsonschema.Schema, error)

	Logger *slog.Logger
}

// WriteGate builds the pre-call write gate for a workload
// (docs/v0.3-plan.md W2.1/W2.2; docs/orchestration-design.md
// hitl_policy.on_mutation).
//
// It returns (nil, nil) when there is no bundle. The gate's whole
// mechanism is parking a call until an operator answers, and the
// default place an operator's answer arrives is the daemon's resume
// path against a durable session. A library embed that constructs its
// own runner with no workload has neither, so defaulting it on there
// would park mutating calls in a process with no way to un-park them —
// a hang, not a safety property. Such an embed opts in by passing a
// bundle (or by registering pkg/approval's plugin itself).
//
// With a bundle, the policy is hitl.on_mutation, whose default is
// require_approval: a workload that says nothing about mutation gets
// gated. Registration order matters and is settled — the effects outbox
// runs first, so a call whose result is being replayed from the log is
// never re-approved (resolved-decision row 144).
func WriteGate(cfg WriteGateConfig) (*plugin.Plugin, error) {
	if cfg.Bundle == nil {
		return nil, nil
	}
	policy, err := onMutation(cfg.Bundle.HITL.EffectiveOnMutation())
	if err != nil {
		return nil, err
	}
	pred := cfg.Predicate
	if pred == nil {
		pred = MutationPredicate(*cfg.Bundle, cfg.Logger)
	}
	gate := cfg.Gate
	if gate == nil && policy == approval.OnMutationRequireApproval {
		// mast has no permissions config surface yet (see
		// pkg/permissions/settings.go's port note), so the default gate
		// carries no deny patterns and runs in ask mode. That is enough
		// for the write gate, which asks regardless of mode; the deny
		// policy and plan-first pre-check become reachable the moment a
		// caller supplies a configured gate.
		gate = permissions.New(permissions.Options{})
	}
	p, err := approval.New(approval.Config{
		Policy:    policy,
		Mutating:  func(name string) bool { return pred(name) == effects.ClassMutating },
		Gate:      gate,
		ChangeSet: changeSetChecker(cfg),
		Logger:    cfg.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("compose: build write gate: %w", err)
	}
	return p, nil
}

// changeSetChecker builds the producer contract's validator for a
// workload (v0.4 W7.0): the thing that decides whether a specialist's
// `proposed_change` names calls this deployment could actually make.
//
// It is always installed alongside a bundle, and it is deliberately
// cheap to install: it only ever fires on a report that carries a
// non-empty change set, which only a roster whose finding schema
// declares the field can produce. Every pre-W7.0 roster passes through
// it untouched.
func changeSetChecker(cfg WriteGateConfig) *approval.ChangeSetChecker {
	schema := cfg.ToolSchemas
	if schema == nil {
		// Fail closed. A composition with no way to look up a tool's
		// arguments cannot tell an executable call from a
		// plausible-looking one, and the whole point of the contract is
		// that the second kind never reaches an operator wearing the
		// first kind's clothes. The specialist is told to send an empty
		// list instead, which is a valid report.
		schema = func(name string) (*jsonschema.Schema, error) {
			return nil, fmt.Errorf("this deployment cannot look up tool %q's arguments, so no proposed change can be checked against it", name)
		}
	}
	declares := func(string) bool { return true }
	if surface, exhaustive := changeSurface(cfg.Specs); exhaustive {
		declares = func(name string) bool { return surface[name] }
		if cfg.Logger != nil {
			cfg.Logger.Info("producer contract active", "executable_tools", sortedKeys(surface))
		}
	}
	return &approval.ChangeSetChecker{Declares: declares, Schema: schema}
}

// changeSurface is the set of tools some change executor in this roster
// can run, and whether that set is the whole of it.
//
// The narrowing matters because "a tool this daemon holds" and "a tool
// that will actually execute if an operator approves this finding" are
// different sets, and the gap between them is where a proposal dies
// silently: an operator approves a patch, the executor turns out not to
// hold patch_k8s_resource, and the incident ends with an approval and
// no change. Catching it at report time turns that into a refusal the
// diagnoser can act on.
//
// Not exhaustive means: some change executor took an un-enumerated
// grant (no tools.mcp key, or a server with no tools: list), which
// CheckCapabilitySplit permits for executors, so no finite name list
// describes its surface. It also covers a roster with no change
// executor at all — the proposal is then for a human to carry out, and
// mast has no allowlist to hold it to. In both cases the contract falls
// back to what the schema resolver alone can say, which still refuses a
// tool this daemon does not hold.
func changeSurface(specs []specialists.Spec) (map[string]bool, bool) {
	names := map[string]bool{}
	executors := 0
	for _, s := range specs {
		if s.Capability != specialists.CapabilityChangeExecutor {
			continue
		}
		executors++
		if s.Tools.InheritsAllMCP() {
			return nil, false
		}
		for _, al := range s.Tools.MCP {
			if len(al.Tools) == 0 {
				return nil, false
			}
			for _, n := range al.Tools {
				names[n] = true
			}
		}
		for _, n := range s.Tools.Builtin {
			names[n] = true
		}
	}
	if executors == 0 {
		return nil, false
	}
	return names, true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// onMutation converts the bundle's policy value to the approval
// package's. The two types are separate because pkg/approval does not
// import pkg/workload — the same reason MutationPredicate exists.
func onMutation(v workload.OnMutation) (approval.OnMutation, error) {
	switch v {
	case workload.OnMutationRequireApproval:
		return approval.OnMutationRequireApproval, nil
	case workload.OnMutationApply:
		return approval.OnMutationApply, nil
	case workload.OnMutationDryRun:
		return approval.OnMutationDryRun, nil
	}
	return "", fmt.Errorf("compose: unknown hitl.on_mutation %q", v)
}

// ApprovedChangeSet reads back what the write gate recorded when a
// specialist's finding proposed an executable change (v0.4 W7.0), for
// pkg/graph's diagnoser→executor routing predicate.
//
// This function is the seam between the two packages. pkg/graph cannot
// read the record itself — pkg/approval's own tests import pkg/graph,
// so the import can only go one way — and internal/compose is the one
// place that already imports both.
//
// A read failure is "nothing was proposed", not an error: the state key
// is absent for every finding that proposed nothing, which is most of
// them. What it must never do is guess. Returning true on a value it
// could not decode would route an unreadable proposal to a specialist
// that changes clusters.
func ApprovedChangeSet(logger *slog.Logger) graph.ChangeSetLookup {
	return func(ctx adkagent.Context, specialist string) (string, bool) {
		state := ctx.State()
		if state == nil {
			return "", false
		}
		v, err := state.Get(approval.ChangeSetStateKey(specialist))
		if err != nil || v == nil {
			return "", false
		}
		changes, err := approval.DecodeChangeSet(v)
		if err != nil {
			if logger != nil {
				logger.Error("a recorded change set could not be read back, so its finding will not be routed to the change executor",
					"specialist", specialist, "error", err.Error())
			}
			return "", false
		}
		if len(changes) == 0 {
			return "", false
		}
		return approval.DescribeChangeSet(changes), true
	}
}
