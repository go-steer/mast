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

package digest_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const importPath = "github.com/go-steer/mast/pkg/digest"

// This package has no caller in mast, and three surfaces are annotated
// on the strength of that: attach.UsageInfo.DigestMethods says it is
// never populated, the attach protocol's v1.2.0/v1.3.0 tool-result
// sidecars say mast emits neither, and Savings.Subagent* says nothing
// fills it. Each of those is true only while nothing imports this
// package — the day something does, they become three lies in the
// three places an operator goes to find out what the daemon reports.
//
// So the claim is a test rather than a comment. A failure here is not
// a regression: it means #221 was answered with "wire it", and the
// work left is to go correct the annotations this test names.
func TestNothingInMastImportsDigest(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	// Guard against the walk silently covering nothing — an empty scan
	// would pass this test while measuring the absence of files rather
	// than the absence of imports.
	scanned := 0
	var importers []string

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch {
			case path == filepath.Join(root, "pkg", "digest"):
				return fs.SkipDir // the package's own files import it by definition
			case strings.HasPrefix(d.Name(), ".") && path != root:
				return fs.SkipDir
			case d.Name() == "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		scanned++

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
		if err != nil {
			// A file that does not parse cannot import anything, and
			// the compiler is the right place to hear about it.
			return nil
		}
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err == nil && p == importPath {
				rel, _ := filepath.Rel(root, path)
				importers = append(importers, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d .go files under %s; the walk is not covering the repo", scanned, root)
	}
	if len(importers) > 0 {
		t.Errorf("pkg/digest is now imported by %v.\n"+
			"That is good news (#221), and it makes three annotations wrong. Update them:\n"+
			"  - attach.UsageInfo.DigestMethods / DigestMethodsInfo — \"never populated on this server\"\n"+
			"  - pkg/attach/events.go protocol log v1.2.0 + v1.3.0 — \"mast emits neither sidecar\"\n"+
			"  - digest.Savings.Subagent* and this package's doc comment — \"there are no callers\"\n"+
			"then delete this test.", importers)
	}
}
