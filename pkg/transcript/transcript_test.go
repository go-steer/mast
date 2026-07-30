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
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/genai"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
)

const testApp = "mast-test"

// services returns the two Service implementations the Store must
// behave identically over: ADK's in-memory service and the SQLite
// database service (the v0.1 durable store, in a temp dir per house
// rule #5 — t.TempDir() lives under os.TempDir()).
func services(t *testing.T) map[string]adksession.Service {
	t.Helper()
	svc, err := database.NewSessionService(sqlite.Open(filepath.Join(t.TempDir(), "sessions.db")))
	if err != nil {
		t.Fatalf("open sqlite session service: %v", err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		t.Fatalf("migrate sqlite session service: %v", err)
	}
	return map[string]adksession.Service{
		"inmemory": adksession.InMemoryService(),
		"sqlite":   svc,
	}
}

// seed creates a session and appends events with strictly increasing
// timestamps (the database service orders events by timestamp, and its
// stale-session check requires monotonic appends).
func seed(t *testing.T, svc adksession.Service, userID, sessionID string, events ...*adksession.Event) {
	t.Helper()
	ctx := context.Background()
	resp, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName:   testApp,
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("create session %q: %v", sessionID, err)
	}
	base := time.Now()
	for i, ev := range events {
		ev.Timestamp = base.Add(time.Duration(i+1) * time.Second)
		if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatalf("append event %d to %q: %v", i, sessionID, err)
		}
	}
}

func textEvent(author, text string) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-1")
	ev.Author = author
	ev.Content = genai.NewContentFromText(text, genai.RoleModel)
	return ev
}

func interruptEvent(author, interruptID, message string) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-1")
	ev.Author = author
	ev.RequestedInput = &adksession.RequestInput{
		InterruptID: interruptID,
		Message:     message,
		ResponseSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"approved": {Type: "boolean"}},
			Required:   []string{"approved"},
		},
	}
	return ev
}

// resolutionEvent is the resume wire shape verified in spike 2: a user
// turn whose FunctionResponse.ID equals the pending InterruptID.
func resolutionEvent(interruptID string) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-1")
	ev.Author = "user"
	part := genai.NewPartFromFunctionResponse("adk_request_input", map[string]any{
		"response": map[string]any{"approved": true},
	})
	part.FunctionResponse.ID = interruptID
	ev.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{part}}
	return ev
}

func TestListEmpty(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			store := NewStore(svc, testApp)
			got, err := store.List(context.Background(), "")
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("List of empty store = %d sessions, want 0", len(got))
			}
		})
	}
}

func TestListStates(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			seed(t, svc, "op", "s-idle", textEvent("agent", "done"))
			seed(t, svc, "op", "s-paused",
				textEvent("agent", "triaging"),
				interruptEvent("triager", "approve-1", "Approve?"))
			seed(t, svc, "op", "s-resolved",
				interruptEvent("triager", "approve-2", "Approve?"),
				resolutionEvent("approve-2"),
				textEvent("triager", "applied"))

			store := NewStore(svc, testApp)
			got, err := store.List(context.Background(), "")
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			states := map[string]string{}
			for _, s := range got {
				states[s.ID] = s.State
			}
			want := map[string]string{
				"s-idle":     StateIdle,
				"s-paused":   StatePaused,
				"s-resolved": StateIdle,
			}
			for id, wantState := range want {
				if states[id] != wantState {
					t.Errorf("session %q state = %q, want %q", id, states[id], wantState)
				}
			}
			for _, s := range got {
				if s.ID == "s-paused" {
					if len(s.PendingInterruptIDs) != 1 || s.PendingInterruptIDs[0] != "approve-1" {
						t.Errorf("s-paused pending = %v, want [approve-1]", s.PendingInterruptIDs)
					}
				} else if len(s.PendingInterruptIDs) != 0 {
					t.Errorf("session %q pending = %v, want none", s.ID, s.PendingInterruptIDs)
				}
			}
		})
	}
}

func TestListUserFilter(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			seed(t, svc, "alice", "s-a", textEvent("agent", "hi"))
			seed(t, svc, "bob", "s-b", textEvent("agent", "hi"))

			store := NewStore(svc, testApp)
			got, err := store.List(context.Background(), "alice")
			if err != nil {
				t.Fatalf("List(alice): %v", err)
			}
			if len(got) != 1 || got[0].ID != "s-a" {
				t.Fatalf("List(alice) = %+v, want just s-a", got)
			}
		})
	}
}

func TestGetPendingDetail(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			seed(t, svc, "op", "s-hitl",
				textEvent("agent", "triaging"),
				interruptEvent("triager", "approve-x", "Approve the rollback?"))

			store := NewStore(svc, testApp)
			// Empty userID exercises the discovery path the CLI uses.
			d, err := store.Get(context.Background(), "", "s-hitl")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if d.State != StatePaused {
				t.Fatalf("state = %q, want %q", d.State, StatePaused)
			}
			if d.UserID != "op" || d.AppName != testApp {
				t.Errorf("identity = %s/%s, want op/%s", d.UserID, d.AppName, testApp)
			}
			if d.EventCount != 2 {
				t.Errorf("EventCount = %d, want 2", d.EventCount)
			}
			if len(d.Pending) != 1 {
				t.Fatalf("Pending = %+v, want exactly 1", d.Pending)
			}
			p := d.Pending[0]
			if p.InterruptID != "approve-x" {
				t.Errorf("InterruptID = %q, want approve-x", p.InterruptID)
			}
			if p.Message != "Approve the rollback?" {
				t.Errorf("Message = %q", p.Message)
			}
			if p.Author != "triager" {
				t.Errorf("Author = %q, want triager", p.Author)
			}
			if p.ResponseSchema == nil || p.ResponseSchema.Type != "object" {
				t.Errorf("ResponseSchema did not round-trip: %+v", p.ResponseSchema)
			}
			if p.RaisedAt.IsZero() {
				t.Error("RaisedAt is zero")
			}
		})
	}
}

func TestGetNotFound(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			store := NewStore(svc, testApp)
			_, err := store.Get(context.Background(), "", "no-such-session")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get(no-such-session) err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestAbort(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			seed(t, svc, "op", "s-abort",
				interruptEvent("triager", "approve-y", "Approve?"))

			store := NewStore(svc, testApp)
			if err := store.Abort(ctx, "", "s-abort", "operator cancelled"); err != nil {
				t.Fatalf("Abort: %v", err)
			}

			d, err := store.Get(ctx, "", "s-abort")
			if err != nil {
				t.Fatalf("Get after abort: %v", err)
			}
			if d.State != StateAborted {
				t.Errorf("state = %q, want %q", d.State, StateAborted)
			}
			if d.AbortReason != "operator cancelled" {
				t.Errorf("AbortReason = %q", d.AbortReason)
			}
			if len(d.Pending) != 0 || len(d.PendingInterruptIDs) != 0 {
				t.Errorf("aborted session still reports pending: %+v", d.Pending)
			}
			if d.EventCount != 2 { // interrupt + abort marker
				t.Errorf("EventCount = %d, want 2", d.EventCount)
			}

			err = store.Abort(ctx, "", "s-abort", "again")
			if !errors.Is(err, ErrAlreadyAborted) {
				t.Fatalf("second Abort err = %v, want ErrAlreadyAborted", err)
			}

			// Abort of a missing session surfaces ErrNotFound.
			err = store.Abort(ctx, "", "no-such-session", "x")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("Abort(no-such-session) err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestReRaisedInterrupt(t *testing.T) {
	// An interrupt ID resolved and then raised again (ADK discourages
	// it, but deterministic IDs make it possible) must count as
	// pending exactly once.
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			seed(t, svc, "op", "s-reraise",
				interruptEvent("triager", "approve-z", "Approve?"),
				resolutionEvent("approve-z"),
				interruptEvent("triager", "approve-z", "Approve again?"))

			store := NewStore(svc, testApp)
			d, err := store.Get(context.Background(), "op", "s-reraise")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if d.State != StatePaused {
				t.Fatalf("state = %q, want %q", d.State, StatePaused)
			}
			if len(d.Pending) != 1 || d.Pending[0].Message != "Approve again?" {
				t.Fatalf("Pending = %+v, want the re-raised interrupt once", d.Pending)
			}
		})
	}
}

func TestOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")

	// Open refuses a path that does not exist (operator typo guard).
	if _, err := Open(path, testApp); err == nil {
		t.Fatal("Open(missing path) succeeded, want error")
	}

	// Write through the same service the daemon uses, then Open the
	// file the way the CLI does.
	svc, err := database.NewSessionService(sqlite.Open(path))
	if err != nil {
		t.Fatalf("open writer service: %v", err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seed(t, svc, "op", "s-cli", interruptEvent("triager", "approve-cli", "Approve?"))

	store, err := Open(path, testApp)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "s-cli" || got[0].State != StatePaused {
		t.Fatalf("List via Open = %+v, want s-cli paused", got)
	}
}
