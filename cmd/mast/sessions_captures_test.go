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
	"context"
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

// The operator-facing end of #296: what `mast sessions show` says about a
// change that has already happened.

// seedCaptureDB writes a session holding the prior-state records the
// write gate takes, in the order the calls ran.
func seedCaptureDB(t *testing.T, records ...approval.CaptureRecord) (dbPath, sessionID string) {
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
	sessionID = "incident-296"
	resp, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: appName, UserID: "op", SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	for i, r := range records {
		raw, err := approval.EncodeCapture(r)
		if err != nil {
			t.Fatalf("encode capture: %v", err)
		}
		ev := adksession.NewEvent(ctx, "inv-1")
		ev.Author = "remediator"
		ev.Content = genai.NewContentFromText("acting", genai.RoleModel)
		ev.Timestamp = time.Now()
		ev.Actions.StateDelta = map[string]any{
			approval.CaptureStateKey(r.FunctionCallID): raw,
		}
		if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}
	return dbPath, sessionID
}

func scaleCaptureRecord() approval.CaptureRecord {
	return approval.CaptureRecord{
		Tool:           "scale_deployment",
		Key:            "scale_deployment(deployment=api, replicas=10)",
		Arguments:      map[string]any{"deployment": "api", "replicas": float64(10)},
		CapturedAt:     time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC),
		Read:           "get_deployment",
		ReadArgs:       map[string]any{"name": "api"},
		Prior:          map[string]any{"spec.replicas": float64(3)},
		PriorFields:    []string{"spec.replicas"},
		Digest:         "d8df381d496ca5ee",
		FunctionCallID: "fc-1",
		Revert: &approval.ProposedChange{
			Tool:      "scale_deployment",
			Arguments: map[string]any{"deployment": "api", "replicas": float64(3)},
		},
	}
}

func TestSessionsShowRendersTheUndo(t *testing.T) {
	db, sid := seedCaptureDB(t, scaleCaptureRecord())
	got := runSessionsCmd(t, "show", sid, "--session-db="+db)

	for _, want := range []string{
		"Prior state captured:",
		"Changed by: scale_deployment(deployment=api, replicas=10)",
		"2026-09-05T09:00:00Z by get_deployment(name=api)",
		"spec.replicas = 3",
		"Undo:       scale_deployment(deployment=api, replicas=3)",
		// The arguments verbatim, because an operator restoring a
		// deployment at 3am should not have to retype a value they can
		// only see rendered.
		`scale_deployment {"deployment":"api","replicas":3}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("`sessions show` does not print %q:\n%s", want, got)
		}
	}
	// The single most expensive thing this view could get wrong is
	// implying mast will roll the change back by itself. It will not: the
	// undo is a proposal that goes back through the write gate.
	if !strings.Contains(got, "proposal, not a button") {
		t.Errorf("the view does not say the undo is a proposal, so an operator could read it as something mast already did:\n%s", got)
	}
	if strings.Contains(got, "replicas=10)\n              (proposal") {
		t.Errorf("the undo re-applies the change instead of reversing it:\n%s", got)
	}
}

// TestSessionsShowSaysWhenThereIsNoUndo: a record with no revert is the
// common case for a workload that has adopted capture but cannot name
// the inverse call. Printing the prior value and going quiet about the
// undo reads as "an undo exists somewhere"; the view has to say the
// declaration is missing, and which tool is missing it.
func TestSessionsShowSaysWhenThereIsNoUndo(t *testing.T) {
	r := scaleCaptureRecord()
	r.Revert = nil
	db, sid := seedCaptureDB(t, r)
	got := runSessionsCmd(t, "show", sid, "--session-db="+db)

	if !strings.Contains(got, "spec.replicas = 3") {
		t.Errorf("the prior value is missing, which is the half of the record that does exist:\n%s", got)
	}
	if !strings.Contains(got, "none — this workload declares no call that puts scale_deployment back") {
		t.Errorf("the view does not say the undo is undeclared, or does not name the tool:\n%s", got)
	}
}

// TestSessionsShowRendersAnUnnarrowedCapture: with no fields declared the
// record holds the whole read, which can be a manifest. The view prints
// the digest rather than the body — the body is in the JSON projection —
// but it must still say a record was taken.
func TestSessionsShowRendersAnUnnarrowedCapture(t *testing.T) {
	r := scaleCaptureRecord()
	r.PriorFields = nil
	r.Prior = map[string]any{"spec": map[string]any{"replicas": float64(3)}}
	db, sid := seedCaptureDB(t, r)
	got := runSessionsCmd(t, "show", sid, "--session-db="+db)

	if !strings.Contains(got, "(whole result of the read, digest d8df381d496ca5ee)") {
		t.Errorf("an un-narrowed capture renders as nothing at all:\n%s", got)
	}
}

// TestSessionsShowElidesAHugePriorValue: the elision exists so a captured
// manifest cannot push the transcript off the screen. It has to elide the
// value and keep the line.
func TestSessionsShowElidesAHugePriorValue(t *testing.T) {
	r := scaleCaptureRecord()
	r.PriorFields = []string{"spec.template"}
	r.Prior = map[string]any{"spec.template": strings.Repeat("y", 4000)}
	r.Revert = nil
	db, sid := seedCaptureDB(t, r)
	got := runSessionsCmd(t, "show", sid, "--session-db="+db)

	if !strings.Contains(got, "spec.template = ") || !strings.Contains(got, "…") {
		t.Errorf("a large captured value did not render elided:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 400 {
			t.Fatalf("a captured value took the view over at %d characters", len(line))
		}
	}
}

// TestSessionsShowIsSilentWithoutCaptures: a session from a workload that
// declares no capture must look exactly as it did before #296.
func TestSessionsShowIsSilentWithoutCaptures(t *testing.T) {
	db, sid := seedDecisionDB(t)
	got := runSessionsCmd(t, "show", sid, "--session-db="+db)
	for _, unwanted := range []string{"Prior state captured:", "Undo:", "mast_effect_capture_"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("`sessions show` printed %q for a session with no captures:\n%s", unwanted, got)
		}
	}
}
