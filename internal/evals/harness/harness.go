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

// Package harness is the runnable form of the v0.3 parity eval suite
// (docs/v0.3-plan.md W0.4): the thing scripts/evals.sh invokes and CI
// gates on.
//
// It runs two suites, and the split is the plan's four-tier strategy
// (§2) rather than an implementation detail:
//
//   - The corpus suite loads the 31 ported LangChain scenarios and the
//     intent table and checks that the metrics can score them. It does
//     not run them. Scoring a trajectory requires a model that chooses,
//     and a scripted provider does not choose — replaying a fixture and
//     asserting the tools match the fixture asserts that the script
//     equals itself. Scoring the corpus is therefore the judge tier's
//     job (W0.5), and what the free tier gates is that the measurement
//     is not a constant function.
//
//   - The differentiator suite runs the five scenarios upstream's
//     harness structurally cannot express, each against the composed
//     runtime, and holds each to its declared outcome.
//
// # Why the second suite has an allowlist and the first does not
//
// The differentiators are the scoreboard's red rows in executable form,
// so most of them fail today and must be allowed to. That declaration
// lives in exactly one place — differentiators.Scenario.Expect — and is
// checked in both directions, so a capability that lands without its
// entry being flipped fails the suite. A separate allowlist file was
// considered and rejected: two records of one bit is the drift the
// bidirectional check exists to prevent, and the Go declaration is
// already a diffable artifact whose shrinking a reviewer can see. What
// this package adds is making that state the report's headline instead
// of something a reader has to reconstruct from test output.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-steer/mast/internal/evals"
	"github.com/go-steer/mast/internal/evals/differentiators"
)

// Tiers, per docs/v0.3-plan.md §2. S (smoke) and U (UAT) have their own
// harnesses — `go test` and scripts/uat-v0.2.sh — so this runner covers
// E and J only.
const (
	// TierDeterministic is the free, credential-free gate. Default.
	TierDeterministic = "deterministic"
	// TierJudge is the metered nightly report. Lands in W0.5.
	TierJudge = "judge"
)

// ErrTierNotImplemented is returned for a tier the harness knows about
// but has not built. It is an error rather than a skip on purpose:
// asking for the judge tier and silently getting the deterministic one
// would report a free run as a metered one.
type ErrTierNotImplemented struct {
	Tier string
	Why  string
}

func (e ErrTierNotImplemented) Error() string {
	return fmt.Sprintf("tier %q is not implemented: %s", e.Tier, e.Why)
}

// Config is what the runner needs from its caller.
type Config struct {
	// Root is the repository root, the base for testdata paths.
	Root string
	// Tier selects the suite. Empty means TierDeterministic.
	Tier string
	// Scratch is a directory the differentiator fixtures own for the
	// duration of the run. Empty means one is made under os.TempDir
	// (house rule #5) and removed afterwards.
	Scratch string
}

// CorpusSummary is what the corpus suite found.
type CorpusSummary struct {
	Dataset   string              `json:"dataset"`
	Scenarios int                 `json:"scenarios"`
	Intents   int                 `json:"intents"`
	Reach     []evals.MetricReach `json:"reach"`
	Dead      []string            `json:"dead_metrics,omitempty"`
}

// ScenarioSummary is one differentiator's outcome, flattened so the
// report can be serialized.
type ScenarioSummary struct {
	ID        string   `json:"id"`
	Invariant string   `json:"invariant"`
	Expected  string   `json:"expected"`
	Observed  string   `json:"observed"`
	Matched   bool     `json:"matched"`
	Blocked   string   `json:"blocked,omitempty"`
	Rows      []string `json:"rows"`
	Reason    string   `json:"reason,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// Summary is the whole run.
type Summary struct {
	Tier   string            `json:"tier"`
	Corpus CorpusSummary     `json:"corpus"`
	Scenes []ScenarioSummary `json:"differentiators"`
	// ExpectedFail is the allowlist, in report form: the scenario IDs
	// declared red today. Shrinking it is v0.3's progress metric, so it
	// is a first-class field rather than something to count by eye.
	ExpectedFail []string `json:"expected_fail"`
	// Problems are the reasons this run failed. Empty means green.
	Problems []string `json:"problems,omitempty"`
}

// OK reports whether the run gates green.
func (s Summary) OK() bool { return len(s.Problems) == 0 }

// Run executes the configured tier.
//
// It returns an error only when the harness itself could not run — an
// unreadable fixture, an unimplemented tier. A scenario that fails, or a
// metric that scores nothing, is a Problem on the Summary, not an error:
// the caller wants the whole report, not the first thing that went
// wrong.
func Run(ctx context.Context, cfg Config) (Summary, error) {
	tier := cfg.Tier
	if tier == "" {
		tier = TierDeterministic
	}
	switch tier {
	case TierDeterministic:
	case TierJudge:
		return Summary{}, ErrTierNotImplemented{
			Tier: TierJudge,
			Why:  "the judge tier lands in W0.5 (docs/v0.3-plan.md) — it needs live provider credentials, costs ~$5-15 a run, and reports rather than gates",
		}
	default:
		return Summary{}, fmt.Errorf("unknown tier %q (want %q or %q)", tier, TierDeterministic, TierJudge)
	}

	sum := Summary{Tier: tier}
	corpus, err := runCorpus(cfg.Root)
	if err != nil {
		return Summary{}, err
	}
	sum.Corpus = corpus
	for _, m := range corpus.Dead {
		sum.Problems = append(sum.Problems, fmt.Sprintf(
			"metric %q scores nothing anywhere in the corpus: it is a constant function, so no board it appears on means anything", m))
	}

	scratch := cfg.Scratch
	if scratch == "" {
		dir, err := os.MkdirTemp("", "mast-evals-")
		if err != nil {
			return Summary{}, fmt.Errorf("harness: scratch dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		scratch = dir
	}

	scenarios := differentiators.All()
	if err := differentiators.Validate(scenarios); err != nil {
		// A malformed registry invalidates the both-directions check, so
		// it is a harness failure and the suite does not run.
		return Summary{}, err
	}
	for _, rep := range differentiators.RunAll(ctx, scenarios, scratch) {
		s, problems := summarize(rep)
		sum.Scenes = append(sum.Scenes, s)
		sum.Problems = append(sum.Problems, problems...)
		if rep.Scenario.Expect == differentiators.Fail {
			sum.ExpectedFail = append(sum.ExpectedFail, rep.Scenario.ID)
		}
	}
	return sum, nil
}

func runCorpus(root string) (CorpusSummary, error) {
	dsPath := filepath.Join(root, "testdata", "evals", "scenarios", "langchain-sre.jsonl")
	ds, err := evals.LoadDataset(dsPath)
	if err != nil {
		return CorpusSummary{}, err
	}
	tbl, err := evals.LoadIntentTable(filepath.Join(root, "testdata", "evals", "intents.yaml"))
	if err != nil {
		return CorpusSummary{}, err
	}
	reach := evals.CorpusReach(tbl, ds)
	return CorpusSummary{
		Dataset:   ds.Meta.Fixture,
		Scenarios: len(ds.Scenarios),
		Intents:   len(tbl.Intents),
		Reach:     reach,
		Dead:      evals.DeadMetrics(reach),
	}, nil
}

func summarize(rep differentiators.Report) (ScenarioSummary, []string) {
	s := rep.Scenario
	out := ScenarioSummary{
		ID:        s.ID,
		Invariant: s.Invariant,
		Expected:  s.Expect.String(),
		Observed:  rep.Outcome.String(),
		Matched:   rep.Matches(),
		Blocked:   s.Blocked,
		Rows:      s.Rows,
		Reason:    rep.Result.Reason,
		Tools:     rep.Result.Trace.CalledTools(),
	}
	if rep.Err != nil {
		out.Error = rep.Err.Error()
	}

	var problems []string
	switch {
	case rep.Outcome == differentiators.Broken:
		problems = append(problems, fmt.Sprintf(
			"%s: BROKEN — the fixture did not produce a run, so this says nothing about mast: %v", s.ID, rep.Err))
	case rep.Matches():
	case rep.Outcome == differentiators.Pass:
		problems = append(problems, fmt.Sprintf(
			"%s: declared FAIL (blocked on %s) but the invariant now holds — the capability landed. "+
				"Flip Expect to Pass and drop Blocked; shrinking the expected-fail list is v0.3's progress metric.", s.ID, s.Blocked))
	default:
		problems = append(problems, fmt.Sprintf(
			"%s: declared PASS but the invariant no longer holds — regression in shipped behaviour: %s", s.ID, rep.Result.Reason))
	}
	return out, problems
}

// WriteText renders the operator-facing report.
func (s Summary) WriteText(w io.Writer) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }

	p("")
	p("mast v0.3 parity evals — tier %s", s.Tier)
	p("")
	p("corpus: %s (%d scenarios, %d intents)", s.Corpus.Dataset, s.Corpus.Scenarios, s.Corpus.Intents)
	for _, r := range s.Corpus.Reach {
		p("  %s", r)
	}
	p("")
	p("differentiators:")
	for _, sc := range s.Scenes {
		mark := "ok"
		if !sc.Matched {
			mark = "!!"
		}
		p("  %s %-22s %-6s (declared %s)", mark, sc.ID, sc.Observed, sc.Expected)
		if sc.Reason != "" {
			p("       %s", wrapIndent(sc.Reason, 7))
		}
		if sc.Blocked != "" {
			p("       blocked on %s", wrapIndent(sc.Blocked, 7))
		}
	}
	p("")
	if len(s.ExpectedFail) == 0 {
		p("expected-fail allowlist: empty — every differentiator holds")
	} else {
		p("expected-fail allowlist: %d of %d (%s)", len(s.ExpectedFail), len(s.Scenes), strings.Join(s.ExpectedFail, ", "))
		p("  shrinking this list is v0.3's progress metric; each entry names the workstream that removes it")
	}
	p("")
	if s.OK() {
		p("PASS")
		return
	}
	p("FAIL")
	for _, prob := range s.Problems {
		p("  - %s", prob)
	}
}

// WriteJSON renders the machine-readable report — the shape W0.5's
// nightly diffs one run against the last.
func (s Summary) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// wrapIndent hard-wraps a reason at a readable width so a long
// observation stays legible in a CI log.
func wrapIndent(s string, indent int) string {
	const width = 68
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := 0
	for i, word := range words {
		if i > 0 && line+1+len(word) > width {
			b.WriteString("\n" + strings.Repeat(" ", indent))
			line = 0
		} else if i > 0 {
			b.WriteString(" ")
			line++
		}
		b.WriteString(word)
		line += len(word)
	}
	return b.String()
}
