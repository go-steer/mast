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
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-steer/mast/internal/compose"
	"github.com/go-steer/mast/internal/evals"
	"github.com/go-steer/mast/internal/evals/judge"
	"github.com/go-steer/mast/pkg/providers/anthropic"
)

// JudgeSummary is the metered tier's board: the 31 ported LangChain
// scenarios run against a live model over the fixture cluster, scored by
// the same deterministic evaluators the free tier checks the
// measurability of, plus the LLM-as-judge quality grade.
//
// It reports; it does not gate. A low score is a finding to act on, not
// a broken build — the plan's §2 four-tier strategy puts the gate at S,
// U and E precisely so that a model having a bad day cannot turn main
// red. What the judge tier *can* fail on is its own machinery: a
// scenario whose run never happened says nothing about mast, so it is a
// Problem.
type JudgeSummary struct {
	// Model is the model under test, Grader the one that scored
	// response_quality. Both are named because the number is only worth
	// reading next to what produced it — and because nothing can enforce
	// that they differ, which they should.
	Model  string `json:"model"`
	Grader string `json:"grader"`
	// Provider is the credential path, or "auto" when compose picked it
	// from the environment.
	Provider string `json:"provider"`

	// Authored is how many of the scenarios ran against a hand-written
	// fixture rather than one derived from quoted spans of the expected
	// response. Stated on the board because a reader's first question
	// about any judge number is how much of the cluster the fixture
	// author chose.
	Authored int `json:"authored_fixtures"`

	Scenes    []JudgeScenario  `json:"scenarios"`
	Aggregate []MetricSummary  `json:"aggregate"`
	Notes     []string         `json:"notes,omitempty"`
	Ceilings  []CeilingFinding `json:"ceilings,omitempty"`

	// Cost is J-cost-tier: a two-tier roster run live, then priced off
	// the meter. Nil when the check could not run at all, which is
	// itself reported as a Problem.
	//
	// It rides this board because it needs the same thing the corpus
	// needs and nothing cheaper has — a real provider answering with
	// real usage numbers. It is unlike every other row here in that its
	// verdict is arithmetic rather than judgment, which is why it can
	// gate (see [Summary.Problems]) where the scores cannot.
	Cost *judge.CostBoard `json:"cost,omitempty"`
	// CostSkipped is why there is no cost board, when the reason is that
	// the run had no live model to price. Carried as a field rather than
	// left as a nil Cost so the board states the absence instead of
	// having one fewer section than the reader expected.
	CostSkipped string `json:"cost_skipped,omitempty"`
}

// JudgeScenario is one corpus row's run.
type JudgeScenario struct {
	ID        string         `json:"id"`
	Category  string         `json:"category"`
	Tools     []string       `json:"tools_called"`
	Answering []string       `json:"answering_tools"`
	Results   []evals.Result `json:"results"`
	Response  string         `json:"response"`
	Ceiling   float64        `json:"intent_coverage_ceiling"`
	Authored  bool           `json:"authored_fixture,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// MetricSummary is one metric's mean across the rows that could score
// it. Vacuous rows are excluded from the mean rather than counted as
// zero — averaging in "there was nothing to check" is how a metric
// starts reporting the corpus's shape as the agent's performance.
type MetricSummary struct {
	Metric string `json:"metric"`
	// Mean is meaningless when Scored is zero; the report says so
	// rather than printing 0.000, which reads as a failing grade.
	Mean    float64 `json:"mean"`
	Scored  int     `json:"scored"`
	Vacuous int     `json:"vacuous"`
	// Diagnostic mirrors evals.Result.Diagnostic: a metric printed for
	// visibility that is not a parity claim. tool_coverage is the
	// standing example — name-level overlap scores mast's consolidated
	// read path as a regression.
	Diagnostic bool `json:"diagnostic,omitempty"`
}

// CeilingFinding names a scenario the read-only surface cannot fully
// cover, with the score it reached and the best it could have. Reported
// beside the board rather than folded into it: adjusting a score for a
// known structural gap is the tuning the plan warns against, but leaving
// the gap unstated would let a reader read a capped row as a miss.
type CeilingFinding struct {
	ID      string  `json:"id"`
	Ceiling float64 `json:"ceiling"`
	Scored  float64 `json:"scored"`
	Why     string  `json:"why"`
}

// runJudge scores the whole corpus against a live model.
//
// Sequential on purpose. Thirty-one runs against a frontier model take
// minutes, which is fine for a nightly, and a rate-limit error in the
// middle of a fan-out would land on the board as a scenario that "said
// nothing about mast" — noise indistinguishable from a real fixture
// failure. Cost and wall-clock are the plan's accepted price for this
// tier (~$5-15 a run).
func runJudge(ctx context.Context, cfg Config) (Summary, error) {
	sum := Summary{Tier: TierJudge}
	ds, tbl, err := loadFixtures(cfg.Root)
	if err != nil {
		return Summary{}, err
	}
	sum.Corpus, sum.Problems = summarizeCorpus(tbl, ds)

	overrides, err := judge.LoadOverrides(filepath.Join(cfg.Root, "testdata", "evals", "judge", "overrides.yaml"))
	if err != nil {
		return Summary{}, err
	}
	fixtures, err := judge.Fixtures(ds, overrides)
	if err != nil {
		return Summary{}, err
	}

	modelName := cfg.Model
	if modelName == "" {
		modelName = anthropic.DefaultModel
	}
	graderName := cfg.Grader
	if graderName == "" {
		graderName = anthropic.DefaultSmallModelID
	}

	under, err := compose.BuildModel(ctx, cfg.Provider, modelName)
	if err != nil {
		return Summary{}, fmt.Errorf("harness: build model under test: %w", err)
	}
	grading, err := compose.BuildModel(ctx, cfg.Provider, graderName)
	if err != nil {
		return Summary{}, fmt.Errorf("harness: build grading model: %w", err)
	}

	scratch, cleanup, err := scratchDir(cfg)
	if err != nil {
		return Summary{}, err
	}
	defer cleanup()

	rig, err := judge.NewRig(tbl, fixtures, under, scratch)
	if err != nil {
		return Summary{}, err
	}
	grader, err := judge.NewJudge(grading)
	if err != nil {
		return Summary{}, err
	}

	board := &JudgeSummary{
		Model:    modelName,
		Grader:   graderName,
		Provider: providerLabel(cfg.Provider),
	}
	if modelName == graderName {
		board.Notes = append(board.Notes, fmt.Sprintf(
			"the model under test and the grader are both %s — a model grading its own output flatters it; pass --grader to separate them", modelName))
	}

	note := progressFn(cfg.Progress)

	// J-cost-tier runs first. It is two specialists and one turn against
	// the same credentials the corpus is about to spend minutes on, and
	// its failures are machinery failures — a roster that will not
	// compose, a provider that will not answer. Finding that out now
	// rather than after thirty-one metered runs is worth the ordering.
	note("[cost] pricing a two-tier roster against %s", modelName)
	switch cost, err := judge.RunCost(ctx, under, modelName, cfg.Provider, scratch); {
	case errors.Is(err, judge.ErrCostNeedsLiveModel):
		// Not a Problem: the check was asked a question this
		// configuration cannot answer. Under an offline fake every score
		// on this board is machinery-shaped anyway, and calling the skip
		// a failure would train a reader to ignore the field that
		// matters when it is real. It is printed, loudly, so that the
		// skip is never read as a pass.
		board.CostSkipped = err.Error()
	case err != nil:
		sum.Problems = append(sum.Problems, fmt.Sprintf(
			"J-cost-tier did not run, so nothing checked that a tiered roster is billed at its own rates: %v", err))
	default:
		board.Cost = cost
		sum.Problems = append(sum.Problems, cost.Findings...)
	}

	for i, sc := range ds.Scenarios {
		if err := ctx.Err(); err != nil {
			return Summary{}, err
		}
		note("[%2d/%2d] %s", i+1, len(ds.Scenarios), sc.ID)
		row := JudgeScenario{ID: sc.ID, Category: sc.Category}
		out, err := rig.Run(ctx, sc)
		if err != nil {
			// The run never happened, so this row is not a score. Saying
			// so is a Problem: an incomplete board read as a complete one
			// understates or overstates whatever it is missing.
			row.Error = err.Error()
			board.Scenes = append(board.Scenes, row)
			sum.Problems = append(sum.Problems, fmt.Sprintf(
				"%s: the run did not complete, so the board is missing a row rather than scoring one: %v", sc.ID, err))
			continue
		}

		grade, err := grader.Grade(ctx, sc, out.Response)
		if err != nil {
			// A grader failure costs one column, not the row: the
			// deterministic metrics are already scored and are the
			// primary ones.
			sum.Problems = append(sum.Problems, fmt.Sprintf(
				"%s: response_quality was not graded, so that column is short a row: %v", sc.ID, err))
		} else {
			out.Quality = &grade
			out.Results = append(out.Results, grade.Result())
		}

		row.Tools = out.Tools
		row.Answering = out.Answering
		row.Results = out.Results
		row.Response = out.Response
		row.Ceiling = out.Ceiling
		row.Authored = out.Authored
		if out.Authored {
			board.Authored++
		}
		board.Scenes = append(board.Scenes, row)

		if out.Ceiling < 1 {
			board.Ceilings = append(board.Ceilings, CeilingFinding{
				ID:      sc.ID,
				Ceiling: out.Ceiling,
				Scored:  scoreOf(out.Results, evals.MetricIntentCoverage),
				Why:     "the corpus expects a write tool, which lookout's read-only surface excludes by design; remediation is mast's effect-outbox path",
			})
		}
	}

	board.Aggregate = aggregate(board.Scenes)
	sum.Judge = board
	return sum, nil
}

// progressFn returns a printer for the per-scenario line, or a no-op
// when the caller did not ask for one. Nil-checking once here rather
// than at each call site keeps the loop readable.
func progressFn(w io.Writer) func(string, ...any) {
	if w == nil {
		return func(string, ...any) {}
	}
	return func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }
}

func providerLabel(p string) string {
	if p == "" {
		return "auto (from the environment)"
	}
	return p
}

func scoreOf(results []evals.Result, metric string) float64 {
	for _, r := range results {
		if r.Metric == metric {
			return r.Score
		}
	}
	return 0
}

// aggregate means each metric over the rows that scored it, in a stable
// order: intent_coverage, then severity_accuracy, then response_quality,
// then the rest alphabetically. The order is the board's reading order
// and is deliberately not the gating set — severity_accuracy sits second
// because that is where a reader has looked for it since v0.3, and it
// stopped being a parity claim when #179 demoted it. What gates is
// marked on each row, not inferred from its column.
func aggregate(scenes []JudgeScenario) []MetricSummary {
	type acc struct {
		sum        float64
		scored     int
		vacuous    int
		diagnostic bool
	}
	byMetric := map[string]*acc{}
	for _, s := range scenes {
		for _, r := range s.Results {
			a, ok := byMetric[r.Metric]
			if !ok {
				a = &acc{}
				byMetric[r.Metric] = a
			}
			a.diagnostic = a.diagnostic || r.Diagnostic
			if r.Vacuous {
				a.vacuous++
				continue
			}
			a.sum += r.Score
			a.scored++
		}
	}

	rank := map[string]int{
		evals.MetricIntentCoverage:   0,
		evals.MetricSeverityAccuracy: 1,
		judge.MetricResponseQuality:  2,
	}
	out := make([]MetricSummary, 0, len(byMetric))
	for metric, a := range byMetric {
		m := MetricSummary{Metric: metric, Scored: a.scored, Vacuous: a.vacuous, Diagnostic: a.diagnostic}
		if a.scored > 0 {
			m.Mean = a.sum / float64(a.scored)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, oki := rank[out[i].Metric]
		rj, okj := rank[out[j].Metric]
		if oki != okj {
			return oki
		}
		if oki && ri != rj {
			return ri < rj
		}
		return out[i].Metric < out[j].Metric
	})
	return out
}

// writeJudge renders the metered board.
func (j *JudgeSummary) write(p func(string, ...any)) {
	p("")
	p("judge tier — model under test %s, grader %s, provider %s", j.Model, j.Grader, j.Provider)
	p("  fixtures: %d of %d hand-authored, the rest derived from quoted spans of the expected response (marked *)",
		j.Authored, len(j.Scenes))
	p("")
	p("  %-42s %-9s %-9s %-9s %s", "scenario", "intent", "severity", "quality", "tools")
	for _, s := range j.Scenes {
		if s.Error != "" {
			p("  %-42s %s", s.ID, "DID NOT RUN — "+s.Error)
			continue
		}
		capped := ""
		if s.Ceiling < 1 {
			capped = fmt.Sprintf(" (ceiling %.2f)", s.Ceiling)
		}
		id := s.ID
		if s.Authored {
			id += "*"
		}
		p("  %-42s %-9s %-9s %-9s %s%s",
			id,
			cell(s.Results, evals.MetricIntentCoverage),
			cell(s.Results, evals.MetricSeverityAccuracy),
			cell(s.Results, judge.MetricResponseQuality),
			strings.Join(s.Tools, ","),
			capped)
	}

	p("")
	for _, m := range j.Aggregate {
		var line string
		if m.Scored == 0 {
			// A mean of nothing is not zero. Printing 0.000 here would
			// read as every row failing the metric.
			line = fmt.Sprintf("  mean %-20s n/a — no scenario had anything to score", m.Metric)
		} else {
			line = fmt.Sprintf("  mean %-20s %.3f over %d scenario(s)", m.Metric, m.Mean, m.Scored)
			if m.Vacuous > 0 {
				line += fmt.Sprintf("; %d had nothing to score and are excluded", m.Vacuous)
			}
		}
		if m.Diagnostic {
			line += " [diagnostic, not a parity claim]"
		}
		p("%s", line)
	}

	if len(j.Ceilings) > 0 {
		p("")
		p("structural ceilings (reported, never folded into the score):")
		for _, c := range j.Ceilings {
			p("  %s scored %.2f against a ceiling of %.2f — %s", c.ID, c.Scored, c.Ceiling, c.Why)
		}
	}
	j.writeCost(p)

	for _, n := range j.Notes {
		p("")
		p("note: %s", n)
	}
	p("")
	p("a low score here is a finding, not a red build: this tier's scoring reports and does not gate.")
	p("J-cost-tier is the exception — its verdict is arithmetic, so a mispriced tier is a Problem.")
}

// writeCost renders the cost board, or says why there isn't one. The
// absent case gets a line of its own for the same reason the check
// exists at all: a section that silently disappears reads as a section
// that passed.
func (j *JudgeSummary) writeCost(p func(string, ...any)) {
	b := j.Cost
	p("")
	if b == nil {
		if j.CostSkipped != "" {
			p("J-cost-tier: SKIPPED — %s", j.CostSkipped)
			p("  no tier was priced this run; this is not evidence that tiered pricing works")
			return
		}
		p("J-cost-tier: DID NOT RUN — see problems above; no tier was priced this run")
		return
	}

	verdict := "every tiered specialist was billed at its own rate"
	if !b.OK() {
		verdict = "a tiered specialist was not billed at its own rate"
	}
	p("J-cost-tier — %s (root %s at $%.5f/1K, provider %s)", verdict, b.RootModel, b.RootRate, b.Provider)
	p("  %-14s %-9s %-24s %-6s %-8s %-11s %-11s %s",
		"specialist", "tier", "resolved", "calls", "tokens", "billed", "at root", "rate/1K")
	for _, s := range b.Scopes {
		billed := fmt.Sprintf("$%.5f", s.CostUSD)
		saved := fmt.Sprintf("$%.5f", s.AtParentRate)
		rate := fmt.Sprintf("$%.5f", s.GotRate)
		if s.Calls == 0 {
			billed, saved, rate = "—", "—", "—"
		}
		p("  %-14s %-9s %-24s %-6d %-8d %-11s %-11s %s",
			s.Name, s.Tier, s.Resolved, s.Calls, s.Tokens, billed, saved, rate)
		if len(s.Ran) > 0 {
			p("  %-14s ran as %s", "", strings.Join(s.Ran, ", "))
		}
	}
	for _, n := range b.Notes {
		p("  note: %s", n)
	}
	for _, f := range b.Findings {
		p("  PROBLEM: %s", f)
	}
}

func cell(results []evals.Result, metric string) string {
	for _, r := range results {
		if r.Metric == metric {
			if r.Vacuous {
				return "n/a"
			}
			return fmt.Sprintf("%.2f", r.Score)
		}
	}
	return "-"
}
