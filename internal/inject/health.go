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

package inject

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
)

// HealthCheck answers "can this dependency still do its job?". A nil
// error is ready. The error is logged and never written to the
// response: /healthz is unauthenticated and a database error routinely
// names a filesystem path, a DSN or a host.
type HealthCheck func(ctx context.Context) error

// healthResponse is the whole body. Deliberately this small — see
// Config.HealthChecks for what is kept out of it and why.
type healthResponse struct {
	OK     bool              `json:"ok"`
	Checks map[string]string `json:"checks"`
}

const (
	healthReady  = "ready"
	healthFailed = "failed"
)

// healthState tracks the last verdict so the daemon logs one line per
// health *transition* rather than one per probe. At kubelet's default
// cadence a line per probe is ~8,600 lines a day per pod, which buries
// the transition that matters inside the noise it created.
type healthState struct {
	mu      sync.Mutex
	known   bool
	lastOK  bool
	lastMsg string
}

// note records the new verdict and reports whether it is a change worth
// logging. The first probe always logs.
func (h *healthState) note(ok bool, msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.known && h.lastOK == ok && h.lastMsg == msg {
		return false
	}
	h.known, h.lastOK, h.lastMsg = true, ok, msg
	return true
}

// handleHealthz answers the readiness probe.
//
//	200 {"ok":true,"checks":{"session_db":"ready"}}
//	503 {"ok":false,"checks":{"session_db":"failed"}}
//
// Registered as its own exact-path route, so it is reachable without
// credentials by construction rather than by a bypass inside the auth
// path: this handler contains no call to authOK and nothing routes
// through it. `/healthz/`, `/healthz/sessions` and `/healthzz` are not
// this route and fall through to the catch-all's 404.
//
// With no checks configured the answer is 200 and an empty object. That
// is the in-memory daemon: the process is up and there is nothing
// durable to verify, which is a different statement from "the database
// is fine" and the empty `checks` is what says so.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body := healthResponse{OK: true, Checks: make(map[string]string, len(s.cfg.HealthChecks))}
	failed := map[string]error{}
	for name, check := range s.cfg.HealthChecks {
		if check == nil {
			continue
		}
		if err := check(ctx); err != nil {
			body.OK = false
			body.Checks[name] = healthFailed
			failed[name] = err
			continue
		}
		body.Checks[name] = healthReady
	}

	s.logHealthTransition(body.OK, failed)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if body.OK {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(body)
}

// logHealthTransition emits at most one line per change of verdict. The
// check errors go here — this is the only place they are allowed to
// appear, because the operator diagnosing a red probe needs the path
// the response body must not carry.
func (s *Server) logHealthTransition(ok bool, failed map[string]error) {
	names := make([]string, 0, len(failed))
	for name := range failed {
		names = append(names, name)
	}
	sort.Strings(names)

	// The message is part of the transition key: session_db recovering
	// and then failing for a different reason is two events, not one.
	key := ""
	for _, name := range names {
		key += name + "=" + failed[name].Error() + ";"
	}
	if !s.health.note(ok, key) {
		return
	}
	if ok {
		s.logger.Info("health check ready")
		return
	}
	attrs := []any{"failed_checks", names}
	for _, name := range names {
		attrs = append(attrs, name+"_error", failed[name].Error())
	}
	s.logger.Error("health check not ready", attrs...)
}
