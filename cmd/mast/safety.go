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

// Originally derived from go-steer/core-agent@56826597ee917f97523567d3c0e60032b8c716d5:cmd/core-agent/guardrails.go

package main

import (
	"fmt"
	"strings"

	"github.com/go-steer/mast/pkg/watchdog"
	"github.com/go-steer/mast/pkg/workload"
)

// Where a resolved posture came from. Constants rather than inline
// strings so the startup log and the tests assert on the same text.
const (
	watchdogSourceFlag    = "--watchdog flag"
	watchdogSourceBundle  = "safety.watchdog"
	watchdogSourceDefault = "mast default"
)

// watchdogInputs is everything resolveWatchdog needs. Split out of
// serve()'s parameter list so the precedence chain is a pure function
// with a table test rather than an inline switch nobody can exercise.
type watchdogInputs struct {
	// Flag is --watchdog. Empty means the operator left it unset —
	// which is why the flag's registered default is "" and not "warn".
	Flag string

	// Bundle is the workload's safety.watchdog. Empty means unset. A
	// run with no bundle at all (one-shot, library embed) leaves this
	// empty too, which is the same thing: nothing declared.
	Bundle string
}

// watchdogResolution is the decided posture plus the reason it came out
// that way. The startup line prints the reason so an operator can tell
// "my bundle did this" from "the default did this" without re-deriving
// the chain — the failure this exists to prevent is an operator editing
// safety.watchdog and not noticing that a flag in the deploy manifest
// has been winning all along.
type watchdogResolution struct {
	// Mode is the resolved posture. Never empty on a nil error.
	Mode watchdog.Mode
	// Source is one of the watchdogSource* constants.
	Source string
}

// String renders the resolution the way the startup log wants it.
func (r watchdogResolution) String() string {
	return fmt.Sprintf("%s [%s]", r.Mode, r.Source)
}

// resolveWatchdog decides a run's watchdog posture.
//
// Precedence is --watchdog > safety.watchdog > watchdog.DefaultMode:
// the invocation beats the bundle beats the default. The bundle sits in the
// middle rather than on top because an operator debugging a halted
// workload needs a way to drop it to warn for one run without editing
// (and later forgetting to revert) the deployed manifest.
//
// Both sources are re-validated here. workload.Load already refuses a
// bad safety.watchdog naming the file, but a bundle can also arrive as
// a plain Go value through mast.RunWorkload, and silently falling
// through to a default is the wrong answer for a field whose whole job
// is to be a backstop.
func resolveWatchdog(in watchdogInputs) (watchdogResolution, error) {
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

	switch flagMode, bundleMode := norm(in.Flag), norm(in.Bundle); {
	case flagMode != "":
		m, err := watchdog.ParseMode(flagMode)
		if err != nil {
			return watchdogResolution{}, fmt.Errorf("--watchdog: %w", err)
		}
		return watchdogResolution{Mode: m, Source: watchdogSourceFlag}, nil
	case bundleMode != "":
		m, err := watchdog.ParseMode(bundleMode)
		if err != nil {
			return watchdogResolution{}, fmt.Errorf("safety.watchdog: %w", err)
		}
		return watchdogResolution{Mode: m, Source: watchdogSourceBundle}, nil
	default:
		return watchdogResolution{Mode: watchdog.DefaultMode, Source: watchdogSourceDefault}, nil
	}
}

// bundleWatchdog reads safety.watchdog off a bundle that may be absent.
// One-shot mode and mast.Run have no bundle; both still resolve a
// posture, so the nil case has to be a value rather than a branch at
// every call site.
func bundleWatchdog(b *workload.Bundle) string {
	if b == nil {
		return ""
	}
	return b.Safety.Watchdog
}
