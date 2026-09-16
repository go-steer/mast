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

// The /perms question, asked of something other than an in-process
// prompter (#364).
//
// prompter.go's PromptBroker answers /perms/stream and /perms/respond
// by bridging permissions.Gate's synchronous Prompter to a remote
// subscriber: the gate blocks inside AskApproval until an operator
// replies. That is core-agent's interaction model and it is a good one
// for a process with a terminal attached.
//
// It is not the model mast's approvals use, and the difference is not
// stylistic. mast's live gate entry point is
// permissions.CheckMutatingToolCall, which never prompts — it returns
// ErrApprovalRequired and the write gate parks the call as a durable
// ADK tool confirmation in the session event log. The question outlives
// the process that asked it. A broker wired as the gate's Prompter in
// this repo would therefore never fan out a frame, because nothing here
// reaches AskApproval: mast registers no bash tool and no built-in file
// tools, so CheckBash, CheckGeneric and the CheckFile* pair have no
// callers (pkg/permissions/denylist.go says so in its port note).
//
// So the routes take a source rather than a broker. PromptBroker is one
// implementation and stays exactly as ported; a park-backed source is
// the other, and it is the one a mast daemon wires. Both speak the same
// wire contract — PromptFrame in, PromptResponse back — which is the
// point: a client that can approve a core-agent tool call can approve a
// mast change without knowing which mechanism is underneath.
//
// The one visible difference is durability, and it runs mast's way: a
// PromptBroker holds its pending prompts in a map and loses every one
// of them when the process dies, while a park is a row an operator can
// still answer tomorrow.

package attach

import (
	"context"
	"errors"

	"github.com/go-steer/mast/pkg/permissions"
)

// PermsSource is what GET /perms/stream and POST /perms/respond read.
//
// Implementations are expected to seed a new subscriber with everything
// still outstanding before streaming anything new. That is what makes a
// reconnect free of a cursor: switchboard's client documents "there is
// no seq, no since, no replay window" and relies on the seed, and a
// source that only published transitions would drop a question for any
// client that attached a moment too late.
type PermsSource interface {
	// SubscribePrompts returns the channel of outstanding and
	// subsequent questions, plus a cleanup the caller must run. The
	// channel closes when ctx is done or the source shuts down.
	SubscribePrompts(ctx context.Context) (<-chan PromptFrame, func())

	// RespondPrompt delivers one answer. ctx carries the authenticated
	// caller: who approved is the audit question a write gate exists to
	// answer, so a source backed by durable approvals reads the
	// approver from here and never from the body.
	//
	// The returned string is the identity the answer was recorded
	// against, which the route echoes so a client can say who approved
	// rather than only that somebody did. Empty means the source
	// recorded nobody — a real answer, and the reason this is a return
	// value rather than something the route derives from ctx itself: a
	// route that echoed the caller would claim attribution on behalf of
	// a source that did not record any.
	//
	// Returns ErrPromptNotFound for an id the source does not hold, and
	// ErrDecisionNotAdmissible for a decision this source refuses on
	// principle rather than on state.
	RespondPrompt(ctx context.Context, id string, decision permissions.Decision) (approver string, err error)
}

// PermsSourceProvider is the capability a registrant implements to put
// something behind the two prompt routes. Checked before
// PromptBrokerProvider, so a registrant may offer both and the richer
// one wins.
type PermsSourceProvider interface {
	AttachPermsSource() PermsSource
}

// ErrDecisionNotAdmissible reports that the source understood the
// decision and refuses to apply it — not that the prompt is gone.
//
// It exists because the two failures need different answers. A mutating
// call admits exactly allow-once and deny (permissions.W2.3: "allow
// every call to this tool for the session", applied to patch_resource,
// hands over the namespace for the session), so a client offering the
// broader buttons is a client whose UI is wrong. Silently narrowing
// allow-session to allow-once would grant less than the operator was
// shown and tell nobody; 400 with the reason puts the defect where
// someone can fix it.
var ErrDecisionNotAdmissible = errors.New("attach: decision not admissible for this prompt")

// brokerSource adapts the ported PromptBroker to PermsSource.
//
// The ctx on RespondPrompt is dropped rather than threaded: a broker
// answers a caller blocked inside AskApproval, and that caller's own
// context is the one that governs. Attribution is not lost here because
// the broker never had it — core-agent grew an attributing Respond
// after this file's port and mast has not taken it (docs/sibling-sync.md).
type brokerSource struct{ b *PromptBroker }

func (s brokerSource) SubscribePrompts(ctx context.Context) (<-chan PromptFrame, func()) {
	return s.b.Subscribe(ctx)
}

func (s brokerSource) RespondPrompt(_ context.Context, id string, d permissions.Decision) (string, error) {
	return "", s.b.Respond(id, d)
}

// permsSourceFor resolves the source behind an entry's prompt routes,
// or nil when the registrant offers neither capability.
func permsSourceFor(entry *Entry) PermsSource {
	if entry == nil || entry.Agent == nil {
		return nil
	}
	if p, ok := entry.Agent.(PermsSourceProvider); ok {
		if src := p.AttachPermsSource(); src != nil {
			return src
		}
	}
	if p, ok := entry.Agent.(PromptBrokerProvider); ok {
		if b := p.AttachPromptBroker(); b != nil {
			return brokerSource{b: b}
		}
	}
	return nil
}
