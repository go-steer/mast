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
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// plantConfig writes a minimal bundle-shaped tree and returns its root.
func plantConfig(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "workload.yaml"), "name: demo\n")
	mustWrite(t, filepath.Join(root, "mcp.json"), `{"mcpServers":{}}`)
	mustWrite(t, filepath.Join(root, "specialists", "triage.tmpl"), body)
	mustWrite(t, filepath.Join(root, "schemas", "finding.json"), `{"type":"object"}`)
	return root
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func identify(root string) configIdentity {
	bundle := &workload.Bundle{Filename: filepath.Join(root, "workload.yaml")}
	specs := []specialists.Spec{{
		Filename:         filepath.Join(root, "specialists", "triage.tmpl"),
		OutputSchemaPath: filepath.Join(root, "schemas", "finding.json"),
	}}
	return identifyConfig(root, bundle.Filename,
		configPaths(bundle, specs, filepath.Join(root, "mcp.json")))
}

func TestConfigIdentityCoversEveryLoadedArtifact(t *testing.T) {
	root := plantConfig(t, "you are a triage specialist")
	id := identify(root)

	want := []string{"mcp.json", "schemas/finding.json", "specialists/triage.tmpl", "workload.yaml"}
	var got []string
	for _, f := range id.Files {
		got = append(got, f.Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Files = %v, want %v", got, want)
	}
	if !strings.HasPrefix(id.Digest, "sha256:") || len(id.Digest) != len("sha256:")+16 {
		t.Errorf("Digest = %q, want sha256: + 16 hex", id.Digest)
	}
	if id.Bytes == 0 || id.Newest.IsZero() {
		t.Errorf("Bytes = %d, Newest = %v, want both populated", id.Bytes, id.Newest)
	}
}

// TestConfigIdentityMovesWithContent is the property the whole thing
// rests on. An operator compares two digests; if a byte can change
// without moving one, the comparison is worse than no comparison.
func TestConfigIdentityMovesWithContent(t *testing.T) {
	root := plantConfig(t, "you are a triage specialist")
	before := identify(root)

	for _, edit := range []struct{ name, path, body string }{
		{"the bundle", filepath.Join(root, "workload.yaml"), "name: demo\nmode: multi_session\n"},
		{"a specialist", filepath.Join(root, "specialists", "triage.tmpl"), "you are a different specialist"},
		{"a schema", filepath.Join(root, "schemas", "finding.json"), `{"type":"string"}`},
		{"the catalog", filepath.Join(root, "mcp.json"), `{"mcpServers":{"k8s":{}}}`},
	} {
		t.Run(edit.name, func(t *testing.T) {
			original, err := os.ReadFile(edit.path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			t.Cleanup(func() { mustWrite(t, edit.path, string(original)) })

			mustWrite(t, edit.path, edit.body)
			if after := identify(root); after.Digest == before.Digest {
				t.Fatalf("digest unchanged at %s after editing %s", after.Digest, edit.path)
			}
		})
	}
}

// TestConfigIdentityIsIndependentOfWhereItIsMounted is why the manifest
// keys on the path relative to the root. The digest an operator computes
// from a checkout has to equal the one the pod logs from /workspace, or
// the comparison the startup line invites cannot be made.
func TestConfigIdentityIsIndependentOfWhereItIsMounted(t *testing.T) {
	a := identify(plantConfig(t, "same body"))
	b := identify(plantConfig(t, "same body"))

	if a.Digest != b.Digest {
		t.Fatalf("digests differ for identical content in two roots: %s vs %s", a.Digest, b.Digest)
	}
	if a.Root == b.Root {
		t.Fatal("the two roots are the same path; the test proves nothing")
	}
}

// TestConfigIdentityIsStableAcrossARemount pins the reason mtime is not
// in the hash. A ConfigMap remount rewrites every mtime whether or not a
// byte changed, and a digest that moved for that would make the drift
// warning fire on nothing — which is how a warning gets ignored.
func TestConfigIdentityIsStableAcrossARemount(t *testing.T) {
	root := plantConfig(t, "unchanged")
	before := identify(root)

	later := time.Now().Add(time.Hour)
	for _, f := range before.Files {
		if err := os.Chtimes(f.Path, later, later); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	after := identify(root)
	if after.Digest != before.Digest {
		t.Fatalf("digest moved on an mtime-only change: %s -> %s", before.Digest, after.Digest)
	}
	if !after.Newest.After(before.Newest) {
		t.Error("Newest did not advance, so the startup line would not show the remount at all")
	}
}

// TestConfigIdentityCountsAVanishedFile covers the projection that lost
// a key — the deployment's items: list names every file it mounts, so
// omitting one is a specialist that silently is not there. It must not
// read as "no change".
func TestConfigIdentityCountsAVanishedFile(t *testing.T) {
	root := plantConfig(t, "you are a triage specialist")
	before := identify(root)

	if err := os.Remove(filepath.Join(root, "specialists", "triage.tmpl")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	after := identify(root)

	if after.Digest == before.Digest {
		t.Fatal("digest unchanged after a projected file vanished")
	}
	if got := after.unreadable(); len(got) != 1 || got[0] != "specialists/triage.tmpl" {
		t.Errorf("unreadable() = %v, want the specialist", got)
	}
}

func TestConfigIdentityNamesWhatChanged(t *testing.T) {
	root := plantConfig(t, "before")
	before := identify(root)
	mustWrite(t, filepath.Join(root, "specialists", "triage.tmpl"), "after")

	got := identify(root).changedFrom(before)
	if len(got) != 1 || got[0] != "specialists/triage.tmpl" {
		t.Fatalf("changedFrom = %v, want just the specialist", got)
	}
}

// --- the watcher ---

// syncBuffer is a writer the watcher's goroutine and the test can both
// touch. slog writes from the goroutine; the assertions read here.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitFor polls until want appears in the log or the deadline passes.
// Polling rather than a fixed sleep: the watcher's cadence is a ticker
// and a sleep long enough to be reliable is long enough to be slow.
func waitFor(t *testing.T, log *syncBuffer, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := log.String(); strings.Contains(got, want) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in the log; got:\n%s", want, log.String())
	return ""
}

func TestWatchConfigWarnsWhenTheMountedFilesChangeUnderIt(t *testing.T) {
	root := plantConfig(t, "before")
	loaded := identify(root)

	log := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchConfig(ctx, logger, loaded, 5*time.Millisecond)

	mustWrite(t, filepath.Join(root, "specialists", "triage.tmpl"), "after")
	got := waitFor(t, log, "NO LONGER MATCHES")

	for _, want := range []string{
		loaded.Digest,             // what is running
		"specialists/triage.tmpl", // what moved
		"restart",                 // what to do
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning does not mention %q:\n%s", want, got)
		}
	}
}

// TestWatchConfigWarnsOncePerEdit keeps the warning worth reading. A
// line repeated every minute for the life of a pod is one an operator
// filters out, and this condition can last for days.
func TestWatchConfigWarnsOncePerEdit(t *testing.T) {
	root := plantConfig(t, "before")
	loaded := identify(root)

	log := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(log, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchConfig(ctx, logger, loaded, 5*time.Millisecond)

	mustWrite(t, filepath.Join(root, "specialists", "triage.tmpl"), "after")
	waitFor(t, log, "NO LONGER MATCHES")
	time.Sleep(100 * time.Millisecond) // ~20 further ticks over the same state

	if n := strings.Count(log.String(), "NO LONGER MATCHES"); n != 1 {
		t.Fatalf("warned %d times for one edit, want 1:\n%s", n, log.String())
	}
}

// TestWatchConfigSaysWhenTheDriftIsGone is the other half of the same
// argument: a stale warning about a condition somebody already fixed is
// read as current by the next person, which is worse than silence.
func TestWatchConfigSaysWhenTheDriftIsGone(t *testing.T) {
	root := plantConfig(t, "before")
	loaded := identify(root)

	log := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(log, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchConfig(ctx, logger, loaded, 5*time.Millisecond)

	spec := filepath.Join(root, "specialists", "triage.tmpl")
	mustWrite(t, spec, "after")
	waitFor(t, log, "NO LONGER MATCHES")

	mustWrite(t, spec, "before")
	waitFor(t, log, "matches the running config again")
}

// TestWatchConfigIsQuietWhenNothingChanges is the assertion that makes
// the three above mean anything.
func TestWatchConfigIsQuietWhenNothingChanges(t *testing.T) {
	root := plantConfig(t, "steady")
	log := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(log, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go watchConfig(ctx, logger, identify(root), 5*time.Millisecond)
	time.Sleep(100 * time.Millisecond)

	if got := log.String(); got != "" {
		t.Fatalf("watcher logged on an unchanged tree:\n%s", got)
	}
}

// TestWatchConfigWithNothingLoadedReturns covers --workload="", where
// buildRoot never resolves a bundle: the goroutine must exit rather than
// tick forever over an empty file list.
func TestWatchConfigWithNothingLoadedReturns(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchConfig(context.Background(), discardLogger(), configIdentity{}, time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchConfig did not return with no files to watch")
	}
}

func TestConfigIdentityLogsThePathAndTheHash(t *testing.T) {
	root := plantConfig(t, "body")
	id := identify(root)

	log := &syncBuffer{}
	id.log(slog.New(slog.NewTextHandler(log, nil)))

	got := log.String()
	for _, want := range []string{"workload config identity", id.Digest, id.Bundle, "files=4"} {
		if !strings.Contains(got, want) {
			t.Errorf("startup line does not carry %q:\n%s", want, got)
		}
	}
}
