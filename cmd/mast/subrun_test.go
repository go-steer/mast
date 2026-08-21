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

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/observability"
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
	sub.attach(pool, obs, built.bundle.Name, discardLogger())

	// A modest dispatch: under every ceiling, so it must be silently
	// counted rather than refused.
	if err := sub.ObserveSubRun("incident-abc", spend("OOMKilled", 100)); err != nil {
		t.Fatalf("ObserveSubRun: %v", err)
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
	err = sub.ObserveSubRun("incident-abc", spend("OOMKilled", 10_000))
	if !errors.Is(err, budget.ErrExceeded) {
		t.Fatalf("a dispatched specialist overspent its declared cap; observer said %v", err)
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
	if err := sub.ObserveSubRun("incident-abc", spend("OOMKilled", 100)); err != nil {
		t.Fatalf("unattached observer refused a dispatch: %v", err)
	}
	if err := sub.ObserveSubRun("", nil); err != nil {
		t.Fatalf("unattached observer refused a nil event: %v", err)
	}
}

// A sub-run event with no outer session cannot be attributed. Metering
// it under "" would look exactly like a workload that never spends, so
// the observer declines to meter and says so once.
func TestDaemonSubRunObserverRefusesToInventASession(t *testing.T) {
	pool := newMeterPool(nil, nil, "", "echo")
	sub := &daemonSubRunObserver{}
	sub.attach(pool, observability.New(), "w", discardLogger())

	if err := sub.ObserveSubRun("", spend("OOMKilled", 100)); err != nil {
		t.Fatalf("ObserveSubRun: %v", err)
	}
	if tokens, _, _ := pool.meter("").Snapshot(); tokens != 0 {
		t.Errorf("unattributed spend was metered under the empty session: %d tokens", tokens)
	}
}
