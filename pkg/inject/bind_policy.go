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
	"fmt"

	"github.com/go-steer/mast/pkg/serverauth"
)

// CheckBindPolicy refuses to expose an unauthenticated inject listener
// beyond loopback (#361). It is the same policy attach
// (go-steer/core-agent#376), A2A (#84) and AG-UI already apply; inject
// is the surface it was never retrofitted to, because the policy was
// written for the listeners added after it.
//
// The refusal is not proportionate to inject's power so much as overdue
// by it. The doors behind this port are /inject (starts a turn),
// /resume (releases a parked mutating call), /abort, /stop, /pause,
// /ack-effects, /monitor-ack and /extend-token. Each of the three
// surfaces that already refuse this shape justifies the refusal with a
// capability inject also has, and mostly had first — A2A cites
// tasks/cancel being destructive, AG-UI cites a run spending budget.
// /resume releasing a write gate is a stronger case than either.
//
// authenticated must mean *this listener* is gated, which for inject
// means a shared bearer token and nothing else. A configured
// [Config.Authenticator] deliberately does not count, and that is the
// one place this policy differs from attach's, where enforced
// multi-session auth does: a user table names who approved on /resume
// and /monitor-ack, but it is not a second way in — the remaining
// routes still read the shared token alone, so a daemon with a table
// and no token has /inject and /abort wide open. Counting it would let
// an attribution feature silently satisfy an authentication check.
//
// An empty addr is not checked. That keeps the embedded and test
// construction path — inject.New(Config{Handler: h}) with no Listen —
// working, matching how A2A and AG-UI scope the same guard.
func CheckBindPolicy(addr string, authenticated bool) error {
	if addr == "" || authenticated || serverauth.IsLoopbackAddr(addr) {
		return nil
	}
	return fmt.Errorf("inject: refusing to bind non-loopback address %q without authentication: "+
		"any host that can reach this port could start turns (/inject), release parked mutating calls "+
		"(/resume), and stop the daemon (/stop). Set MAST_INJECT_TOKEN, or bind a loopback address "+
		"(e.g. --listen=127.0.0.1:7777)", addr)
}
