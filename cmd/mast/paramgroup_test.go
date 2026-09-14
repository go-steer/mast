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

// Guards for the #293 parameter grouping.
//
// serve() reached sixteen parameters, eleven of them consecutive
// strings, and every new daemon flag landed on the end of that list. The
// grouping into workloadOpts / modelOpts / listenOpts / sessionOpts /
// resumeOpts fixed the instance; without a check, the next flag puts it
// back, one parameter at a time, exactly the way it got there. #300's
// lesson is the reason this file exists: a convention nothing measures
// is not a convention.
//
// Two rules, both read off the source with go/ast rather than asserted
// against a golden string, so a reordering that preserves the property
// does not have to be re-blessed.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strings"
	"testing"
)

// groupedFuncs are the daemon entry points #293 regrouped, with the
// parameter count each one currently has. The ceiling is the count
// itself, not a round number above it: passing something new to one of
// these should mean extending the struct that already holds the related
// arguments, and if it genuinely does not belong in any of them, bumping
// the number here is the moment to say so.
var groupedFuncs = map[string]int{
	"serve":      8,
	"buildRoot":  7,
	"dispatch":   4,
	"resume":     5,
	"runTurn":    5,
	"runTurnPre": 7,
}

// maxParamsAnywhere caps every other function in the package's
// non-test files. Nothing is near it (the largest today is
// newScheduledTrigger at nine), so it is a backstop against the shape
// rather than a style rule: a function cannot quietly grow a
// sixteen-parameter list again without this failing first.
//
// Deliberately not applied to _test.go files: test helpers are not a
// maintained surface, and the flat-list hazard is a hazard because
// production call sites are written once and read for years.
const maxParamsAnywhere = 9

// paramTypes returns one rendered type per parameter, expanding Go's
// grouped form — `workloadName, sessionID string` is two entries, which
// is the whole point, since that form is how a same-typed run is
// usually written.
func paramTypes(fset *token.FileSet, fn *ast.FuncDecl) []string {
	if fn.Type.Params == nil {
		return nil
	}
	var out []string
	for _, field := range fn.Type.Params.List {
		var b bytes.Buffer
		if err := printer.Fprint(&b, fset, field.Type); err != nil {
			b.WriteString("?")
		}
		n := len(field.Names)
		if n == 0 {
			n = 1 // unnamed parameter
		}
		for i := 0; i < n; i++ {
			out = append(out, b.String())
		}
	}
	return out
}

// parsePackageMain returns every free function declared in package main in
// this directory, indexed by name, plus the file each came from. Files are
// parsed one at a time rather than with parser.ParseDir, which is deprecated
// for ignoring build tags — here that would only matter if cmd/mast grew a
// constrained file, and the package filter below keeps the result honest
// either way.
func parsePackageMain(t *testing.T) (*token.FileSet, map[string]*ast.FuncDecl, map[string]string) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/mast: %v", err)
	}
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	inFile := map[string]string{}
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if f.Name.Name != "main" {
			continue // an external _test package is a different surface
		}
		seen++
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue // methods carry their receiver's context; the hazard is free functions
			}
			funcs[fn.Name.Name] = fn
			inFile[fn.Name.Name] = name
		}
	}
	if seen == 0 {
		t.Fatal("no package main files in cmd/mast")
	}
	return fset, funcs, inFile
}

// TestGroupedFuncsStayGrouped is the #293 guard. For each regrouped
// entry point: it still exists under that name (a rename must not make
// this check silently vacuous), it has not grown past its recorded
// parameter count, and it declares no two adjacent parameters of the
// same type — the property that makes a transposed pair at a call site
// compile and be wrong at runtime.
func TestGroupedFuncsStayGrouped(t *testing.T) {
	fset, funcs, _ := parsePackageMain(t)

	for name, want := range groupedFuncs {
		fn, ok := funcs[name]
		if !ok {
			t.Errorf("%s() is in the #293 grouped set but no such function exists in package main; "+
				"if it was renamed, rename it here too — a missing name would otherwise make this guard pass by checking nothing", name)
			continue
		}
		types := paramTypes(fset, fn)
		if len(types) > want {
			t.Errorf("%s() now takes %d parameters, was %d (#293). Put the new argument in the "+
				"options struct it belongs to (workloadOpts, modelOpts, listenOpts, sessionOpts, "+
				"resumeOpts, turnDeps); if it belongs in none of them, raise the count here and say why",
				name, len(types), want)
		}
		for i := 1; i < len(types); i++ {
			if types[i] == types[i-1] {
				t.Errorf("%s() declares adjacent parameters %d and %d both of type %s — "+
					"transposing them at a call site compiles and is wrong at runtime, which is "+
					"what #293 removed from this function",
					name, i, i+1, types[i])
				break
			}
		}
	}
}

// TestNoFunctionGrowsAFlatParameterList is the backstop for everything
// the table above does not name. It does not demand grouping; it only
// refuses the size at which a flat list stops being readable.
func TestNoFunctionGrowsAFlatParameterList(t *testing.T) {
	fset, funcs, inFile := parsePackageMain(t)

	for name, fn := range funcs {
		if strings.HasSuffix(inFile[name], "_test.go") {
			continue
		}
		if n := len(paramTypes(fset, fn)); n > maxParamsAnywhere {
			t.Errorf("%s() (%s) takes %d parameters, over the %d the package allows. "+
				"Group the related ones into a struct — see the #293 comment above runFlags",
				name, inFile[name], n, maxParamsAnywhere)
		}
	}
}
