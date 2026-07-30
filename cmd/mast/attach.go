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
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/auth"
	mastsession "github.com/go-steer/mast/pkg/session"
	"github.com/go-steer/mast/pkg/workload"
)

// attachDeps bundles the operator attach surface serve wires when
// --attach-listen is set: the session registry, the HTTP server, and
// the ensure hook the inject handlers call so daemon-created sessions
// appear on GET /sessions while their first turn is still running.
type attachDeps struct {
	reg *attach.SessionRegistry
	srv *attach.Server

	mu         sync.Mutex
	registered map[string]bool
	adapterFor func(sid string) (attach.Registrant, error)
	logger     *slog.Logger
}

// buildAttach constructs the registry (+ resumer over the session
// store) and binds the attach listener. The listener is bound here —
// not served — so a bad --attach-listen fails serve startup loudly
// instead of surfacing as a background-goroutine log line after the
// daemon reported healthy. Callers go att.srv.Serve() once the inject
// server is up, and defer att.srv.Close().
//
// listenSpec is a TCP address ("127.0.0.1:8484") or a Unix socket
// path prefixed "unix:" ("unix:/var/run/mast.sock"). attach.NewServer
// owns the security policy: non-loopback TCP binds are refused unless
// an auth gate (MAST_ATTACH_TOKEN bearer, TLS client CA) is set.
func buildAttach(logger *slog.Logger, listenSpec, bearer string, store *mastsession.Store, adapterFor func(sid string) (attach.Registrant, error)) (*attachDeps, error) {
	reg := attach.NewSessionRegistry().WithResumer(&storeResumer{
		store:      store,
		adapterFor: adapterFor,
	})

	opts := attach.Options{
		Registry: reg,
		Auth:     attach.AuthConfig{BearerToken: bearer},
	}
	if path, ok := strings.CutPrefix(listenSpec, "unix:"); ok {
		opts.UnixSocket = path
	} else {
		opts.Addr = listenSpec
	}
	srv, err := attach.NewServer(opts)
	if err != nil {
		return nil, err
	}
	if err := srv.Bind(); err != nil {
		return nil, err
	}
	logger.Info("attach surface listening", "addr", srv.Addr(), "authenticated", bearer != "")

	return &attachDeps{
		reg:        reg,
		srv:        srv,
		registered: make(map[string]bool),
		adapterFor: adapterFor,
		logger:     logger,
	}, nil
}

// ensure registers sid's adapter on first sight so the session is
// visible (and tail-able) over attach from the moment its first turn
// starts. Nil-receiver-safe so the inject handlers can call it
// unconditionally. Duplicate registrations (e.g. the registry already
// resumed the session for an attach client) are fine — the map guard
// makes the common path cheap and the registry rejects the rare race
// harmlessly.
func (a *attachDeps) ensure(sid string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.registered[sid] {
		return
	}
	ad, err := a.adapterFor(sid)
	if err != nil {
		a.logger.Warn("attach: could not build session adapter", "session", sid, "error", err.Error())
		return
	}
	if _, err := a.reg.Register(ad); err != nil {
		// Already registered via the resumer path — count it seen.
		a.logger.Debug("attach: register skipped", "session", sid, "reason", err.Error())
	}
	a.registered[sid] = true
}

// storeResumer implements attach.SessionResumer over the daemon's
// session store: any session present in the durable store can be
// re-materialized as a fresh adapter, because every daemon session
// runs against the same runner — there is no per-session factory
// state to reconstruct (core-agent's resumer rebuilds a whole agent;
// mast's rebuilds a Config struct).
//
// v0.1 runs the attach surface in single-user mode (no ACL store, no
// multi-session auth), so resumed sessions carry a zero ACL — the
// same trust model as the legacy Register path.
type storeResumer struct {
	store      *mastsession.Store
	adapterFor func(sid string) (attach.Registrant, error)
}

func (z *storeResumer) Resume(ctx context.Context, app, sid string) (attach.Registrant, auth.SessionACL, context.CancelFunc, error) {
	if app != appName {
		return nil, auth.SessionACL{}, nil, attach.ErrSessionACLNotFound
	}
	if _, err := z.store.Get(ctx, "", sid); err != nil {
		// Unknown session → the registry maps this to 404.
		return nil, auth.SessionACL{}, nil, attach.ErrSessionACLNotFound
	}
	ad, err := z.adapterFor(sid)
	if err != nil {
		return nil, auth.SessionACL{}, nil, err
	}
	return ad, auth.SessionACL{}, nil, nil
}

var errAttachNeedsSessionDB = errors.New("--attach-listen requires --session-db: attach live-tail pumps from the eventlog overlay, which needs a durable session database (in-memory sessions have no overlay to tail)")

// attachDescription summarizes the daemon for the agent card +
// session list: the workload's own description when it has one, its
// name otherwise, empty for workload-less daemons.
func attachDescription(bundle *workload.Bundle) string {
	switch {
	case bundle == nil:
		return ""
	case bundle.Description != "":
		return bundle.Description
	default:
		return "mast workload: " + bundle.Name
	}
}
