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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/specialists"
)

const minimalSpec = "---\ndescription: Reads things.\n---\n\nDo the thing.\n"

// legacySuffix is the extension #349 removed. Spelled out here rather
// than read from the package, because the constant that used to be
// exported no longer is — and that is the point of the test: the
// operator's file has this suffix on disk whatever the package calls
// it.
const legacySuffix = ".tmpl"

// TestLegacyExtensionIsRefusedNotSkipped is the removal half of #292,
// filed as #349. The failure this guards against is not that .tmpl
// stopped working — it is that it stopped working *quietly*. LoadDir
// skips what it does not recognise, so without this refusal a .tmpl
// roster loads as zero specialists and the operator's next error is
// about a bundle reference, in a different package, naming a file that
// is sitting right there.
func TestLegacyExtensionIsRefusedNotSkipped(t *testing.T) {
	dir := t.TempDir()
	writeTempSpec(t, dir, "OOMKilled"+legacySuffix, minimalSpec)

	specs, err := specialists.LoadDir(dir)
	if err == nil {
		t.Fatalf("LoadDir accepted a %s roster (returned %d spec(s)) instead of refusing it; "+
			"measured on pkg/config, the operator's next error is "+
			`config: workload %%q references specialist "OOMKilled" not found in <dir>`+
			" — about a missing specialist, not a renamed one", legacySuffix, len(specs))
	}
	// The actionable parts: which file, what to rename it to, and where
	// the rename is written down.
	for _, want := range []string{"OOMKilled" + legacySuffix, specialists.Extension, "issues/292"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

// TestLegacyRefusalNamesEveryFile keeps the message usable on a real
// roster. mast's own is 39 files; "some of your specialists use the old
// extension" sends an operator grepping, and a rename they do one file
// at a time means one failed boot per file.
func TestLegacyRefusalNamesEveryFile(t *testing.T) {
	dir := t.TempDir()
	writeTempSpec(t, dir, "OOMKilled"+legacySuffix, minimalSpec)
	writeTempSpec(t, dir, "Evicted"+legacySuffix, minimalSpec)
	writeTempSpec(t, dir, "BackOff"+specialists.Extension, minimalSpec)

	_, err := specialists.LoadDir(dir)
	if err == nil {
		t.Fatal("LoadDir accepted a roster holding two .tmpl files")
	}
	for _, want := range []string{"OOMKilled" + legacySuffix, "Evicted" + legacySuffix} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
	// The up-to-date file is not the problem and must not be listed as
	// one, or the operator renames a file that was already correct.
	if strings.Contains(err.Error(), "BackOff") {
		t.Errorf("refusal names an up-to-date file: %v", err)
	}
}

// TestLegacyRefusalBeatsAnUnrelatedParseError pins the ordering. A
// pre-v0.9 roster mid-migration plausibly holds both a leftover .tmpl
// and a file someone was editing when the boot failed. The extension
// refusal is the one that explains the upgrade, so it has to win —
// otherwise the operator fixes the frontmatter, boots again, and only
// then learns about the rename.
func TestLegacyRefusalBeatsAnUnrelatedParseError(t *testing.T) {
	dir := t.TempDir()
	writeTempSpec(t, dir, "OOMKilled"+legacySuffix, minimalSpec)
	// No frontmatter: a hard LoadFile error on its own.
	writeTempSpec(t, dir, "Broken"+specialists.Extension, "no frontmatter here\n")

	_, err := specialists.LoadDir(dir)
	if err == nil {
		t.Fatal("LoadDir accepted a directory with both a .tmpl and a malformed spec")
	}
	if !strings.Contains(err.Error(), legacySuffix) {
		t.Errorf("a malformed sibling won the race; operator never hears about the rename: %v", err)
	}
}

// TestOneStemUnderBothExtensionsIsRefused is the migration hazard from
// #292, which the removal does not retire: a rename that leaves the old
// file behind. Before v0.9 both loaded and picking one would have been
// an alphabetical accident; now the .tmpl half is refused outright,
// which is a strictly better answer to the same mistake — but it is
// still worth a test, because "the stale copy is ignored and the roster
// boots" is the outcome that loses someone's edits.
func TestOneStemUnderBothExtensionsIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeTempSpec(t, dir, "OOMKilled"+specialists.Extension, minimalSpec)
	writeTempSpec(t, dir, "OOMKilled"+legacySuffix, minimalSpec)

	_, err := specialists.LoadDir(dir)
	if err == nil {
		t.Fatal("LoadDir accepted one specialist under both extensions")
	}
	if !strings.Contains(err.Error(), "OOMKilled"+legacySuffix) {
		t.Errorf("refusal does not name the stale copy: %v", err)
	}
}

// TestUnrelatedFilesAreStillSkipped guards the other direction. A
// specialists/ dir also holds things like a README or an editor's swap
// file, and matching those would turn a stray file into a load error
// for the whole roster. Skipping is right *here* and wrong for .tmpl
// for one reason: nobody ever wrote a specialist called README.md.
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
