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

package monitor_test

import (
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/monitor"
)

func TestParseLogfmt(t *testing.T) {
	out := strings.Join([]string{
		`kind=pod severity=critical namespace=prod name=api-7d9 reason=CrashLoopBackOff message="back-off 5m0s restarting failed container" transition=new subject_key=pod/prod/api-7d9/CrashLoopBackOff first_seen=2026-08-21T09:00:00Z`,
		`kind=pod severity=warning namespace=prod name=web-2f1 reason=Unhealthy message="readiness probe failed" transition=resolved subject_key=pod/prod/web-2f1/Unhealthy prev_severity=critical`,
		`scanned=412 findings=2 elapsed=1.9s`,
	}, "\n")

	set, err := monitor.Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if set.Scanned != 412 {
		t.Errorf("Scanned = %d, want 412", set.Scanned)
	}
	if len(set.Transitions) != 2 {
		t.Fatalf("got %d transitions, want 2: %+v", len(set.Transitions), set.Transitions)
	}
	first := set.Transitions[0]
	if first.Class != "new" {
		t.Errorf("Class = %q, want new", first.Class)
	}
	if first.SubjectKey != "pod/prod/api-7d9/CrashLoopBackOff" {
		t.Errorf("SubjectKey = %q", first.SubjectKey)
	}
	if got := first.Fields["message"]; got != "back-off 5m0s restarting failed container" {
		t.Errorf("quoted message = %q", got)
	}
	if got := first.Fields["severity"]; got != "critical" {
		t.Errorf("severity = %q", got)
	}
	// The two keys that got promoted to typed fields are not also left
	// in the bag: one fact, one spelling.
	if _, dup := first.Fields["transition"]; dup {
		t.Errorf("transition left in Fields as well as Class: %+v", first.Fields)
	}
	if _, dup := first.Fields["subject_key"]; dup {
		t.Errorf("subject_key left in Fields as well as SubjectKey: %+v", first.Fields)
	}
	if set.Transitions[1].Class != "resolved" {
		t.Errorf("second Class = %q, want resolved", set.Transitions[1].Class)
	}
	if set.Empty() {
		t.Error("Empty() true with two transitions")
	}
}

// TestParseTrustsTheClassification is the leg that fails the moment
// anyone adds a local heuristic.
//
// The record says `escalated`, and every field mast can see says
// otherwise: the severity did not change, prev_severity equals
// severity, and it has been open since yesterday. A reader that
// "sanity checks" the class against the severities — the single most
// tempting thing to add here — would downgrade this to ongoing or drop
// it, and the operator would never hear about a finding lookout had
// decided was getting worse for a reason mast cannot see (a burn rate,
// a repeat count, a policy window).
//
// The classification is not mast's to second-guess. It comes through
// verbatim.
func TestParseTrustsTheClassification(t *testing.T) {
	out := strings.Join([]string{
		`kind=pod severity=warning prev_severity=warning namespace=prod name=api-7d9 reason=Unhealthy transition=escalated subject_key=pod/prod/api-7d9/Unhealthy first_seen=2026-08-20T09:00:00Z last_seen=2026-08-21T09:00:00Z`,
		`scanned=1 findings=1 elapsed=12ms`,
	}, "\n")

	set, err := monitor.Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(set.Transitions) != 1 {
		t.Fatalf("got %d transitions, want 1", len(set.Transitions))
	}
	if got := set.Transitions[0].Class; got != "escalated" {
		t.Errorf("Class = %q, want escalated — the severities are equal, and that is not mast's business", got)
	}
	if got := set.Classes(); len(got) != 1 || got[0] != "escalated=1" {
		t.Errorf("Classes() = %v, want [escalated=1]", got)
	}
}

// A class this build has never heard of is a class lookout added, not
// an error. It rides through with the same fidelity as the five mast's
// docs happen to name.
func TestParseAcceptsAnUnknownClass(t *testing.T) {
	out := "transition=quiesced subject_key=node/gke-pool-3 reason=Drained\nscanned=9 findings=1\n"

	set, err := monitor.Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := set.Transitions[0].Class; got != "quiesced" {
		t.Errorf("Class = %q, want quiesced", got)
	}
}

func TestParseJSONRecords(t *testing.T) {
	out := strings.Join([]string{
		`{"kind":"pod","severity":"critical","transition":"new","subject_key":"pod/prod/api-7d9/CrashLoopBackOff","restarts":14,"acked":false}`,
		`{"scanned":412,"findings":1,"elapsed":"1.9s"}`,
	}, "\n")

	set, err := monitor.Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if set.Scanned != 412 {
		t.Errorf("Scanned = %d, want 412", set.Scanned)
	}
	if len(set.Transitions) != 1 {
		t.Fatalf("got %d transitions, want 1", len(set.Transitions))
	}
	got := set.Transitions[0]
	if got.Class != "new" || got.SubjectKey != "pod/prod/api-7d9/CrashLoopBackOff" {
		t.Errorf("record = %+v", got)
	}
	if got.Fields["restarts"] != "14" {
		t.Errorf("numeric field = %q, want 14", got.Fields["restarts"])
	}
	if got.Fields["acked"] != "false" {
		t.Errorf("bool field = %q, want false", got.Fields["acked"])
	}
}

// The encoding is detected per line, because `format` is the caller's
// flag and a stream that changed mid-flight is still a whole answer.
func TestParseMixedEncodings(t *testing.T) {
	out := strings.Join([]string{
		`transition=new subject_key=a`,
		`{"transition":"resolved","subject_key":"b"}`,
		`scanned=2 findings=2`,
	}, "\n")

	set, err := monitor.Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(set.Transitions) != 2 {
		t.Fatalf("got %d transitions, want 2", len(set.Transitions))
	}
	if set.Transitions[0].Class != "new" || set.Transitions[1].Class != "resolved" {
		t.Errorf("transitions = %+v", set.Transitions)
	}
}

// A healthy quiet cycle: the summary alone, nothing changed. This must
// NOT be an error — it is the common case, and the one W4.5 declines to
// notify on.
func TestParseQuietCycle(t *testing.T) {
	set, err := monitor.Parse("scanned=412 findings=0 elapsed=1.4s\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !set.Empty() {
		t.Errorf("Empty() false: %+v", set.Transitions)
	}
	if set.Scanned != 412 {
		t.Errorf("Scanned = %d, want 412", set.Scanned)
	}
	if got := set.Classes(); len(got) != 0 {
		t.Errorf("Classes() = %v, want empty", got)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{{
		name: "no summary line at all",
		out:  "transition=new subject_key=a\ntransition=new subject_key=b\n",
		want: "does not end with a summary line",
	}, {
		// The one that matters most: a truncated read looks exactly
		// like a quiet cycle unless the terminator is mandatory.
		name: "truncated to nothing",
		out:  "",
		want: "empty result",
	}, {
		name: "summary present but records lost in transit",
		out:  "transition=new subject_key=a\nscanned=400 findings=7\n",
		want: "summary says findings=7 but 1 record line(s) arrived",
	}, {
		name: "summary only counts findings, not scanned",
		out:  "findings=0\n",
		want: "does not end with a summary line",
	}, {
		name: "unclassified record",
		out:  "subject_key=a severity=critical\nscanned=1 findings=1\n",
		want: "carries no transition= class",
	}, {
		name: "classified but unnamed subject",
		out:  "transition=escalated severity=critical\nscanned=1 findings=1\n",
		want: "carries no subject_key=",
	}, {
		name: "non-numeric scanned",
		out:  "scanned=many findings=0\n",
		want: `scanned="many" is not a number`,
	}, {
		name: "bare token where a record was expected",
		out:  "PANIC: runtime error\nscanned=1 findings=1\n",
		want: "bare token where a key=value was expected",
	}, {
		name: "unterminated quote",
		out:  "transition=new subject_key=a message=\"half a mes\nscanned=1 findings=1\n",
		want: "unterminated quoted value",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, err := monitor.Parse(tc.out)
			if err == nil {
				t.Fatalf("Parse returned no error; set = %+v", set)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseResult(t *testing.T) {
	set, err := monitor.ParseResult(map[string]any{
		"output": "transition=new subject_key=a\nscanned=3 findings=1\n",
	})
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if len(set.Transitions) != 1 || set.Scanned != 3 {
		t.Errorf("set = %+v", set)
	}
}

func TestParseResultRejects(t *testing.T) {
	cases := []struct {
		name   string
		result map[string]any
		want   string
	}{{
		name:   "no output key",
		result: map[string]any{"result": "…", "error": "…"},
		want:   `carries no "output" key (got: error, result)`,
	}, {
		// A server answering with structured content is a different
		// tool than the one the bundle meant to name — say so, rather
		// than coercing a shape that has no summary line in it.
		name:   "structured content",
		result: map[string]any{"output": map[string]any{"findings": []any{}}},
		want:   "not the text of a record stream",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := monitor.ParseResult(tc.result)
			if err == nil {
				t.Fatal("ParseResult returned no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestClassesTally(t *testing.T) {
	out := strings.Join([]string{
		`transition=new subject_key=a`,
		`transition=escalated subject_key=b`,
		`transition=new subject_key=c`,
		`transition=quiesced subject_key=d`,
		`scanned=40 findings=4`,
	}, "\n")

	set, err := monitor.Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := strings.Join(set.Classes(), " ")
	if want := "escalated=1 new=2 quiesced=1"; got != want {
		t.Errorf("Classes() = %q, want %q", got, want)
	}
}

// Order is the producer's. A notifier that re-sorted would be inventing
// an emphasis lookout did not write.
func TestParsePreservesOrder(t *testing.T) {
	out := strings.Join([]string{
		`transition=resolved subject_key=z`,
		`transition=new subject_key=a`,
		`transition=escalated subject_key=m`,
		`scanned=3 findings=3`,
	}, "\n")

	set, err := monitor.Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var keys []string
	for _, tr := range set.Transitions {
		keys = append(keys, tr.SubjectKey)
	}
	if got := strings.Join(keys, ","); got != "z,a,m" {
		t.Errorf("order = %q, want z,a,m", got)
	}
}
