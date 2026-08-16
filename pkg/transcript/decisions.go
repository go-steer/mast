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

// The harvest half of the decision feedback loop (v0.4 W8): the write
// gate records one approval.Decision per adjudication on the session's
// event log, and this file reads them back out and writes them
// somewhere an evaluation harness can load.
//
// The export is deliberately its own artifact. It does NOT append to
// testdata/evals/scenarios/langchain-sre.jsonl: that corpus is a ported,
// fixed 31-row dataset whose row count is pinned by
// TestLoadDataset_PortedCorpus, and whose contract is "the trajectories
// upstream's harness scores, unchanged". Mixing a fleet's own
// adjudications into it would break that test, and — worse — would make
// a comparison against upstream a comparison against a corpus mast had
// been quietly editing. Two artifacts, two contracts.

package transcript

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/internal/version"
	"github.com/go-steer/mast/pkg/approval"
)

// RedactionApproverDigest is the default export mode: approver
// identities are replaced by a stable digest (approval.RedactApprover).
const RedactionApproverDigest = "approver_digest"

// RedactionNone is the opt-in mode: raw approver identities.
const RedactionNone = "none"

// argumentsWarning travels inside every export's provenance header.
//
// Argument values are exported verbatim and that is not an oversight:
// the proposed→executed pair is the entire signal the workstream exists
// to capture, and an export with the arguments stripped would record
// only that somebody edited something. The consequence is that an export
// is exactly as sensitive as the arguments the fleet's tools take, and
// the operator running the command is the only one who knows whether
// those carry namespaces, hostnames or credentials. Saying so in the
// file means a consumer who received it second-hand is told too.
const argumentsWarning = "Tool arguments are exported verbatim, including any secrets or cluster topology they carry. Treat this file with the same care as the cluster it describes."

// ExportOptions scopes and shapes a decision export.
type ExportOptions struct {
	// UserID narrows to one user; empty auto-discovers per session.
	UserID string

	// SessionID exports one session. Empty exports every session in the
	// store under the store's app name.
	SessionID string

	// Workload keeps only decisions stamped with this workload name.
	// Empty keeps all of them.
	Workload string

	// Since and Until bound DecidedAt (inclusive lower, exclusive
	// upper). Zero means unbounded.
	Since, Until time.Time

	// IncludeApprover exports raw approver identities instead of
	// digests. Off by default: the whole point of the digest is that a
	// file which leaves the operator's machine should still be able to
	// answer "same approver?" without naming anyone, and a default that
	// has to be remembered is not a default (v0.4 W8).
	IncludeApprover bool

	// Source is recorded in the provenance header as where the rows came
	// from — the session DB path, ordinarily.
	Source string

	// Now supplies the export timestamp. Nil means time.Now.
	Now func() time.Time
}

func (o ExportOptions) redaction() string {
	if o.IncludeApprover {
		return RedactionNone
	}
	return RedactionApproverDigest
}

// ExportMeta is the provenance object on an export's first line.
//
// It exists so a consumer can never mistake a redacted file for a raw
// one, or a partial export for a whole fleet. Every field answers a
// question that is unanswerable from the rows themselves: which mast
// wrote this, when, from where, under which redaction mode, how many
// rows to expect, and what the rows are.
type ExportMeta struct {
	Tool       string    `json:"tool"`
	Version    string    `json:"version"`
	Schema     string    `json:"schema"`
	ExportedAt time.Time `json:"exported_at"`
	Redaction  string    `json:"redaction"`
	Source     string    `json:"source,omitempty"`
	Session    string    `json:"session,omitempty"`
	Workload   string    `json:"workload,omitempty"`

	// Pointers because `omitempty` does nothing to a time.Time — a zero
	// one marshals as "0001-01-01T00:00:00Z", and a header claiming an
	// unbounded export ran since the year 1 is a bound a consumer would
	// have to know to disbelieve. Absent means unbounded.
	Since *time.Time `json:"since,omitempty"`
	Until *time.Time `json:"until,omitempty"`

	Records int    `json:"records"`
	Warning string `json:"warning"`
}

// bound renders one end of the export's time window for the header, or
// nil when that end was never set.
func bound(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// metaLine is the envelope of the header line. The `_meta` key is what
// makes the header self-identifying: a reader that streams the file line
// by line can skip it without knowing the schema, and a reader that
// wants provenance does not have to be told which line has it.
type metaLine struct {
	Meta ExportMeta `json:"_meta"`
}

// Decisions returns the write gate's adjudication records for one
// session, oldest first.
//
// Read through the event log rather than the collapsed session state,
// exactly as AppliedEdits are, so the order is the order the calls were
// decided in — a decision dataset in hash-map order is a decision
// dataset with the sequence thrown away.
func (s *Store) Decisions(ctx context.Context, userID, sessionID string) ([]approval.Decision, error) {
	if IsReservedSessionID(sessionID) {
		return nil, errReserved(sessionID)
	}
	if userID == "" {
		var err error
		userID, err = s.findUserID(ctx, sessionID)
		if err != nil {
			return nil, err
		}
	}
	resp, err := s.svc.Get(ctx, &adksession.GetRequest{
		AppName:   s.appName,
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("get session %q: %w", sessionID, err)
	}
	return scanDecisions(resp.Session.Events()), nil
}

// scanDecisions walks the event log in order and returns the decision
// records, oldest first. A record that cannot be decoded is skipped
// rather than failing the export: one malformed row should cost the
// operator that row, not the harvest.
func scanDecisions(events adksession.Events) []approval.Decision {
	var out []approval.Decision
	for ev := range events.All() {
		delta := ev.Actions.StateDelta
		if len(delta) == 0 {
			continue
		}
		keys := make([]string, 0, len(delta))
		for k := range delta {
			if strings.HasPrefix(k, approval.DecisionStateKeyPrefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			d, err := approval.DecodeDecision(delta[k])
			if err != nil {
				continue
			}
			out = append(out, d)
		}
	}
	return out
}

// ExportDecisions writes the store's adjudication records to w as JSON
// Lines: one provenance object, then one object per decision.
//
// JSONL rather than a JSON array because the consumer is an evaluation
// harness reading rows, and because an export that is appended to over
// time should not require rewriting a closing bracket. It returns the
// number of decision rows written, which excludes the header.
//
// Rows are collected before anything is written, so that the header can
// carry an accurate count — a consumer that finds fewer rows than the
// header promises knows the file was truncated, which is worth more than
// streaming an unbounded export the CLI has no way to produce anyway.
func (s *Store) ExportDecisions(ctx context.Context, w io.Writer, opts ExportOptions) (int, error) {
	rows, err := s.collectDecisions(ctx, opts)
	if err != nil {
		return 0, err
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	enc := json.NewEncoder(w)
	header := metaLine{Meta: ExportMeta{
		Tool:       "mast sessions export-decisions",
		Version:    version.Version,
		Schema:     approval.DecisionSchema,
		ExportedAt: now().UTC(),
		Redaction:  opts.redaction(),
		Source:     opts.Source,
		Session:    opts.SessionID,
		Workload:   opts.Workload,
		Since:      bound(opts.Since),
		Until:      bound(opts.Until),
		Records:    len(rows),
		Warning:    argumentsWarning,
	}}
	if err := enc.Encode(header); err != nil {
		return 0, fmt.Errorf("write export header: %w", err)
	}
	for _, d := range rows {
		if !opts.IncludeApprover {
			d = d.Redacted()
		}
		if err := enc.Encode(d); err != nil {
			return 0, fmt.Errorf("write decision for session %q: %w", d.Session, err)
		}
	}
	return len(rows), nil
}

// collectDecisions gathers the rows an export covers, in session order
// and, within a session, in decision order.
func (s *Store) collectDecisions(ctx context.Context, opts ExportOptions) ([]approval.Decision, error) {
	sessions := []string{opts.SessionID}
	if opts.SessionID == "" {
		summaries, err := s.List(ctx, opts.UserID)
		if err != nil {
			return nil, err
		}
		// List sorts most-recent-first; a dataset reads better oldest
		// first, which is also the order the decisions happened in.
		sessions = sessions[:0]
		for i := len(summaries) - 1; i >= 0; i-- {
			sessions = append(sessions, summaries[i].ID)
		}
	}
	var rows []approval.Decision
	for _, sid := range sessions {
		found, err := s.Decisions(ctx, opts.UserID, sid)
		if err != nil {
			return nil, err
		}
		for _, d := range found {
			if opts.keeps(d) {
				rows = append(rows, d)
			}
		}
	}
	return rows, nil
}

// keeps applies the export's filters to one record.
func (o ExportOptions) keeps(d approval.Decision) bool {
	if o.Workload != "" && d.Workload != o.Workload {
		return false
	}
	if !o.Since.IsZero() && d.DecidedAt.Before(o.Since) {
		return false
	}
	if !o.Until.IsZero() && !d.DecidedAt.Before(o.Until) {
		return false
	}
	return true
}
