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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/mast/pkg/auth"
)

// aclFixture wires the full route set (not just registerOperatorState)
// so a test can prove an amendment changed real access — the point of
// the endpoint is that a caller who got 404 from /status before the
// PUT gets 200 after it.
func aclFixture(t *testing.T, enforceACL bool, store SessionACLStore) (*http.ServeMux, *SessionRegistry) {
	t.Helper()
	reg := NewSessionRegistry()
	if store != nil {
		reg = NewSessionRegistryWithStore(store)
	}
	h := &handlers{
		reg:        reg,
		pool:       newBroadcasterPool(),
		enforceACL: enforceACL,
		closing:    make(chan struct{}),
	}
	mux := http.NewServeMux()
	h.register(mux)
	return mux, reg
}

// aclCall drives one request through the mux as caller. Body is sent
// verbatim when non-empty so malformed-JSON cases are reachable.
func aclCall(t *testing.T, mux *http.ServeMux, method, target, body string, caller auth.Caller) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if caller.Identity != "" || caller.Admin {
		r = r.WithContext(auth.WithCaller(r.Context(), caller))
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	return rr
}

func decodeACL(t *testing.T, rr *httptest.ResponseRecorder) sessionACLResponse {
	t.Helper()
	var out sessionACLResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode acl response %q: %v", rr.Body.String(), err)
	}
	return out
}

// TestAViewerCanBeAddedToARunningSession is the whole feature in one
// test: before the PUT the stranger cannot see the session at all,
// after it they can — with no delete-and-recreate in between.
func TestAViewerCanBeAddedToARunningSession(t *testing.T) {
	t.Parallel()
	mux, reg := aclFixture(t, true, newTestACLStore(t))
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "mast", user: "u", sid: "s1"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	owner := auth.Caller{Identity: "owner@example.com"}
	newcomer := auth.Caller{Identity: "newcomer@example.com"}

	if rr := aclCall(t, mux, http.MethodGet, "/sessions/mast/s1/status", "", newcomer); rr.Code != http.StatusNotFound {
		t.Fatalf("before the amendment the newcomer should be shut out: status = %d, want 404", rr.Code)
	}

	rr := aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl",
		`{"viewers":["newcomer@example.com"]}`, owner)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner PUT /acl: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}

	if rr := aclCall(t, mux, http.MethodGet, "/sessions/mast/s1/status", "", newcomer); rr.Code != http.StatusOK {
		t.Errorf("after the amendment the newcomer should be admitted: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	// And the reverse: a viewer is a viewer, not a contributor.
	if rr := aclCall(t, mux, http.MethodPost, "/sessions/mast/s1/inject", `{"message":"hi"}`, newcomer); rr.Code != http.StatusNotFound {
		t.Errorf("a viewer must not gain write access: inject status = %d, want 404", rr.Code)
	}
}

// TestTheAmendedACLIsTheOneOnDisk — an amendment a restart would lose
// is not an amendment. Also guards the timestamp carry-forward: Put is
// a whole-row Save that defaults a zero CreatedAt to now, so a naive
// write would silently re-date the session.
func TestTheAmendedACLIsTheOneOnDisk(t *testing.T) {
	t.Parallel()
	store := newTestACLStore(t)
	mux, reg := aclFixture(t, true, store)
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "mast", user: "u", sid: "s1"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	before, err := store.Get(context.Background(), "mast", "u", "s1")
	if err != nil {
		t.Fatalf("pre-amendment Get: %v", err)
	}

	rr := aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl",
		`{"viewers":["v@example.com"],"contributors":["c@example.com"]}`,
		auth.Caller{Identity: "owner@example.com"})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT /acl: status = %d; body=%q", rr.Code, rr.Body.String())
	}
	if got := decodeACL(t, rr); !got.Persisted {
		t.Errorf("response says persisted=false for a store-backed owned session")
	}

	after, err := store.Get(context.Background(), "mast", "u", "s1")
	if err != nil {
		t.Fatalf("post-amendment Get: %v", err)
	}
	if len(after.Viewers) != 1 || after.Viewers[0] != "v@example.com" {
		t.Errorf("persisted viewers = %v, want [v@example.com]", after.Viewers)
	}
	if len(after.Contributors) != 1 || after.Contributors[0] != "c@example.com" {
		t.Errorf("persisted contributors = %v, want [c@example.com]", after.Contributors)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("amending the ACL re-dated the session: CreatedAt %v -> %v", before.CreatedAt, after.CreatedAt)
	}
	if !after.LastTouchedAt.Equal(before.LastTouchedAt) {
		t.Errorf("amending the ACL bumped LastTouchedAt %v -> %v; an ACL edit is not session activity", before.LastTouchedAt, after.LastTouchedAt)
	}
}

// TestAnAmendmentNothingEnforcesIsRefused — with multi-session off the
// ACL governs nothing, so accepting a PUT would hand the operator a
// success for an access restriction that does not exist. GET still
// answers, and says why.
func TestAnAmendmentNothingEnforcesIsRefused(t *testing.T) {
	t.Parallel()
	mux, reg := aclFixture(t, false /* enforceACL */, newTestACLStore(t))
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "mast", user: "u", sid: "s1"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	owner := auth.Caller{Identity: "owner@example.com"}

	rr := aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl", `{"viewers":["v@example.com"]}`, owner)
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("PUT with enforcement off: status = %d, want 501; body=%q", rr.Code, rr.Body.String())
	}
	entry, err := reg.Lookup(context.Background(), "mast", "s1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got := entry.ACL().Viewers; len(got) != 0 {
		t.Errorf("refused PUT still amended the ACL: viewers = %v", got)
	}

	rr = aclCall(t, mux, http.MethodGet, "/sessions/mast/s1/acl", "", owner)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET with enforcement off: status = %d, want 200", rr.Code)
	}
	if got := decodeACL(t, rr); got.Enforced {
		t.Errorf("GET reported enforced=true on a daemon with multi-session off")
	}
}

// TestOwnershipTransferIsAdminOnly — the owner reached the handler
// through ActionSessionAdmin, so an "owner" field they are not allowed
// to set has to fail loudly rather than be dropped on the floor. A
// silently ignored transfer reads as a completed one.
func TestOwnershipTransferIsAdminOnly(t *testing.T) {
	t.Parallel()
	mux, reg := aclFixture(t, true, newTestACLStore(t))
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "mast", user: "u", sid: "s1"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	owner := auth.Caller{Identity: "owner@example.com"}
	admin := auth.Caller{Identity: "sre@example.com", Admin: true}

	rr := aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl", `{"owner":"someone-else@example.com"}`, owner)
	if rr.Code != http.StatusForbidden {
		t.Errorf("owner self-transfer: status = %d, want 403; body=%q", rr.Code, rr.Body.String())
	}
	entry, err := reg.Lookup(context.Background(), "mast", "s1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got := entry.ACL().Owner; got != "owner@example.com" {
		t.Fatalf("refused transfer still changed the owner: %q", got)
	}

	// Sending the CURRENT owner back is a read-modify-write round
	// trip, not a transfer, and must be allowed.
	rr = aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl",
		`{"owner":"owner@example.com","viewers":["v@example.com"]}`, owner)
	if rr.Code != http.StatusOK {
		t.Errorf("round-tripping the current owner: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}

	// An admin can hand the session to whoever inherits it.
	rr = aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl", `{"owner":"successor@example.com"}`, admin)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin transfer: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if got := decodeACL(t, rr).Owner; got != "successor@example.com" {
		t.Errorf("owner after admin transfer = %q", got)
	}
}

// TestAnOwnerCannotBeErased — a session whose owner is blanked is
// reachable by admins only. That is a lockout wearing an edit's
// clothes; refuse it at both the handler and the registry.
func TestAnOwnerCannotBeErased(t *testing.T) {
	t.Parallel()
	mux, reg := aclFixture(t, true, newTestACLStore(t))
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "mast", user: "u", sid: "s1"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}

	rr := aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl", `{"owner":""}`,
		auth.Caller{Identity: "sre@example.com", Admin: true})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("clearing the owner: status = %d, want 400; body=%q", rr.Code, rr.Body.String())
	}

	// The registry refuses it too, so an embedder calling SetACL
	// directly can't route around the handler.
	if _, err := reg.SetACL(context.Background(), "mast", "u", "s1", auth.SessionACL{}); err == nil {
		t.Errorf("SetACL accepted an empty owner for an owned session")
	}
	entry, _ := reg.Lookup(context.Background(), "mast", "s1")
	if got := entry.ACL().Owner; got != "owner@example.com" {
		t.Errorf("owner after refused clear = %q", got)
	}
}

// TestTheACLIsReadableByEveryoneOnItAndNobodyElse — GET sits at
// SessionRead so a viewer can see whom to ask for write access; a
// stranger gets the same 404 every other session route gives them.
func TestTheACLIsReadableByEveryoneOnItAndNobodyElse(t *testing.T) {
	t.Parallel()
	mux, reg := aclFixture(t, true, newTestACLStore(t))
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "mast", user: "u", sid: "s1"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	if _, err := reg.SetACL(context.Background(), "mast", "u", "s1", auth.SessionACL{
		Owner:   "owner@example.com",
		Viewers: []string{"v@example.com"},
	}); err != nil {
		t.Fatalf("SetACL: %v", err)
	}

	rr := aclCall(t, mux, http.MethodGet, "/sessions/mast/s1/acl", "", auth.Caller{Identity: "v@example.com"})
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer GET /acl: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	got := decodeACL(t, rr)
	if got.Owner != "owner@example.com" || len(got.Viewers) != 1 || got.Viewers[0] != "v@example.com" {
		t.Errorf("viewer GET /acl body = %+v", got)
	}
	if !got.Enforced || !got.Persisted {
		t.Errorf("enforced=%v persisted=%v, want both true", got.Enforced, got.Persisted)
	}

	rr = aclCall(t, mux, http.MethodGet, "/sessions/mast/s1/acl", "", auth.Caller{Identity: "stranger@example.com"})
	if rr.Code != http.StatusNotFound {
		t.Errorf("stranger GET /acl: status = %d, want 404", rr.Code)
	}
}

// TestPutReplacesRatherThanPatches — omitting a list clears it. The
// alternative (omission means "leave alone") leaves no way to remove
// the last viewer, which is the revocation half of the feature.
func TestPutReplacesRatherThanPatches(t *testing.T) {
	t.Parallel()
	mux, reg := aclFixture(t, true, newTestACLStore(t))
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "mast", user: "u", sid: "s1"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	owner := auth.Caller{Identity: "owner@example.com"}

	if rr := aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl",
		`{"viewers":["a@example.com","b@example.com"]}`, owner); rr.Code != http.StatusOK {
		t.Fatalf("seed PUT: status = %d; body=%q", rr.Code, rr.Body.String())
	}
	rr := aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl", `{"viewers":["b@example.com"]}`, owner)
	if rr.Code != http.StatusOK {
		t.Fatalf("revoking PUT: status = %d; body=%q", rr.Code, rr.Body.String())
	}
	if got := decodeACL(t, rr).Viewers; len(got) != 1 || got[0] != "b@example.com" {
		t.Errorf("viewers after revocation = %v, want [b@example.com]", got)
	}
	// a@ is out, and the gate agrees.
	if rr := aclCall(t, mux, http.MethodGet, "/sessions/mast/s1/status", "", auth.Caller{Identity: "a@example.com"}); rr.Code != http.StatusNotFound {
		t.Errorf("revoked viewer still admitted: status = %d, want 404", rr.Code)
	}

	// An empty array clears the list outright.
	rr = aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl", `{"viewers":[]}`, owner)
	if rr.Code != http.StatusOK {
		t.Fatalf("clearing PUT: status = %d; body=%q", rr.Code, rr.Body.String())
	}
	if got := decodeACL(t, rr).Viewers; len(got) != 0 {
		t.Errorf("viewers after clear = %v, want empty", got)
	}
}

// TestIdentityListsAreNormalized — a blank entry is a client bug that
// Authorize could never match, so it is rejected rather than stored;
// exact duplicates collapse; order survives.
func TestIdentityListsAreNormalized(t *testing.T) {
	t.Parallel()
	mux, reg := aclFixture(t, true, newTestACLStore(t))
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "mast", user: "u", sid: "s1"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	owner := auth.Caller{Identity: "owner@example.com"}

	rr := aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl", `{"viewers":["a@example.com",""]}`, owner)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty identity: status = %d, want 400; body=%q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "viewers[1]") {
		t.Errorf("error should name the offending index; got %q", rr.Body.String())
	}
	// Refused wholesale — a@ must not have been half-applied.
	entry, _ := reg.Lookup(context.Background(), "mast", "s1")
	if got := entry.ACL().Viewers; len(got) != 0 {
		t.Errorf("rejected PUT partially applied: viewers = %v", got)
	}

	rr = aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl",
		`{"viewers":["  b@example.com  ","a@example.com","b@example.com"]}`, owner)
	if rr.Code != http.StatusOK {
		t.Fatalf("normalizing PUT: status = %d; body=%q", rr.Code, rr.Body.String())
	}
	got := decodeACL(t, rr).Viewers
	if len(got) != 2 || got[0] != "b@example.com" || got[1] != "a@example.com" {
		t.Errorf("normalized viewers = %v, want [b@example.com a@example.com]", got)
	}
}

// TestAmendingWhileRequestsAuthorizeIsRaceFree — the reason Entry.ACL
// became an accessor. Before this endpoint the field was written once
// at registration and only ever read; now a slice header is reassigned
// under live traffic. Meaningful only under -race, which presubmits
// run.
func TestAmendingWhileRequestsAuthorizeIsRaceFree(t *testing.T) {
	t.Parallel()
	mux, reg := aclFixture(t, true, newTestACLStore(t))
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "mast", user: "u", sid: "s1"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	owner := auth.Caller{Identity: "owner@example.com"}

	stop := make(chan struct{})
	var readers, writers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				aclCall(t, mux, http.MethodGet, "/sessions/mast/s1/acl", "", owner)
			}
		}()
	}
	for i := 0; i < 2; i++ {
		writers.Add(1)
		go func(n int) {
			defer writers.Done()
			for j := 0; j < 25; j++ {
				body := `{"viewers":["v` + string(rune('a'+n)) + `@example.com"],"contributors":["c@example.com"]}`
				if rr := aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl", body, owner); rr.Code != http.StatusOK {
					t.Errorf("concurrent PUT: status = %d; body=%q", rr.Code, rr.Body.String())
					return
				}
			}
		}(i)
	}
	writers.Wait()
	close(stop)
	readers.Wait()

	if got := len(reg.List()); got != 1 {
		t.Fatalf("registry length after the storm = %d", got)
	}
}

// TestTheReturnedACLIsACopy — ACL() hands out cloned slices, so a
// caller that appends to what it got back is not quietly granting
// itself access to the live session.
func TestTheReturnedACLIsACopy(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.RegisterOwned(&stubRegistrant{app: "mast", user: "u", sid: "s1"}, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	if _, err := reg.SetACL(context.Background(), "mast", "u", "s1", auth.SessionACL{
		Owner:   "owner@example.com",
		Viewers: []string{"v@example.com"},
	}); err != nil {
		t.Fatalf("SetACL: %v", err)
	}
	entry, _ := reg.Lookup(context.Background(), "mast", "s1")

	snapshot := entry.ACL()
	snapshot.Viewers[0] = "attacker@example.com"
	snapshot.Owner = "attacker@example.com"

	live := entry.ACL()
	if live.Owner != "owner@example.com" || live.Viewers[0] != "v@example.com" {
		t.Errorf("mutating the returned ACL reached the live entry: %+v", live)
	}
}

// TestAnUnpersistableAmendmentSaysSo — a session registered via the
// legacy owner-less Register path has no durable row and cannot get
// one (SessionACLStore rejects an ownerless row). The amendment still
// applies in memory; the response must not claim it will survive a
// restart.
func TestAnUnpersistableAmendmentSaysSo(t *testing.T) {
	t.Parallel()
	mux, reg := aclFixture(t, true, newTestACLStore(t))
	if _, err := reg.Register(&stubRegistrant{app: "mast", user: "u", sid: "s1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	admin := auth.Caller{Identity: "sre@example.com", Admin: true}

	rr := aclCall(t, mux, http.MethodGet, "/sessions/mast/s1/acl", "", admin)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin GET: status = %d; body=%q", rr.Code, rr.Body.String())
	}
	if got := decodeACL(t, rr); got.Persisted {
		t.Errorf("an ownerless session reported persisted=true")
	}

	rr = aclCall(t, mux, http.MethodPut, "/sessions/mast/s1/acl", `{"viewers":["v@example.com"]}`, admin)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin PUT on an ownerless session: status = %d; body=%q", rr.Code, rr.Body.String())
	}
	got := decodeACL(t, rr)
	if got.Persisted {
		t.Errorf("amendment of an ownerless session reported persisted=true")
	}
	if len(got.Viewers) != 1 {
		t.Errorf("in-memory amendment did not take: %+v", got)
	}
	if rr := aclCall(t, mux, http.MethodGet, "/sessions/mast/s1/status", "", auth.Caller{Identity: "v@example.com"}); rr.Code != http.StatusOK {
		t.Errorf("the in-memory grant is not enforced: status = %d, want 200", rr.Code)
	}
}

// TestPutOnAnUnknownSessionIs404 — same masking convention as every
// other session route.
func TestPutOnAnUnknownSessionIs404(t *testing.T) {
	t.Parallel()
	mux, _ := aclFixture(t, true, newTestACLStore(t))
	rr := aclCall(t, mux, http.MethodPut, "/sessions/mast/nope/acl", `{"viewers":[]}`,
		auth.Caller{Identity: "sre@example.com", Admin: true})
	if rr.Code != http.StatusNotFound {
		t.Errorf("PUT on unknown session: status = %d, want 404; body=%q", rr.Code, rr.Body.String())
	}
}

// TestSetACLOnAnUnregisteredSessionIsNotFound — the registry-level
// contract behind the handler's 404.
func TestSetACLOnAnUnregisteredSessionIsNotFound(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	_, err := reg.SetACL(context.Background(), "mast", "u", "ghost", auth.SessionACL{Owner: "o@example.com"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("SetACL on an unregistered triple: err = %v, want a not-found error", err)
	}
}
