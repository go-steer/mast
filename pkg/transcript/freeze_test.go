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

package transcript

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// modulePath is this module, as go.mod declares it.
const modulePath = "github.com/go-steer/mast"

// frozenByReference is every type from an otherwise-unsupported package
// in this module that v1.0's freeze commits *through* pkg/transcript's
// exported API (#300, #338 task 2). DESIGN.md § "The v1.0 stability
// promise" and the site's stability page carry the same list in prose;
// this is the copy a compiler reads.
//
// The entries are not a wish. A freeze is transitive through exported
// signatures whether or not a document says so, so the only question
// this list answers is whether the transitivity is known. Adding a line
// here is a decision to promise one more type at v1.0; it is legitimate,
// and it is not something to do while fixing a test.
//
// Why these are frozen by reference rather than copied into this
// package: all four are types a consumer *receives* and never
// constructs, so there is no constructor closure to drag in, and their
// field sets are already committed on the wire as
// approval.DecisionSchema = "mast.decision/v1", emitted by
// `mast sessions export-decisions` and pinned as literals since v0.5. A
// transcript-owned copy would be a second Go spelling of one JSON
// schema — the drift that pin exists to prevent. The opposite call was
// made for pkg/budget, where *pricing.Catalog was an input a consumer
// had to construct: see pkg/budget/imports_test.go.
var frozenByReference = []string{
	// Reached directly from exported signatures here.
	"approval.AppliedEdit",   // Detail.AppliedEdits
	"approval.CaptureRecord", // Detail.Captures
	"approval.Decision",      // (*Store).Decisions

	// Reached through the records' own exported fields. ProposedChange
	// is the one #338's table missed: CaptureRecord.Revert is a
	// *ProposedChange, so the undo half of #296 is frozen too. It is the
	// right answer — a revert an operator can be handed has to be the
	// same shape as a change they can approve — but it was reached by
	// following the fields, not by reading the issue.
	"approval.Authority",      // Decision.Authority
	"approval.Disposition",    // Decision.Disposition
	"approval.Outcome",        // Decision.Outcome
	"approval.ProposedChange", // CaptureRecord.Revert
	"approval.Scope",          // Decision.Scope
}

// TestExportedAPIFreezesOnlyTheRecordedTypes walks this package's
// exported declarations, collects every qualified type from elsewhere
// in this module that appears in one, closes over the fields of the
// ones it finds, and compares the result with frozenByReference.
//
// The failure it exists to catch is silent: adding a field of an
// unsupported type to an exported struct here compiles, passes every
// behavioural test, and enlarges what v1.0 promises. #300 found the
// corpus had spent seven releases enforcing its exceptions with an
// `// Experimental:` marker that was never once written — a boundary
// nobody writes is not a boundary.
func TestExportedAPIFreezesOnlyTheRecordedTypes(t *testing.T) {
	direct, files := scanExportedInModuleRefs(t, ".")
	if files == 0 {
		t.Fatal("no non-test .go files found; this test measured nothing")
	}

	// Close over the fields of each referenced type, within its own
	// package. One level is enough only if it reaches a fixed point, so
	// iterate until it does.
	got := map[string]bool{}
	for k := range direct {
		got[k] = true
	}
	for {
		grew := false
		for qual := range got {
			pkgName, typeName, ok := strings.Cut(qual, ".")
			if !ok {
				continue
			}
			for _, ref := range fieldRefsOf(t, filepath.Join("..", pkgName), typeName) {
				if !got[ref] {
					got[ref] = true
					grew = true
				}
			}
		}
		if !grew {
			break
		}
	}

	want := map[string]bool{}
	for _, q := range frozenByReference {
		want[q] = true
	}
	for q := range got {
		if !want[q] {
			t.Errorf("%s is reachable from pkg/transcript's exported API but is not in frozenByReference; "+
				"v1.0 would promise it. Either keep it out of the exported surface, or add it to the list "+
				"AND to DESIGN.md and docs/site/src/content/docs/reference/stability.md — the prose is the "+
				"promise, this list only checks it", q)
		}
	}
	for q := range want {
		if !got[q] {
			t.Errorf("frozenByReference names %s, which is no longer reachable from the exported API; "+
				"drop it here and in DESIGN.md and the site's stability page, or the promise commits more "+
				"than it needs to", q)
		}
	}
}

// scanExportedInModuleRefs parses dir's non-test files and returns every
// `pkg.Type` appearing in an exported declaration, where pkg resolves to
// another package in this module.
func scanExportedInModuleRefs(t *testing.T, dir string) (map[string]bool, int) {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files++
		inModule := inModuleImports(f)
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !exportedFunc(d) {
					continue
				}
				collectQualified(d.Type, inModule, out)
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || !ts.Name.IsExported() {
						continue
					}
					collectExportedFieldTypes(ts.Type, inModule, out)
				}
			}
		}
	}
	return out, files
}

// exportedFunc reports whether a declaration is part of the package's
// API: an exported function, or an exported method on an exported type.
// An exported method on an unexported type is not reachable.
func exportedFunc(d *ast.FuncDecl) bool {
	if !d.Name.IsExported() {
		return false
	}
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return true
	}
	return rootIdent(d.Recv.List[0].Type).IsExported()
}

func rootIdent(e ast.Expr) *ast.Ident {
	switch t := e.(type) {
	case *ast.StarExpr:
		return rootIdent(t.X)
	case *ast.IndexExpr:
		return rootIdent(t.X)
	case *ast.IndexListExpr:
		return rootIdent(t.X)
	case *ast.Ident:
		return t
	}
	return ast.NewIdent("_")
}

// collectExportedFieldTypes walks a type expression, descending into
// struct fields only when the field itself is exported — an unexported
// field's type is not part of the promise.
func collectExportedFieldTypes(e ast.Expr, inModule map[string]string, out map[string]bool) {
	st, ok := e.(*ast.StructType)
	if !ok {
		collectQualified(e, inModule, out)
		return
	}
	if st.Fields == nil {
		return
	}
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 { // embedded
			collectQualified(f.Type, inModule, out)
			continue
		}
		for _, n := range f.Names {
			if n.IsExported() {
				collectQualified(f.Type, inModule, out)
				break
			}
		}
	}
}

// collectQualified records every selector `x.Sel` in e whose x names an
// import of another package in this module.
func collectQualified(e ast.Node, inModule map[string]string, out map[string]bool) {
	ast.Inspect(e, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if pkgName, ok := inModule[id.Name]; ok && sel.Sel.IsExported() {
			out[pkgName+"."+sel.Sel.Name] = true
		}
		return true
	})
}

// fieldRefsOf parses dir and returns the in-module qualified types
// appearing in typeName's exported fields, plus typeName's own package
// siblings referenced by bare identifier.
func fieldRefsOf(t *testing.T, dir, typeName string) []string {
	t.Helper()
	pkgName := filepath.Base(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		inModule := inModuleImports(f)
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != typeName {
					continue
				}
				qual := map[string]bool{}
				collectExportedFieldTypes(ts.Type, inModule, qual)
				for q := range qual {
					out = append(out, q)
				}
				// Same-package named types, which appear as bare
				// exported identifiers rather than selectors.
				for _, local := range localExportedRefs(ts.Type) {
					out = append(out, pkgName+"."+local)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// localExportedRefs returns exported identifiers used as the type of an
// exported field, excluding the universe's own (error, any, …).
func localExportedRefs(e ast.Expr) []string {
	st, ok := e.(*ast.StructType)
	if !ok || st.Fields == nil {
		return nil
	}
	var out []string
	for _, f := range st.Fields.List {
		exported := len(f.Names) == 0
		for _, n := range f.Names {
			if n.IsExported() {
				exported = true
			}
		}
		if !exported {
			continue
		}
		ast.Inspect(f.Type, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				_ = sel
				return false // qualified names are collectQualified's job
			}
			id, ok := n.(*ast.Ident)
			if !ok || !id.IsExported() {
				return true
			}
			out = append(out, id.Name)
			return true
		})
	}
	return out
}

// inModuleImports maps each import's local name to its package's base
// name, for imports inside this module only.
func inModuleImports(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || !strings.HasPrefix(path, modulePath+"/") {
			continue
		}
		base := path[strings.LastIndex(path, "/")+1:]
		local := base
		if spec.Name != nil {
			local = spec.Name.Name
		}
		out[local] = base
	}
	return out
}
