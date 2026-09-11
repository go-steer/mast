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

package specialists

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadTmpl(t *testing.T, frontmatter string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "analyst.tmpl")
	body := "---\n" + frontmatter + "---\n\nDo the thing.\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFile(path)
	return err
}

// TestUnknownFrontmatterKeyIsRefused applies #302's decision to the
// other artifact an operator writes by hand.
func TestUnknownFrontmatterKeyIsRefused(t *testing.T) {
	err := loadTmpl(t, "description: Reads things.\nmodle: gemini-3.5-flash\n")
	if err == nil {
		t.Fatal("a misspelled frontmatter key loaded; it must be refused")
	}
	if !strings.Contains(err.Error(), "modle") {
		t.Errorf("refusal does not name the key: %v", err)
	}
}

// TestMisspelledToolsKeyIsRefused is the case that decided it, and the
// one place in this file where the old silence was actively unsafe.
//
// ToolAllowlist.InheritsAllMCP() is `MCP == nil`, so an absent tools:
// block means the specialist inherits *every* MCP server the workload
// wires. Misspell the key and the narrow allowlist the file spells out
// is not narrowed at all — the specialist silently receives the whole
// surface, which is the opposite of what the document says, and nothing
// downstream can tell the difference between "inherited because the
// author meant to" and "inherited because a letter was wrong".
//
// Note the contrast with capability:, which is why that one is not the
// example here — an absent capability resolves to read_only, so
// misspelling it fails toward the safe value. tools: fails the other
// way, and it is the reason this loader is strict rather than warning.
func TestMisspelledToolsKeyIsRefused(t *testing.T) {
	err := loadTmpl(t, "description: Reads things.\ntoosl:\n  mcp: []\n")
	if err == nil {
		t.Fatal("a misspelled tools key loaded; the specialist would silently inherit every MCP server")
	}
	if !strings.Contains(err.Error(), "toosl") {
		t.Errorf("refusal does not name the key: %v", err)
	}
}

// TestAbsentToolsStillInheritsEverything pins the behaviour the case
// above depends on. If this ever changes to default-deny, the argument
// for strictness gets weaker and the comment above should be revisited
// rather than left asserting something the code stopped doing.
func TestAbsentToolsStillInheritsEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analyst.tmpl")
	if err := os.WriteFile(path, []byte("---\ndescription: Reads things.\n---\n\nGo.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !spec.Tools.InheritsAllMCP() {
		t.Fatal("an absent tools: block no longer inherits every MCP server; revisit TestMisspelledToolsKeyIsRefused's rationale")
	}
}

// TestSpecialistsTakeNoSchemaVersion records the other half of the
// decision. The bundle got a version; the frontmatter deliberately did
// not, because a specialist is only ever reached through a bundle that
// names it. Pinned as a test so the asymmetry reads as a choice.
func TestSpecialistsTakeNoSchemaVersion(t *testing.T) {
	err := loadTmpl(t, "description: Reads things.\nschema_version: 1\n")
	if err == nil {
		t.Fatal("frontmatter accepted schema_version; specialists are governed by the bundle's version, not their own")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("refusal does not name the key: %v", err)
	}
}

// TestShippedSpecialistsStillLoad is the blast-radius guard, the
// counterpart of workload.TestShippedBundlesStillLoad. 39 .tmpl files
// ship in this tree; strictness must not have broken one.
func TestShippedSpecialistsStillLoad(t *testing.T) {
	n := 0
	err := filepath.Walk("../..", func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || filepath.Ext(p) != ".tmpl" {
			return nil
		}
		n++
		if _, lerr := LoadFile(p); lerr != nil {
			t.Errorf("shipped specialist %s no longer loads: %v", p, lerr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A walk that silently found nothing would pass while checking
	// nothing — the same trap TestNothingInMastImportsDigest named.
	if n < 30 {
		t.Fatalf("walked only %d .tmpl files; the tree has ~39, so this check is not looking where it thinks", n)
	}
}
