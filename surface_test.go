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

package mast_test

import (
	"bufio"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// readSurface parses dev/api-surface.txt into kind → directories.
func readSurface(t *testing.T) map[string][]string {
	t.Helper()
	f, err := os.Open(filepath.Join("dev", "api-surface.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string][]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || (fields[0] != "promised" && fields[0] != "unsupported") {
			t.Fatalf("dev/api-surface.txt: cannot read %q", line)
		}
		out[fields[0]] = append(out[fields[0]], fields[1])
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestThePromisedListHasOneSource: the API gate reads its packages from
// dev/api-surface.txt and the auth-import guard reads them from
// actor_test.go. Two lists of one thing drift, and the drift is silent in
// the worst direction — a package promised in one and not measured by the
// other.
func TestThePromisedListHasOneSource(t *testing.T) {
	got := append([]string(nil), readSurface(t)["promised"]...)
	want := append([]string(nil), promised...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dev/api-surface.txt promises %v, actor_test.go's promised lists %v", got, want)
	}
}

// TestEveryImportablePackageIsADecision walks the module for packages a
// consumer could import — not package main, not under internal/, not in
// testdata, examples or the repo's own tooling — and fails on one
// dev/api-surface.txt does not list. #301 moved thirty-two packages out of
// reach because nothing had ever decided they should be in it; this is
// what stops the next one arriving the same way.
func TestEveryImportablePackageIsADecision(t *testing.T) {
	listed := map[string]bool{}
	for _, dirs := range readSurface(t) {
		for _, d := range dirs {
			listed[d] = true
		}
	}
	skipDir := map[string]bool{"internal": true, "testdata": true, "examples": true, "dev": true, "cmd": true, "docs": true, "node_modules": true, ".claude": true, ".git": true}
	found := map[string]bool{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && (skipDir[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly)
		if err != nil {
			return err
		}
		if f.Name.Name != "main" {
			found[filepath.ToSlash(filepath.Dir(path))] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for dir := range found {
		if !listed[dir] {
			t.Errorf("%s is importable and dev/api-surface.txt does not list it; promise it, mark it unsupported with a reason, or move it under internal/", dir)
		}
	}
	for dir := range listed {
		if !found[dir] {
			t.Errorf("dev/api-surface.txt lists %s, which is not an importable package", dir)
		}
	}
	// Vacuity floor: the root, cli and the four pkg/ paths at least.
	if len(found) < 6 {
		t.Fatalf("found only %d importable packages; the walk is not seeing the module", len(found))
	}
}
