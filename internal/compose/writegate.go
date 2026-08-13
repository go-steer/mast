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

	"google.golang.org/adk/v2/plugin"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/permissions"
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
		Policy:   policy,
		Mutating: func(name string) bool { return pred(name) == effects.ClassMutating },
		Gate:     gate,
		Logger:   cfg.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("compose: build write gate: %w", err)
	}
	return p, nil
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
