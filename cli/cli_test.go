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

package cli_test

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests build testdata/custombin — a main.go of the kind this
// package exists for, with one model and one tool of its own — and run
// the binary, rather than calling Main in-process. The claim is "your
// main.go is the mast binary plus what you added", and the only honest
// test of that is a binary.

func buildCustomBin(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	bin := filepath.Join(t.TempDir(), "custombin")
	build := exec.Command(goBin, "build", "-o", bin, "./testdata/custombin")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the custom binary: %v\n%s", err, out)
	}
	return bin
}

// TestACustomModelAnswersTheTurn: cli.WithModels supplies a model mast's
// providers do not know, and --model reaches it.
func TestACustomModelAnswersTheTurn(t *testing.T) {
	bin := buildCustomBin(t)
	cmd := exec.Command(bin, "--model=acme-echo", "--timeout=30s", "hello there")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("one-shot with the custom model: %v\n%s", err, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "acme says: hello there") {
		t.Fatalf("stdout = %q, want the custom model's answer", got)
	}
}

// TestAModelNameNobodyAnswersStillFails: a name the custom resolver
// declines falls through to mast's providers, which refuse it as they
// always have — the resolver does not turn every name into a model.
func TestAModelNameNobodyAnswersStillFails(t *testing.T) {
	bin := buildCustomBin(t)
	cmd := exec.Command(bin, "--model=nobody-serves-this", "--timeout=30s", "hello")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err == nil || !errorsAs(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("run = %v, want exit 1 (the work was attempted and failed)\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown model") {
		t.Fatalf("stderr does not name the refusal:\n%s", stderr.String())
	}
}

// TestACustomToolIsReachedThroughTheAllowlistAndGated: cli.WithTools adds
// a toolset the workload's specialist names in tools.mcp, the specialist
// calls the tool, and — because the workload does not classify it, so
// mast counts it as mutating — the default on_mutation policy parks the
// call for an operator instead of running it. One leg proves the reach
// and the governance together: a tool the specialist could not reach
// would never park, and a tool that bypassed the gate would have run.
func TestACustomToolIsReachedThroughTheAllowlistAndGated(t *testing.T) {
	bin := buildCustomBin(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	work := t.TempDir()
	marks := filepath.Join(work, "marks")
	if err := os.MkdirAll(marks, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin,
		"--model=toolactor",
		"--workload=testdata/workload",
		"--listen="+addr,
		"--session-db="+filepath.Join(work, "sessions.db"),
		"--auto-resume=false")
	cmd.Env = append(os.Environ(), "HOME="+work, "CUSTOMBIN_MARK_DIR="+marks)
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
		}
	})

	base := "http://" + addr
	waitFor(t, 20*time.Second, func() bool {
		resp, err := http.Get(base + "/")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return true
	}, "the daemon never answered on "+addr, &logs)

	payload := `{"kind":"test","reason":"ApplyChange","namespace":"default","name":"pod-c1","uid":"c1","message":"test","cluster":"test"}`
	resp, err := http.Post(base+"/inject", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("inject = %d, want 202\n%s", resp.StatusCode, logs.String())
	}

	var parks string
	waitFor(t, 30*time.Second, func() bool {
		resp, err := http.Get(base + "/parks")
		if err != nil {
			return false
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		parks = string(b)
		return strings.Contains(parks, "apply_change")
	}, "no park names apply_change", &logs)

	if _, err := os.Stat(filepath.Join(marks, "apply_change.ran")); err == nil {
		t.Fatalf("apply_change ran without an approval; parks: %s", parks)
	}
}

func waitFor(t *testing.T, d time.Duration, ok func() bool, msg string, logs *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s within %s\n--- daemon log ---\n%s", msg, d, logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func errorsAs(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}
