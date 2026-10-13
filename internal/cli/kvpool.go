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
	"github.com/go-steer/mast/internal/compose"
	"github.com/go-steer/mast/internal/kvwindow"
)

// kvPool samples a self-hosted model server's KV cache (core-models
// kvmetrics) and reports, per session, the last turn's window and a
// fold of all of them, for GET /sessions/{id}/usage.
//
// One poller per pool, and a daemon has one model, so one per metrics
// endpoint however many sessions are open: the counters are the
// server's, so a poller per turn would only scrape the same numbers
// several times. It keeps a bounded ring of timestamped readings. Each
// turn brackets itself with an edge reading at its start and its end —
// the latest reading if it is under edgeFresh old, otherwise a fresh
// scrape bounded by edgeTimeout — and its window is
// kvmetrics.Between(start edge, the ring's readings between, end edge).
//
// The start edge is taken before the turn's first model call, so the
// window includes that call's prefill; the end edge before the turn
// returns, under the session's turn lock, so a session's windows are
// recorded in turn order and never overlap. A dead endpoint costs a
// turn at most one edgeTimeout: after a failed scrape the pool stops
// scraping at edges for edgeBackoff, a turn falls back to the nearest
// ring reading, and its window is marked Approximate — its edge then
// sits up to one poll interval off the turn's.
//
// Fleet-level throughout: the server's prefix cache across all of its
// traffic. Reported beside usage, never folded into cost, tokens or a
// budget. A nil *kvPool, or one whose model publishes no metrics,
// samples nothing.
type kvPool struct {
	baseCtx  context.Context
	logger   *slog.Logger
	sampler  func(context.Context) (*kvmetrics.Sampler, error)
	interval time.Duration

	edgeFresh   time.Duration
	edgeTimeout time.Duration
	edgeBackoff time.Duration
	ringCap     int

	initOnce sync.Once
	s        *kvmetrics.Sampler // nil: nothing to sample
	pollers  int                // pollers started; one at most
	cancel   context.CancelFunc
	done     chan struct{}

	mu        sync.Mutex
	ring      []kvmetrics.Reading
	downUntil time.Time
	pollWarn  bool
	sessions  map[string]*kvSession
}

type kvSession struct {
	last *kvwindow.Window
	fold kvwindow.Fold
	warn bool
}

const (
	// kvPollInterval spaces the shared poller's readings, so a turn's
	// peak KV utilization has readings between its edges.
	kvPollInterval = 5 * time.Second
	// kvEdgeFresh: a reading this recent brackets a turn's edge as is.
	kvEdgeFresh = time.Second
	// kvEdgeTimeout bounds an edge scrape, the most sampling can add to
	// a turn. A metrics scrape on the server's network takes
	// milliseconds.
	kvEdgeTimeout = 300 * time.Millisecond
	// kvEdgeBackoff: after a failed scrape, edges use the ring for this
	// long instead of scraping.
	kvEdgeBackoff = 30 * time.Second
	// kvRingCap bounds the readings kept: about 5.7 hours at the poll
	// interval. A turn longer than that keeps its edges, and its peak
	// misses the dropped middle.
	kvRingCap = 4096
)

// newKVPool builds the daemon's pool for provider/model. The sampler is
// resolved lazily, on the first turn, under ctx (the daemon's lifetime),
// so startup never waits on a profile's credential resolution.
func newKVPool(ctx context.Context, provider, modelName string, logger *slog.Logger) *kvPool {
	if logger == nil {
		logger = slog.Default()
	}
	return &kvPool{
		baseCtx: ctx, logger: logger,
		sampler: func(ctx context.Context) (*kvmetrics.Sampler, error) {
			return compose.KVSampler(ctx, provider, modelName)
		},
		interval:    kvPollInterval,
		edgeFresh:   kvEdgeFresh,
		edgeTimeout: kvEdgeTimeout,
		edgeBackoff: kvEdgeBackoff,
		ringCap:     kvRingCap,
		sessions:    map[string]*kvSession{},
	}
}

// init resolves the sampler and starts the poller, once.
func (p *kvPool) init() {
	p.initOnce.Do(func() {
		s, err := p.sampler(p.baseCtx)
		if err != nil {
			p.logger.Warn("kv-cache metrics: sampler unavailable; /usage will carry no kv_cache block", "error", err.Error())
			return
		}
		if s == nil {
			return
		}
		p.s = s
		p.logger.Info("kv-cache metrics: sampling the model server around each turn (fleet-level)", "poll", p.interval.String())
		ctx, cancel := context.WithCancel(p.baseCtx)
		p.cancel, p.done = cancel, make(chan struct{})
		p.pollers++
		go p.poll(ctx)
	})
}

func (p *kvPool) poll(ctx context.Context) {
	defer close(p.done)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sctx, cancel := context.WithTimeout(ctx, p.interval)
			r, err := p.s.Snapshot(sctx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			p.mu.Lock()
			if err != nil {
				first := !p.pollWarn
				p.pollWarn = true
				p.downUntil = time.Now().Add(p.edgeBackoff)
				p.mu.Unlock()
				if first {
					p.logger.Warn("kv-cache metrics: scrape failed; turns run normally, later failures are not logged", "error", err.Error())
				}
				continue
			}
			p.push(r)
			p.downUntil = time.Time{}
			p.mu.Unlock()
		}
	}
}

// push appends r to the ring, dropping the oldest past ringCap. The
// caller holds p.mu.
func (p *kvPool) push(r kvmetrics.Reading) {
	p.ring = append(p.ring, r)
	if n := len(p.ring) - p.ringCap; n > 0 {
		p.ring = append(p.ring[:0], p.ring[n:]...)
	}
}

// stop ends the poller. For daemon shutdown; safe on a nil pool or one
// that never started.
func (p *kvPool) stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// kvTurn is one turn's start edge.
type kvTurn struct {
	start  kvmetrics.Reading
	approx bool
}

// edge returns a reading to bracket a turn's edge at now: the latest if
// it is fresh and newer than after (an end edge must not reuse the start
// edge's reading), else a bounded scrape, else (approx) the latest there
// is.
func (p *kvPool) edge(now, after time.Time) (r kvmetrics.Reading, approx, ok bool) {
	p.mu.Lock()
	var latest kvmetrics.Reading
	have := len(p.ring) > 0
	if have {
		latest = p.ring[len(p.ring)-1]
	}
	down := now.Before(p.downUntil)
	p.mu.Unlock()
	if have && now.Sub(latest.At) <= p.edgeFresh && latest.At.After(after) {
		return latest, false, true
	}
	if !down {
		ctx, cancel := context.WithTimeout(p.baseCtx, p.edgeTimeout)
		fresh, err := p.s.Snapshot(ctx)
		cancel()
		p.mu.Lock()
		if err == nil {
			p.push(fresh)
			p.mu.Unlock()
			return fresh, false, true
		}
		p.downUntil = time.Now().Add(p.edgeBackoff)
		first := !p.pollWarn
		p.pollWarn = true
		p.mu.Unlock()
		if first {
			p.logger.Warn("kv-cache metrics: scrape failed; turns run normally, later failures are not logged", "error", err.Error())
		}
	}
	return latest, true, have
}

// begin takes the turn's start edge, before its first model call.
func (p *kvPool) begin() *kvTurn {
	if p == nil {
		return nil
	}
	p.init()
	if p.s == nil {
		return nil
	}
	r, approx, ok := p.edge(time.Now(), time.Time{})
	if !ok {
		return nil
	}
	return &kvTurn{start: r, approx: approx}
}

// end takes the turn's end edge and records its window. Called before
// the turn returns, under the session's turn lock, so a session's
// windows land in turn order.
func (p *kvPool) end(sessionID string, t *kvTurn) {
	if p == nil || t == nil {
		return
	}
	last, approx, ok := p.edge(time.Now(), t.start.At)
	endFailed := !ok || !last.At.After(t.start.At)
	readings := []kvmetrics.Reading{t.start}
	p.mu.Lock()
	for _, r := range p.ring {
		if r.At.After(t.start.At) && (endFailed || r.At.Before(last.At)) {
			readings = append(readings, r)
		}
	}
	p.mu.Unlock()
	if !endFailed {
		readings = append(readings, last)
	}
	w := kvwindow.From(kvmetrics.Between(readings...))
	w.Approximate = t.approx || approx
	w.EndFailed = w.EndFailed || endFailed
	if endFailed && w.Readings < 2 {
		p.warnOnce(sessionID, "no reading closed the turn's window")
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.session(sessionID)
	s.last = w
	s.fold.Add(w)
}

func (p *kvPool) session(sessionID string) *kvSession {
	s := p.sessions[sessionID]
	if s == nil {
		s = &kvSession{}
		p.sessions[sessionID] = s
	}
	return s
}

func (p *kvPool) warnOnce(sessionID, what string) {
	p.mu.Lock()
	s := p.session(sessionID)
	first := !s.warn
	s.warn = true
	p.mu.Unlock()
	if first {
		p.logger.Warn("kv-cache metrics: "+what+"; the turn ran normally", "session", sessionID)
	}
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
	last := *s.last
	return &attach.KVCacheInfo{Fleet: true, LastTurn: &last, Session: s.fold.Window()}
}
