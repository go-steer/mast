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

package vertexcache

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/genai"
)

// expiredUpdateText is what Vertex returns to Caches.Update once the
// cache's TTL has elapsed. Reconstructed from #325's description rather
// than captured from a transcript — the load-bearing part is the
// phrase, and internal/vertexcacheerr pins that separately.
// (Held as a const and wrapped at use, rather than a package-level
// errors.New: ST1005 reads the literal and this one is a provider's
// capitalized sentence, which is the point of the fixture.)
const expiredUpdateText = "Error 400, Message: The cache content projects/p/locations/l/cachedContents/abc has expired., Status: INVALID_ARGUMENT, Details: []"

func expiredUpdateErr() error { return errors.New(expiredUpdateText) }

func (f *fakeCaches) setNextCacheName(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextCacheNameOnce = name
}

func (f *fakeCaches) setUpdateErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateErr = err
}

// TestRefresh_ExpiredCacheStopsHandingOutTheName is the #325 end-to-end
// case, in manager terms: TTL elapses, the refresh Update comes back
// saying the cache is gone, and from that point the manager must stop
// claiming to hold one.
//
// The pre-fix failure is the assertion below that Name() goes empty.
// doRefresh used to log and return on any Update error, on the premise
// that the handle stays valid until it expires — but this error IS the
// expiry, so Name() kept stamping a dead cache onto every subsequent
// request. Each of those came back a 400 naming a config error, and
// nothing in the process ever reconsidered: the session was down until
// the daemon restarted.
//
// The recovery half matters as much as the stop: going uncached is
// safe, staying uncached for the daemon's lifetime is the failure the
// eviction state exists to avoid, so the next Init must create again.
func TestRefresh_ExpiredCacheStopsHandingOutTheName(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{
		// Near-expiry on Create so the first Name() schedules a refresh,
		// which is how a long-lived daemon reaches this path for real.
		ttlOverride:       500 * time.Millisecond,
		updateErr:         expiredUpdateErr(),
		nextCacheNameOnce: "projects/p/locations/l/cachedContents/abc",
	}
	m := NewManager(fake, "gemini-2.5-flash", Options{
		TTL:              time.Hour,
		RefreshThreshold: 30 * time.Minute,
		Logger:           discardLogger(),
	})
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, testWait, func() bool { return m.Snapshot().Active })

	// The turn that trips the refresh still gets the name: at this point
	// the cache is near-expiry, not known-gone, and the design contract
	// is that the triggering call is not penalised.
	if got := m.Name(context.Background()); got != "projects/p/locations/l/cachedContents/abc" {
		t.Fatalf("Name at the refresh trigger = %q, want the active cache name", got)
	}
	waitFor(t, testWait, func() bool { return fake.updateCount.Load() >= 1 })

	// The refresh has now told us the cache is gone.
	waitFor(t, testWait, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.refreshing
	})
	if m.Snapshot().Active {
		t.Error("Active = true after Update reported the cache expired; the manager still believes it holds a cache that is gone (#325)")
	}
	if got := m.Name(context.Background()); got != "" {
		t.Errorf("Name after an expired-cache refresh = %q, want empty; "+
			"a dead cache name on the next request is a 400 that names the wrong cause (#325)", got)
	}

	// And the manager is back in its pre-Init state, so the next turn
	// re-creates rather than running uncached forever.
	fake.setUpdateErr(nil)
	fake.setNextCacheName("projects/p/locations/l/cachedContents/def")
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, testWait, func() bool { return m.Snapshot().Active })
	if got := m.Name(context.Background()); got != "projects/p/locations/l/cachedContents/def" {
		t.Errorf("Name after re-Init = %q, want the freshly created cache", got)
	}
	if got := fake.createCount.Load(); got != 2 {
		t.Errorf("createCount = %d, want 2 (eviction must leave the manager able to create again)", got)
	}
}

// TestRefresh_TransientUpdateFailureKeepsTheHandle is the other side of
// the same branch, and the reason the verdict is delegated to
// internal/vertexcacheerr instead of being "Update failed."
//
// A throttle, a deadline, a blip: none of these are evidence about
// whether the cache exists. Dropping the handle on one of them would
// trade a hard failure for a silent cost regression — the daemon would
// re-Create caches it already had and pay full input price in between.
func TestRefresh_TransientUpdateFailureKeepsTheHandle(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{
		ttlOverride:       500 * time.Millisecond,
		updateErr:         errors.New("rpc error: code = Unavailable desc = the service is currently unavailable"),
		nextCacheNameOnce: "projects/p/locations/l/cachedContents/abc",
	}
	m := NewManager(fake, "gemini-2.5-flash", Options{
		TTL:              time.Hour,
		RefreshThreshold: 30 * time.Minute,
		Logger:           discardLogger(),
	})
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, testWait, func() bool { return m.Snapshot().Active })

	_ = m.Name(context.Background())
	waitFor(t, testWait, func() bool { return fake.updateCount.Load() >= 1 })
	waitFor(t, testWait, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.refreshing
	})

	if !m.Snapshot().Active {
		t.Error("Active = false after a transient Update failure; an unavailable backend is not evidence the cache is gone")
	}
	if got := m.Name(context.Background()); got != "projects/p/locations/l/cachedContents/abc" {
		t.Errorf("Name after a transient Update failure = %q, want the cache we still hold", got)
	}
}

// TestRefresh_GoneVerdictDoesNotOutliveItsCache pins the staleness
// guard on the new eviction path, the mirror of the one the success
// path has had since #372: a verdict about the cache we refreshed must
// not be applied to whichever cache replaced it while the RPC was in
// flight. Without the guard, a slow Update against a dead cache would
// evict a healthy successor and the daemon would churn Create calls.
func TestRefresh_GoneVerdictDoesNotOutliveItsCache(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{
		ttlOverride:       500 * time.Millisecond,
		updateErr:         expiredUpdateErr(),
		updateStarted:     make(chan struct{}, 1),
		updateRelease:     make(chan struct{}),
		nextCacheNameOnce: "projects/p/locations/l/cachedContents/abc",
	}
	m := NewManager(fake, "gemini-2.5-flash", Options{
		TTL:              time.Hour,
		RefreshThreshold: 30 * time.Minute,
		Logger:           discardLogger(),
	})
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, testWait, func() bool { return m.Snapshot().Active })

	// Hold the refresh's Update RPC in flight.
	_ = m.Name(context.Background())
	select {
	case <-fake.updateStarted:
	case <-time.After(testWait):
		t.Fatal("refresh Update RPC never started")
	}

	// Meanwhile the gemini wrapper sees the eviction on a GenerateContent
	// and the next turn creates a replacement — the ordinary recovery,
	// racing the refresh that is about to report on the old cache.
	m.MarkEvicted("simulated eviction seen on a turn")
	fake.setNextCacheName("projects/p/locations/l/cachedContents/def")
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, testWait, func() bool {
		return m.Snapshot().CacheName == "projects/p/locations/l/cachedContents/def"
	})

	// Now let the stale verdict land.
	close(fake.updateRelease)
	waitFor(t, testWait, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.refreshing
	})

	snap := m.Snapshot()
	if !snap.Active {
		t.Error("Active = false; a gone-verdict about the previous cache evicted its replacement")
	}
	if snap.CacheName != "projects/p/locations/l/cachedContents/def" {
		t.Errorf("CacheName = %q, want the replacement cache to survive the stale verdict", snap.CacheName)
	}
}
