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

package workload_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/workload"
)

// The bundle half of W4.1: what a workload declares to wake itself up.

func TestLoad_ScheduledTrigger(t *testing.T) {
	path := writeBundle(t, "b.yaml", `name: x
specialists: [a]
edge_trigger:
  http:
    path: /inject
    auth: bearer
  scheduled:
    interval: 15m
    jitter: 45s
    prompt: Sweep every namespace for pods in CrashLoopBackOff.
`)
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := b.EdgeTrigger.Scheduled
	if s == nil {
		t.Fatal("edge_trigger.scheduled did not survive the load")
	}
	// Both triggers at once: a cadence does not close the inject door.
	if b.EdgeTrigger.HTTP == nil || b.EdgeTrigger.HTTP.Path != "/inject" {
		t.Errorf("edge_trigger.http = %+v, want the declared HTTP trigger alongside the schedule", b.EdgeTrigger.HTTP)
	}
	if got, err := s.EffectiveInterval(); err != nil || got != 15*time.Minute {
		t.Errorf("EffectiveInterval() = %v, %v; want 15m", got, err)
	}
	if got, err := s.EffectiveJitter(); err != nil || got != 45*time.Second {
		t.Errorf("EffectiveJitter() = %v, %v; want 45s", got, err)
	}
	if !strings.HasPrefix(s.Prompt, "Sweep every namespace") {
		t.Errorf("prompt = %q, want the declared text", s.Prompt)
	}
}

// TestLoad_NoScheduledTriggerIsNil: the shape every pre-W4.1 bundle has.
// Absent means absent — not a zero-valued cadence some caller might
// arm.
func TestLoad_NoScheduledTriggerIsNil(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\nedge_trigger:\n  http:\n    path: /inject\n")
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if b.EdgeTrigger.Scheduled != nil {
		t.Fatalf("edge_trigger.scheduled = %+v, want nil for a bundle that declares none", b.EdgeTrigger.Scheduled)
	}
	// Nil-safe, because cmd/mast asks the bundle before it knows.
	if d, err := b.EdgeTrigger.Scheduled.EffectiveInterval(); d != 0 || err != nil {
		t.Errorf("EffectiveInterval() on a nil trigger = %v, %v; want 0, nil", d, err)
	}
	if d, err := b.EdgeTrigger.Scheduled.EffectiveJitter(); d != 0 || err != nil {
		t.Errorf("EffectiveJitter() on a nil trigger = %v, %v; want 0, nil", d, err)
	}
}

// TestLoad_ScheduledJitterDefault: an omitted jitter is a tenth of the
// interval, capped — non-zero, because the herd it prevents is one
// nobody thinks to declare against.
func TestLoad_ScheduledJitterDefault(t *testing.T) {
	for _, tc := range []struct {
		name, interval string
		want           time.Duration
	}{
		{"a tenth of the interval", "5m", 30 * time.Second},
		{"capped for a long interval", "24h", 30 * time.Second},
		{"and it scales down for a short one", "10s", time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\nedge_trigger:\n  scheduled:\n    interval: "+tc.interval+"\n")
			b, err := workload.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, err := b.EdgeTrigger.Scheduled.EffectiveJitter()
			if err != nil {
				t.Fatalf("EffectiveJitter: %v", err)
			}
			if got != tc.want {
				t.Errorf("EffectiveJitter() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoad_ScheduledJitterZeroIsHonored: "0s" is a declaration, not an
// omission. A test that needs a deterministic cadence — and an operator
// who wants one — must be able to say so, which is why the resolver
// distinguishes the empty string from an explicit zero.
func TestLoad_ScheduledJitterZeroIsHonored(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\nedge_trigger:\n  scheduled:\n    interval: 5m\n    jitter: 0s\n")
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := b.EdgeTrigger.Scheduled.EffectiveJitter()
	if err != nil {
		t.Fatalf("EffectiveJitter: %v", err)
	}
	if got != 0 {
		t.Errorf("EffectiveJitter() = %v, want 0 for an explicitly declared zero", got)
	}
}

// TestLoad_ScheduledErrors: every way a cadence can be wrong is a load
// error. A schedule that fails to parse at runtime is a workload an
// operator believes is running and that has never once woken up, so
// there is no such thing here as a cadence mast half-accepts.
func TestLoad_ScheduledErrors(t *testing.T) {
	for _, tc := range []struct{ name, body, wantErr string }{
		{
			"no interval",
			"name: x\nspecialists: [a]\nedge_trigger:\n  scheduled:\n    prompt: sweep\n",
			"names no interval",
		},
		{
			"unparseable interval",
			"name: x\nspecialists: [a]\nedge_trigger:\n  scheduled:\n    interval: every 15 minutes\n",
			"is not a duration",
		},
		{
			// Zero is not "no schedule": the block is present, so the
			// operator meant to declare one.
			"zero interval",
			"name: x\nspecialists: [a]\nedge_trigger:\n  scheduled:\n    interval: 0s\n",
			"is a bill, not a cadence",
		},
		{
			"sub-second interval",
			"name: x\nspecialists: [a]\nedge_trigger:\n  scheduled:\n    interval: 250ms\n",
			"is a bill, not a cadence",
		},
		{
			"negative interval",
			"name: x\nspecialists: [a]\nedge_trigger:\n  scheduled:\n    interval: -15m\n",
			"is a bill, not a cadence",
		},
		{
			"unparseable jitter",
			"name: x\nspecialists: [a]\nedge_trigger:\n  scheduled:\n    interval: 15m\n    jitter: a bit\n",
			"jitter \"a bit\" is not a duration",
		},
		{
			"negative jitter",
			"name: x\nspecialists: [a]\nedge_trigger:\n  scheduled:\n    interval: 15m\n    jitter: -1s\n",
			"cannot pull one earlier",
		},
		{
			// Jitter that can push a fire past the next tick leaves no
			// cadence to keep, and reorders fires while it does it.
			"jitter as wide as the interval",
			"name: x\nspecialists: [a]\nedge_trigger:\n  scheduled:\n    interval: 15m\n    jitter: 15m\n",
			"no cadence left to keep",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeBundle(t, "b.yaml", tc.body)
			_, err := workload.Load(path)
			if err == nil {
				t.Fatalf("Load accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			// Every refusal names the file. An operator running a dozen
			// bundles needs to know which one to open.
			if !strings.Contains(err.Error(), filepath.Base(path)) {
				t.Errorf("error = %v, want it to name the bundle file %q", err, path)
			}
		})
	}
}
