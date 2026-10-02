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

package transcript

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/approval"
)

var decidedAt = time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)

// decisionEvent is the shape the write gate produces: a record on the
// state delta of the event ADK appends for the adjudicated call.
func decisionEvent(callID string, d approval.Decision) *adksession.Event {
	raw, err := approval.EncodeDecision(d)
	if err != nil {
		panic(err)
	}
	ev := adksession.NewEvent(context.Background(), "inv-1")
	ev.Author = "sre"
	ev.Content = genai.NewContentFromText("decided", genai.RoleModel)
	ev.Actions.StateDelta = map[string]any{approval.DecisionStateKey(callID): raw}
	return ev
}

func approvedDecision(session, tool, approver string, at time.Time) approval.Decision {
	return approval.Decision{
		DecidedAt:    at,
		Session:      session,
		Workload:     "gke-triage",
		Specialist:   "remediator",
		Tool:         tool,
		Outcome:      approval.OutcomeApprove,
		Scope:        approval.ScopeOnce,
		Authority:    approval.AuthorityVerdict,
		Disposition:  approval.DispositionAuthorized,
		ProposedKey:  tool + "(deployment=api)",
		ProposedArgs: map[string]any{"deployment": "api"},
		Approver:     approver,
	}
}

// exported reads an export back: the header, then the rows.
func exported(t *testing.T, buf *bytes.Buffer) (ExportMeta, []approval.Decision) {
	t.Helper()
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	if !sc.Scan() {
		t.Fatal("the export is empty; even a fleet that decided nothing must produce a provenance header")
	}
	var header metaLine
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		t.Fatalf("first line is not a provenance object: %v (%s)", err, sc.Text())
	}
	if header.Meta.Schema == "" {
		t.Errorf("first line decoded but carries no _meta: %s", sc.Text())
	}
	var rows []approval.Decision
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			t.Error("the export contains a blank line; JSONL readers treat that as a parse error")
			continue
		}
		// Every row must be a standalone JSON object — the whole point
		// of the format.
		var probe map[string]any
		if err := json.Unmarshal(line, &probe); err != nil {
			t.Fatalf("row is not JSON: %v (%s)", err, sc.Text())
		}
		if _, isHeader := probe["_meta"]; isHeader {
			t.Errorf("a second provenance object appeared mid-file: %s", sc.Text())
		}
		var d approval.Decision
		if err := json.Unmarshal(line, &d); err != nil {
			t.Fatalf("row is not a decision: %v (%s)", err, sc.Text())
		}
		rows = append(rows, d)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan export: %v", err)
	}
	return header.Meta, rows
}

func exportAll(t *testing.T, store *Store, opts ExportOptions) (ExportMeta, []approval.Decision) {
	t.Helper()
	var buf bytes.Buffer
	n, err := store.ExportDecisions(context.Background(), &buf, opts)
	if err != nil {
		t.Fatalf("ExportDecisions: %v", err)
	}
	meta, rows := exported(t, &buf)
	if n != len(rows) {
		t.Errorf("ExportDecisions returned %d but wrote %d rows", n, len(rows))
	}
	if meta.Records != len(rows) {
		t.Errorf("_meta.records = %d but the file holds %d rows; a consumer could not tell a truncated file from a whole one", meta.Records, len(rows))
	}
	return meta, rows
}

// TestDecisionsProjectedInOrder: the export is read off the event log,
// not the collapsed state, so the rows come back in the order the calls
// were decided in — and one unreadable record costs that row, not the
// harvest.
func TestDecisionsProjectedInOrder(t *testing.T) {
	first := approvedDecision("s-decided", "scale_deployment", "user:sre-oncall", decidedAt)
	second := approvedDecision("s-decided", "rollout_restart", "user:sre-oncall", decidedAt.Add(time.Minute))
	second.Outcome, second.Disposition = approval.OutcomeReject, approval.DispositionRefusedByOperator
	second.Refusal = "denied_by_operator"

	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			seed(t, svc, "op", "s-decided",
				textEvent("agent", "triaging"),
				decisionEvent("fc-1", first),
				func() *adksession.Event {
					ev := textEvent("sre", "noise")
					ev.Actions.StateDelta = map[string]any{
						approval.DecisionStateKey("fc-bad"): "{not json",
						"mast_unrelated_marker":             "ignored",
					}
					return ev
				}(),
				decisionEvent("fc-2", second))

			store := NewStore(svc, testApp)
			got, err := store.Decisions(context.Background(), "", "s-decided")
			if err != nil {
				t.Fatalf("Decisions: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("Decisions = %+v, want the 2 decodable records", got)
			}
			if got[0].Tool != "scale_deployment" || got[1].Tool != "rollout_restart" {
				t.Errorf("Decisions = %q, %q — records must come back in the order they were decided", got[0].Tool, got[1].Tool)
			}
			if got[1].Outcome != approval.OutcomeReject {
				t.Errorf("second Outcome = %q, want the rejection", got[1].Outcome)
			}
		})
	}
}

// TestExportRedactsApproverByDefault is the privacy default. An export
// leaves the operator's machine; a login name in it does not need to.
func TestExportRedactsApproverByDefault(t *testing.T) {
	svc := adksession.InMemoryService()
	seed(t, svc, "op", "s-1",
		decisionEvent("fc-1", approvedDecision("s-1", "scale_deployment", "user:sre-oncall", decidedAt)),
		// A machine identity: naming a mechanism, not a person.
		decisionEvent("fc-2", approvedDecision("s-1", "rollout_restart", "mast:internal", decidedAt.Add(time.Minute))))
	store := NewStore(svc, testApp)

	meta, rows := exportAll(t, store, ExportOptions{Source: "/tmp/sessions.db"})
	if meta.Redaction != RedactionApproverDigest {
		t.Errorf("_meta.redaction = %q, want %q", meta.Redaction, RedactionApproverDigest)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	if got := rows[0].Approver; got != approval.RedactApprover("user:sre-oncall") {
		t.Errorf("approver = %q, want the digest", got)
	}
	if strings.Contains(rows[0].Approver, "sre-oncall") {
		t.Errorf("the default export named the operator: %q", rows[0].Approver)
	}
	if got := rows[1].Approver; got != "mast:internal" {
		t.Errorf("machine approver = %q, want mast:internal in the clear — a dataset that cannot tell a human approval from the scheduler's is not about human judgement", got)
	}
	// Argument values are preserved on purpose; the proposed→executed
	// diff is the signal the workstream exists to capture.
	if rows[0].ProposedArgs["deployment"] != "api" {
		t.Errorf("ProposedArgs = %v, want the arguments preserved", rows[0].ProposedArgs)
	}
	if meta.Warning == "" || !strings.Contains(meta.Warning, "verbatim") {
		t.Errorf("_meta.warning = %q, want the arguments-are-verbatim warning to travel with the file", meta.Warning)
	}
	if meta.Source != "/tmp/sessions.db" || meta.Tool == "" || meta.Schema != approval.DecisionSchema || meta.ExportedAt.IsZero() {
		t.Errorf("_meta = %+v, want full provenance", meta)
	}
}

// TestExportIncludeApprover: the opt-in, and the header that stops a raw
// file being mistaken for a redacted one.
func TestExportIncludeApprover(t *testing.T) {
	svc := adksession.InMemoryService()
	seed(t, svc, "op", "s-1",
		decisionEvent("fc-1", approvedDecision("s-1", "scale_deployment", "user:sre-oncall", decidedAt)))
	store := NewStore(svc, testApp)

	meta, rows := exportAll(t, store, ExportOptions{IncludeApprover: true})
	if meta.Redaction != RedactionNone {
		t.Errorf("_meta.redaction = %q, want %q", meta.Redaction, RedactionNone)
	}
	if len(rows) != 1 || rows[0].Approver != "user:sre-oncall" {
		t.Fatalf("rows = %+v, want the raw identity", rows)
	}
}

// TestExportFilters: only what the CLI can actually pass, so every flag
// this package offers is one a test drives.
func TestExportFilters(t *testing.T) {
	svc := adksession.InMemoryService()
	other := approvedDecision("s-1", "rollout_restart", "user:sre-oncall", decidedAt.Add(2*time.Hour))
	other.Workload = "other-fleet"
	seed(t, svc, "op", "s-1",
		decisionEvent("fc-1", approvedDecision("s-1", "scale_deployment", "user:sre-oncall", decidedAt)),
		decisionEvent("fc-2", other))
	seed(t, svc, "op", "s-2",
		decisionEvent("fc-3", approvedDecision("s-2", "scale_deployment", "user:other", decidedAt.Add(time.Hour))))
	store := NewStore(svc, testApp)

	t.Run("every session by default", func(t *testing.T) {
		meta, rows := exportAll(t, store, ExportOptions{})
		if len(rows) != 3 {
			t.Fatalf("rows = %d, want all 3 across both sessions", len(rows))
		}
		if meta.Since != nil || meta.Until != nil {
			t.Errorf("_meta since/until = %v/%v, want both absent — an unbounded export must not report a window it never had", meta.Since, meta.Until)
		}
	})
	t.Run("one session", func(t *testing.T) {
		meta, rows := exportAll(t, store, ExportOptions{SessionID: "s-2"})
		if len(rows) != 1 || rows[0].Session != "s-2" {
			t.Fatalf("rows = %+v, want only s-2's", rows)
		}
		if meta.Session != "s-2" {
			t.Errorf("_meta.session = %q, want the scope recorded so a partial export cannot pass for a whole one", meta.Session)
		}
	})
	t.Run("one workload", func(t *testing.T) {
		_, rows := exportAll(t, store, ExportOptions{Workload: "other-fleet"})
		if len(rows) != 1 || rows[0].Workload != "other-fleet" {
			t.Fatalf("rows = %+v, want only the other-fleet decision", rows)
		}
	})
	t.Run("time window", func(t *testing.T) {
		// Inclusive lower, exclusive upper: adjacent windows tile
		// without double-counting a row.
		meta, rows := exportAll(t, store, ExportOptions{
			Since: decidedAt.Add(time.Hour),
			Until: decidedAt.Add(2 * time.Hour),
		})
		if len(rows) != 1 || rows[0].Session != "s-2" {
			t.Fatalf("rows = %+v, want only the decision inside the window", rows)
		}
		if meta.Since == nil || !meta.Since.Equal(decidedAt.Add(time.Hour)) ||
			meta.Until == nil || !meta.Until.Equal(decidedAt.Add(2*time.Hour)) {
			t.Errorf("_meta since/until = %v/%v, want the window recorded so a consumer knows the export is a slice", meta.Since, meta.Until)
		}
	})
	t.Run("no matches still writes a header", func(t *testing.T) {
		meta, rows := exportAll(t, store, ExportOptions{Workload: "nothing-here"})
		if len(rows) != 0 {
			t.Fatalf("rows = %+v, want none", rows)
		}
		if meta.Records != 0 {
			t.Errorf("_meta.records = %d, want 0", meta.Records)
		}
	})
}

// TestExportSkipsOpsRows: the companion `:mast-ops` rows are marker
// storage. Exporting them would either fail or emit phantom sessions.
func TestExportSkipsOpsRows(t *testing.T) {
	svc := adksession.InMemoryService()
	seed(t, svc, "op", "s-1",
		decisionEvent("fc-1", approvedDecision("s-1", "scale_deployment", "user:sre-oncall", decidedAt)))
	store := NewStore(svc, testApp)
	if _, _, err := store.PauseGate(context.Background(), "op", "s-1", PauseSpec{
		Reason: ReasonMaintenanceWindow, Message: "patching",
	}); err != nil {
		t.Fatalf("PauseGate: %v", err)
	}
	_, rows := exportAll(t, store, ExportOptions{})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want the one real decision", rows)
	}
}

// TestExportDoesNotTouchThePortedCorpus is a boundary, not a behaviour:
// the export writes where it is told and nowhere else. The 31-row
// langchain-sre corpus is a different artifact with a different contract
// (TestLoadDataset_PortedCorpus pins its count), and an exporter that
// appended to it would make every upstream comparison a comparison
// against a corpus mast had been editing.
func TestExportWritesOnlyToItsWriter(t *testing.T) {
	svc := adksession.InMemoryService()
	seed(t, svc, "op", "s-1",
		decisionEvent("fc-1", approvedDecision("s-1", "scale_deployment", "user:sre-oncall", decidedAt)))
	var buf bytes.Buffer
	n, err := NewStore(svc, testApp).ExportDecisions(context.Background(), &buf, ExportOptions{})
	if err != nil {
		t.Fatalf("ExportDecisions: %v", err)
	}
	if n != 1 || buf.Len() == 0 {
		t.Fatalf("wrote %d rows into a %d-byte buffer, want 1 row", n, buf.Len())
	}
}
