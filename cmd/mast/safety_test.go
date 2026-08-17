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

// The watchdog posture precedence chain: --watchdog > safety.watchdog >
// mast's default, and the source string an operator reads to find out
// which of the three won.
package main

import (
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/watchdog"
	"github.com/go-steer/mast/pkg/workload"
)

func TestResolveWatchdog(t *testing.T) {
	tests := []struct {
		name       string
		in         watchdogInputs
		wantMode   watchdog.Mode
		wantSource string
	}{
		{
			"nothing set takes mast's default",
			watchdogInputs{},
			watchdog.DefaultMode, watchdogSourceDefault,
		},
		{
			"the bundle wins over the default",
			watchdogInputs{Bundle: "enforce"},
			watchdog.ModeEnforce, watchdogSourceBundle,
		},
		{
			"the flag wins over the default",
			watchdogInputs{Flag: "enforce"},
			watchdog.ModeEnforce, watchdogSourceFlag,
		},
		{
			// The reason the flag sits on top: an operator debugging a
			// halted workload has to be able to drop the posture for one
			// run without editing the deployed manifest.
			"the flag wins over the bundle, including downward",
			watchdogInputs{Flag: "warn", Bundle: "enforce"},
			watchdog.ModeWarn, watchdogSourceFlag,
		},
		{
			"case and surrounding space are forgiven",
			watchdogInputs{Bundle: "  Enforce\n"},
			watchdog.ModeEnforce, watchdogSourceBundle,
		},
		{
			"an all-whitespace value is unset, not invalid",
			watchdogInputs{Flag: "   "},
			watchdog.DefaultMode, watchdogSourceDefault,
		},
		{
			"feedback resolves as itself, not as an alias for warn",
			watchdogInputs{Bundle: "feedback"},
			watchdog.ModeFeedback, watchdogSourceBundle,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveWatchdog(tc.in)
			if err != nil {
				t.Fatalf("resolveWatchdog(%+v): %v", tc.in, err)
			}
			if got.Mode != tc.wantMode {
				t.Errorf("Mode = %q, want %q", got.Mode, tc.wantMode)
			}
			if got.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSource)
			}
		})
	}
}

// mast's shipped default. Asserted here rather than left implicit
// because it is a behavioral decision that diverges from upstream's
// (which defaults an unattended run to enforce), and a silent flip back
// would change what every deployment does on a detected loop.
func TestResolveWatchdogDefaultIsFeedback(t *testing.T) {
	got, err := resolveWatchdog(watchdogInputs{})
	if err != nil {
		t.Fatalf("resolveWatchdog: %v", err)
	}
	if got.Mode != watchdog.ModeFeedback {
		t.Errorf("default posture = %q, want feedback: warn is indistinguishable from off on an unattended run, and enforce turns a false positive into an outage", got.Mode)
	}
	if got.Mode.Enforces() {
		t.Error("the default posture halts sessions; nothing that stops a workload should arrive without somebody asking for it")
	}
}

// A bad value from either source is an error naming that source. The
// two error prefixes matter: "--watchdog" sends the operator to the
// invocation and "safety.watchdog" sends them to the bundle, and a run
// that resolves to the default instead sends them nowhere.
func TestResolveWatchdogRefusesUnknownPostures(t *testing.T) {
	tests := []struct {
		name     string
		in       watchdogInputs
		wantWord string
	}{
		{"bad flag", watchdogInputs{Flag: "halt"}, "--watchdog"},
		{"bad bundle value", watchdogInputs{Bundle: "halt"}, "safety.watchdog"},
		{"a bad flag is refused even when the bundle is valid", watchdogInputs{Flag: "halt", Bundle: "warn"}, "--watchdog"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveWatchdog(tc.in)
			if err == nil {
				t.Fatalf("resolveWatchdog(%+v) = %+v, want an error", tc.in, got)
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error = %q, does not name %s", err, tc.wantWord)
			}
			// The rungs have to be in the message; an operator who
			// mistyped one needs the list, not just a rejection.
			for _, rung := range []string{"warn", "feedback", "enforce"} {
				if !strings.Contains(err.Error(), rung) {
					t.Errorf("error = %q, does not list the %q rung", err, rung)
				}
			}
		})
	}
}

// The startup line is the only place a posture announces itself, so the
// rendering is asserted rather than left to whatever %v does.
func TestWatchdogResolutionString(t *testing.T) {
	got := watchdogResolution{Mode: watchdog.ModeEnforce, Source: watchdogSourceBundle}.String()
	if want := "enforce [safety.watchdog]"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestBundleWatchdog(t *testing.T) {
	if got := bundleWatchdog(nil); got != "" {
		t.Errorf("bundleWatchdog(nil) = %q, want empty — one-shot and library runs have no bundle", got)
	}
	b := workload.Bundle{Safety: workload.Safety{Watchdog: "enforce"}}
	if got := bundleWatchdog(&b); got != "enforce" {
		t.Errorf("bundleWatchdog = %q, want enforce", got)
	}
}
