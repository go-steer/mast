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

// One call past the ceiling, on purpose, to get the report back.
//
// A cap that fires produces nothing. For an agent that never got
// started that is the right answer and W10.3 settled it: a refusal is
// the case where nothing was looked at, and mast will not fabricate a
// finding for an agent that did not run.
//
// The premise fails for the agent this exists for. Measured on a live
// triage workload (#271): a diagnoser walked nodeSelector → template →
// metadata → three namespaces → all namespaces → computeclass → HPA →
// pods, and another spent six consecutive log queries and 269k tokens
// hunting an audit entry. Both were stopped by their own cap, and both
// had established a great deal by the time they were. What came back
// was an unresolved delegation. The tokens were spent either way, and
// the operator got an incident with nothing attached.
//
// So the grant is narrow and it is the model that writes the report,
// not mast. The refused agent is allowed exactly ONE more model call,
// with every tool but its finish tool taken away, and told to report
// what it can already support. Nothing is synthesized: if the model has
// nothing, it says so in its own report shape, which is a different and
// far more useful artifact than silence.
//
// Three bounds make it safe to hand out:
//
//   - It is once per agent per meter, latched here. The invalid-report
//     retry loop W10.3 found (3,292 finish_task calls in ninety
//     seconds) is fed by refusals that cost nothing; a grant that could
//     be re-taken would feed it a model call at a time, which is worse.
//     The second ask gets the ordinary refusal.
//   - It is opt-in per workload. A cap that can be exceeded by one call
//     is not what every operator declared, and the overshoot is real —
//     one call's worth, on an agent already at its ceiling.
//   - It grants nothing to an agent that has not spent anything. There
//     is no report to salvage from an agent refused on its first call,
//     and that is exactly the case W10.3 is about.
//
// One interaction is worth stating rather than discovering. The grant
// does not care which ceiling refused the call, but the turn driver
// does: a specialist's own cap closes one path and the turn routes on,
// so the report comes back to the coordinator as an answer, while the
// workload's cap still ends the turn with ErrRefused (W10.3). In the
// second case the report is written and lands in the session transcript
// and the event stream, but there is no turn result left to carry it.
// That is the right order — a session out of budget stops — and it is
// why a workload whose only ceiling is session-wide should expect this
// flag to buy it an artifact in the log rather than a better answer.

package budget

// AllowFinalReport reports whether author may make one last model call
// to write the report it was stopped before finishing — and consumes
// the grant, so a second ask returns false.
//
// False is the answer for an unconfigured meter, a nil meter, an author
// that has already taken its grant, and an author with no spend on the
// record. The caller treats false as the ordinary refusal.
//
// The grant does NOT relax any ceiling. It is a statement about one
// call at the seam that asks; Observe still folds what that call costs,
// the overshoot lands in the reported total, and the next Allow refuses
// on arithmetic that now includes it.
func (m *Meter) AllowFinalReport(author string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.finalReport {
		return false
	}
	if m.finalReportTaken[author] {
		return false
	}
	// Nothing spent, nothing to report. An author with no scope is
	// checked against the session's own spend: it is metered into the
	// session totals only, so that is the only record it has.
	spent := m.total
	if u, ok := m.spent[author]; ok && u != nil {
		spent = *u
	}
	if spent.calls == 0 {
		return false
	}
	if m.finalReportTaken == nil {
		m.finalReportTaken = make(map[string]bool, 1)
	}
	m.finalReportTaken[author] = true
	return true
}

// FinalReportsTaken counts the grants handed out, for the surfaces that
// have to report an overshoot rather than let it arrive unannounced in
// a total.
func (m *Meter) FinalReportsTaken() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.finalReportTaken)
}
