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
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCopyableCodeImportsNothingInternal holds the examples and the
// custom-binary fixture to what a reader who copies them can build. They
// live inside this module, so the compiler lets them import mast's
// internal/ packages — and a copy outside the module would then not
// compile. When #301 moved pkg/agent under internal/, two starters still
// imported it and every check in CI stayed green.
func TestCopyableCodeImportsNothingInternal(t *testing.T) {
	const internal = "github.com/go-steer/mast/internal"
	checked := 0
	for _, root := range []string{"examples", filepath.Join("cli", "testdata")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			checked++
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				if p == internal || strings.HasPrefix(p, internal+"/") {
					t.Errorf("%s imports %s; code meant to be copied out of this module cannot", path, p)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	// Vacuity floor: the starters, the slim embed and the custom binary.
	if checked < 4 {
		t.Fatalf("checked only %d files; the walk is not seeing the examples", checked)
	}
}
