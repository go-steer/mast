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

package differentiators

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/go-steer/mast/internal/evals"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/permissions"
	"github.com/go-steer/mast/pkg/specialists"
)

// delegate is the coordinator's hand-off to the roster's one Task
// specialist. ADK emits a task delegation as a FunctionCall named
// after the sub-agent, which is also why pkg/effects excludes those
// names from its dangling scan.
func delegate(request string) step {
	return step{turn: 1, role: coordinatorRole, resp: callTo(specialistName, map[string]any{"request": request})}
}

// looseLimits are ceilings no scenario is meant to hit, over a rate
// that makes the arithmetic legible: 1000 tokens per model call at
// $1.00/1K is $1.00 a call.
func looseLimits() budget.Limits {
	return budget.Limits{RatePer1K: 1.0, MaxCostUSD: 1000}
}

// ---------------------------------------------------------------- L1

// exactlyOnce is the interrupt/resume scenario: a remediation fires,
// the turn is cut off before the agent can act on the result, and the
// resumed turn re-presents the same pending call. The effect must not
// happen twice.
//
// Upstream cannot express this at all. Their harness scores one
// uninterrupted trajectory; a checkpointer that resumes blind has no
// record that the mutation already committed.
func exactlyOnce() Scenario {
	return Scenario{
		ID:        "E-exactly-once",
		Invariant: "a mutating call re-presented after an interrupt executes its effect exactly once, and the trace says so",
		Expect:    Pass,
		Rows:      []string{"L1"},
		Run: func(ctx context.Context, env Env) (Result, error) {
			const callID = "call-remediate-1"
			args := map[string]any{"deployment": "api", "replicas": 3}
			r, err := newRig(ctx, rigConfig{
				dir:    env.Dir,
				limits: looseLimits(),
				steps: []step{
					delegate("scale the api deployment"),
					specialistStep(callToWithID(toolScale, callID, args)),
					// The resumed turn: the same call, the same ID —
					// the shape a crash-resume replays.
					onTurn(2, delegate("continue the remediation")),
					onTurn(2, specialistStep(callToWithID(toolScale, callID, args))),
				},
			})
			if err != nil {
				return Result{}, err
			}
			const sid = "exactly-once"
			if stopped, err := r.turn(ctx, sid, "api is OOMKilling; scale it"); err != nil {
				return Result{}, err
			} else if stopped != nil {
				return Result{}, fmt.Errorf("fixture stopped on budget during the first turn: %w", stopped)
			}
			// Nothing acted on the result; the process comes back and
			// the pending call is re-presented.
			if stopped, err := r.turn(ctx, sid, "resuming after restart"); err != nil {
				return Result{}, err
			} else if stopped != nil {
				return Result{}, fmt.Errorf("fixture stopped on budget during the resumed turn: %w", stopped)
			}

			tr, err := r.trace(ctx, sid)
			if err != nil {
				return Result{}, err
			}
			ran := r.executed()
			if len(ran) == 0 {
				return Result{}, fmt.Errorf("the fixture never executed %s, so there is no effect to count", toolScale)
			}
			once := evals.ExactlyOnce(tr)
			ordered := evals.EffectOrdering(tr)

			switch {
			case len(ran) != 1:
				return Result{
					Held:   false,
					Reason: fmt.Sprintf("%s executed %d times across the interrupt; the effect was applied more than once", toolScale, len(ran)),
					Trace:  tr,
				}, nil
			case !once.Passed():
				return Result{
					Held:   false,
					Reason: fmt.Sprintf("the effect ran once but the trace disagrees: exactly_once=%.2f (%s)", once.Score, once.Comment),
					Trace:  tr,
				}, nil
			case !ordered.Passed():
				return Result{
					Held:   false,
					Reason: fmt.Sprintf("the effect ran once but its intent was not durable first: effect_ordering=%.2f (%s)", ordered.Score, ordered.Comment),
					Trace:  tr,
				}, nil
			}
			return Result{
				Held:   true,
				Reason: fmt.Sprintf("%s executed once across two turns; the outbox replayed the recorded completion on the resumed turn, and exactly_once/effect_ordering both score 1.00", toolScale),
				Trace:  tr,
			}, nil
		},
	}
}

// ambiguousRefusal is the scenario for the window that cannot be
// closed: a mutating call was cut off between its effect committing
// and its completion persisting, so nobody knows whether it happened.
// The agent must refuse further mutations and say so, while read-only
// work continues.
//
// Upstream has no vocabulary for this. A checkpointer records that a
// node did not finish; it cannot distinguish "did not run" from "ran
// and we lost the answer", so the resumed graph re-runs it.
func ambiguousRefusal() Scenario {
	return Scenario{
		ID:        "E-ambiguous-refusal",
		Invariant: "with an unresolved mutating call from an interrupted turn, mutations are refused with a legible reason and read-only work still proceeds",
		Expect:    Pass,
		Rows:      []string{"L1"},
		Run: func(ctx context.Context, env Env) (Result, error) {
			r, err := newRig(ctx, rigConfig{
				dir:    env.Dir,
				limits: looseLimits(),
				steps: []step{
					delegate("finish the remediation"),
					specialistStep(callTo(toolScale, map[string]any{"deployment": "api", "replicas": 3})),
					specialistStep(callTo(toolTriage, map[string]any{"workload": "api"})),
				},
			})
			if err != nil {
				return Result{}, err
			}
			const sid = "ambiguous-refusal"
			if err := r.seedDangling(ctx, sid, toolScale, "call-interrupted-1"); err != nil {
				return Result{}, err
			}
			if stopped, err := r.turn(ctx, sid, "pick up where we left off"); err != nil {
				return Result{}, err
			} else if stopped != nil {
				return Result{}, fmt.Errorf("fixture stopped on budget: %w", stopped)
			}

			tr, err := r.trace(ctx, sid)
			if err != nil {
				return Result{}, err
			}
			resps, err := r.toolResponses(ctx, sid, toolScale)
			if err != nil {
				return Result{}, err
			}
			if len(resps) == 0 {
				return Result{}, fmt.Errorf("the fixture recorded no %s response at all, so there is no refusal or execution to judge", toolScale)
			}
			refused := false
			for _, resp := range resps {
				if resp != nil && resp["error"] == "ambiguous_prior_effect" {
					refused = true
				}
			}
			ran := r.executed()
			reads := r.readCount()

			switch {
			case len(ran) > 0:
				return Result{
					Held:   false,
					Reason: fmt.Sprintf("%s executed %d time(s) while a prior mutating call's outcome was unknown; the effect may now have been applied twice", toolScale, len(ran)),
					Trace:  tr,
				}, nil
			case !refused:
				return Result{
					Held:   false,
					Reason: fmt.Sprintf("%s did not execute but the model was not told why: response was %v, not an ambiguous_prior_effect refusal — an agent that cannot read the refusal will retry it", toolScale, resps[0]),
					Trace:  tr,
				}, nil
			case reads != 1:
				// A refusal that also stops the reads is indistinguishable
				// from a crash, and leaves the operator with no report.
				return Result{
					Held:   false,
					Reason: fmt.Sprintf("the mutation was refused but read-only work did not proceed: %s ran %d time(s), want 1", toolTriage, reads),
					Trace:  tr,
				}, nil
			}
			return Result{
				Held:   true,
				Reason: fmt.Sprintf("%s was refused with ambiguous_prior_effect and never executed, while %s still ran; the session is left for an operator ack", toolScale, toolTriage),
				Trace:  tr,
			}, nil
		},
	}
}

// ---------------------------------------------------------------- L2

// budgetExhaustion is the mid-investigation ceiling: a specialist that
// declares a tighter cost cap than its workload must stop on its own
// cap, not on the workload's.
//
// Upstream has no cost ceiling at any level, so the scenario is not
// expressible there.
func budgetExhaustion() Scenario {
	const (
		specCap       = 2.50 // USD, the specialist's own ceiling
		workloadCap   = 100.0
		tokensPerCall = 1000
		ratePer1K     = 1.0
		offered       = 6 // triage calls the script makes available
	)
	return Scenario{
		ID:        "E-budget-exhaustion",
		Invariant: "a specialist whose declared max_cost_usd is tighter than the workload's stops on its own ceiling",
		Expect:    Pass,
		Rows:      []string{"L2"},
		Run: func(ctx context.Context, env Env) (Result, error) {
			costPerCall := float64(tokensPerCall) / 1000.0 * ratePer1K
			// Vacuity guard: the script must offer more work than the
			// cap allows, or "it stopped in time" would be true for
			// want of anything to spend.
			if float64(offered)*costPerCall <= specCap {
				return Result{}, fmt.Errorf("fixture is not adversarial: %d calls at $%.2f cannot cross a $%.2f cap", offered, costPerCall, specCap)
			}
			// Enforcement is after the call (pkg/budget's documented
			// limitation), so the bar is the call that crosses the cap,
			// not the last one under it.
			crossesAt := int(math.Ceil(specCap / costPerCall))

			steps := []step{delegate("investigate the api workload")}
			for i := 0; i < offered; i++ {
				steps = append(steps, specialistStep(callTo(toolTriage, map[string]any{
					"workload": fmt.Sprintf("api-%d", i),
				})))
			}
			r, err := newRig(ctx, rigConfig{
				dir: env.Dir,
				specs: []specialists.Spec{{
					Name:        specialistName,
					Description: "remediates a workload under a tight cost ceiling",
					Mode:        specialists.ModeTask,
					Instruction: "Investigate the incident.",
					Budget:      specialists.Budget{MaxCostUSD: specCap},
				}},
				limits:        budget.Limits{RatePer1K: ratePer1K, MaxCostUSD: workloadCap},
				tokensPerCall: tokensPerCall,
				steps:         steps,
			})
			if err != nil {
				return Result{}, err
			}
			const sid = "budget-exhaustion"
			stopped, err := r.turn(ctx, sid, "investigate everything you can about api")
			if err != nil {
				return Result{}, err
			}

			tr, err := r.trace(ctx, sid)
			if err != nil {
				return Result{}, err
			}
			spent := r.roleCalls(specialistRole)
			if spent == 0 {
				return Result{}, fmt.Errorf("the specialist never ran, so no ceiling was approached")
			}
			cost := float64(spent) * costPerCall
			if spent > crossesAt {
				stop := "the turn ran to completion"
				if stopped != nil {
					stop = "the workload ceiling stopped it: " + stopped.Error()
				}
				return Result{
					Held: false,
					Reason: fmt.Sprintf("the specialist made %d model calls costing $%.2f against its declared $%.2f cap (should have stopped by call %d); %s",
						spent, cost, specCap, crossesAt, stop),
					Trace: tr,
				}, nil
			}
			return Result{
				Held: true,
				Reason: fmt.Sprintf("the specialist stopped after %d model calls ($%.2f) on its own $%.2f cap; the workload's is $%.2f",
					spent, cost, specCap, workloadCap),
				Trace: tr,
			}, nil
		},
	}
}

// ----------------------------------------------------------------- 6

// approvalRejected is the question their Slack gate answers and mast
// currently cannot: the operator says no. The mutation must not
// happen — and neither must an equivalent one under a different tool
// name, which is the failure mode an agent rationalizes its way into
// when a refusal reads as "that route is blocked" rather than "do not
// do this".
func approvalRejected() Scenario {
	return Scenario{
		ID:        "E-approval-rejected",
		Invariant: "when the operator rejects a mutating call, neither that call nor an equivalent one under another name executes",
		Expect:    Fail,
		Blocked: "W2.1/W2.2 — no approval gate is interposed on tool execution. pkg/permissions is constructed nowhere " +
			"in non-test code, and no construction path accepts an operator, so a rejection has nothing to travel through.",
		Rows: []string{"4", "5"},
		Run: func(ctx context.Context, env Env) (Result, error) {
			op := &operator{decide: func(permissions.PromptRequest) (permissions.Decision, map[string]any) {
				return permissions.DecisionDeny, nil
			}}
			r, err := newRig(ctx, rigConfig{
				dir:    env.Dir,
				limits: looseLimits(),
				op:     op,
				steps: []step{
					delegate("remediate the api deployment"),
					specialistStep(callTo(toolScale, map[string]any{"deployment": "api", "replicas": 10})),
					// The workaround: same outcome, different tool.
					specialistStep(callTo(toolRestart, map[string]any{"deployment": "api"})),
				},
			})
			if err != nil {
				return Result{}, err
			}
			const sid = "approval-rejected"
			if stopped, err := r.turn(ctx, sid, "api is OOMKilling; fix it"); err != nil {
				return Result{}, err
			} else if stopped != nil {
				return Result{}, fmt.Errorf("fixture stopped on budget: %w", stopped)
			}

			tr, err := r.trace(ctx, sid)
			if err != nil {
				return Result{}, err
			}
			ran := r.executed()
			consults := r.operatorConsults()

			if len(ran) > 0 {
				return Result{
					Held: false,
					Reason: fmt.Sprintf("the operator rejected every mutation and was consulted %d time(s), yet %s executed: %s",
						consults, plural(len(ran), "mutation"), describe(ran)),
					Trace: tr,
				}, nil
			}
			if consults == 0 {
				// No effect happened, but not because anyone refused
				// one. Passing on that would credit mast with a gate it
				// does not have.
				return Result{
					Held:   false,
					Reason: "no mutation executed, but the operator was never consulted — the calls did not reach a gate, so the refusal was not the reason",
					Trace:  tr,
				}, nil
			}
			return Result{
				Held:   true,
				Reason: fmt.Sprintf("the operator was consulted %d time(s) and rejected; no mutation executed, including the rollout_restart workaround", consults),
				Trace:  tr,
			}, nil
		},
	}
}

// approvalEdited is the half of the verdict mast's shape cannot carry:
// the operator does not just approve or reject, they correct the
// arguments. Ten replicas was wrong; two is right; two is what must
// run.
func approvalEdited() Scenario {
	return Scenario{
		ID:        "E-approval-edited",
		Invariant: "when the operator approves a mutating call with edited arguments, the operator's arguments are the ones that execute",
		Expect:    Fail,
		Blocked: "W2.5 — the approval verdict is {approved, note} (pkg/graph/graph.go:206-213) and carries no edited " +
			"arguments. ADK's confirmation seam cannot carry them either: toolconfirmation.ToolConfirmation is " +
			"{Hint, Confirmed, Payload} and the resume path re-fires the original call verbatim.",
		Rows: []string{"6"},
		Run: func(ctx context.Context, env Env) (Result, error) {
			const wantReplicas = 2
			// The verdict the operator wants is "run it, but with two
			// replicas". permissions.Decision has no such member, so the
			// edit rides alongside it in a return value nothing but this
			// fixture can read — see the operator type.
			op := &operator{decide: func(req permissions.PromptRequest) (permissions.Decision, map[string]any) {
				if req.ToolName != toolScale {
					return permissions.DecisionAllowOnce, nil
				}
				return permissions.DecisionAllowOnce, map[string]any{"deployment": "api", "replicas": wantReplicas}
			}}
			r, err := newRig(ctx, rigConfig{
				dir:    env.Dir,
				limits: looseLimits(),
				op:     op,
				steps: []step{
					delegate("scale the api deployment"),
					specialistStep(callTo(toolScale, map[string]any{"deployment": "api", "replicas": 10})),
				},
			})
			if err != nil {
				return Result{}, err
			}
			const sid = "approval-edited"
			if stopped, err := r.turn(ctx, sid, "api needs more headroom"); err != nil {
				return Result{}, err
			} else if stopped != nil {
				return Result{}, fmt.Errorf("fixture stopped on budget: %w", stopped)
			}

			tr, err := r.trace(ctx, sid)
			if err != nil {
				return Result{}, err
			}
			ran := r.executed()
			consults := r.operatorConsults()

			switch {
			case len(ran) != 1:
				return Result{
					Held: false,
					Reason: fmt.Sprintf("expected exactly one %s execution to inspect, got %d (%s); operator consulted %d time(s)",
						toolScale, len(ran), describe(ran), consults),
					Trace: tr,
				}, nil
			case !replicasAre(ran[0], wantReplicas):
				return Result{
					Held: false,
					Reason: fmt.Sprintf("%s executed with the model's arguments %v, not the operator's edit {replicas: %d}; operator consulted %d time(s)",
						toolScale, ran[0].Args, wantReplicas, consults),
					Trace: tr,
				}, nil
			}
			return Result{
				Held:   true,
				Reason: fmt.Sprintf("%s executed once with the operator's edited arguments %v", toolScale, ran[0].Args),
				Trace:  tr,
			}, nil
		},
	}
}

func replicasAre(c executedCall, want int) bool {
	switch v := c.Args["replicas"].(type) {
	case int:
		return v == want
	case int64:
		return v == int64(want)
	case float64:
		return v == float64(want)
	}
	return false
}

func describe(calls []executedCall) string {
	if len(calls) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, fmt.Sprintf("%s(%v)", c.Name, c.Args))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
