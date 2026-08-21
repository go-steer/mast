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
	"iter"
	"time"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
)

// collectContext is the agent.Context a monitoring cycle's collection
// call runs under (v0.5 W4.2).
//
// # Why this type exists at all
//
// ADK's tool interface takes an agent.Context, and an agent.Context is
// normally minted from an invocation — a model asked for a tool, so
// there is a turn, a session, an event stream and a function-call id to
// hang it on. The collection leg has none of that on purpose: it runs
// before the model is woken, which is the property scoreboard row 9 is
// about. Something has to stand in.
//
// The precondition read (v0.4 W7) never needed one because it runs
// inside a BeforeToolCallback, where ADK has already built a real
// context. This is the first caller of the direct-run seam that runs
// outside a turn.
//
// # Why it is hand-written and not ADK's ContextMock
//
// ADK exports ContextMock as an embedding hook for exactly this shape,
// and embedding it would be four lines. It would also mean that the next
// ADK release to widen agent.Context silently gives the collection leg a
// nil-returning implementation of whatever was added, discovered in
// production. Written out, that release is a compile error and somebody
// has to decide what a collection call should answer. That is the trade
// this file is making, and it is the same one CheckMonitorCollectSurface
// makes one package over: refuse rather than guess.
//
// # What it answers, and the three groups
//
//  1. Real. Identity — app, user, session, agent name. These are
//     genuine facts about the fire, and an MCP server that logs its
//     caller should see the scheduled session rather than an empty
//     string.
//  2. Empty but harmless. No user content, no branch, no isolation
//     scope, no artifacts, no memory, no resumed input. A collection
//     call has no conversation to read and nothing to look up.
//  3. Refused. RequestConfirmation and SearchMemory return errors
//     rather than nil. A collection call that tried to park for a human
//     would park forever — nobody is waiting on a 3am cycle, and there
//     is no invocation to resume into. Returning an error surfaces that
//     as a failed cycle in the log, which is the truthful outcome.
//
// EndInvocation is a no-op and Ended is always false: there is no
// invocation to end, and answering true would tell a tool to stop
// before it started.
//
// # The ack leg shares it (v0.5 W4.6)
//
// An operator's acknowledgement runs the bundle's monitor.ack tool
// through the same seam, and needs the same nothing: no turn, no
// invocation, nobody to confirm to. It goes one step further and passes
// an empty sessionID — a collection call at least belongs to the fire it
// opened, while an ack arrives when a human reads their chat, which is
// rarely the moment a cycle is running. Group 3's refusals read the same
// either way, which is why their messages name the seam rather than the
// caller.
type collectContext struct {
	context.Context

	appName   string
	userID    string
	sessionID string

	actions *session.EventActions
}

// newCollectContext builds the context for one collection call.
//
// Each call gets its own, so the EventActions a tool might write into is
// never shared between two collection calls — and is discarded either
// way, because there is no event for it to ride out on. A tool that
// tries to set state or transfer from a collection call is doing
// something the collection leg has no way to honour; discarding it is
// visible in that the effect simply does not happen, and is preferable
// to a nil that panics inside somebody else's tool.
func newCollectContext(ctx context.Context, appName, userID, sessionID string) *collectContext {
	return &collectContext{
		Context:   ctx,
		appName:   appName,
		userID:    userID,
		sessionID: sessionID,
		actions: &session.EventActions{
			StateDelta:    map[string]any{},
			ArtifactDelta: map[string]int64{},
		},
	}
}

// collectAgentName is the agent name a collection call presents. It is
// deliberately in the namespaced "mast:" form no specialist name can
// take, for the same reason schedulerIdentity is: a tool call that
// arrived without a model behind it should not be readable as one that
// did.
const collectAgentName = "mast:monitor"

// --- group 1: real facts about the fire ------------------------------

func (c *collectContext) AppName() string      { return c.appName }
func (c *collectContext) UserID() string       { return c.userID }
func (c *collectContext) SessionID() string    { return c.sessionID }
func (c *collectContext) AgentName() string    { return collectAgentName }
func (c *collectContext) InvocationID() string { return "" }

// --- group 2: empty, because a collection call has no turn -----------

func (c *collectContext) UserContent() *genai.Content                          { return nil }
func (c *collectContext) Branch() string                                       { return "" }
func (c *collectContext) IsolationScope() string                               { return "" }
func (c *collectContext) ReadonlyState() session.ReadonlyState                 { return emptyState{} }
func (c *collectContext) State() session.State                                 { return emptyState{} }
func (c *collectContext) Artifacts() adkagent.Artifacts                        { return nil }
func (c *collectContext) Memory() adkagent.Memory                              { return nil }
func (c *collectContext) Session() session.Session                             { return nil }
func (c *collectContext) Agent() adkagent.Agent                                { return nil }
func (c *collectContext) RunConfig() *adkagent.RunConfig                       { return &adkagent.RunConfig{} }
func (c *collectContext) FunctionCallID() string                               { return "" }
func (c *collectContext) Actions() *session.EventActions                       { return c.actions }
func (c *collectContext) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }
func (c *collectContext) ResumedInput(string) (any, bool)                      { return nil, false }
func (c *collectContext) Path() string                                         { return "" }
func (c *collectContext) RunID() string                                        { return "" }
func (c *collectContext) SubScheduler() adkagent.DynamicSubScheduler           { return nil }
func (c *collectContext) OutputForAncestors() []string                         { return nil }
func (c *collectContext) EndInvocation()                                       {}
func (c *collectContext) Ended() bool                                          { return false }

// --- group 3: refused, because the alternative is a silent hang ------

// RequestConfirmation refuses. ADK's mcptoolset calls it when a tool is
// configured to require confirmation, and the flow it starts ends in an
// event on an invocation's stream that a human answers. A collection
// call has no invocation and nobody is waiting on it, so starting that
// flow would wedge the cycle rather than ask anyone anything.
func (c *collectContext) RequestConfirmation(hint string, _ any) error {
	return errors.New("a tool mast runs on its own behalf cannot ask for confirmation: it runs outside any turn, on no invocation, with nobody waiting to answer (hint was: " + hint + ")")
}

// SearchMemory refuses for the same reason it would fail anyway: there
// is no memory service on this path. An explicit error beats ADK's
// nil-Memory panic.
func (c *collectContext) SearchMemory(context.Context, string) (*memory.SearchResponse, error) {
	return nil, errors.New("a tool mast runs on its own behalf has no memory service to search")
}

// --- derivations -----------------------------------------------------
//
// Each returns a shallow copy with the embedded context replaced, which
// is what every ADK implementation of these does. The EventActions is
// shared with the original deliberately: a derived context is the same
// call, and two of them writing to two discarded maps instead of one is
// a distinction with no consequence that would still surprise a reader.

func (c *collectContext) WithContext(ctx context.Context) adkagent.InvocationContext {
	return c.with(ctx)
}

func (c *collectContext) WithAgentContext(ctx context.Context) adkagent.Context {
	return c.with(ctx)
}

func (c *collectContext) WithAgentTimeout(d time.Duration) (adkagent.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(c.Context, d)
	return c.with(ctx), cancel
}

func (c *collectContext) WithAgentCancel() (adkagent.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(c.Context)
	return c.with(ctx), cancel
}

// WithDelta and WithICDelta ignore their deltas. Both exist to carry
// invocation-shaped overrides — branch, isolation scope, agent — and
// this context reports none of those as anything but empty, so applying
// a delta would produce a context that claims a branch it is not on.
func (c *collectContext) WithDelta(*adkagent.CommonContextDelta) adkagent.Context {
	return c.with(c.Context)
}

func (c *collectContext) WithICDelta(*adkagent.InvocationContextDelta) adkagent.InvocationContext {
	return c.with(c.Context)
}

func (c *collectContext) with(ctx context.Context) *collectContext {
	cp := *c
	cp.Context = ctx
	return &cp
}

// The assertion this whole file is for: if ADK widens agent.Context,
// this line stops compiling and a human decides what a collection call
// answers, rather than a nil arriving in a tool at 3am.
var _ adkagent.Context = (*collectContext)(nil)

// emptyState is a state that is always empty and never accepts a write.
// A collection call has no session state — it runs before the session
// the fire will open exists — and the honest answer to a tool that tries
// to write one is an error rather than a discarded success.
type emptyState struct{}

func (emptyState) Get(string) (any, error) { return nil, session.ErrStateKeyNotExist }

func (emptyState) Set(key string, _ any) error {
	return errors.New("a monitor.collect call cannot set session state (key " + key + "): it runs before the cycle's session is opened")
}

func (emptyState) All() iter.Seq2[string, any] {
	return func(func(string, any) bool) {}
}

var (
	_ session.State         = emptyState{}
	_ session.ReadonlyState = emptyState{}
)
