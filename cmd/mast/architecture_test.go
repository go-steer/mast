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

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	mastModulePath    = "github.com/go-steer/mast"
	tailLineThreshold = 150
	// Existing non-test functions > 150 code lines (measured 2026-11-20):
	//   179  mast.go:runTurn
	//   177  internal/cli/main.go:run
	//   161  internal/evals/harness/judge.go:runJudge
	//   155  internal/cli/oneshot.go:runOneShot
	maxNonTestFuncsOver150Lines = 4
)

// packageLayer defines strict acyclic architecture layers across mast
// (higher layers may only import strictly lower layers: dstLayer < srcLayer).
var packageLayer = map[string]int{
	// Layer 0: leaf domain, protocol, storage, and standalone tool packages.
	"charts":                            0,
	"dev/pricing-diff":                  0,
	"dev/upstream-drift":                0,
	"examples/deploy/ax/entrypoint":     0,
	"examples/workflows/fan-out-fan-in": 0,
	"internal/auth":                     0,
	"internal/digest":                   0,
	"internal/effects":                  0,
	"internal/envelope":                 0,
	"internal/eventlog":                 0,
	"internal/federation":               0,
	"internal/injectedauth":             0,
	"internal/instruction":              0,
	"internal/kvwindow":                 0,
	"internal/modelretry":               0,
	"internal/modeltext":                0,
	"internal/modeltier":                0,
	"internal/monitor":                  0,
	"internal/notify":                   0,
	"internal/observability":            0,
	"internal/permissions":              0,
	"internal/providers/mock":           0,
	"internal/serverauth":               0,
	"internal/taskclass":                0,
	"internal/version":                  0,
	"internal/watchdog":                 0,
	"pkg/budget":                        0,

	// Layer 1: protocol servers, agent constructors, write gate, and bundle types.
	"internal/a2a":             1,
	"internal/agent":           1,
	"internal/agui":            1,
	"internal/attach":          1,
	"internal/inject":          1,
	"internal/mcp":             1,
	"internal/providers/usage": 1,
	"pkg/approval":             1,
	"pkg/workload":             1,

	// Layer 2: operator adapters, routing, transcript store, and specialists.
	"internal/attachadapter": 2,
	"internal/evals":         2,
	"internal/router":        2,
	"internal/transcript":    2,
	"internal/usagetrack":    2,
	"pkg/specialists":        2,

	// Layer 3: config discovery, graph/planner orchestration, and public transcript view.
	"examples/deploy/slim":             3,
	"examples/workflows/llm-as-router": 3,
	"internal/config":                  3,
	"internal/evals/outcome":           3,
	"internal/graph":                   3,
	"internal/planner":                 3,
	"pkg/transcript":                   3,

	// Layer 4: runtime composition and tool catalog inspection.
	"internal/compose":     4,
	"internal/toolcatalog": 4,

	// Layer 5: root library facade, CLI daemon wiring, and eval drivers.
	".":                              5,
	"internal/cli":                   5,
	"internal/evals/cmd/outcome":     5,
	"internal/evals/differentiators": 5,
	"internal/evals/judge":           5,

	// Layer 6: public CLI entrypoint, binary main, and eval harness.
	"cli":                    6,
	"cmd/mast":               6,
	"internal/evals/harness": 6,

	// Layer 7: eval CLI command over the harness.
	"internal/evals/cmd/evals": 7,
}

func TestArchitectureLayeringAndComplexityTail(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Fatalf("expected go.mod at repoRoot %s: %v", repoRoot, err)
	}

	fset := token.NewFileSet()
	allGoPkgs := make(map[string]bool)
	nonTestPkgs := make(map[string]bool)
	var over150 []string

	err = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".worktrees", ".claude", "bin", "dist", "vendor", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		pkgRel := filepath.ToSlash(filepath.Dir(rel))
		allGoPkgs[pkgRel] = true
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, raw, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}

		nonTestPkgs[pkgRel] = true
		for _, problem := range checkPackageImports(rel, pkgRel, f, packageLayer) {
			t.Error(problem)
		}

		lines := strings.Split(string(raw), "\n")
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			start := fset.Position(fn.Pos()).Line
			end := fset.Position(fn.End()).Line
			if countCodeLines(lines, start, end) > tailLineThreshold {
				over150 = append(over150, rel+":"+fn.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range checkRegisteredPackages(allGoPkgs, packageLayer) {
		t.Error(problem)
	}
	if len(allGoPkgs) < 60 || len(nonTestPkgs) < 59 {
		t.Errorf("walked %d total packages and %d non-test packages (want >= 60 and >= 59)", len(allGoPkgs), len(nonTestPkgs))
	}
	if len(over150) > maxNonTestFuncsOver150Lines {
		t.Errorf("non-test functions over %d code lines = %d (ratchet ceiling %d): %v",
			tailLineThreshold, len(over150), maxNonTestFuncsOver150Lines, over150)
	}
}

func TestArchitectureValidators(t *testing.T) {
	t.Parallel()

	layers := map[string]int{
		"internal/leaf": 0,
		"internal/mid":  1,
		".":             2,
		"cmd/mast":      3,
	}

	parseSrc := func(t *testing.T, src string) *ast.File {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), "sample.go", src, 0)
		if err != nil {
			t.Fatalf("parse sample: %v", err)
		}
		return f
	}

	// 1. Valid downward imports (including importing root package "." and external stdlib/deps).
	validCmd := parseSrc(t, `package main
import (
	"fmt"
	"github.com/go-steer/mast"
	`+"`github.com/go-steer/mast/internal/leaf`"+`
	"google.golang.org/genai"
)
`)
	if errs := checkPackageImports("cmd/mast/main.go", "cmd/mast", validCmd, layers); len(errs) != 0 {
		t.Fatalf("valid downward imports flagged: %v", errs)
	}

	// 2. Unregistered source package rejected.
	if errs := checkPackageImports("internal/unknown/x.go", "internal/unknown", validCmd, layers); len(errs) != 1 || !strings.Contains(errs[0], "not registered") {
		t.Fatalf("unregistered source errors = %v, want 1 not-registered error", errs)
	}

	// 3. Same-layer import, upward import (including root "."), and unregistered internal destination rejected.
	badMid := parseSrc(t, `package mid
import (
	"github.com/go-steer/mast/internal/mid"
	"github.com/go-steer/mast"
	"github.com/go-steer/mast/internal/ghost"
)
`)
	errs := checkPackageImports("internal/mid/mid.go", "internal/mid", badMid, layers)
	if len(errs) != 3 {
		t.Fatalf("badMid errors = %v (len %d), want 3 (same-layer, upward root, unregistered dst)", errs, len(errs))
	}

	// 4. Bidirectional package set check catches both unregistered test-only packages and stale entries with no .go files.
	exactPkgs := map[string]bool{"internal/leaf": true, "internal/mid": true, ".": true, "cmd/mast": true}
	if errs := checkRegisteredPackages(exactPkgs, layers); len(errs) != 0 {
		t.Fatalf("exact package set flagged: %v", errs)
	}
	driftedPkgs := map[string]bool{"internal/leaf": true, ".": true, "cmd/mast": true, "e2e/smoke": true}
	regErrs := checkRegisteredPackages(driftedPkgs, layers)
	if len(regErrs) != 2 {
		t.Fatalf("drifted package set errors = %v (len %d), want 2 (unregistered e2e/smoke + stale internal/mid)", regErrs, len(regErrs))
	}

	// 5. countCodeLines skips blank lines and //-comment lines, and counts code lines (including trailing comments).
	sampleLines := []string{
		"// leading doc comment",
		"func demo() {",
		"",
		"   \t  ",
		"\t// indented comment",
		"\tx := 1 // inline comment counts as code",
		"\t_ = x",
		"}",
	}
	if got := countCodeLines(sampleLines, 2, 8); got != 4 {
		t.Fatalf("countCodeLines = %d, want 4 (func header, two statements, closing brace)", got)
	}
	if got := countCodeLines(sampleLines, 1, 1); got != 0 {
		t.Fatalf("countCodeLines on comment line = %d, want 0", got)
	}
}

func checkRegisteredPackages(allGoPkgs map[string]bool, layers map[string]int) []string {
	var errs []string
	var walkedList []string
	for pkg := range allGoPkgs {
		walkedList = append(walkedList, pkg)
	}
	sort.Strings(walkedList)
	for _, pkg := range walkedList {
		if _, ok := layers[pkg]; !ok {
			errs = append(errs, fmt.Sprintf("package %q contains .go files but is not registered in packageLayer", pkg))
		}
	}
	var layerList []string
	for pkg := range layers {
		layerList = append(layerList, pkg)
	}
	sort.Strings(layerList)
	for _, pkg := range layerList {
		if !allGoPkgs[pkg] {
			errs = append(errs, fmt.Sprintf("packageLayer registers %q, which contains no .go files under repoRoot", pkg))
		}
	}
	return errs
}

func internalImportPkg(impPath string) (string, bool) {
	if impPath == mastModulePath {
		return ".", true
	}
	if after, ok := strings.CutPrefix(impPath, mastModulePath+"/"); ok {
		return after, true
	}
	return "", false
}

func checkPackageImports(relFile, pkgRel string, f *ast.File, layers map[string]int) []string {
	srcLayer, ok := layers[pkgRel]
	if !ok {
		return []string{fmt.Sprintf("%s: package %q is not registered in packageLayer architecture map", relFile, pkgRel)}
	}
	var errs []string
	for _, imp := range f.Imports {
		impPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: invalid import literal %s: %v", relFile, imp.Path.Value, err))
			continue
		}
		dstPkg, isInternal := internalImportPkg(impPath)
		if !isInternal {
			continue
		}
		dstLayer, known := layers[dstPkg]
		if !known {
			errs = append(errs, fmt.Sprintf("%s: imports unregistered internal package %q", relFile, dstPkg))
			continue
		}
		if dstLayer >= srcLayer {
			errs = append(errs, fmt.Sprintf("layering violation in %s (%s, layer %d): cannot import %s (layer %d)",
				relFile, pkgRel, srcLayer, dstPkg, dstLayer))
		}
	}
	return errs
}

func countCodeLines(lines []string, startLine, endLine int) int {
	n := 0
	for i := startLine - 1; i < endLine && i < len(lines); i++ {
		s := strings.TrimSpace(lines[i])
		if s == "" || strings.HasPrefix(s, "//") {
			continue
		}
		n++
	}
	return n
}
