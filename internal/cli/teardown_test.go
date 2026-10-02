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

package cli

import (
	"reflect"
	"testing"
)

// serve's teardown used to be ten defers, and their order is load-bearing:
// the attach server closes before the eventlog it tails, and the signal
// context is the last thing released. runTeardown replaced them when
// serve was split into phases, so it is held to what defer guaranteed —
// last-in first-out, every step runs even if a later-registered one
// panics — plus the one thing it adds: the watchdog is disarmed after
// the last step, never before.
func TestRunTeardownUnwindsLikeTheDefersItReplaced(t *testing.T) {
	var got []string
	d := &daemon{disarmTeardown: func() { got = append(got, "disarm") }}
	d.onTeardown(func() { got = append(got, "signal context") })
	d.onTeardown(func() { got = append(got, "eventlog") })
	d.onTeardown(func() { got = append(got, "attach") })
	d.runTeardown()
	want := []string{"attach", "eventlog", "signal context", "disarm"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("teardown order = %q, want %q", got, want)
	}
}

func TestRunTeardownRunsEveryStepPastAPanic(t *testing.T) {
	var got []string
	d := &daemon{disarmTeardown: func() { got = append(got, "disarm") }}
	d.onTeardown(func() { got = append(got, "first") })
	d.onTeardown(func() { panic("a Close that panicked") })
	d.onTeardown(func() { got = append(got, "last") })
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("the panic was swallowed; a defer would have re-raised it")
			}
		}()
		d.runTeardown()
	}()
	want := []string{"last", "first", "disarm"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("steps run = %q, want %q", got, want)
	}
}
