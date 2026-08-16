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

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/approval"
)

// seedDecisionDB writes a session DB holding one edited call: the
// applied-edit record `mast sessions show` has printed since W2.5, and
// the decision record W8 added beside it.
func seedDecisionDB(t *testing.T) (dbPath, sessionID string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "sessions.db")
	svc, err := database.NewSessionService(sqlite.Open(dbPath))
	if err != nil {
		t.Fatalf("open session service: %v", err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		t.Fatalf("migrate session service: %v", err)
	}
	ctx := context.Background()
	sessionID = "incident-123"
	resp, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: appName, UserID: "op", SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	edit := approval.AppliedEdit{
		Tool:         "scale_deployment",
		Approver:     "user:sre-oncall",
		ProposedKey:  "scale_deployment(deployment=api, replicas=10)",
		ExecutedKey:  "scale_deployment(deployment=api, replicas=2)",
		ProposedArgs: map[string]any{"deployment": "api", "replicas": float64(10)},
		ExecutedArgs: map[string]any{"deployment": "api", "replicas": float64(2)},
		Note:         "10 would exhaust the node pool",
	}
	rawEdit, err := json.Marshal(edit)
	if err != nil {
		t.Fatalf("marshal applied edit: %v", err)
	}
	decision := approval.Decision{
		DecidedAt:      time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC),
		Session:        sessionID,
		Workload:       "gke-triage",
		Specialist:     "remediator",
		FunctionCallID: "fc-1",
		Tool:           "scale_deployment",
		Outcome:        approval.OutcomeEdit,
		Scope:          approval.ScopeOnce,
		Authority:      approval.AuthorityVerdict,
		Disposition:    approval.DispositionAuthorized,
		ProposedKey:    edit.ProposedKey,
		ProposedArgs:   edit.ProposedArgs,
		ExecutedKey:    edit.ExecutedKey,
		ExecutedArgs:   edit.ExecutedArgs,
		Approver:       edit.Approver,
		Note:           edit.Note,
	}
	rawDecision, err := approval.EncodeDecision(decision)
	if err != nil {
		t.Fatalf("encode decision: %v", err)
	}

	ev := adksession.NewEvent(ctx, "inv-1")
	ev.Author = "remediator"
	ev.Content = genai.NewContentFromText("scaled", genai.RoleModel)
	ev.Timestamp = time.Now()
	ev.Actions.StateDelta = map[string]any{
		approval.EditStateKey("fc-1"):     string(rawEdit),
		approval.DecisionStateKey("fc-1"): rawDecision,
	}
	if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatalf("append event: %v", err)
	}
	return dbPath, sessionID
}

func runSessionsCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd, err := parseSessionsArgs(args)
	if err != nil {
		t.Fatalf("parseSessionsArgs(%v): %v", args, err)
	}
	var out bytes.Buffer
	if err := cmd.run(context.Background(), &out); err != nil {
		t.Fatalf("mast sessions %v: %v", args, err)
	}
	return out.String()
}

// decodeJSONL splits an export into its provenance header and its rows,
// failing on anything that is not one JSON object per line.
func decodeJSONL(t *testing.T, s string) (map[string]any, []approval.Decision) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(s))
	if !sc.Scan() {
		t.Fatal("empty export: even a store with no decisions must emit a provenance header")
	}
	var first map[string]any
	if err := json.Unmarshal(sc.Bytes(), &first); err != nil {
		t.Fatalf("header line is not JSON: %v (%s)", err, sc.Text())
	}
	meta, ok := first["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("first line carries no _meta object: %s", sc.Text())
	}
	var rows []approval.Decision
	for sc.Scan() {
		var d approval.Decision
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			t.Fatalf("row is not a decision: %v (%s)", err, sc.Text())
		}
		rows = append(rows, d)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan export: %v", err)
	}
	return meta, rows
}

// TestSessionsExportDecisions_Stdout: the operator-facing default. The
// rows go to stdout as clean JSONL — nothing else may be printed there,
// because the ordinary use is a pipe.
func TestSessionsExportDecisions_Stdout(t *testing.T) {
	db, sid := seedDecisionDB(t)

	got := runSessionsCmd(t, "export-decisions", sid, "--session-db="+db)
	meta, rows := decodeJSONL(t, got)

	if meta["redaction"] != "approver_digest" {
		t.Errorf("_meta.redaction = %v, want approver_digest — the default must be the safe one", meta["redaction"])
	}
	if meta["source"] != db {
		t.Errorf("_meta.source = %v, want the session DB the rows came from", meta["source"])
	}
	if meta["schema"] != approval.DecisionSchema {
		t.Errorf("_meta.schema = %v, want %q", meta["schema"], approval.DecisionSchema)
	}
	if meta["records"] != float64(len(rows)) {
		t.Errorf("_meta.records = %v but the file holds %d rows", meta["records"], len(rows))
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want the one adjudication", rows)
	}
	d := rows[0]
	if d.Approver != approval.RedactApprover("user:sre-oncall") {
		t.Errorf("approver = %q, want the digest", d.Approver)
	}
	if strings.Contains(got, "sre-oncall") {
		t.Errorf("the default export named the operator:\n%s", got)
	}
	if !d.Edited() || d.ExecutedArgs["replicas"] != float64(2) {
		t.Errorf("row = %+v, want the operator's correction — the proposed→executed diff is the signal", d)
	}
	if d.ProposedArgs["replicas"] != float64(10) {
		t.Errorf("ProposedArgs = %v, want the model's proposal preserved", d.ProposedArgs)
	}
}

// TestSessionsExportDecisions_IncludeApprover: the opt-in names names,
// and says in the file that it did.
func TestSessionsExportDecisions_IncludeApprover(t *testing.T) {
	db, sid := seedDecisionDB(t)

	got := runSessionsCmd(t, "export-decisions", sid, "--session-db="+db, "--include-approver")
	meta, rows := decodeJSONL(t, got)
	if meta["redaction"] != "none" {
		t.Errorf("_meta.redaction = %v, want none", meta["redaction"])
	}
	if len(rows) != 1 || rows[0].Approver != "user:sre-oncall" {
		t.Fatalf("rows = %+v, want the raw identity", rows)
	}
}

// TestSessionsExportDecisions_OutFile: with --out the JSONL goes to the
// file and the operator gets told, on stdout, which redaction mode they
// just wrote to disk.
func TestSessionsExportDecisions_OutFile(t *testing.T) {
	db, sid := seedDecisionDB(t)
	path := filepath.Join(t.TempDir(), "decisions.jsonl")

	got := runSessionsCmd(t, "export-decisions", sid, "--session-db="+db, "--out="+path, "--include-approver")
	if !strings.Contains(got, "exported 1 decision(s)") {
		t.Errorf("stdout = %q, want the count", got)
	}
	if !strings.Contains(got, "names the people") {
		t.Errorf("stdout = %q, want a warning that the file names approvers", got)
	}

	raw, err := os.ReadFile(path) // #nosec G304 -- test-owned temp path
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if _, rows := decodeJSONL(t, string(raw)); len(rows) != 1 {
		t.Fatalf("file holds %d rows, want 1", len(rows))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat export: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("export mode = %o, want 0600: the rows carry tool arguments verbatim and, here, approver names", perm)
	}
}

// TestSessionsShowIsUnchangedByDecisionRecords is the regression guard
// on a shipped surface. The decision record rides the same session as
// the applied-edit record; `mast sessions show` must print exactly what
// it printed before W8, because the uat legs and every operator's eye
// are trained on that output.
func TestSessionsShowIsUnchangedByDecisionRecords(t *testing.T) {
	db, sid := seedDecisionDB(t)

	got := runSessionsCmd(t, "show", sid, "--session-db="+db)
	for _, want := range []string{
		"Operator edit applied:",
		"  Proposed: scale_deployment(deployment=api, replicas=10)",
		"  Executed: scale_deployment(deployment=api, replicas=2)",
		"  Approver: user:sre-oncall",
		"  Note:     10 would exhaust the node pool",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("`sessions show` no longer prints %q:\n%s", want, got)
		}
	}
	// Nothing new. A decision block here would be a second rendering of
	// the same adjudication, and would break the uat's field assertions.
	for _, unwanted := range []string{"Decision:", "Disposition:", "Authority:", "mast_decision_"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("`sessions show` grew a %q line; the decision record is an export surface, not a show surface:\n%s", unwanted, got)
		}
	}
}
