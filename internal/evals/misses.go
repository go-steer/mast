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

import "sort"

// Consequential misses (#170) — why the obvious metric is not here.
//
// The natural thing to measure is "did the run call the tools that had
// data for this scenario". Computed against both judged boards at the
// v0.4.0 tag, it flags 11 of 31 scenarios on Claude and 18 of 31 on
// Gemini — and the flagged scenarios score *better*, not worse:
// response_quality 0.886 vs 0.838 on Claude, 0.986 vs 0.962 on Gemini.
//
// That is the consolidation thesis showing up in the measurement.
// testdata/evals/intents.yaml maps 19 intents across 11 lookout tools,
// and 14 of the 19 are satisfiable by two or three different tools. Most
// "skipped" tools were genuinely redundant: something else had already
// answered the question. A metric that counts them is noise with a
// plausible name — the same trap tool_coverage fell into, one level up.
//
// What is worth counting is a consequential miss: an expected intent
// left unanswered when a tool in the catalog would have answered it. Not
// "a tool went uncalled" — "a question went unanswered and the catalog
// could have answered it". Filtered to that, the whole population across
// both v0.4.0 boards is four rows, one on Claude and three on Gemini,
// all of them node or pod saturation. Four is a list a human reads, so
// it is reported as a list rather than averaged into a rate — the same
// disposition #169's violations have, for the same reason.
//
// The partition is the point. An unsatisfied intent that no lookout tool
// serves is a statement about the catalog, not about the model: LC-13
// expects a rollback, and lookout excludes write tools by design. That
// row is already reported as a structural ceiling, and counting it here
// would confuse "the catalog cannot answer this" with "the model did not
// ask".

// ConsequentialMiss is one expected intent the run left unanswered that
// a tool in the catalog would have answered.
//
// ServedBy is every catalog tool that satisfies the intent, and each one
// is by construction a tool the run did not call: an intent with a
// called tool behind it is a satisfied intent and never reaches here.
type ConsequentialMiss struct {
	Intent   string   `json:"intent"`
	ServedBy []string `json:"served_by"`
}

// MissReport partitions one scenario's unsatisfied expectations by whose
// problem each one is: the model's, the catalog's, or the table's.
type MissReport struct {
	// Consequential is the subset attributable to tool selection.
	Consequential []ConsequentialMiss `json:"consequential,omitempty"`
	// OutOfCatalog is the intents no lookout tool serves — the structural
	// ceiling, reported beside the board and deliberately not here.
	OutOfCatalog []string `json:"out_of_catalog,omitempty"`
	// Untabled is expected tool names intents.yaml has never seen. They
	// deflate intent_coverage on purpose (a table gap should be visible),
	// but they are not a tool-selection failure and are not counted as
	// one.
	Untabled []string `json:"untabled,omitempty"`
}

// Empty reports whether the scenario left nothing unsatisfied at all.
func (m MissReport) Empty() bool {
	return len(m.Consequential) == 0 && len(m.OutOfCatalog) == 0 && len(m.Untabled) == 0
}

// ClassifyMisses explains what [IntentCoverage] scored: for every
// expected intent the run did not satisfy, whether a tool in the catalog
// would have satisfied it.
//
// It reads the same coverage the score does, so a miss the number counts
// and this list omits is not reachable.
func ClassifyMisses(tbl IntentTable, sc Scenario, tr Trace) MissReport {
	cov := readCoverage(tbl, sc, tr)
	rep := MissReport{Untabled: cov.unknown}
	for _, id := range cov.missing {
		served := tbl.ToolsSatisfying(id)
		if len(served) == 0 {
			rep.OutOfCatalog = append(rep.OutOfCatalog, id)
			continue
		}
		rep.Consequential = append(rep.Consequential, ConsequentialMiss{Intent: id, ServedBy: served})
	}
	return rep
}

// coverage is the shared reading of one scenario's expectations against
// one trace: what it wanted, what the table could not name, and what
// went unanswered.
//
// [IntentCoverage] scores it and [ClassifyMisses] explains it, and both
// read it from here rather than each deriving it. Two derivations of the
// same miss set would eventually disagree, and the disagreement would be
// invisible: a board whose number says one thing and whose list says
// another is worse than either alone.
type coverage struct {
	want    []string
	unknown []string
	missing []string
}

func readCoverage(tbl IntentTable, sc Scenario, tr Trace) coverage {
	cov := coverage{}
	cov.want, cov.unknown = tbl.IntentsFor(sc.Outputs.ExpectedTools)

	got := make(map[string]bool)
	for _, id := range tbl.SatisfiedBy(tr.CalledTools()) {
		got[id] = true
	}
	for _, id := range cov.want {
		if !got[id] {
			cov.missing = append(cov.missing, id)
		}
	}
	return cov
}

// ToolsSatisfying returns the lookout tools that satisfy an intent, in
// name order. Empty means the read-only catalog cannot answer that
// question at all, which is a ceiling rather than a miss.
func (t IntentTable) ToolsSatisfying(intent string) []string {
	var out []string
	for name, lt := range t.LookoutTools {
		for _, id := range lt.Satisfies {
			if id == intent {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
