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
	"sync"
)

// refusalStop is the daemon's implementation of approval.TurnStop: the
// handle the write gate ends a turn through when the model will not
// take no for an answer (#449).
//
// One per turn, armed with that turn's cancel, installed on the run
// context. The gate reaches it through the context rather than through
// a session lookup, which is what makes it reach a planner-dispatched
// specialist running on its own sub-runner — see approval.WithTurnStop.
//
// It is two things at once and has to be, for the reason the watchdog's
// Enforcer is: cancelling is the only way to stop a run, and a
// cancelled run surfaces as "context canceled" with nothing in it about
// why. So the reason is written down first and recovered afterwards, by
// a caller that has already seen the run fail and is deciding what to
// tell the operator it failed of.
type refusalStop struct {
	mu     sync.Mutex
	reason error
	cancel context.CancelFunc
}

// arm gives the stop the turn's cancel. Until it is called the stop
// still records a reason and simply cannot act on it, which is the
// correct behaviour for the window before the run starts: there is no
// run to stop.
func (s *refusalStop) arm(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel = cancel
}

// StopTurn implements approval.TurnStop.
//
// The first reason wins. A gate that suppresses the threshold call and
// then suppresses another before the cancellation has propagated would
// otherwise overwrite the reason with a later, equivalent one — same
// class of event, different count — and the operator would read a
// number that does not match the log line that fired.
func (s *refusalStop) StopTurn(reason error) {
	s.mu.Lock()
	if s.reason == nil {
		s.reason = reason
	}
	cancel := s.cancel
	s.mu.Unlock()
	// Outside the lock: cancel runs the context's done handlers, and
	// nothing good comes of holding a mutex a handler might want.
	if cancel != nil {
		cancel()
	}
}

// why returns what ended the turn, or nil if this stop did not end it.
//
// Call it wherever a run error is being classified, alongside the
// watchdog's Preflight and for the same reason — the run's own error is
// "context canceled" whichever of them pulled the trigger.
func (s *refusalStop) why() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}
