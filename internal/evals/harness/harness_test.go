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

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/go-steer/mast/internal/evals"
	"github.com/go-steer/mast/internal/evals/differentiators"
)

const repoRoot = "../../.."

// TestRun_Deterministic is the W0.4 done-when: the gating tier runs
// green against today's tree, with the known-red scenarios accounted for
// rather than hidden.
func TestRun_Deterministic(t *testing.T) {
	sum, err := Run(context.Background(), Config{Root: repoRoot, Scratch: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sum.OK() {
		t.Fatalf("suite is red:\n  - %s", strings.Join(sum.Problems, "\n  - "))
	}
	if sum.Corpus.Scenarios != 31 {
		t.Errorf("corpus loaded %d scenarios, want 31", sum.Corpus.Scenarios)
	}
	if len(sum.Corpus.Dead) != 0 {
		t.Errorf("dead metrics %v; a constant function is a harness failure", sum.Corpus.Dead)
	}
	if got, want := len(sum.Scenes), len(differentiators.All()); got != want {
		t.Errorf("reported %d differentiators, want %d", got, want)
	}
	// Every scene must carry the evidence a reader needs to act on it.
	for _, sc := range sum.Scenes {
		if sc.Reason == "" {
			t.Errorf("%s: no reason reported", sc.ID)
		}
		if len(sc.Rows) == 0 {
			t.Errorf("%s: names no scoreboard row", sc.ID)
		}
		if sc.Observed == differentiators.Fail.String() && sc.Blocked == "" {
			t.Errorf("%s: red with no blocking workstream named", sc.ID)
		}
	}
	if len(sum.ExpectedFail) == 0 {
		t.Error("expected-fail list is empty; W0.3 left three scenarios red, so either a capability landed " +
			"(update this test and the plan) or the allowlist stopped being reported")
	}
}

// TestRun_JudgeTierRefused pins that asking for the metered tier fails
// loudly. Silently running the free tier instead would report a
// deterministic result as a LangChain-comparable one.
func TestRun_JudgeTierRefused(t *testing.T) {
	_, err := Run(context.Background(), Config{Root: repoRoot, Tier: TierJudge, Scratch: t.TempDir()})
	if err == nil {
		t.Fatal("judge tier ran; it is not implemented until W0.5")
	}
	var notImpl ErrTierNotImplemented
	if !errors.As(err, &notImpl) {
		t.Fatalf("error = %v, want ErrTierNotImplemented so the runner can exit 2 rather than 1", err)
	}
	if !strings.Contains(notImpl.Why, "W0.5") {
		t.Errorf("Why = %q, want it to name the workstream that builds it", notImpl.Why)
	}
}

func TestRun_RejectsUnknownTier(t *testing.T) {
	_, err := Run(context.Background(), Config{Root: repoRoot, Tier: "vibes", Scratch: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "unknown tier") {
		t.Fatalf("error = %v, want an unknown-tier rejection", err)
	}
}

// TestRun_MissingFixtureIsAHarnessError, not a red board: a suite that
// cannot find its corpus has measured nothing.
func TestRun_MissingFixtureIsAHarnessError(t *testing.T) {
	_, err := Run(context.Background(), Config{Root: t.TempDir(), Scratch: t.TempDir()})
	if err == nil {
		t.Fatal("Run succeeded with no fixtures on disk")
	}
	if !strings.Contains(err.Error(), "langchain-sre.jsonl") {
		t.Errorf("error = %v, want it to name the missing fixture", err)
	}
}

// TestSummarize_BothDirections is the mapping from an observed outcome
// to a gate decision, checked in every combination. The two mismatch
// directions are different defects and must read differently: one says a
// capability landed, the other says one regressed.
func TestSummarize_BothDirections(t *testing.T) {
	scene := func(expect differentiators.Outcome, blocked string) differentiators.Scenario {
		return differentiators.Scenario{
			ID:        "E-example",
			Invariant: "the thing holds",
			Expect:    expect,
			Blocked:   blocked,
			Rows:      []string{"L1"},
		}
	}
	live := differentiators.Result{Held: true, Reason: "observed something", Trace: evals.Trace{
		Calls: []evals.Call{{Name: "k8s_triage_workload", ID: "c1"}},
	}}

	tests := []struct {
		name        string
		rep         differentiators.Report
		wantProblem string
	}{
		{
			name: "declared pass, passes",
			rep:  differentiators.Report{Scenario: scene(differentiators.Pass, ""), Result: live, Outcome: differentiators.Pass},
		},
		{
			name: "declared fail, fails",
			rep:  differentiators.Report{Scenario: scene(differentiators.Fail, "W2.5 — no channel"), Result: live, Outcome: differentiators.Fail},
		},
		{
			name:        "declared fail, now passes",
			rep:         differentiators.Report{Scenario: scene(differentiators.Fail, "W2.5 — no channel"), Result: live, Outcome: differentiators.Pass},
			wantProblem: "the capability landed",
		},
		{
			name:        "declared pass, now fails",
			rep:         differentiators.Report{Scenario: scene(differentiators.Pass, ""), Result: live, Outcome: differentiators.Fail},
			wantProblem: "regression in shipped behaviour",
		},
		{
			name: "broken beats its declaration",
			rep: differentiators.Report{
				Scenario: scene(differentiators.Fail, "W2.5 — no channel"),
				Outcome:  differentiators.Broken,
				Err:      errors.New("empty trace"),
			},
			wantProblem: "BROKEN",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := summarize(tc.rep)
			if tc.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) != 1 {
				t.Fatalf("problems = %v, want exactly one", problems)
			}
			if !strings.Contains(problems[0], tc.wantProblem) {
				t.Errorf("problem = %q, want it to mention %q", problems[0], tc.wantProblem)
			}
		})
	}
}

// TestSummarize_BrokenIsNotAllowlistable is the load-bearing half of the
// three-valued outcome: a fixture defect declared as an expected fail
// must still fail the gate. Kept separate from the table above because
// it is the property the whole design rests on.
func TestSummarize_BrokenIsNotAllowlistable(t *testing.T) {
	rep := differentiators.Report{
		Scenario: differentiators.Scenario{
			ID: "E-example", Invariant: "x", Expect: differentiators.Broken, Rows: []string{"L1"},
		},
		Outcome: differentiators.Broken,
		Err:     errors.New("fixture never ran"),
	}
	// Even when the declaration matches the observation exactly, Broken
	// is a problem. Report.Matches() is true here, and that must not be
	// enough to pass.
	if !rep.Matches() {
		t.Fatal("premise: this report's outcome matches its declaration")
	}
	_, problems := summarize(rep)
	if len(problems) != 1 || !strings.Contains(problems[0], "BROKEN") {
		t.Fatalf("problems = %v, want a Broken report to fail the gate even when declared", problems)
	}
}

func TestSummary_WriteText(t *testing.T) {
	sum := Summary{
		Tier: TierDeterministic,
		Corpus: CorpusSummary{
			Dataset: "langchain-sre", Scenarios: 31, Intents: 19,
			Reach: []evals.MetricReach{{Metric: evals.MetricIntentCoverage, Scenarios: 31, Reaches: 31}},
		},
		Scenes: []ScenarioSummary{
			{ID: "E-approval-edited", Expected: "FAIL", Observed: "FAIL", Matched: true,
				Blocked: "W2.5 — no channel", Reason: "the model's arguments executed"},
		},
		ExpectedFail: []string{"E-approval-edited"},
	}

	var buf bytes.Buffer
	sum.WriteText(&buf)
	out := buf.String()
	for _, want := range []string{
		"tier deterministic",
		"intent_coverage",
		"E-approval-edited",
		"blocked on W2.5",
		"expected-fail allowlist: 1 of 1",
		"progress metric",
		"PASS",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}

	sum.Problems = []string{"E-approval-edited: something"}
	buf.Reset()
	sum.WriteText(&buf)
	if out := buf.String(); !strings.Contains(out, "FAIL") || !strings.Contains(out, "E-approval-edited: something") {
		t.Errorf("failing report does not say so:\n%s", out)
	}
}

// TestSummary_WriteJSON keeps the machine-readable shape usable by
// W0.5's nightly delta, which diffs one run's expected-fail list against
// the last.
func TestSummary_WriteJSON(t *testing.T) {
	sum, err := Run(context.Background(), Config{Root: repoRoot, Scratch: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var buf bytes.Buffer
	if err := sum.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var back Summary
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("the report does not round-trip: %v", err)
	}
	if back.Tier != sum.Tier || len(back.Scenes) != len(sum.Scenes) {
		t.Errorf("round-trip lost data: %+v", back)
	}
	if len(back.ExpectedFail) != len(sum.ExpectedFail) {
		t.Errorf("expected-fail list = %v, want %v", back.ExpectedFail, sum.ExpectedFail)
	}
}
