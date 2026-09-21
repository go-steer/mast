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

package compose

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/mast/internal/modelretry"
	"github.com/go-steer/mast/pkg/workload"
)

// TestARuntimeModelIsDrivenByTheProcessPolicy. Asserting the policy
// rather than the type, because the property that matters is that every
// runtime model in a process shares ONE cooldown — a fan-out of eight
// specialists each carrying its own would answer a single shed with
// eight retries, which is the load amplification #452 set out not to
// add.
func TestARuntimeModelIsDrivenByTheProcessPolicy(t *testing.T) {
	// Enough to construct a client; nothing here calls one.
	t.Setenv("GOOGLE_API_KEY", "not-a-real-key")
	t.Setenv("ANTHROPIC_API_KEY", "not-a-real-key")

	for _, name := range []string{"gemini-3.7-flash", "claude-opus-5"} {
		t.Run(name, func(t *testing.T) {
			m, err := NewRuntimeModel(context.Background(), "", name, workload.BuiltinTools{})
			if err != nil {
				t.Fatalf("NewRuntimeModel: %v", err)
			}
			if got := modelretry.PolicyOf(m); got != modelretry.Shared() {
				t.Errorf("PolicyOf(%s) = %p, want the process-wide policy %p", name, got, modelretry.Shared())
			}
			if got := m.Name(); got != name {
				t.Errorf("Name() = %q, want %q — the board, the cost check and pkg/budget all key off this", got, name)
			}
		})
	}
}

// TestAnOfflineFakeIsNotWrapped. A fake cannot produce a provider
// rejection, so wrapping one would only add an indirection to the path
// every e2e and UAT run takes.
func TestAnOfflineFakeIsNotWrapped(t *testing.T) {
	for _, name := range []string{"echo", "toolactor"} {
		m, err := NewRuntimeModel(context.Background(), "", name, workload.BuiltinTools{})
		if err != nil {
			t.Fatalf("NewRuntimeModel(%s): %v", name, err)
		}
		if p := modelretry.PolicyOf(m); p != nil {
			t.Errorf("%s came back wrapped in a retry policy, want the bare fake", name)
		}
	}
}

// TestNewRuntimeModelPassesTheBuildErrorThrough, rather than turning a
// missing credential into a wrapper around nil.
func TestNewRuntimeModelPassesTheBuildErrorThrough(t *testing.T) {
	m, err := NewRuntimeModel(context.Background(), "", "not-a-model", workload.BuiltinTools{})
	if err == nil {
		t.Fatalf("NewRuntimeModel returned %v, want the unknown-model error", m)
	}
	if m != nil {
		t.Errorf("NewRuntimeModel returned a model alongside an error: %v", m)
	}
}

// mayCallBuildModel reports whether a BuildModel call in function fn of
// file rel is allowed to skip the runtime constructor.
//
// Scoped to the function rather than the file, because "this file is
// allowed" is how an exemption written for one call silently covers the
// next three.
func mayCallBuildModel(rel, fn string) bool {
	switch {
	case rel == "internal/compose/compose.go":
		// The one wrapper. Any other function here is as capable of
		// shipping an unprotected model as a caller outside the package.
		return fn == "NewRuntimeModel"
	case strings.HasPrefix(rel, "internal/evals/"):
		// The eval tiers build their own policy: a nightly with a
		// 90-minute budget can afford four attempts over half a minute
		// to keep a metered row, and an unattended turn cannot. They are
		// measurements, not workloads, and handing them the runtime
		// constructor would put two schedules on one model and report
		// the count in two places.
		return true
	}
	return false
}

// TestEveryRuntimeModelPathIsRetrying is the drift guard #452 needs and
// could not get any other way.
//
// The gap it closes is not a bug in a function, it is a call site
// somebody will add later: a new entry point that reaches for
// BuildModel because that is the name it knew, and ships a model with
// no resilience and no symptom until a provider sheds load at 3am.
// Reviewing for it does not scale; the compiler cannot see it, because
// both constructors return the same type.
//
// Deliberately a source scan rather than a lint rule. It lives next to
// the constructor it is about, its exemption list carries the reason
// for each entry, and a reader who trips it is handed the reasoning
// rather than a rule number.
func TestEveryRuntimeModelPathIsRetrying(t *testing.T) {
	root := repoRootFor(t)
	fset := token.NewFileSet()

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if mayCallBuildModel(rel, fn.Name.Name) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || calleeName(call.Fun) != "BuildModel" {
					return true
				}
				offenders = append(offenders, fmt.Sprintf("%s:%d (in %s)",
					rel, fset.Position(call.Pos()).Line, fn.Name.Name))
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(offenders) > 0 {
		t.Errorf("these call compose.BuildModel directly, so the model they build has no retry:\n  %s\n\n"+
			"Use compose.NewRuntimeModel, which is BuildModel plus the bounded retry a running workload needs (#452). "+
			"If the caller is a measurement rather than a workload it wants its own modelretry policy — add it to "+
			"mayCallBuildModel in this file with the reason.",
			strings.Join(offenders, "\n  "))
	}
}

// TestTheDriftGuardCanSeeACallItShouldReject keeps the scan above from
// being a check that passes because it looks at nothing. A guard whose
// failure mode is silence is the exact defect shape it exists to catch.
func TestTheDriftGuardCanSeeACallItShouldReject(t *testing.T) {
	const src = `package p

import "github.com/go-steer/mast/internal/compose"

func f() { compose.BuildModel(nil, "", "gemini-3.7-flash", bt) }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatalf("parsing the probe: %v", err)
	}
	var found int
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calleeName(call.Fun) == "BuildModel" {
			found++
		}
		return true
	})
	if found != 1 {
		t.Errorf("the scan found %d BuildModel call(s) in a file that has exactly 1", found)
	}
}

// calleeName is the identifier a call expression names, qualified or
// not: both `BuildModel(...)` and `compose.BuildModel(...)` report
// "BuildModel". Unqualified matters because compose.go calls it that
// way, and a caller inside this package is exactly as capable of
// skipping the retry as one outside it.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// repoRootFor walks up for the nearest ancestor holding a go.mod.
func repoRootFor(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod in any ancestor")
		}
		dir = parent
	}
}
