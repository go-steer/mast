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

package specialists_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/specialists"
)

const minimalSpec = "---\ndescription: Reads things.\n---\n\nDo the thing.\n"

// TestLegacyExtensionStillLoads is the compatibility half of #292. An
// out-of-tree bundle is exactly the thing this project tells people to
// write, so the rename cannot be a flag day: a .tmpl roster keeps
// loading, with the same names it had before, for one release.
func TestLegacyExtensionStillLoads(t *testing.T) {
	dir := t.TempDir()
	writeTempSpec(t, dir, "OOMKilled"+specialists.LegacyExtension, minimalSpec)

	specs, err := specialists.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("got %d specs, want 1", len(specs))
	}
	// The name is filename-derived, so the extension has to come off
	// the stem or every legacy roster silently renames itself and stops
	// matching the bundle that lists it.
	if got, want := specs[0].Name, "OOMKilled"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if !specs[0].LegacyExtension {
		t.Error("LegacyExtension = false for a .tmpl file; nothing downstream can warn")
	}
}

// TestCurrentExtensionIsNotFlagged is the other side of the same
// assertion: a deprecation warning that fires on the current spelling
// teaches operators to ignore it.
func TestCurrentExtensionIsNotFlagged(t *testing.T) {
	dir := t.TempDir()
	writeTempSpec(t, dir, "OOMKilled"+specialists.Extension, minimalSpec)

	specs, err := specialists.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "OOMKilled" {
		t.Fatalf("got %+v, want one spec named OOMKilled", specs)
	}
	if specs[0].LegacyExtension {
		t.Error("LegacyExtension = true for a .specialist.md file")
	}

	var buf bytes.Buffer
	specialists.WarnLegacyExtension(slog.New(slog.NewTextHandler(&buf, nil)), specs)
	if buf.Len() != 0 {
		t.Errorf("WarnLegacyExtension logged on an up-to-date roster: %s", buf.String())
	}
}

// TestWarnLegacyExtensionNamesTheFiles pins the warning's content, not
// just that one was emitted. "some specialists are deprecated" sends an
// operator grepping a roster of 39; the filenames are the actionable
// part, and the target spelling is what stops them guessing.
func TestWarnLegacyExtensionNamesTheFiles(t *testing.T) {
	dir := t.TempDir()
	writeTempSpec(t, dir, "OOMKilled"+specialists.LegacyExtension, minimalSpec)
	writeTempSpec(t, dir, "Evicted"+specialists.LegacyExtension, minimalSpec)
	writeTempSpec(t, dir, "BackOff"+specialists.Extension, minimalSpec)

	specs, err := specialists.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	var buf bytes.Buffer
	specialists.WarnLegacyExtension(slog.New(slog.NewTextHandler(&buf, nil)), specs)
	got := buf.String()
	if got == "" {
		t.Fatal("no warning for a roster holding two .tmpl files")
	}
	for _, want := range []string{
		"OOMKilled" + specialists.LegacyExtension,
		"Evicted" + specialists.LegacyExtension,
		specialists.Extension,
		"issues/292",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning does not mention %q: %s", want, got)
		}
	}
	// The up-to-date one must not be swept in with them.
	if strings.Contains(got, "BackOff") {
		t.Errorf("warning names an up-to-date file: %s", got)
	}
	if !strings.Contains(got, "count=2") {
		t.Errorf("warning does not carry the count: %s", got)
	}
}

// TestBothExtensionsForOneStemIsRefused is the migration hazard. A
// rename that leaves the old file behind gives a directory two copies
// of one specialist, and the realistic case is that the .tmpl is the
// stale one. Loading either would be an alphabetical accident, and
// loading the stale one means edits to the renamed file do nothing —
// so this fails the load rather than picking.
func TestBothExtensionsForOneStemIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeTempSpec(t, dir, "OOMKilled"+specialists.Extension, minimalSpec)
	writeTempSpec(t, dir, "OOMKilled"+specialists.LegacyExtension, minimalSpec)

	_, err := specialists.LoadDir(dir)
	if err == nil {
		t.Fatal("LoadDir accepted one specialist under both extensions")
	}
	for _, want := range []string{"OOMKilled", specialists.LegacyExtension, "292"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

// TestUnrelatedFilesAreStillSkipped guards the widening. LoadDir used
// to match one extension; it now matches two, and a specialists/ dir
// also holds things like a README or an editor's swap file. Matching
// those would turn a stray file into a load error for the whole
// roster.
func TestUnrelatedFilesAreStillSkipped(t *testing.T) {
	dir := t.TempDir()
	writeTempSpec(t, dir, "OOMKilled"+specialists.Extension, minimalSpec)
	for _, name := range []string{"README.md", "notes.txt", ".OOMKilled.specialist.md.swp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not a specialist\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	specs, err := specialists.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "OOMKilled" {
		t.Fatalf("got %+v, want only OOMKilled", specs)
	}
}
