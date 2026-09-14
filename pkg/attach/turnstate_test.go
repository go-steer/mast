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

package attach

import (
	"context"
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/eventlog"
)

// statusOnlyRegistrant implements StatusProvider and NOT
// TurnStateProvider — the pre-#313 shape, and the shape every
// registrant that cannot read durable state still has.
type statusOnlyRegistrant struct {
	eventfulRegistrant
	state string
}

func (p *statusOnlyRegistrant) AttachStatus() StatusInfo {
	return StatusInfo{State: p.state, ModelName: "gemini-3.7-flash"}
}

// parkedRegistrant adds the turn-state capability on top, reporting a
// durable pause the way mast's adapter does: StatusInfo says paused,
// and the capability says what the pause is waiting on.
type parkedRegistrant struct {
	statusOnlyRegistrant
	turnState string
}

func (p *parkedRegistrant) AttachTurnState() string { return p.turnState }

func statusOnly(state string) *statusOnlyRegistrant {
	return &statusOnlyRegistrant{
		eventfulRegistrant: eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "mast", user: "u", sid: "s1"},
			handle:         &eventlog.Handle{Stream: newSlowStream()},
		},
		state: state,
	}
}

func parked(turnState string) *parkedRegistrant {
	return &parkedRegistrant{
		statusOnlyRegistrant: *statusOnly(AgentStatePaused),
		turnState:            turnState,
	}
}

func snapshotFor(t *testing.T, agent Registrant) StatusUpdate {
	t.Helper()
	b, err := newBroadcaster(&Entry{AppName: "mast", UserID: "u", SessionID: "s1", Agent: agent})
	if err != nil {
		t.Fatalf("newBroadcaster: %v", err)
	}
	t.Cleanup(b.Close)
	return b.statusSnapshot()
}

// The boot snapshot is the whole point of #313's reach beyond the
// transition frame. A client that connects to a session that parked an
// hour ago never saw the frame the adapter emitted then; the snapshot
// is all it gets, and until now the snapshot said `idle` — the same
// string a finished turn gets.
func TestStatusSnapshotReportsWhatAParkedSessionAwaits(t *testing.T) {
	t.Parallel()
	for _, want := range []string{TurnStateAwaitingPermission, TurnStateAwaitingElicit} {
		got := snapshotFor(t, parked(want))
		if got.TurnState != want {
			t.Errorf("snapshot turn_state = %q, want %q", got.TurnState, want)
		}
		if got.Model != "gemini-3.7-flash" {
			t.Errorf("snapshot model = %q, want the StatusProvider's value — the capability overrides the turn state, not the rest of the frame", got.Model)
		}
	}
}

// And it reaches the wire: the boot status-update a real subscriber
// receives carries it, not just the internal projection.
func TestBootStatusFrameCarriesTheAwaitingState(t *testing.T) {
	t.Parallel()
	b, err := newBroadcaster(&Entry{AppName: "mast", UserID: "u", SessionID: "s1", Agent: parked(TurnStateAwaitingPermission)})
	if err != nil {
		t.Fatalf("newBroadcaster: %v", err)
	}
	t.Cleanup(b.Close)
	ch := b.Subscribe(context.Background(), 0)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				t.Fatal("stream closed before the status-update arrived")
			}
			if f.Type != EventStatusUpdate {
				continue
			}
			su, ok := f.TypedData.(StatusUpdate)
			if !ok {
				t.Fatalf("status-update payload = %T, want StatusUpdate", f.TypedData)
			}
			if su.TurnState != TurnStateAwaitingPermission {
				t.Fatalf("boot status-update turn_state = %q, want %q", su.TurnState, TurnStateAwaitingPermission)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for the boot status-update")
		}
	}
}

// An unrecognised answer falls back to the derivation. The point is
// not defensiveness for its own sake: the provider computes a string
// from durable state, and a client that receives a turn_state outside
// the spec's four values has no defined behavior, while the coarse
// derivation is merely imprecise.
func TestStatusSnapshotRefusesAnOffSpecTurnState(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "awaiting_approval", "AWAITING_PERMISSION", "waiting"} {
		p := parked(bad)
		p.state = AgentStateRunning
		if got := snapshotFor(t, p).TurnState; got != TurnStateStreaming {
			t.Errorf("turn_state %q: snapshot = %q, want the derivation's %q", bad, got, TurnStateStreaming)
		}
	}
}

// A registrant with no turn-state capability keeps the pre-#313
// derivation exactly: running is streaming, everything else is idle.
// Nothing about this change makes a registrant that cannot read
// durable state start lying about it.
func TestStatusSnapshotWithoutTheCapabilityKeepsTheDerivation(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]string{
		AgentStateRunning:  TurnStateStreaming,
		AgentStatePaused:   TurnStateIdle,
		AgentStateDeferred: TurnStateIdle,
		AgentStateIdle:     TurnStateIdle,
		"":                 TurnStateIdle,
	} {
		if got := snapshotFor(t, statusOnly(state)).TurnState; got != want {
			t.Errorf("state %q: turn_state = %q, want %q", state, got, want)
		}
	}
}

// A parked session is streaming again the moment a resume turn starts,
// because the adapter's own AttachTurnState reports streaming — the
// snapshot must not second-guess it from the durable projection, which
// still reads "parked" until the FunctionResponse lands mid-turn.
func TestStatusSnapshotHonorsStreamingOverAPark(t *testing.T) {
	t.Parallel()
	p := parked(TurnStateStreaming)
	p.state = AgentStateRunning
	if got := snapshotFor(t, p).TurnState; got != TurnStateStreaming {
		t.Errorf("turn_state = %q, want %q", got, TurnStateStreaming)
	}
}
