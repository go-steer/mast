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

package mast

import "context"

// actorKey is the context key WithActor stores under. Unexported so the
// only way to set it is WithActor, and the only reader is this package.
type actorKey struct{}

// WithActor returns ctx carrying the name of whoever is behind the call,
// for the library entry points that record one in an audit field.
// Today that is ResumeByToken, which writes it into the pause record's
// ConsumedBy and, on an interrupt resume with no response, into the
// default {"resumed_by": ...} verdict.
//
// name is recorded as given. An embedder with an authenticated user
// passes whatever string it wants the audit trail to show — an email,
// a SPIFFE ID, "alice (asserted by sa:relay)". An empty name is the
// same as not calling WithActor: the entry point names its own
// mechanism instead ("library ResumeByToken"), because a caller-less
// context is an in-process path, not a failed lookup.
//
// # Why a string, and why this package owns it
//
// This is the root package's only contact with caller identity, and the
// root is inside the v1.0 promise (DESIGN.md, "The v1.0 stability
// promise"). It used to read pkg/auth's Caller off the context, which
// committed a frozen entry point to an unsupported package's context
// key — a dependency apidiff cannot see, because a context key is not
// part of any signature.
//
// Caller identity is moving to go-steer/purser, shared with core-agent
// (docs/sibling-sync.md). purser is pre-1.0 and says its API will move
// when mast and core-agent migrate onto it, so naming its Caller here
// would freeze a type its owner has not frozen. DESIGN.md's rule for an
// input the consumer constructs, from a dependency already scheduled to
// change, is to own a narrow interface instead; everything the audit
// field has ever recorded is one string, so that is the interface. An
// embedder whose HTTP stack puts a purser Caller on the request
// context copies it across in one line.
func WithActor(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, actorKey{}, name)
}

// actorFrom returns the name WithActor put on ctx, or fallback when
// there is none. fallback names the mechanism; see WithActor.
func actorFrom(ctx context.Context, fallback string) string {
	if name, ok := ctx.Value(actorKey{}).(string); ok && name != "" {
		return name
	}
	return fallback
}
