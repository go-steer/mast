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

// Originally derived from go-steer/core-agent@25d8531cf8d1d69459471009a9e7e2e9b0dff1e2

package attach

import (
	"context"

	"github.com/go-steer/mast/pkg/auth"
)

// SessionResumer reconstructs a session that exists on disk
// (persisted ACL row) but not in the current daemon's in-memory
// SessionRegistry. Called by Registry.Lookup / LookupSingle on miss
// when a Resumer is configured.
//
// Implementations:
//
//  1. Confirm the session exists — typically via
//     SessionACLStore.FindByAppSID, or whatever durable store the
//     implementation reads.
//  2. Reconstruct the agent with the EXPLICIT sessionID (not a
//     freshly minted one) so ADK's session.Service reattaches the
//     prior conversation history from the eventlog.
//  3. Return the new Registrant and a cancelOnEvict CancelFunc the
//     registry invokes when the entry is later evicted (idle
//     sweep). The cancel typically shuts down the per-session wake
//     loop the resumer spawned.
//
// The auth.SessionACL return is a **fallback, not the answer**: when
// the registry has an ACL store wired it registers the resumed entry
// under the persisted row it read itself, and the returned value is
// used only where there is nothing else to go on — no store, or no
// row for this session. Returning the zero value is therefore the
// correct thing for a resumer over a store the registry already has;
// it is also what an implementation does by accident, which is why
// the registry stopped taking this value at its word (#223). Do not
// use it to *change* a session's ACL — that is what
// SessionRegistry.SetACL and PUT /sessions/{id}/acl are for.
//
// Return ErrSessionACLNotFound when no such session exists — the
// registry maps that to ErrSessionNotFound so the handler returns
// 404. Any other error surfaces as 500 with the resume-failure
// message (see core-agent's docs/session-resume-design.md OQ #2).
//
// mast's implementation is cmd/mast's storeResumer, over the
// transcript store. The interface lives in pkg/attach so the
// handlers can consult it without importing the composition layer;
// core-agent's equivalent is built in its pkg/compose, which is
// where the daemon-wide wiring (model, gate template, tools,
// eventlog handle, MCP servers) is assembled.
//
// The cancelOnEvict return may be nil — the registry treats nil as
// "no background work to stop." Test implementations of the
// interface commonly return nil.
type SessionResumer interface {
	Resume(ctx context.Context, app, sid string) (Registrant, auth.SessionACL, context.CancelFunc, error)
}
