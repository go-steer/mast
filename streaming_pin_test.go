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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every runner site in this module runs StreamingModeNone, and that is
// a decision rather than a coincidence (#331).
//
// Readers across the tree take one runner event to be one whole model
// response: they emit a frame, count a tool call, or render a message
// per event. Under StreamingModeSSE that is false — ADK yields every
// provider chunk as a Partial event *before* the guard that decides
// whether to act on it (v2.2.0 internal/llminternal/base_flow.go:630-636),
// so each consumer sees the same call once per chunk plus once on the
// aggregate.
//
// Seven non-test files say "mast runs StreamingModeNone" in a comment;
// exactly two check it.
// Prose is not a check: nothing stopped someone adding a --stream flag
// and turning every one of those comments into a lie in the same commit
// that made them wrong. This test is the check, and its failure message
// is the audit list.
//
// It does not forbid SSE. It forbids turning SSE on *silently*.

// runConfigProblems returns one message per RunConfig literal in src
// that sets StreamingMode to anything other than StreamingModeNone, and
// the number of literals it examined.
//
// It takes source text rather than a path so the checker itself can be
// exercised on fixtures — a tree walk that finds nothing is not evidence
// that it would find something.
//
// Omitting the field is fine and is the common case: ADK's zero value is
// the empty string, and the only mode that streams is an exact match on
// "sse" (v2.2.0 internal/llminternal/base_flow.go:799), so a bare
// RunConfig{} is non-streaming. Anything the AST cannot read as the
// StreamingModeNone constant — a variable, a function call, a string
// literal — counts as a problem, because a pin that accepts values it
// cannot evaluate pins nothing.
func runConfigProblems(label, src string) ([]string, int) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, label, src, parser.ParseComments)
	if err != nil {
		return []string{label + ": parse: " + err.Error()}, 0
	}
	var problems []string
	sites := 0
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || trailingName(lit.Type) != "RunConfig" {
			return true
		}
		sites++
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "StreamingMode" {
				continue
			}
			if trailingName(kv.Value) == "StreamingModeNone" {
				continue
			}
			pos := fset.Position(kv.Pos())
			problems = append(problems, label+":"+strconv.Itoa(pos.Line)+
				": a runner site sets StreamingMode to something other than\n"+
				"\tStreamingModeNone. Most event consumers in this module assume one\n"+
				"\tevent is one whole model response, and under SSE it is not —\n"+
				"\tsee #331.\n"+
				"\n"+
				"\tThe safe set is exactly the non-test files that name `.Partial`\n"+
				"\t(re-derive it with: grep -rn '\\.Partial' --include='*.go'):\n"+
				"\t  pkg/watchdog/bridge.go — skips partials as of #331\n"+
				"\t  pkg/agent/stall.go — skips partials\n"+
				"\t  cmd/mast/agui.go — skips partials as of #400\n"+
				"\tAudit everything else that reads an event before deleting this\n"+
				"\tcheck. What is left is the A2A pair: cmd/mast/a2a.go's\n"+
				"\temitStreamProgress emits one narration frame per event, so under\n"+
				"\tSSE its progress stream fragments (its result artifact does not —\n"+
				"\tthat is a last-wins capture). Written up in #408, with the reason\n"+
				"\tit is degradation rather than the tool-dispatch bug #400 was.\n"+
				"\tpkg/a2a/server.go carries a comment asserting message\n"+
				"\tgranularity that turning SSE on would falsify.\n"+
				"\n"+
				"\tNote that cmd/mast/agui.go being safe is not the same as AG-UI\n"+
				"\tstreaming working: it now emits whole messages under SSE rather\n"+
				"\tthan wrong ones. Feeding chunks into AG-UI's delta frames is\n"+
				"\t#407, and it is the change that should turn SSE on here.\n"+
				"\n"+
				"\tThis test does not forbid SSE. It forbids turning it on without\n"+
				"\twalking that list — delete the check in the same change that\n"+
				"\tfinishes the walk.")
		}
		return true
	})
	return problems, sites
}

// trailingName is the identifier at the end of an expression: `X` for
// `X`, and `X` for `pkg.X`. Anything else has no name.
func trailingName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.StarExpr:
		return trailingName(t.X)
	}
	return ""
}

func TestEveryRunnerSiteIsNonStreaming(t *testing.T) {
	var problems []string
	total := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// testdata holds fixtures that are not this module's code;
			// node_modules is the docs site's.
			case ".git", "node_modules", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		// Non-test files only. A _test.go that constructs an SSE
		// RunConfig is exercising the mode deliberately — which is the
		// audit this asks for, not a way around it — and it ships no
		// runner site.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found, sites := runConfigProblems(path, string(src))
		problems = append(problems, found...)
		total += sites
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, p := range problems {
		t.Error(p)
	}
	// The failure mode this guards is the walk silently stopping to
	// match anything — a renamed type, a moved package, a changed
	// literal style — and reporting a clean tree because it looked at
	// nothing. There were 14 RunConfig literals when this was written.
	if total < 10 {
		t.Errorf("examined %d RunConfig literals, expected at least 10; "+
			"this test is no longer checking what it thinks it is", total)
	}
}

// The tree sets StreamingModeNone everywhere, so the walk above passes
// whether or not the checker works. This is what shows it would not:
// the checker is run against each shape directly.
func TestRunConfigCheckerReadsTheField(t *testing.T) {
	const qualifiedSSE = `package p

func run() { r.Run(ctx, adkagent.RunConfig{StreamingMode: adkagent.StreamingModeSSE}) }
`
	const bareSSE = `package p

func run() { r.Run(ctx, RunConfig{StreamingMode: StreamingModeSSE}) }
`
	const none = `package p

func run() { r.Run(ctx, adkagent.RunConfig{StreamingMode: adkagent.StreamingModeNone}) }
`
	const zero = `package p

func run() { r.Run(ctx, adkagent.RunConfig{}) }
`
	// Not evaluable, so not provably none: a --stream flag would land
	// in exactly this shape.
	const viaVariable = `package p

func run() { r.Run(ctx, adkagent.RunConfig{StreamingMode: mode}) }
`
	// A different struct that happens to carry the field is not a
	// runner site and must not be counted as one.
	const otherStruct = `package p

var c = someOtherConfig{StreamingMode: "sse"}
`
	for _, tc := range []struct {
		name  string
		src   string
		want  int
		sites int
	}{
		{"qualified sse", qualifiedSSE, 1, 1},
		{"bare sse", bareSSE, 1, 1},
		{"explicit none", none, 0, 1},
		{"zero value", zero, 0, 1},
		{"via variable", viaVariable, 1, 1},
		{"other struct", otherStruct, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, sites := runConfigProblems(tc.name+".go", tc.src)
			if len(got) != tc.want {
				t.Errorf("got %d problems, want %d: %v", len(got), tc.want, got)
			}
			if sites != tc.sites {
				t.Errorf("examined %d RunConfig literals, want %d", sites, tc.sites)
			}
		})
	}

	// The message has to be usable by whoever trips it, which means naming
	// the consumer that still breaks and the way to re-derive the list.
	// cmd/mast/a2a.go and #408 are in this list because they are what is
	// left unaudited; agui.go stays in it because it moved to the safe set
	// (#400) and a reader needs to see that it was checked, not omitted.
	got, _ := runConfigProblems("x.go", qualifiedSSE)
	for _, want := range []string{"cmd/mast/a2a.go", "#408", "cmd/mast/agui.go", "pkg/watchdog/bridge.go", "#331", ".Partial"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("failure message does not mention %q:\n%s", want, got[0])
		}
	}
}
