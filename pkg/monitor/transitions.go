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

// Package monitor carries what an unattended monitoring cycle learned
// between one run and the next (v0.5 W4.4).
//
// # What this package deliberately does not know
//
// It does not know what a transition MEANS. The tool that classified
// the run — `k8s_findings_diff` for a Kubernetes workload, anything
// with the same output contract for anything else — owns that
// vocabulary, and mast passes its answer through verbatim. There is no
// enum here, no `const TransitionEscalated`, no comparison of two
// severities to decide whether something got worse, and no
// fingerprinting to decide whether two findings are the same subject.
// Every one of those would be a second source of truth for a question
// something else already answered, and the second source is the one
// that will disagree at 3am.
//
// The rule to apply when extending this package: mast may check that a
// record is WELL FORMED, and may not check that it is RIGHT. A missing
// subject key is malformed — nothing downstream can ack or de-duplicate
// a subject it cannot name. A transition class mast has never seen is
// not malformed; it is lookout shipping a new class, and it rides
// through untouched.
//
// # The wire contract
//
// One record per line, either logfmt or a flat JSON object, detected
// per line — the same leniency lookout's own `--report` reader offers,
// for the same reason: the producer's `format` is a caller's choice and
// a consumer that only speaks one of them fails on a config change it
// has no part in.
//
// The stream is terminated by a mandatory summary line carrying
// `scanned=<n> findings=<n>`. A result without one is VOID and is an
// error here, not an empty set. That distinction is the whole reason
// the summary exists: a truncated read, a killed process or a half
// written pipe all produce a prefix of a healthy answer, and a prefix
// of "nothing changed" is indistinguishable from "nothing changed"
// unless somebody insists on the terminator. `findings=<n>` is checked
// against the records actually parsed for the same reason — the count
// comes from the producer's own writer and cannot drift from what it
// emitted, so a disagreement means bytes went missing in transit.
package monitor

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Transition is one subject's change since the previous run.
type Transition struct {
	// SubjectKey is the producer's instance-grain key for the thing
	// that changed. mast treats it as an opaque identifier: it is what
	// an ack is keyed by (W4.6) and what two cycles are joined on, and
	// nothing here parses its internal structure.
	SubjectKey string `json:"subject_key"`

	// Class is the transition class, verbatim as the classifying tool
	// wrote it — `new`, `ongoing`, `escalated`, `resolved`,
	// `suppressed` in lookout's current vocabulary, and whatever it
	// adds next without a change here.
	//
	// Named Class rather than Transition to keep call sites readable;
	// the JSON tag is the producer's own spelling, because the envelope
	// the model reads should say what lookout says.
	Class string `json:"transition"`

	// Fields is every other key on the record, unread. severity,
	// prev_severity, first_seen, message, the lot — carried so the
	// model and the notifier have the full record, and deliberately not
	// promoted to typed fields, because typing them here is how a
	// consumer starts having opinions about them.
	Fields map[string]string `json:"fields,omitempty"`
}

// Set is one cycle's classification: what changed, and how much was
// looked at to find out.
type Set struct {
	// Transitions are the records, in the order the producer emitted
	// them. Order is preserved rather than sorted: the producer chose
	// it, and a notifier that re-orders is inventing an emphasis.
	//
	// Serialized as `records` because the field this Set lands in is
	// itself called `transitions` in the wake-up envelope, and
	// `transitions.transitions` reads like a mistake. Never omitted:
	// an explicit empty list is "we classified and nothing changed",
	// and a missing key is "we do not know", which are the two answers
	// a monitor most needs to keep apart.
	Transitions []Transition `json:"records"`

	// Scanned is the summary's `scanned=` — the subjects considered,
	// which is almost always much larger than the transitions. It is
	// the difference between "quiet" and "not looking": a cycle with
	// zero transitions and 400 scanned is healthy, and a cycle with
	// zero of both is a monitor that has stopped monitoring.
	Scanned int `json:"scanned"`
}

// Empty reports whether nothing changed. Note that this is not the same
// question as "did the cycle work" — see Scanned.
func (s Set) Empty() bool { return len(s.Transitions) == 0 }

// Classes tallies the set by class as `<class>=<n>`, sorted by class so
// the rendering is stable between cycles.
//
// Built from the classes actually present rather than from a list mast
// keeps: a tally that enumerated a vocabulary would silently drop the
// first record of a class lookout added after this was written, which
// is precisely the failure this package is arranged to make impossible.
func (s Set) Classes() []string {
	counts := make(map[string]int, len(s.Transitions))
	for _, t := range s.Transitions {
		counts[t.Class]++
	}
	out := make([]string, 0, len(counts))
	for class := range counts {
		out = append(out, class+"="+strconv.Itoa(counts[class]))
	}
	sort.Strings(out)
	return out
}

// ParseResult reads a transition set out of a tool result as mast's
// direct-run seam returns it.
//
// The `output` key is ADK's, not the producer's: its MCP tool adapter
// files a server's text content under that name (and a server's
// structured content under the same one). A tool that answers with
// structured content instead of the line contract is an error rather
// than something to coerce — the contract is a stream of records with a
// terminator, and a JSON blob shaped some other way is a different tool
// than the one the bundle meant to name.
func ParseResult(result map[string]any) (Set, error) {
	raw, ok := result["output"]
	if !ok {
		keys := make([]string, 0, len(result))
		for k := range result {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return Set{}, fmt.Errorf("tool result carries no \"output\" key (got: %s); the transition source must answer with the one-record-per-line contract", strings.Join(keys, ", "))
	}
	text, ok := raw.(string)
	if !ok {
		return Set{}, fmt.Errorf("tool result \"output\" is %T, not the text of a record stream; a tool that answers with structured content cannot be a transition source", raw)
	}
	return Parse(text)
}

// Parse reads a record stream into a Set. See the package doc for the
// contract; the errors below are all "the bytes are not a whole
// answer", never "the answer is wrong".
func Parse(output string) (Set, error) {
	lines := make([]string, 0, 8)
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return Set{}, fmt.Errorf("empty result: a scanned-and-healthy run still ends with a summary line, so nothing at all means the read was truncated")
	}

	last := lines[len(lines)-1]
	summary, err := parseRecord(last)
	if err != nil {
		return Set{}, fmt.Errorf("summary line: %w", err)
	}
	scanned, sok := summary["scanned"]
	emitted, eok := summary["findings"]
	if !sok || !eok {
		return Set{}, fmt.Errorf("result does not end with a summary line (`scanned=<n> findings=<n>`); it ends with %q, so what arrived is a prefix of an answer and reading it as \"nothing changed\" would be a guess", elide(last))
	}
	scannedN, err := strconv.Atoi(strings.TrimSpace(scanned))
	if err != nil {
		return Set{}, fmt.Errorf("summary line: scanned=%q is not a number", scanned)
	}
	emittedN, err := strconv.Atoi(strings.TrimSpace(emitted))
	if err != nil {
		return Set{}, fmt.Errorf("summary line: findings=%q is not a number", emitted)
	}

	records := lines[:len(lines)-1]
	if len(records) != emittedN {
		return Set{}, fmt.Errorf("summary says findings=%d but %d record line(s) arrived; the count comes from the producer's own writer, so a disagreement means the stream was truncated", emittedN, len(records))
	}

	set := Set{Scanned: scannedN, Transitions: make([]Transition, 0, len(records))}
	for i, line := range records {
		fields, err := parseRecord(line)
		if err != nil {
			return Set{}, fmt.Errorf("record %d: %w", i+1, err)
		}
		class := strings.TrimSpace(fields["transition"])
		if class == "" {
			return Set{}, fmt.Errorf("record %d carries no transition= class: %q; the transition source is the tool that classifies, and a record it did not classify is not one mast may classify for it", i+1, elide(line))
		}
		subject := strings.TrimSpace(fields["subject_key"])
		if subject == "" {
			return Set{}, fmt.Errorf("record %d is classified %q but carries no subject_key=; nothing downstream can ack, suppress or de-duplicate a subject it cannot name", i+1, class)
		}
		delete(fields, "transition")
		delete(fields, "subject_key")
		t := Transition{SubjectKey: subject, Class: class}
		if len(fields) > 0 {
			t.Fields = fields
		}
		set.Transitions = append(set.Transitions, t)
	}
	return set, nil
}

// parseRecord reads one line as either a flat JSON object or a logfmt
// record, chosen by its first non-space byte.
func parseRecord(line string) (map[string]string, error) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "{") {
		return parseJSONRecord(trimmed)
	}
	return parseLogfmt(trimmed)
}

func parseJSONRecord(line string) (map[string]string, error) {
	var raw map[string]any
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("not a JSON object: %v", err)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch val := v.(type) {
		case string:
			out[k] = val
		case nil:
			out[k] = ""
		default:
			// json.Number, bool, and anything nested. Rendered rather
			// than rejected: `scanned` is a JSON number in the JSON
			// encoding and a string in the logfmt one, and the whole
			// point of detecting per line is that the two encodings
			// carry the same record.
			b, err := json.Marshal(val)
			if err != nil {
				return nil, fmt.Errorf("field %q: %v", k, err)
			}
			out[k] = string(b)
		}
	}
	return out, nil
}

// parseLogfmt reads `k=v k="quoted v"` pairs. Values are quoted by the
// producer exactly when they contain a space, an `=`, a quote or a
// control character, using Go quoting — so unquoting is strconv's job
// and a value that fails to unquote is a malformed line rather than
// something to salvage.
func parseLogfmt(line string) (map[string]string, error) {
	out := map[string]string{}
	for i := 0; i < len(line); {
		if line[i] == ' ' {
			i++
			continue
		}
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			return nil, fmt.Errorf("not a logfmt record: %q has a bare token where a key=value was expected", elide(line[i:]))
		}
		key := line[i : i+eq]
		if key == "" {
			return nil, fmt.Errorf("not a logfmt record: %q has an empty key", elide(line))
		}
		i += eq + 1
		if i < len(line) && line[i] == '"' {
			end := closingQuote(line[i:])
			if end < 0 {
				return nil, fmt.Errorf("key %q: unterminated quoted value", key)
			}
			val, err := strconv.Unquote(line[i : i+end+1])
			if err != nil {
				return nil, fmt.Errorf("key %q: %v", key, err)
			}
			out[key] = val
			i += end + 1
			continue
		}
		sp := strings.IndexByte(line[i:], ' ')
		if sp < 0 {
			out[key] = line[i:]
			break
		}
		out[key] = line[i : i+sp]
		i += sp
	}
	return out, nil
}

// closingQuote returns the index of the quote that closes the one at
// s[0], skipping escaped quotes. -1 if there is none.
func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// elide keeps an error message readable when the offending line is a
// full finding — and keeps a cluster's data out of a log line that a
// truncated read would otherwise dump whole.
func elide(s string) string {
	const max = 120
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
