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
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/watchdog"
)

// The daemon's sub-run observer bills a planner dispatch to the OUTER
// session's meter — the same pool, the same per-specialist scopes the
// roster declared (#226). gke-triage is the fixture for the same reason
// TestMeterPoolEnforcesSpecialistCeilings uses it: the workload
// declares no ceilings of its own, so only the specialist's $0.25 can
// stop this.
func TestDaemonSubRunObserverMetersToTheOuterSession(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "workloads", "gke-triage")
	built, err := buildRoot(context.Background(), discardLogger(),
		mastagent.NewEchoModel("echo"), "", "echo", dir, "coordinator", hostSeams{})
	if err != nil {
		t.Fatalf("buildRoot: %v", err)
	}
	pool := newMeterPool(built.bundle, built.specs, "", "echo")
	obs := observability.New()
	sub := &daemonSubRunObserver{}
	sub.attach(pool, obs, nil, nil, built.bundle.Name, discardLogger())

	sink := sub.SubRun("incident-abc", "OOMKilled")
	defer sink.Close()

	// A modest dispatch: under every ceiling, so it must be silently
	// counted rather than refused.
	if err := sink.Observe(spend("OOMKilled", 100)); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	tokens, _, calls := pool.meter("incident-abc").Snapshot()
	if tokens != 100 || calls != 1 {
		t.Errorf("outer session meter = %d tokens / %d calls, want 100/1", tokens, calls)
	}
	// The sub-run must not have invented a session of its own.
	if other, _, _ := pool.meter("invoke-OOMKilled").Snapshot(); other != 0 {
		t.Errorf("a second session meter picked up %d tokens; the dispatch was billed to the wrong session", other)
	}

	// And the specialist's declared ceiling binds on this door: 10k
	// tokens at echo's $0.05/1K is $0.50, twice OOMKilled's $0.25.
	err = sink.Observe(spend("OOMKilled", 10_000))
	if !errors.Is(err, budget.ErrExceeded) {
		t.Fatalf("a dispatched specialist overspent its declared cap; sink said %v", err)
	}
	if !strings.Contains(err.Error(), "OOMKilled") {
		t.Errorf("the refusal should name the specialist it stopped: %v", err)
	}
}

// An observer with no sinks yet must not panic and must not refuse:
// buildRoot runs before the meter pool exists, so the zero state is
// reachable by construction even though no turn can be in flight there.
func TestDaemonSubRunObserverBeforeAttachIsInert(t *testing.T) {
	sub := &daemonSubRunObserver{}
	sink := sub.SubRun("incident-abc", "OOMKilled")
	if sink == nil {
		t.Fatal("an unattached observer returned a nil sink; nil is a claim about the host, not about its wiring")
	}
	if err := sink.Observe(spend("OOMKilled", 100)); err != nil {
		t.Fatalf("unattached sink refused a dispatch: %v", err)
	}
	if err := sink.Observe(nil); err != nil {
		t.Fatalf("unattached sink refused a nil event: %v", err)
	}
	sink.Close()
}

// A sub-run event with no outer session cannot be attributed. Metering
// it under "" would look exactly like a workload that never spends, so
// the observer declines to meter and says so once.
func TestDaemonSubRunObserverRefusesToInventASession(t *testing.T) {
	pool := newMeterPool(nil, nil, "", "echo")
	sub := &daemonSubRunObserver{}
	sub.attach(pool, observability.New(), newWatchdogPool(watchdog.ModeEnforce), nil, "w", discardLogger())

	sink := sub.SubRun("", "OOMKilled")
	if err := sink.Observe(spend("OOMKilled", 100)); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	sink.Close()
	if tokens, _, _ := pool.meter("").Snapshot(); tokens != 0 {
		t.Errorf("unattributed spend was metered under the empty session: %d tokens", tokens)
	}
}

// toolCallEvent fakes one model turn that calls the named tool with the
// same arguments every time — the runaway shape RepeatedToolCallSignal
// counts. IDs are distinct so the per-run dedup set cannot collapse
// them: these are five real calls, not one part re-emitted five times.
func toolCallEvent(author, tool string, id string) *session.Event {
	call := genai.NewPartFromFunctionCall(tool, map[string]any{"name": "api", "ns": "prod"})
	call.FunctionCall.ID = id
	return &session.Event{
		Author: author,
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{call}},
		},
	}
}

// TestDaemonSubRunWatchdogSeesADispatchLoop is the watchdog half of
// #226. A specialist spinning inside one invoke_specialist dispatch
// emits its calls on a private runner, so through v0.4 the session's
// watchdog never saw one of them and --watchdog=enforce could not halt
// a loop it could not observe.
//
// The dispatch is stopped AND the session is halted, which is the
// deliberate difference from the budget half: a ceiling is cumulative
// and stopping the sub-run is the whole remedy, while a watchdog trip
// is a latch that means an operator has to reset the session.
func TestDaemonSubRunWatchdogSeesADispatchLoop(t *testing.T) {
	wds := newWatchdogPool(watchdog.ModeEnforce)
	tracker := newTurnTracker(nil, discardLogger(), observability.New(), "w")
	cancelled := false
	tracker.registerCancel("incident-abc", func() { cancelled = true })

	sub := &daemonSubRunObserver{}
	sub.attach(newMeterPool(nil, nil, "", "echo"), observability.New(), wds, tracker, "w", discardLogger())
	sink := sub.SubRun("incident-abc", "OOMKilled")

	// DefaultRepeatThreshold identical calls in one dispatch.
	var halt error
	for i := range watchdog.DefaultRepeatThreshold {
		if err := sink.Observe(toolCallEvent("OOMKilled", "get_k8s_resource", string(rune('a'+i)))); err != nil {
			halt = err
			break
		}
	}
	sink.Close()

	if halt == nil {
		t.Fatal("a specialist looped inside a dispatch and the sink never stopped it")
	}
	if !watchdog.IsTripped(halt) {
		t.Errorf("the sink stopped the dispatch with %v, want a *watchdog.TrippedError so the planner's partial says why", halt)
	}
	if tripped, reason := wds.enforcer("incident-abc").Tripped(); !tripped {
		t.Error("the dispatch was stopped but the session was not halted; the next turn would re-dispatch the same loop")
	} else if !strings.Contains(reason, "repeated-tool-call") {
		t.Errorf("halt reason = %q, want it to name the signal", reason)
	}
	if !cancelled {
		t.Error("the turn running the dispatch was not cancelled; the planner keeps spending after a halt")
	}
}

// The postures below enforce are not a weaker halt, they are no halt:
// detection is identical in all three and only the reaction differs, so
// a dispatch loop under feedback must be observed, retained and fed
// back — and must not stop anything.
func TestDaemonSubRunWatchdogUnderFeedbackDoesNotHalt(t *testing.T) {
	wds := newWatchdogPool(watchdog.ModeFeedback)
	tracker := newTurnTracker(nil, discardLogger(), observability.New(), "w")
	cancelled := false
	tracker.registerCancel("incident-abc", func() { cancelled = true })

	sub := &daemonSubRunObserver{}
	sub.attach(newMeterPool(nil, nil, "", "echo"), observability.New(), wds, tracker, "w", discardLogger())
	sink := sub.SubRun("incident-abc", "OOMKilled")

	for i := range watchdog.DefaultRepeatThreshold {
		if err := sink.Observe(toolCallEvent("OOMKilled", "get_k8s_resource", string(rune('a'+i)))); err != nil {
			t.Fatalf("feedback mode stopped a dispatch: %v", err)
		}
	}
	sink.Close()

	if cancelled {
		t.Error("feedback mode cancelled the turn; only enforce halts")
	}
	if tripped, _ := wds.enforcer("incident-abc").Tripped(); tripped {
		t.Error("feedback mode tripped the session enforcer")
	}
	// Observed all the same: retained for GET /guardrails, and queued
	// for the planner — the model still running, and the one that would
	// otherwise dispatch the same specialist again.
	if got := wds.alerts("incident-abc"); got.count == 0 {
		t.Error("the loop was neither retained nor reportable; an unobserved alert is the same as no watchdog")
	}
	if wds.feedback("incident-abc").Pending() == 0 {
		t.Error("nothing was queued for the planner's next turn")
	}
}

// Each dispatch gets its own dedup set, and the session's signals span
// them. A host that shared one set across dispatches would silently
// drop the second dispatch's identical calls as re-emissions of the
// first's, which is the direction that turns a runaway into a no-op.
func TestDaemonSubRunDedupIsPerDispatchAndSignalsSpanThem(t *testing.T) {
	wds := newWatchdogPool(watchdog.ModeEnforce)
	sub := &daemonSubRunObserver{}
	sub.attach(newMeterPool(nil, nil, "", "echo"), observability.New(), wds, nil, "w", discardLogger())

	// The same call, with the same ID, in each of DefaultRepeatThreshold
	// separate dispatches. Within one dispatch that ID would dedup to a
	// single observation; across dispatches it is the same specialist
	// making the same call over and over, which is the signal.
	var halt error
	for range watchdog.DefaultRepeatThreshold {
		sink := sub.SubRun("incident-abc", "OOMKilled")
		if err := sink.Observe(toolCallEvent("OOMKilled", "get_k8s_resource", "same-id")); err != nil {
			halt = err
		}
		sink.Close()
		if halt != nil {
			break
		}
	}
	if halt == nil {
		t.Fatal("identical calls across dispatches never tripped; the dedup set is shared between them")
	}
}
