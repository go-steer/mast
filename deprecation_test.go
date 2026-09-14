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

package mast

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every `// Deprecated:` marker in this module names the release that
// removes it (docs/compatibility-policy.md §3, #304).
//
// The policy's cycle is "deprecated in a minor, removed in a major,
// after two released minors and 90 days". None of that is checkable
// from the tree — but the thing that makes a cycle a cycle rather than
// a permanent second way to do something is the end date, and that is
// checkable. So it is checked here rather than asked for in prose,
// because the promise this whole exercise replaced was enforced by an
// `// Experimental:` marker that was never once written (#300).
//
// The marker is also the only announcement channel that reaches a
// consumer at their own keyboard: staticcheck's SA1019 and gopls both
// read this exact form. A marker without a removal version tells them
// to move without telling them by when.
//
// Scope is the whole module, deliberately, and not only the six covered
// paths. An unsupported package may break without a cycle — but if it
// goes to the trouble of announcing one, the announcement has to mean
// something. mast's only marker for eight releases was an inherited one
// in an unsupported package with no end at all, which is the case this
// check exists to make impossible to repeat.

// deprecationMarker is the godoc convention: a paragraph beginning with
// "Deprecated:" in a declaration's doc comment.
const deprecationMarker = "Deprecated:"

// removalVersion is the sentence the policy requires: "Removed in
// v2.0." — a major and a minor, optionally a patch.
var removalVersion = regexp.MustCompile(`Removed in v[0-9]+\.[0-9]+(\.[0-9]+)?\b`)

// deprecationProblems returns one message per marker in src that does
// not name its removal version. It takes source text rather than a path
// so the checker itself can be exercised on fixtures — a tree walk that
// finds nothing is not evidence that it would find something.
func deprecationProblems(label, src string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, label, src, parser.ParseComments)
	if err != nil {
		return []string{label + ": parse: " + err.Error()}
	}
	var problems []string
	for _, group := range f.Comments {
		for _, para := range strings.Split(group.Text(), "\n\n") {
			para = strings.TrimSpace(para)
			if !strings.HasPrefix(para, deprecationMarker) {
				continue
			}
			if removalVersion.MatchString(para) {
				continue
			}
			pos := fset.Position(group.Pos())
			problems = append(problems, label+":"+strconv.Itoa(pos.Line)+
				": deprecation marker does not name its removal version.\n"+
				"\tgot:  "+strings.ReplaceAll(para, "\n", " ")+"\n"+
				"\twant: \""+deprecationMarker+" <what to use instead>. Removed in vX.Y.\"\n"+
				"\tsee docs/compatibility-policy.md §3 — a deprecation with no end\n"+
				"\tis a permanent second way to do something, not a cycle")
		}
	}
	return problems
}

func TestDeprecationMarkersNameTheirRemoval(t *testing.T) {
	var problems []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// testdata holds fixtures that are not this module's
			// API; node_modules is the docs site's.
			case ".git", "node_modules", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		problems = append(problems, deprecationProblems(path, string(src))...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// The tree currently holds no deprecation markers at all, so the walk
// above passes vacuously. This is what shows it would not: the checker
// is run against both shapes directly.
func TestDeprecationCheckerReadsTheMarker(t *testing.T) {
	const undated = `package p

// Old is the previous spelling.
//
// Deprecated: use New.
type Old interface{ M() }
`
	const dated = `package p

// Old is the previous spelling.
//
// Deprecated: use New. Removed in v2.0.
type Old interface{ M() }
`
	const unrelated = `package p

// New mentions the word deprecated in prose, which is not a marker.
type New interface{ M() }
`
	if got := deprecationProblems("undated.go", undated); len(got) != 1 {
		t.Errorf("undated marker: got %d problems, want 1: %v", len(got), got)
	} else if !strings.Contains(got[0], "Removed in vX.Y") {
		t.Errorf("undated marker: message does not state the wanted form: %s", got[0])
	}
	if got := deprecationProblems("dated.go", dated); len(got) != 0 {
		t.Errorf("dated marker: got %d problems, want 0: %v", len(got), got)
	}
	if got := deprecationProblems("unrelated.go", unrelated); len(got) != 0 {
		t.Errorf("prose mentioning deprecation: got %d problems, want 0: %v", len(got), got)
	}
}
