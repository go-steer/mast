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

// What a refused specialist gets to say when the gate grants it a last
// call (#271).
//
// refusalreport_test.go covers the case where nothing was looked at: the
// delegation resolves to nothing, and mast invents no finding to fill
// it. These cover the case where a great deal was looked at and the
// ceiling landed mid-investigation. The measured original: a diagnoser
// stopped after six log queries and 269k tokens, whose entire
// contribution to the incident was an unresolved delegation.
package agent_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
)

// grantingGate refuses the named agents the way stubGate does, and
// additionally offers the final-report grant — once per agent, as a real
// meter does, so a test that leaked a second grant would see it here
// rather than in a hung run.
type grantingGate struct {
	refuse map[string]string
	// grant names the agents whose refusal comes with the offer. An
	// agent absent from it is refused the ordinary way, which is how
	// these tests pin that the grant is not automatic.
	grant map[string]bool

	mu     sync.Mutex
	asked  []string
	issued []string
}

func (g *grantingGate) Allow(agentName string) error {
	if reason, ok := g.refuse[agentName]; ok {
		return errors.New(reason)
	}
	return nil
}

func (g *grantingGate) AllowFinalReport(agentName string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, agentName)
	if !g.grant[agentName] {
		return false
	}
	for _, a := range g.issued {
		if a == agentName {
			return false
		}
	}
	g.issued = append(g.issued, agentName)
	return true
}

func (g *grantingGate) counts() (asked, issued int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.asked), len(g.issued)
}

// reportingModel is the half of the design mast does not write. It
// captures every request it is handed and answers with a report that
// satisfies findingSchema — which is the point: the schema is filled by
// the model, from what it saw, not by mast from a template.
type reportingModel struct {
	name    string
	summary string

	mu   sync.Mutex
	reqs []*model.LLMRequest
}

func (m *reportingModel) Name() string { return m.name }

func (m *reportingModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	m.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(call(mastagent.FinishTaskToolName, map[string]any{"summary": m.summary}), nil)
	}
}

func (m *reportingModel) requests() []*model.LLMRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*model.LLMRequest(nil), m.reqs...)
}

func mkTool(t *testing.T, name string) tool.Tool {
	t.Helper()
	ft, err := functiontool.New(functiontool.Config{Name: name, Description: name},
		func(_ adkagent.Context, _ struct{}) (string, error) { return "", nil })
	if err != nil {
		t.Fatalf("functiontool.New(%q): %v", name, err)
	}
	return ft
}

func reportingSpecialist(t *testing.T, name string, m model.LLM, tools ...tool.Tool) adkagent.Agent {
	t.Helper()
	a, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name: name, Description: name, Instruction: "diagnose the incident",
		Model:        m,
		Tools:        tools,
		OutputSchema: findingSchema(),
	})
	if err != nil {
		t.Fatalf("NewTaskAgent(%q): %v", name, err)
	}
	return a
}

// declaredTools lists the function names a request puts on the wire.
// Config.Tools rather than req.Tools, because the wire declarations are
// what the model is actually offered.
func declaredTools(req *model.LLMRequest) []string {
	var out []string
	if req == nil || req.Config == nil {
		return out
	}
	for _, tl := range req.Config.Tools {
		if tl == nil {
			continue
		}
		for _, fd := range tl.FunctionDeclarations {
			if fd != nil {
				out = append(out, fd.Name)
			}
		}
	}
	return out
}

// promptText flattens every text part in a request, which is where the
// final-report instruction has to land to be read.
func promptText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				b.WriteString(p.Text + "\n")
			}
		}
	}
	return b.String()
}

// The headline. A schemad specialist that hits its ceiling and is
// granted the final report writes one, and the incident gets a finding
// instead of silence.
func TestAGrantedSpecialistReportsWhatItAlreadyFound(t *testing.T) {
	coordModel := &countingModel{name: "coord", delegate: "sp"}
	spModel := &reportingModel{name: "sp", summary: "nodeSelector references a pool that no longer exists"}
	root := coordinator(t, "root", coordModel,
		reportingSpecialist(t, "sp", spModel, mkTool(t, "list_k8s_events"), mkTool(t, "get_k8s_logs")))
	gate := &grantingGate{
		refuse: map[string]string{"sp": "specialist cap: 12 model calls (turns) > cap 12"},
		grant:  map[string]bool{"sp": true},
	}

	events := boundedRun(t, root, gate, 20)

	reqs := spModel.requests()
	if len(reqs) != 1 {
		t.Fatalf("the refused specialist made %d model calls, want exactly 1: the grant "+
			"buys one call, and a second would be the overshoot compounding", len(reqs))
	}

	// Every tool but the report tool is withdrawn, so the only move the
	// grant leaves is to report. A grant that left the investigation
	// tools in place would just buy one more query.
	if got := declaredTools(reqs[0]); len(got) != 1 || got[0] != mastagent.FinishTaskToolName {
		t.Errorf("the final call offered %v, want only %q: the grant is for reporting, "+
			"not for one more look", got, mastagent.FinishTaskToolName)
	}
	if _, ok := reqs[0].Tools["list_k8s_events"]; ok {
		t.Error("req.Tools still carries an investigation tool; the wire declarations and " +
			"the executable set have to agree about what this turn offers")
	}
	if text := promptText(reqs[0]); !strings.Contains(text, mastagent.FinalReportInstruction) {
		t.Errorf("the final call did not carry the instruction, so the model has no way to "+
			"know it is the last one:\n%s", text)
	}
	var fcc *genai.FunctionCallingConfig
	if tc := reqs[0].Config.ToolConfig; tc != nil {
		fcc = tc.FunctionCallingConfig
	}
	if fcc == nil || fcc.Mode != genai.FunctionCallingConfigModeAny ||
		len(fcc.AllowedFunctionNames) != 1 || fcc.AllowedFunctionNames[0] != mastagent.FinishTaskToolName {
		t.Errorf("the report was offered rather than forced (%+v); a model that answers in "+
			"prose spends the grant and produces nothing", fcc)
	}

	// And the payoff: the model's finding, not mast's refusal boilerplate.
	text := transcript(events)
	if !strings.Contains(text, spModel.summary) {
		t.Errorf("the specialist's finding is not in the transcript:\n%s", text)
	}
	if strings.Contains(text, mastagent.RefusalMarker) {
		t.Errorf("a granted specialist still emitted the unreportable-refusal text:\n%s", text)
	}
}

// The bound that keeps the grant from being a fabrication. mast fills
// nothing in: if the gate declines, the behaviour is exactly what
// refusalreport_test.go pins, and the specialist's model is never
// called.
func TestAGateThatDeclinesTheGrantRefusesExactlyAsBefore(t *testing.T) {
	coordModel := &countingModel{name: "coord", delegate: "sp"}
	spModel := &reportingModel{name: "sp", summary: "must not be reached"}
	root := coordinator(t, "root", coordModel, reportingSpecialist(t, "sp", spModel))
	gate := &grantingGate{refuse: map[string]string{"sp": "specialist cap"}}

	events := boundedRun(t, root, gate, 20)

	if got := len(spModel.requests()); got != 0 {
		t.Errorf("the specialist made %d model calls after the grant was declined, want 0", got)
	}
	if asked, issued := gate.counts(); asked == 0 || issued != 0 {
		t.Errorf("gate asked=%d issued=%d, want asked>0 issued=0", asked, issued)
	}
	if text := transcript(events); !strings.Contains(text, mastagent.RefusalMarker) {
		t.Errorf("the ordinary refusal is not in the transcript:\n%s", text)
	}
}

// An agent with no report channel — a Chat-mode coordinator — must not
// have its grant consumed. Asking a gate that latches on the ask would
// spend the agent's one chance on a call it could not have reported
// through.
func TestTheGrantIsNotAskedForWhenThereIsNothingToReportThrough(t *testing.T) {
	coordModel := &countingModel{name: "coord"}
	root := coordinator(t, "root", coordModel)
	gate := &grantingGate{
		refuse: map[string]string{"root": "workload cap"},
		grant:  map[string]bool{"root": true},
	}

	events := boundedRun(t, root, gate, 20)

	if asked, _ := gate.counts(); asked != 0 {
		t.Errorf("the gate was asked for a grant %d time(s) by an agent with no finish_task; "+
			"a latching gate would have burned it", asked)
	}
	if got := coordModel.count(); got != 0 {
		t.Errorf("the refused coordinator called its model %d time(s), want 0", got)
	}
	if text := transcript(events); !strings.Contains(text, mastagent.RefusalMarker) {
		t.Errorf("the refusal is not in the transcript:\n%s", text)
	}
}
