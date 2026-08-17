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
	"fmt"
	"testing"

	"github.com/go-steer/mast/pkg/watchdog"
	"github.com/go-steer/mast/pkg/workload"
)

// Every rung loads, and loads as itself. The empty case is the one
// worth stating out loud: unset is not "warn", it is "leave it to the
// host", and that is what lets --watchdog override a bundle.
func TestLoad_SafetyWatchdog(t *testing.T) {
	for _, posture := range []string{
		workload.WatchdogWarn, workload.WatchdogFeedback, workload.WatchdogEnforce,
	} {
		t.Run(posture, func(t *testing.T) {
			path := writeBundle(t, "b.yaml",
				fmt.Sprintf("name: x\nspecialists: [a]\nsafety:\n  watchdog: %s\n", posture))
			b, err := workload.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if b.Safety.Watchdog != posture {
				t.Errorf("Safety.Watchdog = %q, want %q", b.Safety.Watchdog, posture)
			}
		})
	}

	silent := writeBundle(t, "c.yaml", "name: x\nspecialists: [a]\n")
	sb, err := workload.Load(silent)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if sb.Safety.Watchdog != "" {
		t.Errorf("Safety.Watchdog = %q on a bundle that declares none, want empty — an unset posture must stay distinguishable from a declared warn", sb.Safety.Watchdog)
	}
}

// The tripwire for the vocabulary copy. pkg/workload is stdlib-only by
// design, so its Watchdog* constants are string copies of
// pkg/watchdog's Mode constants rather than the constants themselves.
// This test lives in the test binary, where importing pkg/watchdog
// costs the production dependency graph nothing, and fails the moment
// the two lists disagree in either direction.
func TestSafetyWatchdogVocabularyMatchesTheWatchdog(t *testing.T) {
	// Forward: everything the loader accepts, the watchdog parses to
	// the mode of the same name.
	for _, posture := range []string{
		workload.WatchdogWarn, workload.WatchdogFeedback, workload.WatchdogEnforce,
	} {
		m, err := watchdog.ParseMode(posture)
		if err != nil {
			t.Errorf("the loader accepts %q but watchdog.ParseMode rejects it: %v", posture, err)
			continue
		}
		if string(m) != posture {
			t.Errorf("watchdog.ParseMode(%q) = %q; the bundle vocabulary and the runtime vocabulary have drifted", posture, m)
		}
	}

	// Backward: a fourth rung added to pkg/watchdog changes ParseMode's
	// "want" list, and this comparison is what notices. Without it, the
	// new posture would be a value the runtime honors and every bundle
	// declaring it is refused at load.
	_, err := watchdog.ParseMode("not-a-posture")
	if err == nil {
		t.Fatal("watchdog.ParseMode accepted a nonsense posture")
	}
	want := fmt.Sprintf(`unknown watchdog mode "not-a-posture" (want %q, %q or %q)`,
		workload.WatchdogWarn, workload.WatchdogFeedback, workload.WatchdogEnforce)
	if err.Error() != want {
		t.Errorf("watchdog.ParseMode error =\n  %s\nwant\n  %s\npkg/watchdog's ladder changed; update workload.Watchdog* and the loader's validate switch to match", err, want)
	}
}
