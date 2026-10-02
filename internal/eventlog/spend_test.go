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

package eventlog

import (
	"context"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// openSpendStore returns a store over a fresh on-disk SQLite database
// plus the handle, so a test can reopen it the way a restarted daemon
// does. On-disk for the same reason the guardrail tests are: the claim
// is about what the *next* process reads.
func openSpendStore(t *testing.T) (*SpendStore, *gorm.DB) {
	t.Helper()
	h, err := Open(context.Background(), sqlite.Open(filepath.Join(t.TempDir(), "eventlog.db")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	s, err := NewSpendStore(context.Background(), h.DB)
	if err != nil {
		t.Fatalf("NewSpendStore: %v", err)
	}
	return s, h.DB
}

// sameUSD compares dollars the way they accumulate — by summing floats.
func sameUSD(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

func TestSpendStoreFoldsBySessionAndAuthor(t *testing.T) {
	ctx := context.Background()
	s, _ := openSpendStore(t)

	// The overwhelmingly common case: a session this process started, with
	// nothing on the books. It has to be a clean zero, not an error.
	st, err := s.Fold(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("Fold on an empty ledger: %v", err)
	}
	if st.Session != (SpendTotals{}) || len(st.ByAuthor) != 0 || st.Unpriced != 0 {
		t.Fatalf("an empty ledger folded to %+v, want the zero value", st)
	}

	at := time.Date(2026, 8, 18, 14, 0, 0, 0, time.UTC)
	rows := []SpendRecord{
		{Author: "coordinator", Tokens: 1_200, CostUSD: 0.06, At: at},
		{Author: "OOMKilled", Tokens: 800, CostUSD: 0.04, At: at.Add(time.Second)},
		{Author: "coordinator", Tokens: 400, CostUSD: 0.02, At: at.Add(2 * time.Second)},
		// A call the catalog could not price still consumed tokens, and a
		// restored total has to be labelled the same approximation a live
		// one would be.
		{Author: "OOMKilled", Tokens: 600, CostUSD: 0.03, Unpriced: true, At: at.Add(3 * time.Second)},
	}
	for i, r := range rows {
		if err := s.Append(ctx, gApp, gUser, gSID, r); err != nil {
			t.Fatalf("Append row %d: %v", i, err)
		}
	}

	st, err = s.Fold(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if st.Session.Tokens != 3_000 || st.Session.Calls != 4 || !sameUSD(st.Session.CostUSD, 0.15) {
		t.Errorf("session total = %+v, want 3000 tokens / $0.1500 / 4 calls", st.Session)
	}
	if st.Unpriced != 1 {
		t.Errorf("unpriced = %d, want 1", st.Unpriced)
	}
	// Per-author, because a specialist's own ceiling has to come back too
	// and the author is the only key that can be re-attributed against a
	// roster the next process might have edited.
	coord := st.ByAuthor["coordinator"]
	if coord.Tokens != 1_600 || coord.Calls != 2 || !sameUSD(coord.CostUSD, 0.08) {
		t.Errorf("coordinator = %+v, want 1600 tokens / $0.0800 / 2 calls", coord)
	}
	spec := st.ByAuthor["OOMKilled"]
	if spec.Tokens != 1_400 || spec.Calls != 2 || !sameUSD(spec.CostUSD, 0.07) {
		t.Errorf("OOMKilled = %+v, want 1400 tokens / $0.0700 / 2 calls", spec)
	}
}

// The whole point of the table: a second store over the same database
// reads what the first one wrote, and reads only that session's rows.
func TestSpendStoreSurvivesAReopenAndScopesBySession(t *testing.T) {
	ctx := context.Background()
	s, db := openSpendStore(t)

	if err := s.Append(ctx, gApp, gUser, gSID, SpendRecord{Author: "coordinator", Tokens: 1_000, CostUSD: 0.05}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Neighbours on every axis the composite index keys on. A fold that
	// picked any of these up would charge a session for a bill that is
	// not its own.
	for _, n := range []struct{ app, user, sid string }{
		{gApp, gUser, "other-session"},
		{gApp, "someone-else", gSID},
		{"other-app", gUser, gSID},
	} {
		if err := s.Append(ctx, n.app, n.user, n.sid, SpendRecord{Author: "coordinator", Tokens: 9_000, CostUSD: 9.99}); err != nil {
			t.Fatalf("Append neighbour %+v: %v", n, err)
		}
	}

	reopened, err := NewSpendStore(ctx, db)
	if err != nil {
		t.Fatalf("NewSpendStore on an existing database: %v", err)
	}
	st, err := reopened.Fold(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if st.Session.Calls != 1 || st.Session.Tokens != 1_000 || !sameUSD(st.Session.CostUSD, 0.05) {
		t.Fatalf("after reopen: %+v, want the one row this session wrote (1000 tokens / $0.0500)", st.Session)
	}
}

// History is the per-call trail behind the total — for the operator
// asking where the money went, not how much of it there was.
func TestSpendStoreHistoryIsOldestFirst(t *testing.T) {
	ctx := context.Background()
	s, _ := openSpendStore(t)

	at := time.Date(2026, 8, 18, 14, 0, 0, 0, time.UTC)
	for i, tok := range []int64{100, 200, 300} {
		if err := s.Append(ctx, gApp, gUser, gSID, SpendRecord{
			Author: "coordinator", Tokens: tok, CostUSD: float64(tok) / 20_000,
			At: at.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	hist, err := s.History(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("History returned %d rows, want 3", len(hist))
	}
	for i, want := range []int64{100, 200, 300} {
		if hist[i].Tokens != want {
			t.Errorf("row %d = %d tokens, want %d (History must be oldest-first)", i, hist[i].Tokens, want)
		}
	}
	if !hist[0].At.Equal(at) {
		t.Errorf("row 0 At = %s, want the recorded %s", hist[0].At, at)
	}
}

// A nil store is the "no --attach-listen, so no durable connection"
// case at the call sites. It must be inert, not a panic and not an
// error a caller has to branch on.
func TestNilSpendStoreIsInert(t *testing.T) {
	ctx := context.Background()
	var s *SpendStore

	if err := s.Append(ctx, gApp, gUser, gSID, SpendRecord{Tokens: 10, CostUSD: 1}); err != nil {
		t.Errorf("Append on a nil store: %v", err)
	}
	st, err := s.Fold(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Errorf("Fold on a nil store: %v", err)
	}
	if st.Session != (SpendTotals{}) {
		t.Errorf("Fold on a nil store returned %+v, want the zero value", st.Session)
	}
	hist, err := s.History(ctx, gApp, gUser, gSID)
	if err != nil || hist != nil {
		t.Errorf("History on a nil store = %v, %v; want nil, nil", hist, err)
	}
}

// Append is called from the meter's spend hook, which runs on whatever
// goroutine made the model call. Concurrent appends must not lose rows
// or interleave into a wrong total.
func TestSpendStoreAppendIsConcurrencySafe(t *testing.T) {
	ctx := context.Background()
	s, _ := openSpendStore(t)

	const writers = 8
	const each = 5
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := s.Append(ctx, gApp, gUser, gSID, SpendRecord{
					Author: "coordinator", Tokens: 100, CostUSD: 0.005,
				}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Append: %v", err)
	}

	st, err := s.Fold(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if st.Session.Calls != writers*each || st.Session.Tokens != int64(writers*each*100) {
		t.Errorf("folded %+v, want %d calls / %d tokens", st.Session, writers*each, writers*each*100)
	}
}

// A row with no session has nowhere to be folded back to, so it is
// refused at the door rather than written where nothing will read it.
func TestSpendStoreAppendRequiresAppAndSession(t *testing.T) {
	ctx := context.Background()
	s, _ := openSpendStore(t)

	if err := s.Append(ctx, gApp, gUser, "", SpendRecord{Tokens: 1}); err == nil {
		t.Error("Append with no session was accepted")
	}
	if err := s.Append(ctx, "", gUser, gSID, SpendRecord{Tokens: 1}); err == nil {
		t.Error("Append with no app was accepted")
	}
}
