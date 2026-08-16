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
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/workload"
)

func TestValidateDispatch(t *testing.T) {
	for _, ok := range []string{"", workload.DispatchCoordinator, workload.DispatchGraph, workload.DispatchFanout, workload.DispatchBounded, workload.DispatchAuto} {
		if err := validateDispatch(ok); err != nil {
			t.Errorf("validateDispatch(%q) = %v, want nil", ok, err)
		}
	}
	err := validateDispatch("sideways")
	if err == nil || !strings.Contains(err.Error(), "sideways") {
		t.Fatalf("validateDispatch(\"sideways\") = %v, want a rejection quoting the value", err)
	}
	// A near miss, because that is the realistic typo: the flag is
	// rejected here or the shape silently falls back to coordinator and
	// bills like one.
	if err := validateDispatch("bound"); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("validateDispatch(\"bound\") = %v, want a rejection quoting the value", err)
	}
}

// The binary's terminal default is coordinator, NOT auto. Auto reads
// the roster and would re-shape a bundle that has always run as a
// coordinator the moment somebody adds a `_synthesis` specialist to it —
// so a bundle opts into that by writing `dispatch: auto`, and an
// upgrade of mast never changes how an existing bundle runs.
func TestResolveDispatch(t *testing.T) {
	fanout := &workload.Bundle{Dispatch: workload.DispatchFanout}
	silent := &workload.Bundle{}

	tests := []struct {
		name   string
		flag   string
		bundle *workload.Bundle
		want   string
	}{
		{"flag wins", workload.DispatchGraph, fanout, workload.DispatchGraph},
		{"bundle wins over an unset flag", "", fanout, workload.DispatchFanout},
		{"unset flag, silent bundle -> coordinator", "", silent, workload.DispatchCoordinator},
		{"a bundle may opt into auto", "", &workload.Bundle{Dispatch: workload.DispatchAuto}, workload.DispatchAuto},
		// `auto` passes through here and is resolved against the roster
		// by the caller (compose.RosterShape) — resolveDispatch settles
		// precedence, not shape.
		{"an operator may opt into auto for one run", workload.DispatchAuto, fanout, workload.DispatchAuto},
		{"no bundle at all -> coordinator", "", nil, workload.DispatchCoordinator},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveDispatch(tc.flag, tc.bundle); got != tc.want {
				t.Fatalf("resolveDispatch(%q) = %q, want %q", tc.flag, got, tc.want)
			}
		})
	}
}
