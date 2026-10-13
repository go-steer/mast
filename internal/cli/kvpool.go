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

package cli

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/go-steer/core-models/kvmetrics"

	"github.com/go-steer/mast/internal/attach"
)

// kvPollInterval is how often a turn's window is sampled between its
// opening and closing scrapes, so the peak KV utilization has readings
// to come from. A turn shorter than one interval gets its two endpoints.
const kvPollInterval = 5 * time.Second

// kvStopTimeout bounds a turn's closing scrape. It runs after the turn
// has returned, so it can never delay one; the bound only keeps a dead
// endpoint from holding a goroutine.
const kvStopTimeout = 15 * time.Second

// kvPool samples a self-hosted model server's KV cache around each turn
// (core-models kvmetrics) and keeps, per session, the last turn's window
// and a fold of all of them, for GET /sessions/{id}/usage.
//
// Fleet-level throughout: the server's prefix cache across all of its
// traffic while the turn ran. It is reported beside usage and never
// folded into cost, tokens or a budget.
//
// A nil *kvPool samples nothing; every method is nil-safe, which is the
// pool a daemon gets when its model is not a profile with a metrics
// endpoint. Sampling never fails, blocks or slows a turn: the opening
// scrape runs in the background, the closing one after the turn has
// returned, and a failure is logged once per session.
type kvPool struct {
	sampler  *kvmetrics.Sampler
	interval time.Duration
	logger   *slog.Logger

	mu       sync.Mutex
	sessions map[string]*kvSession
	inflight sync.WaitGroup
}

type kvSession struct {
	last   *kvmetrics.Window
	fold   attach.KVWindow
	turns  int
	warned bool
}

func newKVPool(s *kvmetrics.Sampler, logger *slog.Logger) *kvPool {
	if s == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &kvPool{sampler: s, interval: kvPollInterval, logger: logger, sessions: map[string]*kvSession{}}
}

// kvTurn is one turn's sampling, started by begin and finished by end.
type kvTurn struct {
	done chan struct{}
	run  *kvmetrics.Run
	err  error
}

// begin starts sampling for a turn without waiting for the opening
// scrape. The scrape's context is detached from the turn's: the turn
// ending does not cut the window short, end does.
func (p *kvPool) begin(ctx context.Context) *kvTurn {
	if p == nil {
		return nil
	}
	t := &kvTurn{done: make(chan struct{})}
	p.inflight.Add(1)
	go func() {
		defer close(t.done)
		t.run, t.err = p.sampler.Start(context.WithoutCancel(ctx), p.interval)
	}()
	return t
}

// end takes the turn's closing scrape and records the window, in the
// background, after the turn has returned.
func (p *kvPool) end(sessionID string, t *kvTurn) {
	if p == nil || t == nil {
		return
	}
	go func() {
		defer p.inflight.Done()
		<-t.done
		if t.err != nil {
			p.warnOnce(sessionID, "opening scrape failed", t.err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), kvStopTimeout)
		defer cancel()
		w, err := t.run.Stop(ctx)
		if err != nil {
			p.warnOnce(sessionID, "closing scrape failed", err)
			if w.Readings == 0 {
				return
			}
		}
		p.record(sessionID, w)
	}()
}

// wait blocks until every turn's sampling has been recorded. For tests
// and an orderly shutdown.
func (p *kvPool) wait() {
	if p != nil {
		p.inflight.Wait()
	}
}

func (p *kvPool) session(sessionID string) *kvSession {
	s := p.sessions[sessionID]
	if s == nil {
		s = &kvSession{}
		p.sessions[sessionID] = s
	}
	return s
}

func (p *kvPool) warnOnce(sessionID, what string, err error) {
	p.mu.Lock()
	s := p.session(sessionID)
	first := !s.warned
	s.warned = true
	p.mu.Unlock()
	if first {
		p.logger.Warn("kv-cache metrics: "+what+"; the turn ran normally and later failures for this session are not logged",
			"session", sessionID, "error", err.Error())
	}
}

// record keeps w as the session's last window and folds it into the
// session's: counts summed (staying absent until a turn reports them),
// the peak the highest seen, Reset and EndFailed if any turn's were.
func (p *kvPool) record(sessionID string, w kvmetrics.Window) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.session(sessionID)
	s.last = &w
	s.turns++
	f := &s.fold
	if s.turns == 1 || w.Start.Before(f.Start) {
		f.Start = w.Start
	}
	if w.End.After(f.End) {
		f.End = w.End
	}
	f.Queries = addPtr(f.Queries, w.Queries)
	f.Hits = addPtr(f.Hits, w.Hits)
	f.HitRate = nil
	if f.Queries != nil && f.Hits != nil && *f.Queries > 0 {
		r := *f.Hits / *f.Queries
		f.HitRate = &r
	}
	if w.HitRateGauge != nil {
		g := *w.HitRateGauge
		f.HitRateGauge = &g
	}
	if w.PeakKVUsage != nil && (f.PeakKVUsage == nil || *w.PeakKVUsage > *f.PeakKVUsage) {
		v := *w.PeakKVUsage
		f.PeakKVUsage = &v
	}
	f.Readings += w.Readings
	f.Reset = f.Reset || w.Reset
	f.EndFailed = f.EndFailed || w.EndFailed
	f.Turns = s.turns
}

// info is the session's kv_cache block for /usage, or nil when nothing
// has been sampled.
func (p *kvPool) info(sessionID string) *attach.KVCacheInfo {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[sessionID]
	if s == nil || s.last == nil {
		return nil
	}
	fold := s.fold
	return &attach.KVCacheInfo{Fleet: true, LastTurn: kvWindowOf(*s.last), Session: &fold}
}

func kvWindowOf(w kvmetrics.Window) *attach.KVWindow {
	return &attach.KVWindow{
		Start: w.Start, End: w.End,
		Queries: w.Queries, Hits: w.Hits, HitRate: w.HitRate, HitRateGauge: w.HitRateGauge,
		PeakKVUsage: w.PeakKVUsage, Readings: w.Readings, Reset: w.Reset, EndFailed: w.EndFailed,
	}
}

func addPtr(a, b *float64) *float64 {
	switch {
	case b == nil:
		return a
	case a == nil:
		v := *b
		return &v
	default:
		v := *a + *b
		return &v
	}
}
