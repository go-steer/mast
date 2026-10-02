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

package transcript_test

import (
	"go/importer"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"testing"
)

const (
	modulePath = "github.com/go-steer/mast"
	self       = modulePath + "/pkg/transcript"
	// The store this package is a read facade over. Its types reach the
	// public API through aliases, so they are this package's surface, not
	// a dependency of it.
	impl = modulePath + "/internal/transcript"
)

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
// package: all are types a consumer *receives* and never constructs, so
// there is no constructor closure to drag in, and their field sets are
// already committed on the wire as approval.DecisionSchema =
// "mast.decision/v1", emitted by `mast sessions export-decisions` and
// pinned as literals since v0.5. A transcript-owned copy would be a
// second Go spelling of one JSON schema — the drift that pin exists to
// prevent. The opposite call was made for pkg/budget, where
// *pricing.Catalog was an input a consumer had to construct: see
// pkg/budget/imports_test.go.
var frozenByReference = []string{
	// Reached directly from exported signatures here.
	"approval.AppliedEdit",   // Detail.AppliedEdits
	"approval.CaptureRecord", // Detail.Captures
	"approval.Decision",      // (*Store).Decisions

	// Reached through the records' own exported fields. ProposedChange
	// is the one #338's table missed: CaptureRecord.Revert is a
	// *ProposedChange, so the undo half of #296 is frozen too.
	"approval.Authority",      // Decision.Authority
	"approval.Disposition",    // Decision.Disposition
	"approval.Outcome",        // Decision.Outcome
	"approval.ProposedChange", // CaptureRecord.Revert
	"approval.Scope",          // Decision.Scope
}

// TestExportedAPIFreezesOnlyTheRecordedTypes type-checks this package and
// walks everything its exported API reaches — signatures, exported
// fields, exported methods, to a fixed point, through the aliases into
// the internal store — collecting every type from another package in
// this module, and compares that set with frozenByReference.
//
// The failure it exists to catch is silent: adding a field of an
// unsupported type to an exported record compiles, passes every
// behavioural test, and enlarges what v1.0 promises.
//
// It was an AST walk over this directory until #301 made this package a
// facade whose records are aliases of internal/transcript's. An AST walk
// cannot follow an alias, and the moved copy, reading ../approval from
// its new directory, stopped finding approval's fields at all — which is
// how it noticed. The type checker follows both.
func TestExportedAPIFreezesOnlyTheRecordedTypes(t *testing.T) {
	pkg, err := importer.ForCompiler(token.NewFileSet(), "source", nil).Import(self)
	if err != nil {
		t.Fatalf("type-check %s: %v", self, err)
	}
	got := map[string]string{} // "pkg.Type" -> what reached it
	seen := map[*types.Named]bool{}
	var walk func(t types.Type, from string)
	walk = func(typ types.Type, from string) {
		switch x := typ.(type) {
		case *types.Alias:
			walk(types.Unalias(x), from)
		case *types.Named:
			obj := x.Obj()
			if obj.Pkg() == nil || !strings.HasPrefix(obj.Pkg().Path(), modulePath) {
				return
			}
			if p := obj.Pkg().Path(); p != self && p != impl {
				key := obj.Pkg().Name() + "." + obj.Name()
				if _, ok := got[key]; !ok {
					got[key] = from
				}
			}
			if seen[x] {
				return
			}
			seen[x] = true
			walk(x.Underlying(), obj.Name())
			for i := 0; i < x.NumMethods(); i++ {
				if m := x.Method(i); m.Exported() {
					walk(m.Type(), obj.Name()+"."+m.Name())
				}
			}
		case *types.Pointer:
			walk(x.Elem(), from)
		case *types.Slice:
			walk(x.Elem(), from)
		case *types.Array:
			walk(x.Elem(), from)
		case *types.Map:
			walk(x.Key(), from)
			walk(x.Elem(), from)
		case *types.Signature:
			for i := 0; i < x.Params().Len(); i++ {
				walk(x.Params().At(i).Type(), from)
			}
			for i := 0; i < x.Results().Len(); i++ {
				walk(x.Results().At(i).Type(), from)
			}
		case *types.Struct:
			for i := 0; i < x.NumFields(); i++ {
				if f := x.Field(i); f.Exported() || f.Embedded() {
					walk(f.Type(), from+"."+f.Name())
				}
			}
		case *types.Interface:
			for i := 0; i < x.NumMethods(); i++ {
				if m := x.Method(i); m.Exported() {
					walk(m.Type(), from+"."+m.Name())
				}
			}
		}
	}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		if obj := scope.Lookup(name); obj.Exported() {
			walk(obj.Type(), name)
		}
	}
	if len(seen) < 5 {
		t.Fatalf("walked only %d named types; the walk is not seeing the API", len(seen))
	}

	want := map[string]bool{}
	for _, q := range frozenByReference {
		want[q] = true
	}
	var extra, missing []string
	for q, from := range got {
		if !want[q] {
			extra = append(extra, q+" (via "+from+")")
		}
	}
	for q := range want {
		if _, ok := got[q]; !ok {
			missing = append(missing, q)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	for _, q := range extra {
		t.Errorf("%s is reachable from pkg/transcript's exported API but is not in frozenByReference; "+
			"v1.0 would promise it. Either keep it out of the exported surface, or add it to the list "+
			"AND to DESIGN.md and docs/site/src/content/docs/reference/stability.md — the prose is the "+
			"promise, this list only checks it", q)
	}
	for _, q := range missing {
		t.Errorf("frozenByReference names %s, which is no longer reachable from the exported API; "+
			"drop it here and in DESIGN.md and the site's stability page, or the promise commits more "+
			"than it needs to", q)
	}
}
