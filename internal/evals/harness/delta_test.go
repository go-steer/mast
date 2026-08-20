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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/mast/internal/evals"
	"github.com/go-steer/mast/internal/evals/judge"
)

func row(id string, intent, quality float64) JudgeScenario {
	return JudgeScenario{ID: id, Results: []evals.Result{
		{Metric: evals.MetricIntentCoverage, Score: intent},
		{Metric: judge.MetricResponseQuality, Score: quality},
	}}
}

func delta(t *testing.T, prev, cur Summary) string {
	t.Helper()
	var buf bytes.Buffer
	cur.WriteDelta(&buf, prev)
	return buf.String()
}

// TestWriteDelta_ReportsMovementAndSuppressesNoise is the nightly's whole
// job: say what moved, and stay quiet about what did not. A delta that
// flags every hundredth of a sampled score teaches its reader to skip it.
func TestWriteDelta_ReportsMovementAndSuppressesNoise(t *testing.T) {
	board := func(scenes []JudgeScenario, mean float64) Summary {
		return Summary{
			Tier: TierJudge,
			Judge: &JudgeSummary{
				Model: "claude-opus-4-7", Grader: "claude-haiku-4-5",
				Scenes:    scenes,
				Aggregate: []MetricSummary{{Metric: evals.MetricIntentCoverage, Mean: mean, Scored: len(scenes)}},
			},
		}
	}
	prev := board([]JudgeScenario{
		row("LC-01", 1, 1),
		row("LC-02", 0.5, 0.75),
		row("LC-03", 1, 1),
	}, 0.833)
	cur := board([]JudgeScenario{
		row("LC-01", 1, 1),      // unchanged
		row("LC-02", 1, 0.755),  // intent up a lot, quality up inside the floor
		row("LC-04", 0.5, 0.25), // new row; LC-03 gone
	}, 0.833)

	out := delta(t, prev, cur)
	for _, want := range []string{
		"LC-02 intent_coverage: 1.000 (was 0.500, up 0.500)",
		"LC-04: new row",
		"LC-03: on the previous board, absent from this one",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("delta is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "LC-01 ") {
		t.Errorf("an unchanged row was reported as movement:\n%s", out)
	}
	if strings.Contains(out, "LC-02 response_quality") {
		t.Errorf("a 0.005 move was reported as movement; the noise floor is not holding:\n%s", out)
	}
	// A flat mean over rows that did move must still be printed, so the
	// reader can see the aggregate hid something.
	if !strings.Contains(out, "intent_coverage      0.833 (was 0.833, flat)") {
		t.Errorf("the aggregate line is missing or misrendered:\n%s", out)
	}
}

// TestWriteDelta_UnscoredMetricIsNotAFlatTrend: a metric that scored
// nothing in both runs must not read as steady at zero, and one that
// stopped reaching the corpus is a finding rather than a drop.
func TestWriteDelta_UnscoredMetricIsNotAFlatTrend(t *testing.T) {
	board := func(mean float64, scored int) Summary {
		return Summary{Tier: TierJudge, Judge: &JudgeSummary{
			Aggregate: []MetricSummary{{Metric: evals.MetricSeverityAccuracy, Mean: mean, Scored: scored}},
		}}
	}

	if out := delta(t, board(0, 0), board(0, 0)); !strings.Contains(out, "n/a both runs") {
		t.Errorf("a metric that scored nothing twice was reported as flat at zero:\n%s", out)
	}
	if out := delta(t, board(0.8, 31), board(0, 0)); !strings.Contains(out, "stopped reaching the corpus") {
		t.Errorf("a metric that went unscorable was reported as a score change:\n%s", out)
	}
	if out := delta(t, board(0, 0), board(0.8, 31)); !strings.Contains(out, "the previous run had nothing to score") {
		t.Errorf("a metric that became scorable was reported as a rise from zero:\n%s", out)
	}
}

// TestWriteDelta_ModelChangeInvalidatesComparison: two models are two
// measurements. Presenting their difference as movement would read as a
// regression whenever the nightly's model pin changes.
func TestWriteDelta_ModelChangeInvalidatesComparison(t *testing.T) {
	prev := Summary{Tier: TierJudge, Judge: &JudgeSummary{Model: "claude-opus-4-7", Grader: "claude-haiku-4-5"}}
	cur := Summary{Tier: TierJudge, Judge: &JudgeSummary{Model: "gemini-3-pro", Grader: "claude-haiku-4-5"}}
	if out := delta(t, prev, cur); !strings.Contains(out, "not comparable") {
		t.Errorf("delta compared two different models without saying so:\n%s", out)
	}
}

// TestWriteDelta_AllowlistDirectionsReadDifferently pins the asymmetry:
// an entry leaving the expected-fail list is progress, an entry arriving
// is something to check. The same set difference, opposite meanings.
func TestWriteDelta_AllowlistDirectionsReadDifferently(t *testing.T) {
	prev := Summary{Tier: TierDeterministic, ExpectedFail: []string{"E-budget-exhaustion", "E-approval-edited"}}
	cur := Summary{Tier: TierDeterministic, ExpectedFail: []string{"E-approval-edited", "E-approval-rejected"}}

	out := delta(t, prev, cur)
	if !strings.Contains(out, "E-budget-exhaustion no longer declared red — a capability landed") {
		t.Errorf("a shrinking allowlist was not reported as progress:\n%s", out)
	}
	if !strings.Contains(out, "E-approval-rejected newly declared red") {
		t.Errorf("a growing allowlist was not flagged:\n%s", out)
	}

	same := delta(t, prev, prev)
	if !strings.Contains(same, "unchanged (2)") {
		t.Errorf("an unchanged allowlist was not reported as such:\n%s", same)
	}
}

// TestWriteDelta_RowStoppedRunning: a row that scored last night and did
// not run tonight is the one case where a missing number matters more
// than any number that changed.
func TestWriteDelta_RowStoppedRunning(t *testing.T) {
	prev := Summary{Tier: TierJudge, Judge: &JudgeSummary{Scenes: []JudgeScenario{row("LC-01", 1, 1)}}}
	cur := Summary{Tier: TierJudge, Judge: &JudgeSummary{Scenes: []JudgeScenario{
		{ID: "LC-01", Error: "429 rate limited"},
	}}}
	out := delta(t, prev, cur)
	if !strings.Contains(out, "did not run this time — 429 rate limited") {
		t.Errorf("a row that stopped running was not reported:\n%s", out)
	}
}

// TestWriteDelta_ValidityMovesWhileTheScoresDoNot is why #169's counts
// are in the delta at all: the two boards below score identically, and
// the second one reached that score without reading anything.
func TestWriteDelta_ValidityMovesWhileTheScoresDoNot(t *testing.T) {
	board := func(v ValidityBoard) Summary {
		return Summary{Tier: TierJudge, Judge: &JudgeSummary{
			Model: "claude-opus-4-7", Grader: "claude-haiku-4-5",
			Scenes:    []JudgeScenario{row("LC-01", 1, 1)},
			Aggregate: []MetricSummary{{Metric: evals.MetricIntentCoverage, Mean: 1, Scored: 1}},
			Validity:  v,
		}}
	}
	prev := board(ValidityBoard{Calls: 12, EmptyReads: 2})
	cur := board(ValidityBoard{
		Calls: 12, EmptyReads: 9, Blind: []string{"LC-04", "LC-07"},
		Malformed: []ScenarioViolation{{Scenario: "LC-02", Violation: evals.Violation{Kind: evals.ViolationUnknownTool}}},
	})

	out := delta(t, prev, cur)
	if !strings.Contains(out, "no scenario moved") {
		t.Fatalf("the scores were meant to be flat between these two boards:\n%s", out)
	}
	for _, want := range []string{
		"malformed 0 → 1",
		"empty reads 2 → 9",
		"started running blind",
		"LC-04, LC-07",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the delta is missing %q:\n%s", want, out)
		}
	}

	// And back the other way: a board that stopped running blind should
	// say so, or the only direction the delta reports is bad news.
	back := delta(t, cur, prev)
	if !strings.Contains(back, "no longer running blind: LC-04, LC-07") {
		t.Errorf("the recovery was not reported:\n%s", back)
	}

	// Two boards with no calls at all get no section rather than a row
	// of zeroes, which would read as a measurement that happened.
	empty := delta(t, board(ValidityBoard{}), board(ValidityBoard{}))
	if strings.Contains(empty, "malformed") {
		t.Errorf("a delta between two boards with no calls printed a validity section:\n%s", empty)
	}
}

// TestLoadSummary_RoundTripsARealBoard is the nightly's actual sequence:
// yesterday's JSON on disk, today's run, one delta.
func TestLoadSummary_RoundTripsARealBoard(t *testing.T) {
	sum, err := Run(context.Background(), Config{Root: repoRoot, Scratch: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	path := filepath.Join(t.TempDir(), "board.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := sum.WriteJSON(f); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	back, err := LoadSummary(path)
	if err != nil {
		t.Fatalf("LoadSummary: %v", err)
	}
	if out := delta(t, back, sum); !strings.Contains(out, "unchanged") {
		t.Errorf("a board compared against itself reported movement:\n%s", out)
	}

	if _, err := LoadSummary(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("LoadSummary accepted a path that does not exist")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("not a board"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadSummary(bad); err == nil {
		t.Error("LoadSummary accepted a file that is not a board")
	}
}
