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

package agent_test

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"

	mastagent "github.com/go-steer/mast/pkg/agent"
)

// A system prompt is text, not a template.
//
// ADK's llmagent.Config.Instruction is documented as a template: the
// instruction processor runs InjectSessionState over it before every
// request, a `{name}` matching ^[a-zA-Z_][a-zA-Z0-9_]*$ is looked up in
// session state, and a key that is not there returns ErrStateKeyNotExist
// and fails the request. Not a degraded prompt — no request is sent.
//
// mast puts operator-authored prose in that field. A specialist's body
// (pkg/specialists/loader.go, verbatim after TrimSpace), a bundle's
// coordinator instruction (pkg/router), the planner's rendered prompt
// (pkg/planner). Braces are ordinary in all three: a shell variable in
// `${MAST_HOME}/bin/mast`, a JSON shape the specialist is told to emit,
// a kubectl -o jsonpath. Each of those ends the turn before a token is
// sent, naming neither the file nor the brace.
//
// docs/specialists-design.md said the opposite — "nothing substitutes
// into a specialist file; the body reaches the agent verbatim as its
// system prompt" — and that sentence is the stated reason the .tmpl
// extension was removed in #292. It was true of mast and false of the
// substrate mast hands the text to.
//
// The fix is to pass InstructionProvider, which ADK explicitly
// documents as the non-templating alternative. These tests pin it.
// They are written against a body that *reads* like a prompt rather
// than a minimal `{x}`, because the point is that this is what normal
// prose looks like, not what an adversary supplies.

// braceModel records every system instruction it is handed and answers.
// What reaches the provider is the only evidence that matters here: a
// test that asserted only that the turn did not error would also pass
// against a prompt silently mangled on the way.
//
// delegate, when set, names a sub-agent this model calls once before
// answering, so a Task or SingleTurn agent — neither of which may be a
// runner root — can be exercised under a coordinator.
type braceModel struct {
	name     string
	delegate string

	systems []string
}

func (m *braceModel) Name() string { return m.name }

func (m *braceModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	if req.Config != nil && req.Config.SystemInstruction != nil {
		var b strings.Builder
		for _, p := range req.Config.SystemInstruction.Parts {
			if p != nil {
				b.WriteString(p.Text)
			}
		}
		m.systems = append(m.systems, b.String())
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		if _, ok := req.Tools[mastagent.FinishTaskToolName]; ok {
			yield(call(mastagent.FinishTaskToolName, map[string]any{"result": "done"}), nil)
			return
		}
		if m.delegate != "" && !answered(req, m.delegate) {
			yield(call(m.delegate, map[string]any{"request": "go"}), nil)
			return
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText("ok", genai.RoleModel)}, nil)
	}
}

// sawSystem reports whether any agent's system instruction carried want.
func (m *braceModel) sawSystem(want string) bool {
	for _, s := range m.systems {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// runInstruction drives one turn and returns the error the runner
// produced, if any, rather than failing the test on it — the error is
// the subject of these tests.
func runInstruction(t *testing.T, root adkagent.Agent) error {
	t.Helper()
	r, err := runner.New(runner.Config{
		AppName:           "instruction-template-test",
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	for _, err := range r.Run(context.Background(), "user", "s1",
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			return err
		}
	}
	return nil
}

// shellVarPrompt is the upstream reproducer's shape and the likeliest
// real one: an operator documenting how to invoke something.
const shellVarPrompt = `You maintain a Go service.

To run the smoke test, invoke "${MAST_HOME}/bin/mast" -p check.
Report what it printed.`

func TestACoordinatorPromptMayContainAShellVariable(t *testing.T) {
	m := &braceModel{name: "m"}
	a, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name: "root", Description: "root", Instruction: shellVarPrompt, Model: m,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	if err := runInstruction(t, a); err != nil {
		t.Fatalf("a coordinator prompt naming ${MAST_HOME} ended the turn: %v", err)
	}
	if !m.sawSystem("${MAST_HOME}/bin/mast") {
		t.Errorf("the provider was sent system instructions %q, want one carrying ${MAST_HOME}/bin/mast unchanged", m.systems)
	}
}

// A specialist body is the worst-exposed of the three: it is a whole
// markdown file an operator wrote, and telling a specialist to answer
// in a named JSON shape is ordinary authoring.
func TestASpecialistPromptMayDescribeAJSONShape(t *testing.T) {
	const body = `Summarise the incident.

Answer with one JSON object: {"severity": "...", "owner": "..."}.
Use {finding} as the key when you have no owner.`

	// One model per agent, as pkg/agent's other tree tests do: a shared
	// model would try to delegate from inside the delegate.
	rootM := &braceModel{name: "root-m", delegate: "triage"}
	subM := &braceModel{name: "sub-m"}
	sub, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name: "triage", Description: "triage", Instruction: body, Model: subM,
	})
	if err != nil {
		t.Fatalf("NewTaskAgent: %v", err)
	}
	if err := runInstruction(t, coordinator(t, "root", rootM, sub)); err != nil {
		t.Fatalf("a specialist body describing a JSON shape ended the turn: %v", err)
	}
	if len(subM.systems) == 0 {
		t.Fatal("the specialist was never called, so this run proves nothing")
	}
	if !subM.sawSystem("{finding}") {
		t.Errorf("the specialist was sent system instructions %q, want one carrying {finding} unchanged", subM.systems)
	}
}

// The quiet one, and the reason the vacuity floor below is an
// assertion rather than a formality. A SingleTurn specialist runs as a
// dynamic child, so its failed instruction build does not end the
// turn: ADK hands the coordinator
//
//	{"error": "... failed to inject session state into instruction: state key does not exist"}
//
// as the delegation's result, and the coordinator answers the operator
// from it. The classifier never ran, nothing was refused, and the run
// reports success — measured, not reasoned about.
func TestASingleTurnPromptMayContainBraces(t *testing.T) {
	rootM := &braceModel{name: "root-m", delegate: "classify"}
	subM := &braceModel{name: "sub-m"}
	sub, err := mastagent.NewSingleTurnAgent(mastagent.SingleTurnAgentConfig{
		Name: "classify", Description: "classify", Instruction: shellVarPrompt, Model: subM,
	})
	if err != nil {
		t.Fatalf("NewSingleTurnAgent: %v", err)
	}
	if err := runInstruction(t, coordinator(t, "root", rootM, sub)); err != nil {
		t.Fatalf("a single-turn prompt naming ${MAST_HOME} ended the turn: %v", err)
	}
	if len(subM.systems) == 0 {
		t.Fatal("the classifier was never called and the run still reported success — its prompt failed to build and the coordinator answered from the error string")
	}
	if !subM.sawSystem("${MAST_HOME}/bin/mast") {
		t.Errorf("the classifier was sent system instructions %q, want one carrying ${MAST_HOME}/bin/mast unchanged", subM.systems)
	}
}

// The negative that keeps the fix honest. Substitution is not merely
// unused here — it must not happen, or an operator who writes {plan}
// gets whatever pkg/graph last put under that key spliced into their
// prompt. A session that HAS the key is the case where templating and
// not-templating differ observably.
func TestAPromptIsNotSubstitutedEvenWhenTheKeyExists(t *testing.T) {
	m := &braceModel{name: "m"}
	a, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name: "root", Description: "root",
		Instruction: "Continue from {plan}.", Model: m,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	svc := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        "instruction-template-test",
		Agent:          a,
		SessionService: svc,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	ctx := context.Background()
	created, err := svc.Create(ctx, &session.CreateRequest{
		AppName: "instruction-template-test", UserID: "user", SessionID: "s1",
		State: map[string]any{"plan": "SECRET-PLAN"},
	})
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	for _, err := range r.Run(ctx, "user", created.Session.ID(),
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
	}

	if m.sawSystem("SECRET-PLAN") {
		t.Errorf("session state was spliced into the system prompt: %q", m.systems)
	}
	if !m.sawSystem("{plan}") {
		t.Errorf("the provider was sent system instructions %q, want one carrying {plan} unchanged", m.systems)
	}
}

// The guard. The three constructors in this package are the only places
// in shipped mast that build an llmagent.Config, and the fix above is
// only as durable as that stays true — a fourth site written from ADK's
// own examples would reach for Instruction, and nothing at runtime
// would say so: the prompt works until someone writes a brace into it.
//
// Scoped to non-test files deliberately. A test that builds an llmagent
// directly (pkg/approval and pkg/effects both do) is exercising ADK's
// agent rather than shipping mast's prompt handling, and forcing a
// provider on those would make this guard about tidiness instead of
// about what an operator's prose does.
func TestNoShippedCodePassesAPromptThroughADKsTemplateField(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var offenders []string
	checked := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "node_modules", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Config" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "llmagent" {
				return true
			}
			checked++
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				if key.Name == "Instruction" || key.Name == "GlobalInstruction" {
					rel, _ := filepath.Rel(root, path)
					offenders = append(offenders, fmt.Sprintf("%s:%d sets llmagent.Config.%s",
						rel, fset.Position(key.Pos()).Line, key.Name))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// The vacuity floor. A walk that found no llmagent.Config at all
	// would report a clean tree for the wrong reason — a moved package,
	// a renamed import alias, a root that resolved somewhere else.
	if checked < 3 {
		t.Fatalf("found %d llmagent.Config literals in shipped code, want at least the 3 in pkg/agent: this guard is not looking at what it thinks it is", checked)
	}
	if len(offenders) > 0 {
		t.Errorf("shipped code passes a prompt through ADK's templated Instruction field, so a brace in it becomes a session-state lookup:\n\t%s\n\nUse InstructionProvider — see instructionProvider in pkg/agent/instruction.go.",
			strings.Join(offenders, "\n\t"))
	}
}

// moduleRoot walks up from the test's working directory to the
// directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
