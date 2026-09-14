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

package budget

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// modulePath is this module, as go.mod declares it.
const modulePath = "github.com/go-steer/mast"

// pkg/budget imports nothing else in this module, and that is a
// property rather than an accident (#338, #339).
//
// v1.0 freezes this package's exported surface (#300). A freeze is
// transitive: a type from another package reachable through an exported
// signature here is frozen too, and 32 of this module's packages are
// named as explicitly unsupported. budget.Limits carried a
// *pricing.Catalog once, which froze pkg/pricing's table shape by
// accident; the Pricer interface replaced it so that what crosses the
// seam is a number, and Detailer does the same job for the usage
// sidecar — pkg/providers/usage names budget, never the reverse.
//
// A new import here is not necessarily wrong. It is a decision about
// what v1.0 promises, and it should be made on purpose.
func TestBudgetImportsNothingElseInThisModule(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import %s: %v", name, spec.Path.Value, err)
			}
			if path == modulePath || strings.HasPrefix(path, modulePath+"/") {
				t.Errorf("%s imports %s; pkg/budget is meant to import nothing else in this module — see the comment above", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test .go files found; this test measured nothing")
	}
}
