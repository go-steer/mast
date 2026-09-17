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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests in sessiondb_test.go pin the decision; these boot the real
// binary, because a decision function that nothing calls would pass
// every one of them while `mast --attach-listen` still exited at
// startup. What #329 is about is the daemon starting, so that is what
// is asserted here.

// buildMast compiles the binary under test.
func buildMast(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	bin := filepath.Join(t.TempDir(), "mast")
	build := exec.Command(goBin, "build", "-o", bin, ".")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mast: %v\n%s", err, out)
	}
	return bin
}

// bootMast starts the daemon with an isolated HOME and returns what it
// logged plus how it exited, empty meaning it was still serving. HOME is
// per-test (house rule #5): an implied store must never land in the
// developer's real home directory.
//
// The wait is on the daemon saying it is listening rather than on a
// fixed sleep — a boot that succeeds takes about a second, and a
// hard-coded settle time would pay the worst case on every run.
func bootMast(t *testing.T, home string, extra ...string) (string, string) {
	t.Helper()
	bin := buildMast(t)
	args := append([]string{
		"--workload=../../examples/workloads/gke-triage",
		"--model=echo",
		"--listen=127.0.0.1:0",
		"--attach-listen=127.0.0.1:0",
	}, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "MAST_ATTACH_TOKEN=t", "MAST_INJECT_TOKEN=t")
	// The daemon's copier writes while this goroutine polls; syncBuffer
	// (configidentity_test.go) is the package's existing answer to that.
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mast: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	deadline := time.After(30 * time.Second)
	for {
		select {
		case err := <-done:
			return out.String(), errString(err)
		case <-deadline:
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("mast neither listened nor exited within 30s:\n%s", out.String())
		case <-time.After(50 * time.Millisecond):
			if strings.Contains(out.String(), "inject server listening") {
				_ = cmd.Process.Kill()
				<-done
				return out.String(), "" // still serving when we stopped it
			}
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The #329 gate: the daemon shape mast exists for, started the way a
// first-time operator starts it.
func TestAttachWithoutSessionDBBootsAndImpliesOne(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and boots the binary")
	}
	home := t.TempDir()
	log, exit := bootMast(t, home)
	if exit != "" {
		t.Fatalf("mast exited (%s) instead of implying a session db:\n%s", exit, log)
	}
	db := filepath.Join(home, ".mast", "sessions.db")
	if _, err := os.Stat(db); err != nil {
		t.Errorf("no implied session db at %s: %v\n%s", db, err, log)
	}
	// The line has to name both the cause and the way to move the file:
	// a database appearing in a home directory nobody named is the
	// surprise this sentence exists to prevent.
	for _, want := range []string{"implied by --attach-listen", db, "--session-db="} {
		if !strings.Contains(log, want) {
			t.Errorf("startup log does not mention %q:\n%s", want, log)
		}
	}
	// #329's last bullet: making a durable store the attach default must
	// not promote a pre-existing non-error to the log. A session that has
	// never been written is the ordinary state of a cold boot.
	if strings.Contains(log, `"level":"ERROR"`) {
		t.Errorf("a cold boot on a fresh implied store logged an error:\n%s", log)
	}
}

// An operator who passes a path keeps it; nobody's database moves, and
// nothing is created in the home directory.
func TestAttachWithAnExplicitSessionDBIsUntouched(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and boots the binary")
	}
	home := t.TempDir()
	named := filepath.Join(t.TempDir(), "named.db")
	log, exit := bootMast(t, home, "--session-db="+named)
	if exit != "" {
		t.Fatalf("mast exited (%s):\n%s", exit, log)
	}
	if _, err := os.Stat(named); err != nil {
		t.Errorf("the named session db was not opened at %s: %v\n%s", named, err, log)
	}
	if _, err := os.Stat(filepath.Join(home, ".mast")); err == nil {
		t.Errorf("an implied store was created despite an explicit --session-db:\n%s", log)
	}
	if strings.Contains(log, "implied by --attach-listen") {
		t.Errorf("startup claims an implication that did not happen:\n%s", log)
	}
}

// The two shapes the implication must refuse rather than paper over.
func TestAttachRefusesWhatItCannotImply(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and boots the binary")
	}
	for _, tc := range []struct{ name, flag, want string }{
		{
			// A string flag cannot tell "unset" from "explicitly empty"
			// by value, so this is the case that would be swallowed
			// without the explicit-flag map.
			name: "an explicitly empty --session-db",
			flag: "--session-db=",
			want: "with an empty --session-db",
		},
		{
			name: "postgres with no DSN",
			flag: "--session-db-driver=postgres",
			want: "cannot be invented",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, exit := bootMast(t, t.TempDir(), tc.flag)
			if exit == "" {
				t.Fatalf("mast kept running; it should have refused:\n%s", log)
			}
			if !strings.Contains(log, tc.want) {
				t.Errorf("refusal does not say %q:\n%s", tc.want, log)
			}
		})
	}
}
