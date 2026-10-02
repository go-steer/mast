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
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The exit codes are a frozen contract, and until #301 every one of them
// was an os.Exit in run(). Main returns them instead, so a custom
// main.go decides when the process ends. cli_surface_test.go pins what
// the codes are; this pins that each path still produces its own. A
// conversion that turned a refusal into a fall-through would otherwise
// exit 0 having done nothing.
//
// Exit 3 is not here: it needs a serve whose drain expires with an
// interrupted session, which scripts/uat-v0.2.sh's S4-exit3 leg drives
// against the real binary.
func TestMainReturnsTheContractedExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"help is not an error", []string{"-h"}, exitOK},
		{"version", []string{"--version"}, exitOK},
		{"an unknown flag", []string{"--no-such-flag"}, exitUsage},
		{"an unknown task class", []string{"--task=no-such-class", "hi"}, exitUsage},
		{"a typo'd watchdog posture", []string{"--watchdog=loud"}, exitUsage},
		{"a serve flag in one-shot", []string{"--workload=x", "hi"}, exitUsage},
		{"a flag after the prompt", []string{"hi", "--session-db=x"}, exitUsage},
		{"--task without a prompt", []string{"--task=chat"}, exitUsage},
		{"a one-shot turn that completes", []string{"--model=echo", "--timeout=30s", "hi"}, exitOK},
		{"a serve whose workload does not exist", []string{
			"--model=echo", "--listen=127.0.0.1:0", "--auto-resume=false",
			"--workload=" + filepath.Join(t.TempDir(), "absent"),
		}, exitFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Main(context.Background(), tc.args); got != tc.want {
				t.Errorf("Main(%q) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// TestCancellingMainsContextDrainsServeMode is the one behaviour Main
// adds over the binary it replaced: a caller's context is the parent of
// serve's signal context, so cancelling it is a SIGTERM — the same
// drain, and with nothing interrupted, exit 0. cmd/mast passes
// context.Background(), so the binary cannot tell the difference.
func TestCancellingMainsContextDrainsServeMode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, []string{"--model=echo", "--listen=127.0.0.1:0", "--auto-resume=false"})
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case got := <-done:
		if got != exitOK {
			t.Fatalf("Main after cancel = %d, want %d (a clean drain)", got, exitOK)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve mode did not return after its context was cancelled")
	}
}
