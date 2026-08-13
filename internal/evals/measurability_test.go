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

import (
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/effects"
)

func reachByMetric(t *testing.T, reach []MetricReach) map[string]MetricReach {
	t.Helper()
	out := make(map[string]MetricReach, len(reach))
	for _, r := range reach {
		out[r.Metric] = r
	}
	return out
}

// TestCorpusReach_RealCorpus pins that both gating corpus metrics can
// actually score the ported dataset — the property upstream's harness
// lacks on both of its custom-code metrics.
func TestCorpusReach_RealCorpus(t *testing.T) {
	ds := loadLangChain(t)
	tbl := loadTable(t)

	reach := CorpusReach(tbl, ds)
	if len(reach) != 3 {
		t.Fatalf("CorpusReach returned %d metrics, want 3", len(reach))
	}
	by := reachByMetric(t, reach)

	for _, metric := range []string{MetricIntentCoverage, MetricSeverityAccuracy} {
		r, ok := by[metric]
		if !ok {
			t.Fatalf("%s missing from the reach table", metric)
		}
		if r.Diagnostic {
			t.Errorf("%s is marked diagnostic; it gates", metric)
		}
		// Every scenario declares expected tools and every expected
		// response opens with a severity token, so both reach the whole
		// corpus. A drop here means the fixture or the extractor moved.
		if r.Reaches != 31 || r.Scenarios != 31 {
			t.Errorf("%s reaches %d/%d scenarios, want 31/31", metric, r.Reaches, r.Scenarios)
		}
	}
	if !by[MetricToolCoverage].Diagnostic {
		t.Errorf("%s must stay diagnostic: name-level overlap scores a consolidated read path as a regression", MetricToolCoverage)
	}
	if dead := DeadMetrics(reach); len(dead) != 0 {
		t.Errorf("DeadMetrics = %v, want none on the real corpus", dead)
	}
}

// TestCorpusReach_DetectsUpstreamDefects reproduces the two ways
// upstream's custom-code evaluators became constant functions and
// requires the reach table to name each one. This is the check that
// makes "a wholly-vacuous corpus is a harness failure, not a green
// board" mechanical.
func TestCorpusReach_DetectsUpstreamDefects(t *testing.T) {
	tbl := loadTable(t)

	tests := []struct {
		name string
		// mutate rewrites a well-formed scenario into the defective shape.
		mutate   func(*Scenario)
		wantDead []string
		wantLive string
	}{
		{
			// Upstream's tool_coverage reads expected_trajectory, a key no
			// row carries, so its expected set is always empty and it
			// returns 1.0 with "no expected trajectory to check against".
			name:     "no expected tools anywhere",
			mutate:   func(s *Scenario) { s.Outputs.ExpectedTools = nil },
			wantDead: []string{MetricIntentCoverage},
			wantLive: MetricSeverityAccuracy,
		},
		{
			// Upstream's severity_accuracy matches a bracketed token the
			// corpus never writes, so extraction fails on the example side
			// for all 31 rows regardless of what the agent produced.
			name:     "no severity token anywhere",
			mutate:   func(s *Scenario) { s.Outputs.ExpectedResponse = "the cluster looks unwell" },
			wantDead: []string{MetricSeverityAccuracy},
			wantLive: MetricIntentCoverage,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ds := loadLangChain(t)
			for i := range ds.Scenarios {
				tc.mutate(&ds.Scenarios[i])
			}

			reach := CorpusReach(tbl, ds)
			by := reachByMetric(t, reach)
			for _, metric := range tc.wantDead {
				if !by[metric].Dead() {
					t.Errorf("%s reaches %d scenarios, want 0 — the defect went undetected",
						metric, by[metric].Reaches)
				}
			}
			if by[tc.wantLive].Dead() {
				t.Errorf("%s went dead too; the probe is not discriminating between metrics", tc.wantLive)
			}

			dead := DeadMetrics(reach)
			for _, metric := range tc.wantDead {
				if !contains(dead, metric) {
					t.Errorf("DeadMetrics = %v, want it to name %s", dead, metric)
				}
			}
			// The diagnostic metric goes dead in the first case too, and
			// must never gate: it is not a claim about mast.
			if contains(dead, MetricToolCoverage) {
				t.Errorf("DeadMetrics = %v, want the diagnostic metric excluded", dead)
			}
		})
	}
}

// TestCorpusReach_IsTraceIndependent pins the property the probe relies
// on: for the three corpus metrics, whether there is anything to score
// is a fact about the scenario, not about the run. CorpusReach passes an
// empty Trace to isolate the corpus half; if a future change made any of
// these metrics vacuous-or-not depending on the trace, that probe would
// silently start measuring the wrong thing.
func TestCorpusReach_IsTraceIndependent(t *testing.T) {
	ds := loadLangChain(t)
	tbl := loadTable(t)

	// A populated trace: a read, a completed mutation, and a final
	// response carrying a severity — enough to move every evaluator's
	// score without touching what it has to score against.
	busy := Trace{
		Calls: []Call{
			{Name: "k8s_triage_workload", ID: "c1", EventIndex: 0, Completed: true, ResponseIndex: 1},
			{Name: "scale_deployment", ID: "c2", Class: effects.ClassMutating, EventIndex: 2, Completed: true, ResponseIndex: 3},
		},
		FinalText: "CRITICAL: the deployment is wedged",
	}

	for _, sc := range ds.Scenarios {
		pairs := []struct {
			metric      string
			empty, busy Result
		}{
			{MetricIntentCoverage, IntentCoverage(tbl, sc, Trace{}), IntentCoverage(tbl, sc, busy)},
			{MetricToolCoverage, ToolCoverage(sc, Trace{}), ToolCoverage(sc, busy)},
			{MetricSeverityAccuracy, SeverityAccuracy(sc, Trace{}), SeverityAccuracy(sc, busy)},
		}
		for _, p := range pairs {
			if p.empty.Vacuous != p.busy.Vacuous {
				t.Fatalf("%s/%s: vacuity depends on the trace (empty=%v, busy=%v); "+
					"CorpusReach's empty-trace probe no longer isolates the corpus",
					sc.ID, p.metric, p.empty.Vacuous, p.busy.Vacuous)
			}
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestMetricReach_StringNamesTheDefect keeps the operator-facing line
// honest: a dead metric has to read as broken, not as a low score.
func TestMetricReach_StringNamesTheDefect(t *testing.T) {
	dead := MetricReach{Metric: MetricSeverityAccuracy, Scenarios: 31}
	if got := dead.String(); !strings.Contains(got, "DEAD") {
		t.Errorf("String() = %q, want it to flag the dead metric", got)
	}
	live := MetricReach{Metric: MetricSeverityAccuracy, Scenarios: 31, Reaches: 31}
	if got := live.String(); strings.Contains(got, "DEAD") {
		t.Errorf("String() = %q, want no dead flag on a reachable metric", got)
	}
}
