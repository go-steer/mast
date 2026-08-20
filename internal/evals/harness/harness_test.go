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
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/mast/internal/evals"
	"github.com/go-steer/mast/internal/evals/differentiators"
	"github.com/go-steer/mast/internal/evals/judge"
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
	// W0.3 left three scenarios red; W2.5 retired the last of them. An
	// empty allowlist is now the expected state, and re-growing one is
	// the thing to notice: a scenario may only go back to declared-Fail
	// with a named blocking workstream, which the loop above enforces.
	if len(sum.ExpectedFail) != 0 {
		t.Errorf("expected-fail allowlist has re-grown to %v; shrinking it is the progress metric, so a new entry "+
			"needs a plan row saying why (docs/v0.3-plan.md W0.4)", sum.ExpectedFail)
	}
	for _, sc := range sum.Scenes {
		if sc.Observed != differentiators.Pass.String() {
			t.Errorf("%s observed %s: %s", sc.ID, sc.Observed, sc.Reason)
		}
	}
}

// TestRun_JudgeTierUnbuildableModelFailsLoudly pins the tier boundary.
// Silently falling back to the free tier when the metered one cannot
// start would report a deterministic result as a LangChain-comparable
// one, and it is the runner's exit-2 case rather than exit 1: nothing
// was measured.
func TestRun_JudgeTierUnbuildableModelFailsLoudly(t *testing.T) {
	_, err := Run(context.Background(), Config{
		Root: repoRoot, Tier: TierJudge, Scratch: t.TempDir(),
		Model: "no-such-model-9000", Grader: "echo",
	})
	if err == nil {
		t.Fatal("the judge tier started with a model that cannot be built")
	}
	if !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("error = %v, want it to name the unbuildable model", err)
	}
}

// TestRun_JudgeTierRunsTheWholeCorpus exercises the metered tier's
// plumbing end to end without credentials or cost, by pointing it at
// compose's offline echo model. It is not a quality check — echo does
// not choose tools, so every score is meaningless — it checks that all
// 31 rows reach the board, that both model names are recorded, and that
// a grader which cannot produce a grade costs one column rather than the
// row.
func TestRun_JudgeTierRunsTheWholeCorpus(t *testing.T) {
	sum, err := Run(context.Background(), Config{
		Root: repoRoot, Tier: TierJudge, Scratch: t.TempDir(),
		Model: "echo", Grader: "echo",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Judge == nil {
		t.Fatal("the judge tier produced no board")
	}
	if got := len(sum.Judge.Scenes); got != 31 {
		t.Fatalf("board has %d rows, want 31", got)
	}
	for _, s := range sum.Judge.Scenes {
		if s.Error != "" {
			t.Errorf("%s: did not run: %s", s.ID, s.Error)
		}
	}
	if sum.Judge.Model != "echo" || sum.Judge.Grader != "echo" {
		t.Errorf("board records model %q / grader %q, want both named", sum.Judge.Model, sum.Judge.Grader)
	}
	// Grading with the model under test is a real methodological
	// problem, so the board has to say so rather than leave it to be
	// noticed.
	if len(sum.Judge.Notes) == 0 || !strings.Contains(sum.Judge.Notes[0], "grading its own output") {
		t.Errorf("notes = %v, want the same-model warning", sum.Judge.Notes)
	}
	// The echo model returns the prompt, which is not a grade. That
	// must show up as a missing column, not as a zero.
	if len(sum.Problems) == 0 {
		t.Error("an ungradeable reply from the grader was recorded as a score")
	}
	for _, p := range sum.Problems {
		if !strings.Contains(p, "response_quality was not graded") {
			t.Errorf("unexpected problem: %s", p)
		}
	}
	// LC-13 is the one structurally-capped row; the rest must not be.
	if len(sum.Judge.Ceilings) != 1 || sum.Judge.Ceilings[0].ID != "LC-13-rollback-needed-after-bad" {
		t.Errorf("ceilings = %+v, want only LC-13", sum.Judge.Ceilings)
	}
	// Under echo there is nothing to price: the fake collapses every
	// tier back onto itself. That is a skip the board has to state — an
	// absent cost section would read as one that passed — and it is not
	// a Problem, because the question, not mast, is what this
	// configuration cannot answer.
	if sum.Judge.Cost != nil {
		t.Errorf("the offline fake produced a cost board: %+v", sum.Judge.Cost)
	}
	if sum.Judge.CostSkipped == "" {
		t.Error("J-cost-tier was skipped without saying so; a silent skip reads as a pass")
	}
}

// TestAggregate_ExcludesVacuousRows pins the averaging rule. Counting a
// row that had nothing to score as a zero reports the corpus's shape as
// the agent's performance.
func TestAggregate_ExcludesVacuousRows(t *testing.T) {
	scenes := []JudgeScenario{
		{ID: "a", Results: []evals.Result{{Metric: evals.MetricIntentCoverage, Score: 1}}},
		{ID: "b", Results: []evals.Result{{Metric: evals.MetricIntentCoverage, Score: 0.5}}},
		{ID: "c", Results: []evals.Result{{Metric: evals.MetricIntentCoverage, Score: 0, Vacuous: true}}},
	}
	got := aggregate(scenes)
	if len(got) != 1 {
		t.Fatalf("aggregate = %+v, want one metric", got)
	}
	if got[0].Mean != 0.75 || got[0].Scored != 2 || got[0].Vacuous != 1 {
		t.Errorf("aggregate = %+v, want mean 0.75 over 2 scored with 1 vacuous", got[0])
	}
}

// TestSummary_WriteTextJudgeTier keeps the metered board honest about
// what it is: a report, with its ceilings stated and its models named.
func TestSummary_WriteTextJudgeTier(t *testing.T) {
	sum := Summary{
		Tier: TierJudge,
		Corpus: CorpusSummary{
			Dataset: "langchain-sre", Scenarios: 31, Intents: 19,
			Reach: []evals.MetricReach{{Metric: evals.MetricIntentCoverage, Scenarios: 31, Reaches: 31}},
		},
		Judge: &JudgeSummary{
			Model: "claude-opus-4-7", Grader: "claude-haiku-4-5", Provider: "anthropic-vertex",
			Scenes: []JudgeScenario{{
				ID: "LC-13-rollback-needed-after-bad", Ceiling: 0.5, Tools: []string{"k8s_recent_changes"},
				Results: []evals.Result{{Metric: evals.MetricIntentCoverage, Score: 0.5}},
			}},
			Aggregate: []MetricSummary{{Metric: evals.MetricIntentCoverage, Mean: 0.5, Scored: 1}},
			Ceilings: []CeilingFinding{{
				ID: "LC-13-rollback-needed-after-bad", Ceiling: 0.5, Scored: 0.5, Why: "the corpus expects a write tool",
			}},
		},
	}

	var buf bytes.Buffer
	sum.WriteText(&buf)
	out := buf.String()
	for _, want := range []string{
		"tier judge",
		"model under test claude-opus-4-7",
		"grader claude-haiku-4-5",
		"LC-13-rollback-needed-after-bad",
		"ceiling 0.50",
		"structural ceilings",
		"does not gate",
		"REPORTED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	// The differentiator section belongs to the free tier; printing an
	// empty one here would read as five scenarios having vanished.
	if strings.Contains(out, "differentiators:") {
		t.Errorf("the judge board printed an empty differentiator section:\n%s", out)
	}

	sum.Problems = []string{"LC-02: the run did not complete"}
	buf.Reset()
	sum.WriteText(&buf)
	if out := buf.String(); !strings.Contains(out, "INCOMPLETE") || strings.Contains(out, "REPORTED") {
		t.Errorf("an incomplete board reported itself as complete:\n%s", out)
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

// TestJudgeSummary_WriteCost covers the three states the cost section
// can be in, because each one is a different claim to a reader skimming
// a nightly: priced and correct, priced and wrong, or not priced at all.
func TestJudgeSummary_WriteCost(t *testing.T) {
	rows := []judge.ScopeCost{
		{
			Name: "analyst", Tier: "small", Resolved: "claude-haiku-4-5",
			Ran:   []string{"claude-haiku-4-5-20251001"},
			Calls: 3, Tokens: 4200, CostUSD: 0.0042,
			WantRate: 0.001, GotRate: 0.001, AtParentRate: 0.315,
		},
		{
			Name: "_synthesis", Tier: "frontier", Resolved: "claude-opus-4-7",
			Ran:   []string{"claude-opus-4-7"},
			Calls: 1, Tokens: 900, CostUSD: 0.0675,
			WantRate: 0.075, GotRate: 0.075, AtParentRate: 0.0675,
		},
	}

	t.Run("priced", func(t *testing.T) {
		out := renderJudge(t, &JudgeSummary{
			Model: "claude-opus-4-7", Cost: &judge.CostBoard{
				Provider: "anthropic-vertex", RootModel: "claude-opus-4-7", RootRate: 0.075,
				Scopes: rows,
			},
		})
		for _, want := range []string{
			"J-cost-tier",
			"every tiered specialist was billed at its own rate",
			"claude-haiku-4-5",
			// The counterfactual is the number the row is about: what
			// these tokens would have cost at the parent's rate.
			"$0.31500",
			"ran as claude-haiku-4-5-20251001",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("cost section is missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("mispriced", func(t *testing.T) {
		bad := &judge.CostBoard{
			Provider: "anthropic-vertex", RootModel: "claude-opus-4-7", RootRate: 0.075,
			Scopes:   rows,
			Findings: []string{"analyst ran on claude-haiku-4-5 but was billed at $0.07500/1K"},
		}
		out := renderJudge(t, &JudgeSummary{Model: "claude-opus-4-7", Cost: bad})
		if !strings.Contains(out, "a tiered specialist was not billed at its own rate") {
			t.Errorf("a mispriced board did not say so in its headline:\n%s", out)
		}
		if !strings.Contains(out, "PROBLEM: analyst ran on") {
			t.Errorf("the finding was not printed:\n%s", out)
		}
	})

	t.Run("skipped", func(t *testing.T) {
		out := renderJudge(t, &JudgeSummary{
			Model:       "echo",
			CostSkipped: "judge: cost: J-cost-tier needs a live model: root model \"echo\" collapses every tier back to itself",
		})
		if !strings.Contains(out, "J-cost-tier: SKIPPED") {
			t.Errorf("a skipped check left no trace in the report:\n%s", out)
		}
		if !strings.Contains(out, "not evidence that tiered pricing works") {
			t.Errorf("the skip did not say what it is not evidence of:\n%s", out)
		}
	})
}

// TestSummarizeCorpus_DiagnosticDeathIsReported covers the gap #179's
// demotion opened. evals.DeadMetrics names gating metrics only, so a
// diagnostic that can score nothing anywhere would have gone from a
// harness failure to a silent column — and severity_accuracy, whose
// extractor has now broken twice, is exactly such a metric. It is not a
// red parity row, but the harness has to say it measures nothing.
func TestSummarizeCorpus_DiagnosticDeathIsReported(t *testing.T) {
	ds, tbl, err := loadFixtures(repoRoot)
	if err != nil {
		t.Fatalf("loadFixtures: %v", err)
	}
	for i := range ds.Scenarios {
		ds.Scenarios[i].Outputs.ExpectedResponse = "the cluster looks unwell"
	}

	sum, _ := summarizeCorpus(tbl, ds)
	if len(sum.Dead) != 0 {
		t.Errorf("Dead = %v, want empty: no gating metric died here", sum.Dead)
	}
	if !slices.Contains(sum.DeadDiagnostics, evals.MetricSeverityAccuracy) {
		t.Errorf("DeadDiagnostics = %v, want it to name %s", sum.DeadDiagnostics, evals.MetricSeverityAccuracy)
	}
}

// TestSummarizeCorpus_DeadDiagnosticDoesNotGate is the half of the line
// above that #179 described and did not implement. It appended the dead
// diagnostic to problems, which Summary.OK reads, so the report said
// "does not gate" while the code gated. Nothing caught it because no
// diagnostic was dead yet; #174's tool_coverage fix makes one dead
// permanently, so the whole E tier would have gone red on a fact about
// the ported corpus.
func TestSummarizeCorpus_DeadDiagnosticDoesNotGate(t *testing.T) {
	ds, tbl, err := loadFixtures(repoRoot)
	if err != nil {
		t.Fatalf("loadFixtures: %v", err)
	}

	sum, problems := summarizeCorpus(tbl, ds)
	if !slices.Contains(sum.DeadDiagnostics, evals.MetricToolCoverage) {
		t.Fatalf("DeadDiagnostics = %v, want it to name %s on the shipped corpus", sum.DeadDiagnostics, evals.MetricToolCoverage)
	}
	for _, p := range problems {
		if strings.Contains(p, evals.MetricToolCoverage) {
			t.Errorf("problem %q gates on a dead diagnostic; a diagnostic is not a parity claim, so it cannot redden the suite", p)
		}
	}
	if len(problems) != 0 {
		t.Errorf("problems = %v, want none on the shipped corpus", problems)
	}

	// And the report still says it, on a line of its own.
	var buf bytes.Buffer
	Summary{Tier: TierDeterministic, Corpus: sum}.WriteText(&buf)
	out := buf.String()
	if !strings.Contains(out, "dead diagnostic") || !strings.Contains(out, evals.MetricToolCoverage) {
		t.Errorf("report does not name the dead diagnostic:\n%s", out)
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("a dead diagnostic failed the report:\n%s", out)
	}
}

func renderJudge(t *testing.T, j *JudgeSummary) string {
	t.Helper()
	var buf bytes.Buffer
	Summary{Tier: TierJudge, Judge: j}.WriteText(&buf)
	return buf.String()
}
