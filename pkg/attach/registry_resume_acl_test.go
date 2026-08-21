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

package attach

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/go-steer/mast/pkg/auth"
)

// forgetfulResumer is the resumer the real world writes: it rebuilds
// the Registrant and returns the zero auth.SessionACL, because the ACL
// is the registry's durable state and a factory has no particular
// reason to go re-read it. mast's own storeResumer is exactly this
// shape (cmd/mast/attach.go).
//
// The existing stubResumer returns row.ACL(), which is why nothing
// caught this: the test double was better behaved than any resumer
// anyone actually writes (#223).
type forgetfulResumer struct {
	acl   auth.SessionACL // what this resumer claims, zero unless a test says otherwise
	calls int
	err   error
}

func (f *forgetfulResumer) Resume(_ context.Context, app, sid string) (Registrant, auth.SessionACL, context.CancelFunc, error) {
	f.calls++
	if f.err != nil {
		return nil, auth.SessionACL{}, nil, f.err
	}
	return &stubRegistrant{app: app, user: "u", sid: sid}, f.acl, nil, nil
}

// seedOwnedSession writes one persisted, owned session and returns a
// registry sharing that store but with nothing in memory — a daemon
// that restarted, or a session the eviction sweep dropped.
func seedOwnedSession(t *testing.T, store SessionACLStore, owner string) *SessionRegistry {
	t.Helper()
	warm := NewSessionRegistryWithStore(store)
	if _, err := warm.RegisterOwned(&stubRegistrant{app: "mast", user: "u", sid: "s1"}, owner); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	return NewSessionRegistryWithStore(store)
}

// The owner asks for a session that is on disk and not in memory. The
// resume gate reads the persisted row, sees them, and admits them —
// and then the entry that resume creates is governed by whatever the
// resumer handed back, which is the zero ACL. Zero means "no owner",
// which under the authorization matrix means admins only. So the
// owner's own request builds the entry that locks them out of it.
func TestAResumedSessionKeepsTheOwnerItWasStoredWith(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	reg := seedOwnedSession(t, store, "alice@example.com")
	reg.WithResumer(&forgetfulResumer{})

	entry, err := reg.Lookup(context.Background(), "mast", "s1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got := entry.ACL().Owner; got != "alice@example.com" {
		t.Errorf("resumed entry owner = %q, want alice@example.com — "+
			"a resumed session with no owner admits admins only, so this is the owner locked out of their own session", got)
	}
}

// The same thing through the doors an operator actually knocks on: the
// registry-level assertion above says the ACL survived, this one says
// access did.
func TestTheOwnerCanStillReachASessionThatHadToBeResumed(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	seed := NewSessionRegistryWithStore(store)
	if _, err := seed.RegisterOwned(&stubRegistrant{app: "mast", user: "u", sid: "s1"}, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}

	// A fresh daemon over the same store: the row is there, memory is
	// empty, and the first request has to resume.
	mux, reg := aclFixture(t, true, store)
	reg.WithResumer(&forgetfulResumer{})
	alice := auth.Caller{Identity: "alice@example.com"}

	if rr := aclCall(t, mux, http.MethodGet, "/sessions/mast/s1/status", "", alice); rr.Code != http.StatusOK {
		t.Fatalf("owner reading her own resumed session: status = %d, want 200 "+
			"(a 404 here is the resume gate admitting her and the resumed entry refusing her)", rr.Code)
	}
	// And the ACL she reads back is the one on disk, not an empty one
	// that would tell her the session has no owner.
	rr := aclCall(t, mux, http.MethodGet, "/sessions/mast/s1/acl", "", alice)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET acl: status = %d, want 200", rr.Code)
	}
	if got := decodeACL(t, rr); got.Owner != "alice@example.com" {
		t.Errorf("owner on the resumed session reads %q, want alice@example.com", got.Owner)
	}
}

// An amendment made before the eviction has to survive it. This is the
// half of #216 that only shows up one restart later: PUT wrote the
// viewer to disk and to memory, and memory is what went away.
func TestAnAmendedACLSurvivesAResume(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	warm, warmReg := aclFixture(t, true, store)
	if _, err := warmReg.RegisterOwned(&stubRegistrant{app: "mast", user: "u", sid: "s1"}, "alice@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	alice := auth.Caller{Identity: "alice@example.com"}
	bob := auth.Caller{Identity: "bob@example.com"}

	body := `{"viewers":["bob@example.com"]}`
	if rr := aclCall(t, warm, http.MethodPut, "/sessions/mast/s1/acl", body, alice); rr.Code != http.StatusOK {
		t.Fatalf("PUT acl: status = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	cold, coldReg := aclFixture(t, true, store)
	coldReg.WithResumer(&forgetfulResumer{})
	if rr := aclCall(t, cold, http.MethodGet, "/sessions/mast/s1/status", "", bob); rr.Code != http.StatusOK {
		t.Errorf("the viewer alice added is shut out after a resume: status = %d, want 200", rr.Code)
	}
}

// The row wins even when the resumer is confidently wrong. A resumer
// that returns a stale or invented ACL is not a second opinion — the
// row is what PUT /acl writes and what the resume gate authorized
// against, so an entry governed by anything else disagrees with the
// check that let the caller in.
func TestThePersistedRowOutranksTheResumer(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	reg := seedOwnedSession(t, store, "alice@example.com")
	reg.WithResumer(&forgetfulResumer{acl: auth.SessionACL{
		Owner:   "mallory@example.com",
		Viewers: []string{"mallory-friend@example.com"},
	}})

	entry, err := reg.Lookup(context.Background(), "mast", "s1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	got := entry.ACL()
	if got.Owner != "alice@example.com" || len(got.Viewers) != 0 {
		t.Errorf("resumed ACL = %+v, want alice@example.com with no viewers — the resumer does not get to reassign a session", got)
	}
}

// With no row to read, the resumer's ACL is all there is, and it is
// still honored. This is the legacy path (a session registered before
// the store existed) and the storeless one, and neither should have
// changed.
func TestWithNoPersistedRowTheResumerStillDecides(t *testing.T) {
	t.Parallel()
	declared := auth.SessionACL{Owner: "alice@example.com", Contributors: []string{"bob@example.com"}}

	t.Run("no store wired at all", func(t *testing.T) {
		t.Parallel()
		reg := NewSessionRegistry().WithResumer(&forgetfulResumer{acl: declared})
		entry, err := reg.Lookup(context.Background(), "mast", "s1")
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		if !reflect.DeepEqual(entry.ACL(), declared) {
			t.Errorf("ACL = %+v, want %+v", entry.ACL(), declared)
		}
	})

	t.Run("store wired, session not in it", func(t *testing.T) {
		t.Parallel()
		reg := NewSessionRegistryWithStore(newTestACLStore(t)).WithResumer(&forgetfulResumer{acl: declared})
		entry, err := reg.Lookup(context.Background(), "mast", "s1")
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		if !reflect.DeepEqual(entry.ACL(), declared) {
			t.Errorf("ACL = %+v, want %+v", entry.ACL(), declared)
		}
	})
}

// unreadableStore fails every read. Writes go to the embedded store so
// a test can still seed one.
type unreadableStore struct {
	SessionACLStore
	err error
}

func (u *unreadableStore) Get(context.Context, string, string, string) (SessionACLRow, error) {
	return SessionACLRow{}, u.err
}

func (u *unreadableStore) FindByAppSID(context.Context, string, string) (SessionACLRow, error) {
	return SessionACLRow{}, u.err
}

// An ungated Lookup reads the row for the ACL rather than for
// authorization, so a store that will not answer must not fail the
// resume — nothing about a transient read error says the session
// should stop being reachable. The gated path keeps its existing
// fail-closed behavior, which resume_gate_test.go pins.
func TestAStoreThatCannotBeReadDoesNotFailAnUngatedResume(t *testing.T) {
	t.Parallel()
	boom := errors.New("store unavailable")
	declared := auth.SessionACL{Owner: "alice@example.com"}
	reg := NewSessionRegistryWithStore(&unreadableStore{SessionACLStore: newTestACLStore(t), err: boom}).
		WithResumer(&forgetfulResumer{acl: declared})

	entry, err := reg.Lookup(context.Background(), "mast", "s1")
	if err != nil {
		t.Fatalf("Lookup: %v — a read error is not a reason to refuse a resume", err)
	}
	if entry.ACL().Owner != "alice@example.com" {
		t.Errorf("owner = %q, want the resumer's, which is the only ACL available", entry.ACL().Owner)
	}
}

// The row is read once per resume, not once per caller: the read sits
// outside the singleflight (it has to, so one caller's deny is never
// another's result), but the resume itself still collapses.
func TestConcurrentResumesStillCallTheResumerOnce(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	reg := seedOwnedSession(t, store, "alice@example.com")
	resumer := &forgetfulResumer{}
	reg.WithResumer(resumer)

	for range 3 {
		if _, err := reg.Lookup(context.Background(), "mast", "s1"); err != nil {
			t.Fatalf("Lookup: %v", err)
		}
	}
	if resumer.calls != 1 {
		t.Errorf("resumer called %d times, want 1 — later lookups should hit memory", resumer.calls)
	}
}
