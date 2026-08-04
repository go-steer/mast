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
			// The abort marker lives in the companion ops row, not the
			// primary transcript (issue #46) — EventCount is the
			// primary's alone.
			if d.EventCount != 1 {
				t.Errorf("EventCount = %d, want 1 (abort marker must not land in the primary row)", d.EventCount)
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

func TestInterruptedMarker(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			seed(t, svc, "op", "s-interrupted",
				textEvent("triager", "working..."))

			store := NewStore(svc, testApp)
			if err := store.MarkInterrupted(ctx, "op", "s-interrupted", "daemon shutdown (SIGTERM)"); err != nil {
				t.Fatalf("MarkInterrupted: %v", err)
			}

			d, err := store.Get(ctx, "", "s-interrupted")
			if err != nil {
				t.Fatalf("Get after mark: %v", err)
			}
			if d.State != StateInterrupted {
				t.Errorf("state = %q, want %q", d.State, StateInterrupted)
			}
			if d.InterruptReason != "daemon shutdown (SIGTERM)" {
				t.Errorf("InterruptReason = %q", d.InterruptReason)
			}

			// Clean completion inside the drain window clears the marker.
			if err := store.ClearInterrupted(ctx, "op", "s-interrupted"); err != nil {
				t.Fatalf("ClearInterrupted: %v", err)
			}
			d, err = store.Get(ctx, "", "s-interrupted")
			if err != nil {
				t.Fatalf("Get after clear: %v", err)
			}
			if d.State != StateIdle {
				t.Errorf("state after clear = %q, want %q", d.State, StateIdle)
			}
			if d.InterruptReason != "" {
				t.Errorf("InterruptReason after clear = %q, want empty", d.InterruptReason)
			}

			// Re-marking after a clear works (a later shutdown).
			if err := store.MarkInterrupted(ctx, "op", "s-interrupted", "second shutdown"); err != nil {
				t.Fatalf("re-MarkInterrupted: %v", err)
			}
			d, err = store.Get(ctx, "", "s-interrupted")
			if err != nil {
				t.Fatalf("Get after re-mark: %v", err)
			}
			if d.State != StateInterrupted || d.InterruptReason != "second shutdown" {
				t.Errorf("after re-mark: state=%q reason=%q", d.State, d.InterruptReason)
			}

			// The empty string is the cleared state, so it is not a
			// valid marking reason.
			if err := store.MarkInterrupted(ctx, "op", "s-interrupted", ""); err == nil {
				t.Error("MarkInterrupted with empty reason succeeded, want error")
			}

			// Marking a missing session surfaces ErrNotFound.
			if err := store.MarkInterrupted(ctx, "", "no-such-session", "x"); !errors.Is(err, ErrNotFound) {
				t.Errorf("MarkInterrupted(no-such-session) err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestInterruptedPrecedence(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			// Paused trumps interrupted: a marked session whose log
			// carries a pending RequestedInput is resumable and reports
			// paused.
			seed(t, svc, "op", "s-mark-paused",
				interruptEvent("triager", "approve-a", "Approve?"))
			store := NewStore(svc, testApp)
			if err := store.MarkInterrupted(ctx, "op", "s-mark-paused", "daemon shutdown"); err != nil {
				t.Fatalf("MarkInterrupted: %v", err)
			}
			d, err := store.Get(ctx, "", "s-mark-paused")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if d.State != StatePaused {
				t.Errorf("marked+pending state = %q, want %q", d.State, StatePaused)
			}

			// Aborted trumps both.
			if err := store.Abort(ctx, "op", "s-mark-paused", "operator cancelled"); err != nil {
				t.Fatalf("Abort: %v", err)
			}
			d, err = store.Get(ctx, "", "s-mark-paused")
			if err != nil {
				t.Fatalf("Get after abort: %v", err)
			}
			if d.State != StateAborted {
				t.Errorf("marked+pending+aborted state = %q, want %q", d.State, StateAborted)
			}
		})
	}
}

// liveHandleEvent builds an appendable text event for write-lease
// tests (fresh ID per call so database-service primary keys don't
// collide).
func liveHandleEvent(text string) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-live")
	ev.Author = "triager"
	ev.Content = genai.NewContentFromText(text, genai.RoleModel)
	return ev
}

// TestMarkersDoNotInvalidateLiveHandle is the regression test for
// issues #45/#46: ADK's database service treats a session handle as a
// write lease (optimistic concurrency on last_update_time), so a
// marker appended to the PRIMARY row would make the runner's next
// AppendEvent fail with "stale session error" — killing the very turn
// the shutdown marker was recording (and, for Abort, violating its
// marker-not-preemption contract). Markers therefore go to the
// companion ops row; this test simulates the runner by holding a live
// handle across marker writes and appending through it afterwards.
func TestMarkersDoNotInvalidateLiveHandle(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			seed(t, svc, "op", "s-live") // created empty; events appended below

			// The "runner": grab a handle and stream an event through it.
			live, err := svc.Get(ctx, &adksession.GetRequest{
				AppName: testApp, UserID: "op", SessionID: "s-live",
			})
			if err != nil {
				t.Fatalf("get live handle: %v", err)
			}
			// Natural timestamps only (small sleeps for strict
			// monotonicity): pinning timestamps into the future here
			// drove the row's UpdateTime backwards on the marker
			// append and neutralized the OCC check this test exists
			// to exercise — the pre-fix code PASSED it (#54).
			time.Sleep(2 * time.Millisecond)
			ev1 := liveHandleEvent("turn event 1")
			if err := svc.AppendEvent(ctx, live.Session, ev1); err != nil {
				t.Fatalf("append via live handle (pre-mark): %v", err)
			}

			store := NewStore(svc, testApp)

			// Shutdown marks the session mid-turn...
			if err := store.MarkInterrupted(ctx, "op", "s-live", "daemon shutdown"); err != nil {
				t.Fatalf("MarkInterrupted: %v", err)
			}
			// ...and an operator aborts it mid-turn too.
			if err := store.Abort(ctx, "op", "s-live", "operator cancelled"); err != nil {
				t.Fatalf("Abort: %v", err)
			}

			// The runner's next streamed event MUST still append —
			// this line failed with "stale session error" when markers
			// wrote to the primary row.
			time.Sleep(2 * time.Millisecond)
			ev2 := liveHandleEvent("turn event 2")
			if err := svc.AppendEvent(ctx, live.Session, ev2); err != nil {
				t.Fatalf("append via live handle after markers: %v (write-lease regression — markers must not touch the primary row)", err)
			}

			// And the markers are visible through the projection.
			d, err := store.Get(ctx, "", "s-live")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if d.State != StateAborted || d.AbortReason != "operator cancelled" {
				t.Errorf("state = %q (%q), want aborted", d.State, d.AbortReason)
			}
			if d.EventCount != 2 {
				t.Errorf("primary EventCount = %d, want 2 (both runner events, no marker events)", d.EventCount)
			}
		})
	}
}

// TestOpsRowsHiddenFromList: companion rows are marker storage, not
// sessions — List must not surface them.
func TestOpsRowsHiddenFromList(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			seed(t, svc, "op", "s-hide", textEvent("triager", "hello"))
			store := NewStore(svc, testApp)
			if err := store.MarkInterrupted(ctx, "op", "s-hide", "daemon shutdown"); err != nil {
				t.Fatalf("MarkInterrupted: %v", err)
			}
			summaries, err := store.List(ctx, "")
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(summaries) != 1 || summaries[0].ID != "s-hide" {
				t.Fatalf("List = %+v, want exactly the primary session", summaries)
			}
			if summaries[0].State != StateInterrupted {
				t.Errorf("state = %q, want interrupted (ops-row marker must fold into the primary)", summaries[0].State)
			}
		})
	}
}

// TestMarkBeforeSessionExists: a SIGTERM can land before the runner's
// auto-create has committed the primary row. The marker parks in the
// ops row and surfaces once the primary appears.
func TestMarkBeforeSessionExists(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := NewStore(svc, testApp)

			if err := store.MarkInterrupted(ctx, "op", "s-ghost", "daemon shutdown"); err != nil {
				t.Fatalf("MarkInterrupted before primary exists: %v", err)
			}
			if _, err := store.Get(ctx, "op", "s-ghost"); err == nil {
				t.Fatal("Get(ghost primary) succeeded, want error while primary is missing")
			}

			seed(t, svc, "op", "s-ghost", textEvent("triager", "late create"))
			d, err := store.Get(ctx, "op", "s-ghost")
			if err != nil {
				t.Fatalf("Get after primary created: %v", err)
			}
			if d.State != StateInterrupted {
				t.Errorf("state = %q, want interrupted (parked marker must surface)", d.State)
			}
		})
	}
}

// TestLegacyAbortMarkerStillHonored: v0.1.0 wrote abort markers into
// the primary row's own state; existing DBs must keep reporting
// aborted, and re-aborting them stays idempotent.
func TestLegacyAbortMarkerStillHonored(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			legacy := adksession.NewEvent(ctx, "operator-abort")
			legacy.Author = "operator"
			legacy.Content = genai.NewContentFromText("session aborted by operator: old style", genai.RoleUser)
			legacy.Actions.StateDelta[abortReasonKey] = "old style"
			legacy.Actions.StateDelta[abortTimeKey] = time.Now().UTC().Format(time.RFC3339Nano)
			seed(t, svc, "op", "s-legacy", legacy)

			store := NewStore(svc, testApp)
			d, err := store.Get(ctx, "", "s-legacy")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if d.State != StateAborted || d.AbortReason != "old style" {
				t.Errorf("state = %q (%q), want legacy aborted", d.State, d.AbortReason)
			}
			if err := store.Abort(ctx, "op", "s-legacy", "again"); !errors.Is(err, ErrAlreadyAborted) {
				t.Errorf("re-abort of legacy-aborted err = %v, want ErrAlreadyAborted", err)
			}
		})
	}
}

func TestScanInterrupted(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := NewStore(svc, testApp)

			// One interrupted, one idle, one paused, one aborted, plus a
			// resolved (cleared) marker — only the interrupted one is a
			// candidate.
			seed(t, svc, "op", "s-int", textEvent("triager", "working..."))
			seed(t, svc, "op", "s-idle", textEvent("triager", "done"))
			seed(t, svc, "op", "s-paused", interruptEvent("triager", "i-1", "approve?"))
			seed(t, svc, "op", "s-aborted", textEvent("triager", "mid"))
			seed(t, svc, "op", "s-cleared", textEvent("triager", "mid"))

			if err := store.MarkInterrupted(ctx, "op", "s-int", "daemon shutdown (SIGTERM)"); err != nil {
				t.Fatalf("MarkInterrupted(s-int): %v", err)
			}
			// A paused session that ALSO carries an interrupt marker must
			// still project paused (precedence), so it is not a candidate.
			if err := store.MarkInterrupted(ctx, "op", "s-paused", "daemon shutdown"); err != nil {
				t.Fatalf("MarkInterrupted(s-paused): %v", err)
			}
			if err := store.MarkInterrupted(ctx, "op", "s-aborted", "daemon shutdown"); err != nil {
				t.Fatalf("MarkInterrupted(s-aborted): %v", err)
			}
			if err := store.Abort(ctx, "op", "s-aborted", "operator"); err != nil {
				t.Fatalf("Abort(s-aborted): %v", err)
			}
			if err := store.MarkInterrupted(ctx, "op", "s-cleared", "daemon shutdown"); err != nil {
				t.Fatalf("MarkInterrupted(s-cleared): %v", err)
			}
			if err := store.ClearInterrupted(ctx, "op", "s-cleared"); err != nil {
				t.Fatalf("ClearInterrupted(s-cleared): %v", err)
			}

			got, err := store.ScanInterrupted(ctx)
			if err != nil {
				t.Fatalf("ScanInterrupted: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("ScanInterrupted found %d candidates, want 1: %+v", len(got), got)
			}
			c := got[0]
			if c.SessionID != "s-int" || c.UserID != "op" {
				t.Fatalf("candidate = %q/%q, want s-int/op", c.UserID, c.SessionID)
			}
			if c.InterruptReason != "daemon shutdown (SIGTERM)" {
				t.Errorf("InterruptReason = %q", c.InterruptReason)
			}
			if c.InterruptedAt.IsZero() {
				t.Error("InterruptedAt is zero; freshness window would treat it as ancient")
			}
			if c.EventCount == 0 || c.Events == nil {
				t.Errorf("candidate carries no events (count=%d), the effects scan needs them", c.EventCount)
			}
			// The loaded events must be the PRIMARY transcript, not the
			// ops row.
			if !hasText(c.Events, "working...") {
				t.Error("candidate.Events is not the primary transcript")
			}
		})
	}
}

func hasText(events adksession.Events, want string) bool {
	for ev := range events.All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.Text == want {
				return true
			}
		}
	}
	return false
}

func TestAutoResumeAttemptCounter(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := NewStore(svc, testApp)
			seed(t, svc, "op", "s-loop", textEvent("triager", "working"))

			// Unmarked reads as zero (fail-open direction for the breaker).
			if n, last := store.AutoResumeAttempts(ctx, "op", "s-loop"); n != 0 || !last.IsZero() {
				t.Fatalf("fresh attempts = %d,%v want 0,zero", n, last)
			}

			before := time.Now().Add(-time.Second)
			n, err := store.RecordAutoResumeAttempt(ctx, "op", "s-loop")
			if err != nil {
				t.Fatalf("RecordAutoResumeAttempt: %v", err)
			}
			if n != 1 {
				t.Fatalf("first attempt count = %d, want 1", n)
			}
			n, err = store.RecordAutoResumeAttempt(ctx, "op", "s-loop")
			if err != nil {
				t.Fatalf("RecordAutoResumeAttempt #2: %v", err)
			}
			if n != 2 {
				t.Fatalf("second attempt count = %d, want 2", n)
			}
			got, last := store.AutoResumeAttempts(ctx, "op", "s-loop")
			if got != 2 {
				t.Fatalf("read-back attempts = %d, want 2", got)
			}
			if last.Before(before) {
				t.Fatalf("last-attempt stamp %v predates the test", last)
			}

			// A successful resume clears the counter.
			if err := store.ClearAutoResumeAttempts(ctx, "op", "s-loop"); err != nil {
				t.Fatalf("ClearAutoResumeAttempts: %v", err)
			}
			if n, _ := store.AutoResumeAttempts(ctx, "op", "s-loop"); n != 0 {
				t.Fatalf("attempts after clear = %d, want 0", n)
			}

			// The counter is a marker, not a state: derivation unchanged.
			d, err := store.Get(ctx, "", "s-loop")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if d.State != StateIdle {
				t.Fatalf("state with attempt counter = %q, want %q", d.State, StateIdle)
			}
		})
	}
}

func TestAckEffects(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := NewStore(svc, testApp)
			seed(t, svc, "op", "s1", textEvent("agent", "working"))

			// No marker yet: fail-closed direction is "not acked".
			if _, ok := store.EffectsAckedAt(ctx, "", "s1"); ok {
				t.Fatal("EffectsAckedAt reported an ack on an unmarked session")
			}

			before := time.Now().Add(-time.Second)
			if err := store.AckEffects(ctx, "", "s1", "operator checked the cluster"); err != nil {
				t.Fatalf("AckEffects: %v", err)
			}
			at, ok := store.EffectsAckedAt(ctx, "", "s1")
			if !ok {
				t.Fatal("EffectsAckedAt = false after AckEffects")
			}
			if at.Before(before) || at.After(time.Now().Add(time.Second)) {
				t.Fatalf("ack watermark %v is not around now", at)
			}

			// Re-ack overwrites forward (last write wins).
			time.Sleep(5 * time.Millisecond)
			if err := store.AckEffects(ctx, "", "s1", "second check"); err != nil {
				t.Fatalf("re-AckEffects: %v", err)
			}
			at2, ok := store.EffectsAckedAt(ctx, "", "s1")
			if !ok || !at2.After(at) {
				t.Fatalf("re-ack watermark %v (ok=%v) did not move forward from %v", at2, ok, at)
			}

			// The ack is a watermark, not a state: derivation unchanged.
			d, err := store.Get(ctx, "", "s1")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if d.State != StateIdle {
				t.Fatalf("state after ack = %q, want %q (ack must not perturb the state ladder)", d.State, StateIdle)
			}

			// Reserved IDs are refused like every other surface (#56).
			if err := store.AckEffects(ctx, "", "s1:mast-ops", "x"); err == nil {
				t.Fatal("AckEffects accepted a reserved ops-row ID")
			}
			if _, ok := store.EffectsAckedAt(ctx, "", "s1:mast-ops"); ok {
				t.Fatal("EffectsAckedAt reported an ack for a reserved ops-row ID")
			}

			// Unknown session: no ack, no error surface to trip on.
			if _, ok := store.EffectsAckedAt(ctx, "", "missing"); ok {
				t.Fatal("EffectsAckedAt reported an ack for a nonexistent session")
			}
		})
	}
}
