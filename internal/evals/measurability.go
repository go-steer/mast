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

package evals

import "fmt"

// MetricReach reports how much of the corpus one metric can actually
// score — the guard against a metric that is green because it never ran.
//
// Upstream's harness is the cautionary case, twice over: its
// tool_coverage reads a key no scenario carries and returns 1.0 on all
// 31 rows, and its severity_accuracy extracts a bracketed token the
// corpus never writes and returns 0 on all 31. Both are constant
// functions, and nothing in either output says so. Result.Vacuous marks
// the individual case; this is the corpus-wide roll-up that turns it
// into a harness failure instead of a green board.
type MetricReach struct {
	Metric string
	// Diagnostic mirrors Result.Diagnostic: a diagnostic metric is
	// reported but never gates, because it is not a claim about mast.
	Diagnostic bool
	// Scenarios is how many the metric was probed against.
	Scenarios int
	// Reaches is how many of them give it something to score.
	Reaches int
}

// Dead reports a metric that can score nothing anywhere in the corpus.
// Its score carries no information, whatever it happens to be.
func (r MetricReach) Dead() bool { return r.Scenarios > 0 && r.Reaches == 0 }

// String renders one row of the reach table.
func (r MetricReach) String() string {
	s := fmt.Sprintf("%-18s %d/%d scenarios scorable", r.Metric, r.Reaches, r.Scenarios)
	if r.Diagnostic {
		s += " (diagnostic)"
	}
	if r.Dead() {
		s += "  <- DEAD: scores nothing, anywhere"
	}
	return s
}

// CorpusReach measures the corpus-side metrics against the loaded
// dataset and intent table.
//
// The three metrics here are the ones whose vacuity is a property of the
// *expectation* rather than of any run: a scenario that declares no
// expected tools gives intent_coverage nothing to score no matter what
// the agent does, and one whose expected response carries no severity
// token gives severity_accuracy nothing to compare. That is why the
// probe passes an empty Trace — it isolates the corpus half, and it uses
// the real evaluators so the guard follows any change to what vacuity
// means. TestCorpusReach_IsTraceIndependent pins the property the probe
// relies on.
//
// effect_ordering and exactly_once are deliberately absent: their
// vacuity is a property of the run (no mutating effects to order), not
// of the corpus, so there is nothing here for them to be measured
// against. The differentiator tier is what proves those two are not
// constants — E-exactly-once asserts both score 1.00 on a run that
// actually mutates, and would catch an evaluator that had degenerated
// into returning a fixed value.
func CorpusReach(tbl IntentTable, ds Dataset) []MetricReach {
	reach := []MetricReach{
		{Metric: MetricIntentCoverage},
		{Metric: MetricToolCoverage, Diagnostic: true},
		{Metric: MetricSeverityAccuracy},
	}
	for _, sc := range ds.Scenarios {
		probes := []Result{
			IntentCoverage(tbl, sc, Trace{}),
			ToolCoverage(sc, Trace{}),
			SeverityAccuracy(sc, Trace{}),
		}
		for i := range reach {
			reach[i].Scenarios++
			if !probes[i].Vacuous {
				reach[i].Reaches++
			}
		}
	}
	return reach
}

// DeadMetrics returns the gating metrics that score nothing anywhere.
// A non-empty result is a harness failure, not a red scoreboard row:
// the measurement is broken, so the board says nothing either way.
func DeadMetrics(reach []MetricReach) []string {
	var dead []string
	for _, r := range reach {
		if r.Diagnostic || !r.Dead() {
			continue
		}
		dead = append(dead, r.Metric)
	}
	return dead
}
