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

// Originally derived from go-steer/core-agent@9f8162687f33510b4681b42c6ce8c692c5c095ee:pkg/attach/peers_persist_test.go

package attach

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/auth"
)

// statePath returns a path inside a fresh temp dir. The dir, not just
// the file, is per-test: the persister writes its temp file alongside
// the target, so a shared dir would let parallel tests see each
// other's in-flight writes.
func statePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "peers.jsonl")
}

func mustRegisterOwned(t *testing.T, r *PeerRegistry, name, endpoint, owner string, ttl int) *Peer {
	t.Helper()
	p, err := r.RegisterOwned(RegisterRequest{
		Name: name, Endpoint: endpoint, HeartbeatTTLSec: ttl,
		Labels: map[string]string{"cluster": name},
	}, owner)
	if err != nil {
		t.Fatalf("RegisterOwned(%s): %v", name, err)
	}
	return p
}

func mustReopen(t *testing.T, path string, opts ...PeerRegistryOption) *PeerRegistry {
	t.Helper()
	r, err := NewPeerRegistryWithState(path, opts...)
	if err != nil {
		t.Fatalf("NewPeerRegistryWithState: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// TestPeerState_SurvivesRestart is the whole point of #180: a hub
// that comes back up already knows its fleet instead of waiting a
// heartbeat interval for everyone to re-register.
func TestPeerState_SurvivesRestart(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	now, _ := clockFn(time.Now())

	first := mustReopen(t, path, withClock(now))
	a := mustRegisterOwned(t, first, "cluster-a", "https://10.0.0.1:7777", "ops@example.com", 120)
	mustRegisterOwned(t, first, "cluster-b", "https://10.0.0.2:7777", "ops@example.com", 120)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := mustReopen(t, path, withClock(now))
	if got := second.Len(); got != 2 {
		t.Fatalf("peers after restart = %d, want 2", got)
	}
	reloaded, ok := second.Lookup(a.RegistrationID)
	if !ok {
		t.Fatalf("registration id %s did not survive the restart", a.RegistrationID)
	}
	if reloaded.Endpoint != a.Endpoint || reloaded.Labels["cluster"] != "cluster-a" {
		t.Errorf("reloaded peer = %+v, want endpoint/labels preserved", reloaded)
	}
	if !reloaded.LeaseExpiresAt.Equal(a.LeaseExpiresAt) {
		t.Errorf("lease expiry = %v, want %v", reloaded.LeaseExpiresAt, a.LeaseExpiresAt)
	}
}

// TestPeerState_HubRestartsBehindTheHandlers is the restart test #180
// asked for specifically: drop and rebuild the registry rather than
// assert on the write, and do it behind the HTTP surface an operator
// actually talks to.
//
// The registry-level tests above prove the state round-trips. This
// one proves the property the state exists to hold: after the hub
// process is replaced, GET /peers still hands alice her own
// registration ID and still withholds it from bob. Every piece of
// #384 that a restart could quietly undo — the ownership record, the
// redaction that depends on it — is exercised through the handler
// that enforces it.
func TestPeerState_HubRestartsBehindTheHandlers(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	alice := auth.Caller{Identity: "alice"}
	bob := auth.Caller{Identity: "bob"}

	idsFor := func(mux *http.ServeMux, c auth.Caller) map[string]string {
		t.Helper()
		rec := doPeerReq(t, mux, c, http.MethodGet, "/peers", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /peers as %s = %d: %s", c.Identity, rec.Code, rec.Body.String())
		}
		var body struct {
			Peers []Peer `json:"peers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode /peers: %v", err)
		}
		out := make(map[string]string, len(body.Peers))
		for _, p := range body.Peers {
			out[p.Name] = p.RegistrationID
		}
		return out
	}

	// The hub, alive.
	first := mustReopen(t, path)
	firstMux := peerHubMux(first)
	req, _ := json.Marshal(RegisterRequest{Name: "cluster-a", Endpoint: "https://10.0.0.1:7777"})
	rec := doPeerReq(t, firstMux, alice, http.MethodPost, "/peers", string(req))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /peers = %d: %s", rec.Code, rec.Body.String())
	}
	var registered Peer
	if err := json.Unmarshal(rec.Body.Bytes(), &registered); err != nil {
		t.Fatalf("decode register response: %v", err)
	}

	// The hub, gone. Nothing of it survives but the file — neither
	// first nor firstMux is touched again below.
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The hub, restarted — a different registry, different handlers.
	second := mustReopen(t, path)
	secondMux := peerHubMux(second)

	if got := idsFor(secondMux, alice)["cluster-a"]; got != registered.RegistrationID {
		t.Errorf("owner's view of registration_id after restart = %q, want %q", got, registered.RegistrationID)
	}
	if got, ok := idsFor(secondMux, bob)["cluster-a"]; !ok {
		t.Error("bob cannot see the peer at all after a restart; discovery is not owner-scoped, only the registration id is")
	} else if got != "" {
		t.Errorf("non-owner sees registration_id %q after a restart: #384 redaction did not survive", got)
	}

	// And the capability still works for the owner: the ID reloaded
	// from disk is the live one, not a stale string.
	del := doPeerReq(t, secondMux, alice, http.MethodDelete, "/peers/"+registered.RegistrationID, "")
	if del.Code != http.StatusNoContent {
		t.Fatalf("DELETE /peers/{id} as owner after restart = %d: %s", del.Code, del.Body.String())
	}
	if second.Len() != 0 {
		t.Errorf("peer count after deregister = %d, want 0", second.Len())
	}
}

// TestPeerState_PreservesOwner is the security-relevant one, and the
// reason the on-disk record is a separate struct from Peer.
//
// Peer.Owner is `json:"-"` — it's hub-side authorization state that
// discovery responses deliberately withhold. Persist Peer directly
// (the obvious implementation) and every registration reloads
// ownerless, so canManage collapses to `c.Admin || c.Identity == ""`:
// the real owner loses control of its own registration, and in
// single-user mode (empty identity) every caller gains it. A restart
// would silently undo the #384 enumerate-then-delete hardening.
//
// Fails on pre-fix code: swap peerRecord for Peer in recordOf and
// this reports the owner as unable to manage its own peer.
func TestPeerState_PreservesOwner(t *testing.T) {
	t.Parallel()
	path := statePath(t)

	first := mustReopen(t, path)
	p := mustRegisterOwned(t, first, "cluster-a", "https://10.0.0.1:7777", "ops@example.com", 120)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := mustReopen(t, path)
	reloaded, ok := second.Lookup(p.RegistrationID)
	if !ok {
		t.Fatalf("peer did not survive restart")
	}
	if reloaded.Owner != "ops@example.com" {
		t.Fatalf("Owner after restart = %q, want ops@example.com", reloaded.Owner)
	}
	if !canManage(auth.Caller{Identity: "ops@example.com"}, reloaded) {
		t.Error("owner cannot manage its own registration after a restart")
	}
	if canManage(auth.Caller{Identity: "intruder@example.com"}, reloaded) {
		t.Error("a non-owner can manage the registration after a restart")
	}
	// The single-user posture is where a dropped owner is worst: an
	// empty identity would match an empty Owner and hand every caller
	// the deregistration capability.
	if canManage(auth.Caller{Identity: ""}, reloaded) {
		t.Error("an anonymous caller can manage the registration after a restart")
	}
}

// TestPeerState_LeaseIsReclampedToCurrentConfig is mast's addition to
// the port, and the question #180 sent whoever took it to ask: does a
// registration that outlives the process also outlive a config change
// that would no longer admit it?
//
// It must not. A lease is a grant — "count this peer as live until T"
// — and #166 established for budget grants that replaying one against
// a config that no longer supports it is arithmetic on a number that
// stopped meaning anything. Lower the ceiling from five minutes to
// thirty seconds and a reloaded five-minute lease would be honored
// twice over: once because its expiry is still far out, and then
// indefinitely, because Heartbeat re-derives the TTL from the reloaded
// lease and renews at the ceiling the operator just removed.
//
// Fails on pre-fix code: drop the clamp in loadPeerState and the
// peer comes back holding the old grant, then keeps it.
func TestPeerState_LeaseIsReclampedToCurrentConfig(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	now, advance := clockFn(time.Now())

	// Registered under a generous ceiling.
	first := mustReopen(t, path, withClock(now), WithMaxTTL(5*time.Minute))
	p := mustRegisterOwned(t, first, "cluster-a", "https://10.0.0.1:7777", "ops@example.com", 300)
	if got := p.LeaseExpiresAt.Sub(p.LastHeartbeat); got != 5*time.Minute {
		t.Fatalf("granted lease = %s, want 5m", got)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Restarted under a tighter one.
	advance(10 * time.Second)
	second := mustReopen(t, path, withClock(now), WithMaxTTL(30*time.Second))
	reloaded, ok := second.Lookup(p.RegistrationID)
	if !ok {
		t.Fatalf("peer did not survive restart")
	}
	if got := reloaded.LeaseExpiresAt.Sub(reloaded.LastHeartbeat); got != 30*time.Second {
		t.Errorf("reloaded lease = %s, want it clamped to the running config's 30s", got)
	}

	// And the clamp is not a one-shot cosmetic: the renewal derives
	// from it, so the old ceiling cannot walk back in on the next
	// heartbeat.
	hb, err := second.Heartbeat(p.RegistrationID)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := hb.LeaseExpiresAt.Sub(hb.LastHeartbeat); got != 30*time.Second {
		t.Errorf("renewed lease = %s, want 30s — the pre-restart ceiling came back", got)
	}
}

// TestPeerState_ClampIsDurable: narrowing a lease on load has to reach
// the file, or the next boot reads the original width again and the
// clamp is only ever as good as the process that applied it.
func TestPeerState_ClampIsDurable(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	now, _ := clockFn(time.Now())

	first := mustReopen(t, path, withClock(now), WithMaxTTL(5*time.Minute))
	p := mustRegisterOwned(t, first, "cluster-a", "https://10.0.0.1:7777", "ops@example.com", 300)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := mustReopen(t, path, withClock(now), WithMaxTTL(30*time.Second))
	if err := second.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Third boot, ceiling raised again. The lease must still be the
	// clamped one — the grant is gone, not merely hidden.
	third := mustReopen(t, path, withClock(now), WithMaxTTL(5*time.Minute))
	reloaded, ok := third.Lookup(p.RegistrationID)
	if !ok {
		t.Fatalf("peer did not survive restart")
	}
	if got := reloaded.LeaseExpiresAt.Sub(reloaded.LastHeartbeat); got != 30*time.Second {
		t.Errorf("lease after clamp-then-reopen = %s, want the clamped 30s to have been written down", got)
	}
}

// TestPeerState_ClampCanExpireAPeer: the clamp runs before the
// expired-lease drop, so a peer that is only still live under the old
// ceiling is not live at all. A hub silent for four minutes, reloaded
// under a thirty-second ceiling, is a dead fleet member — reporting it
// as present because of a grant the config withdrew is the failure the
// clamp exists to prevent.
func TestPeerState_ClampCanExpireAPeer(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	now, advance := clockFn(time.Now())

	first := mustReopen(t, path, withClock(now), WithMaxTTL(5*time.Minute))
	mustRegisterOwned(t, first, "quiet", "https://10.0.0.1:7777", "ops@example.com", 300)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	advance(4 * time.Minute)
	second := mustReopen(t, path, withClock(now), WithMaxTTL(30*time.Second))
	if got := second.Len(); got != 0 {
		t.Errorf("peers after restart = %d, want 0 — a 4m-silent peer is not live under a 30s ceiling", got)
	}
}

// TestPeerState_HeartbeatsArePersisted guards the reason heartbeats
// write to disk at all. Reload drops expired leases, so a file that
// only recorded the original registration would discard exactly the
// peers that have been alive longest.
//
// Fails on pre-fix code: drop the snapshot from Heartbeat and the
// reloaded registry is empty.
func TestPeerState_HeartbeatsArePersisted(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	now, advance := clockFn(time.Now())

	first := mustReopen(t, path, withClock(now))
	p := mustRegisterOwned(t, first, "cluster-a", "https://10.0.0.1:7777", "ops@example.com", 60)
	advance(45 * time.Second)
	if _, err := first.Heartbeat(p.RegistrationID); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// t+70s: past the original lease, inside the extended one.
	advance(25 * time.Second)
	second := mustReopen(t, path, withClock(now))
	if got := second.Len(); got != 1 {
		t.Fatalf("peers after restart = %d, want 1 (the heartbeat-extended lease)", got)
	}
}

// TestPeerState_ExpiredLeasesAreNotResurrected: a peer that stopped
// heartbeating before the hub came back is stale by definition, and
// reporting it as live — even for the few seconds until the prune
// loop ticks — is a worse answer than not reporting it.
func TestPeerState_ExpiredLeasesAreNotResurrected(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	now, advance := clockFn(time.Now())

	first := mustReopen(t, path, withClock(now))
	mustRegisterOwned(t, first, "gone", "https://10.0.0.1:7777", "ops@example.com", 30)
	mustRegisterOwned(t, first, "alive", "https://10.0.0.2:7777", "ops@example.com", 300)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	advance(60 * time.Second)
	second := mustReopen(t, path, withClock(now))
	peers := second.List(nil)
	if len(peers) != 1 || peers[0].Name != "alive" {
		t.Fatalf("peers after restart = %v, want only [alive]", peerNames(peers))
	}
	// The expired entry should also be gone from the file, not just
	// filtered on read — otherwise it lingers forever.
	if body := readState(t, path); strings.Contains(body, `"gone"`) {
		t.Errorf("expired peer still in state file:\n%s", body)
	}
}

// TestPeerState_DeregisterAndPrunePersist: removals have to reach the
// file too, or a restart resurrects a peer the operator deleted.
func TestPeerState_DeregisterAndPrunePersist(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	now, advance := clockFn(time.Now())

	first := mustReopen(t, path, withClock(now))
	p := mustRegisterOwned(t, first, "deleted", "https://10.0.0.1:7777", "ops@example.com", 300)
	mustRegisterOwned(t, first, "pruned", "https://10.0.0.2:7777", "ops@example.com", 30)
	mustRegisterOwned(t, first, "kept", "https://10.0.0.3:7777", "ops@example.com", 300)
	first.Deregister(p.RegistrationID)
	advance(60 * time.Second)
	if got := first.Prune(); got != 1 {
		t.Fatalf("Prune removed %d, want 1", got)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body := readState(t, path)
	if strings.Contains(body, `"deleted"`) || strings.Contains(body, `"pruned"`) {
		t.Errorf("removed peers still in state file:\n%s", body)
	}
	if !strings.Contains(body, `"kept"`) {
		t.Errorf("surviving peer missing from state file:\n%s", body)
	}
}

// TestPeerState_FileIsOwnerOnly: the file holds registration IDs, and
// a registration ID is the capability to deregister the peer. Group-
// or world-readable state on a shared volume hands that out.
func TestPeerState_FileIsOwnerOnly(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	r := mustReopen(t, path)
	mustRegisterOwned(t, r, "cluster-a", "https://10.0.0.1:7777", "ops@example.com", 120)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != peerStateFileMode {
		t.Errorf("state file mode = %v, want %v (it contains deregistration capabilities)", got, peerStateFileMode)
	}
}

// TestPeerState_HandEditedRecordsAreRevalidated: the file is operator-
// writable and outlives any single binary, so it is untrusted input
// in the same way a request body is. A javascript:/file: endpoint must
// not reach an operator UI that will dial it with operator credentials
// (#384) just because it arrived via disk instead of via POST.
//
// The last two rows are mast's addition: a lease with no length is
// not a lease, because a length is what the reload re-clamps. Neither
// row would survive even without its guard — the clamp collapses a
// lengthless lease to one that has already expired — so what the
// guards buy is a stated reason in the log instead of a peer that
// silently isn't there. This test pins that they don't load; the
// reason they don't is in loadPeerState.
func TestPeerState_HandEditedRecordsAreRevalidated(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	beat := time.Now().Format(time.RFC3339Nano)
	future := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	writeState(t, path, strings.Join([]string{
		`{"registration_id":"reg-1","name":"good","endpoint":"https://10.0.0.1:7777","last_heartbeat":"` + beat + `","lease_expires_at":"` + future + `"}`,
		`{"registration_id":"reg-2","name":"hostile","endpoint":"javascript:alert(1)","last_heartbeat":"` + beat + `","lease_expires_at":"` + future + `"}`,
		`{"registration_id":"reg-3","name":"relative","endpoint":"/peers","last_heartbeat":"` + beat + `","lease_expires_at":"` + future + `"}`,
		`{"registration_id":"","name":"anonymous","endpoint":"https://10.0.0.4:7777","last_heartbeat":"` + beat + `","lease_expires_at":"` + future + `"}`,
		`{"registration_id":"reg-5","name":"no-heartbeat","endpoint":"https://10.0.0.5:7777","lease_expires_at":"` + future + `"}`,
		`{"registration_id":"reg-6","name":"backwards","endpoint":"https://10.0.0.6:7777","last_heartbeat":"` + future + `","lease_expires_at":"` + beat + `"}`,
	}, "\n"))

	r := mustReopen(t, path)
	got := peerNames(r.List(nil))
	if len(got) != 1 || got[0] != "good" {
		t.Errorf("loaded peers = %v, want only [good]", got)
	}
}

// TestPeerState_MalformedLineIsSkippedNotFatal: temp+rename means we
// never write a partial file, so a bad line is external damage. The
// peers it described re-register within a heartbeat; refusing to boot
// would turn a recoverable degradation into an outage.
func TestPeerState_MalformedLineIsSkippedNotFatal(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	beat := time.Now().Format(time.RFC3339Nano)
	future := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	writeState(t, path, strings.Join([]string{
		`{"registration_id":"reg-1","name":"before","endpoint":"https://10.0.0.1:7777","last_heartbeat":"` + beat + `","lease_expires_at":"` + future + `"}`,
		`{"registration_id":"reg-2","name":"trunc`,
		``,
		`not json at all`,
		`{"registration_id":"reg-3","name":"after","endpoint":"https://10.0.0.3:7777","last_heartbeat":"` + beat + `","lease_expires_at":"` + future + `"}`,
	}, "\n"))

	r := mustReopen(t, path)
	got := peerNames(r.List(nil))
	if len(got) != 2 || got[0] != "after" || got[1] != "before" {
		t.Errorf("loaded peers = %v, want [after before] — good records around a bad line must survive", got)
	}
}

// TestPeerState_DuplicateRegistrationIDsAreRejected: the registry
// keys byID on the registration ID, so loading two records that share
// one leaves byName pointing at a peer byID no longer holds — a split
// view where Len() and List() disagree. Only a hand-edited file can
// produce it; first-line-wins.
func TestPeerState_DuplicateRegistrationIDsAreRejected(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	beat := time.Now().Format(time.RFC3339Nano)
	future := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	writeState(t, path, strings.Join([]string{
		`{"registration_id":"reg-dup","name":"first","endpoint":"https://10.0.0.1:7777","last_heartbeat":"` + beat + `","lease_expires_at":"` + future + `"}`,
		`{"registration_id":"reg-dup","name":"second","endpoint":"https://10.0.0.2:7777","last_heartbeat":"` + beat + `","lease_expires_at":"` + future + `"}`,
	}, "\n"))

	r := mustReopen(t, path)
	if got, want := r.Len(), 1; got != want {
		t.Fatalf("Len = %d, want %d", got, want)
	}
	if got := peerNames(r.List(nil)); len(got) != 1 || got[0] != "first" {
		t.Errorf("loaded peers = %v, want [first]", got)
	}
}

// TestPeerState_RepeatedFailuresLogOnce: a broken volume fails once
// per heartbeat per peer, forever. Logging every failure buries the
// transition under thousands of copies of itself, so only the first
// failure and the recovery are reported.
func TestPeerState_RepeatedFailuresLogOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := &peerPersister{path: filepath.Join(dir, "missing-dir", "peers.jsonl")}

	var logged []string
	note := func() {
		seq := p.written + 1
		if msg := p.noteResult(p.write(peerSnapshot{seq: seq})); msg != "" {
			logged = append(logged, msg)
		}
	}
	note()
	note()
	note()
	// Same persister, now a writable path.
	p.path = filepath.Join(dir, "peers.jsonl")
	note()

	if len(logged) != 2 {
		t.Fatalf("logged %d lines, want 2 (one failure, one recovery):\n%v", len(logged), logged)
	}
	if !strings.Contains(logged[0], "durability degraded") {
		t.Errorf("first line = %q, want the failure notice", logged[0])
	}
	if !strings.Contains(logged[1], "writes recovered") {
		t.Errorf("second line = %q, want the recovery notice", logged[1])
	}
}

// TestPeerState_UnreadableFileIsFatal: the operator asked for
// durability. Coming up empty and pretending otherwise is the exact
// claimed-but-unenforced failure this sweep is closing, so an
// unreadable file fails the boot instead. A directory stands in for
// "can't be read" because it fails the same way for root, which CI
// often is; a chmod-000 file would not.
func TestPeerState_UnreadableFileIsFatal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := NewPeerRegistryWithState(dir); err == nil {
		t.Fatal("NewPeerRegistryWithState on an unreadable path returned no error")
	}
}

// TestPeerState_UnwritableDirIsFatalAtStartup: the first snapshot is
// written eagerly so a bad volume mount surfaces at boot, not at the
// first registration hours later where the only witness is a log line.
func TestPeerState_UnwritableDirIsFatalAtStartup(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "no-such-dir", "peers.jsonl")
	if _, err := NewPeerRegistryWithState(path); err == nil {
		t.Fatal("NewPeerRegistryWithState with a missing parent directory returned no error")
	}
}

func TestPeerState_EmptyPathIsError(t *testing.T) {
	t.Parallel()
	if _, err := NewPeerRegistryWithState(""); err == nil {
		t.Fatal("NewPeerRegistryWithState(\"\") returned no error")
	}
}

// TestPeerState_MissingFileIsAFirstStart: a hub's very first boot has
// no state file, and that is not a failure.
func TestPeerState_MissingFileIsAFirstStart(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	r := mustReopen(t, path)
	if r.Len() != 0 {
		t.Errorf("fresh registry has %d peers", r.Len())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("state file not created on first start: %v", err)
	}
}

// TestPeerState_NoTempFilesLeftBehind: the temp+rename dance must not
// litter the volume, including on the paths where the write is
// skipped as stale.
func TestPeerState_NoTempFilesLeftBehind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.jsonl")
	r := mustReopen(t, path)
	for i := range 5 {
		p := mustRegisterOwned(t, r, "peer-"+string(rune('a'+i)), "https://10.0.0.1:7777", "ops@example.com", 120)
		if _, err := r.Heartbeat(p.RegistrationID); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "peers.jsonl" {
			t.Errorf("leftover file in state dir: %s", e.Name())
		}
	}
}

// TestPeerState_StaleSnapshotNeverOverwritesNewer pins the ordering
// rule directly. Snapshots are stamped under the registry's write
// lock; the persister drops any stamp it has already passed. Without
// that, two concurrent mutations could land out of order and leave
// the file describing a state that never existed.
func TestPeerState_StaleSnapshotNeverOverwritesNewer(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	p := &peerPersister{path: path}

	newer := peerSnapshot{seq: 7, records: []peerRecord{{
		RegistrationID: "reg-new", Name: "new",
		Endpoint: "https://10.0.0.2:7777", LeaseExpiresAt: time.Now().Add(time.Hour),
	}}}
	older := peerSnapshot{seq: 3, records: []peerRecord{{
		RegistrationID: "reg-old", Name: "old",
		Endpoint: "https://10.0.0.1:7777", LeaseExpiresAt: time.Now().Add(time.Hour),
	}}}
	if err := p.write(newer); err != nil {
		t.Fatalf("write newer: %v", err)
	}
	if err := p.write(older); err != nil {
		t.Fatalf("write older: %v", err)
	}
	if body := readState(t, path); !strings.Contains(body, `"new"`) || strings.Contains(body, `"old"`) {
		t.Errorf("stale snapshot overwrote the newer one:\n%s", body)
	}
}

// TestPeerState_InMemoryRegistryWritesNothing: persistence is opt-in,
// and the default hub must not touch the filesystem.
func TestPeerState_InMemoryRegistryWritesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := NewPeerRegistry()
	defer func() { _ = r.Close() }()
	mustRegisterOwned(t, r, "cluster-a", "https://10.0.0.1:7777", "ops@example.com", 120)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("in-memory registry wrote %d files", len(entries))
	}
}

func readState(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return string(b)
}

func writeState(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body+"\n"), peerStateFileMode); err != nil {
		t.Fatalf("write state: %v", err)
	}
}
