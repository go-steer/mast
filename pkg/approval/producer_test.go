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

package approval

import (
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent/llmagent"
)

// The producer contract as a daemon runs it: the real write gate, over a
// real Task specialist, refusing or recording a real report.
//
// The unit tests in changeset_test.go check the pieces. This file checks
// that the pieces are wired to the seam — that the gate is consulted on
// finish_task at all, that its refusal reaches the model, and that an
// accepted change set lands in durable state where the routing decision
// can read it after the approval pause.

// producerSchema is the report contract for a change-set-producing
// roster, in the shape examples/workloads/gke-triage/schemas/finding.json
// declares it.
func producerSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"detail": {Type: genai.TypeString},
			ChangeSetField: {Type: genai.TypeArray, Items: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"tool":      {Type: genai.TypeString},
					"arguments": {Type: genai.TypeString},
				},
				Required: []string{"tool", "arguments"},
			}},
		},
		Required: []string{"detail"},
	}
}

// producerGate is the write gate a daemon builds, with the catalog
// checker installed and nothing else to do: no tool in this probe is
// mutating, so anything the gate does here it does on the report.
func producerGate(t *testing.T, checker *ChangeSetChecker) llmagent.BeforeToolCallback {
	t.Helper()
	g, err := newWriteGate(Config{
		Policy:    OnMutationApply,
		Mutating:  func(string) bool { return false },
		ChangeSet: checker,
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("newWriteGate: %v", err)
	}
	return g.beforeTool
}

func report(changes ...map[string]any) map[string]any {
	list := make([]any, 0, len(changes))
	for _, c := range changes {
		list = append(list, c)
	}
	return map[string]any{"detail": "api is OOMKilled", ChangeSetField: list}
}

func change(tool, args string) map[string]any {
	return map[string]any{"tool": tool, "arguments": args}
}

// recordedChangeSet reads the change set the gate wrote for one
// specialist out of the durable session.
func recordedChangeSet(t *testing.T, p *reportProbe, specialist string) []ProposedChange {
	t.Helper()
	state := sessionState(t, p.svc)
	v, ok := state[ChangeSetStateKey(specialist)]
	if !ok {
		return nil
	}
	got, err := DecodeChangeSet(v)
	if err != nil {
		t.Fatalf("DecodeChangeSet: %v", err)
	}
	return got
}

// TestProducerContractRecordsAnExecutableChange is the accepting half:
// a change set naming a declared tool with arguments that fit its schema
// is accepted, and it survives into durable state.
//
// Durable is the load-bearing word. Under graph dispatch the approval
// pause re-enters the workflow at START, so the routing decision that
// sends this finding to the change executor cannot be a Go variable from
// the first pass — it has to be re-read, from here.
func TestProducerContractRecordsAnExecutableChange(t *testing.T) {
	c := catalog()
	p := runReportProbe(t, producerSchema(),
		report(change("scale_deployment", `{"deployment":"api","replicas":2}`)),
		producerGate(t, &c))

	if len(p.responses) != 1 {
		t.Fatalf("finish_task responses = %v, want 1", p.responses)
	}
	if _, refused := p.responses[0]["error"]; refused {
		t.Fatalf("an executable change was refused: %v", p.responses[0])
	}

	got := recordedChangeSet(t, p, "reporter")
	if len(got) != 1 {
		t.Fatalf("recorded change set = %+v, want the one entry the specialist proposed", got)
	}
	sig, err := got[0].Signature()
	if err != nil {
		t.Fatalf("Signature: %v", err)
	}
	// The recorded arguments are the NORMALIZED ones — what the tool
	// would receive — not the string the model sent. An operator
	// approving this signature is approving the call that would fire.
	if sig != `scale_deployment({"deployment":"api","replicas":2})` {
		t.Errorf("recorded signature = %s", sig)
	}
}

// TestProducerContractRefusesAnInventedTool is the failure the whole
// contract exists for: a model naming a plausible tool this workload
// cannot call. The refusal has to reach the MODEL — a report that is
// silently dropped, or a turn that errors out, both end with an operator
// holding no finding.
func TestProducerContractRefusesAnInventedTool(t *testing.T) {
	c := catalog()
	p := runReportProbe(t, producerSchema(),
		report(change("kubectl_scale", `{"deployment":"api"}`)),
		producerGate(t, &c))

	if len(p.responses) == 0 {
		t.Fatal("no finish_task response at all")
	}
	if p.responses[0]["error"] != "invalid_proposed_change" {
		t.Fatalf("the model was told %v, want the producer contract's refusal", p.responses[0])
	}
	detail, _ := p.responses[0]["detail"].(string)
	for _, want := range []string{"kubectl_scale", "tool_catalog", "empty list"} {
		if !strings.Contains(detail, want) {
			t.Errorf("refusal does not mention %q, so the specialist cannot act on it: %s", want, detail)
		}
	}
	if p.rounds < 2 {
		t.Errorf("model was asked %d time(s); a refused report must come back for another turn", p.rounds)
	}
	if got := recordedChangeSet(t, p, "reporter"); got != nil {
		t.Errorf("a refused change set was recorded anyway: %+v", got)
	}
}

// TestProducerContractRefusesArgumentsTheToolWouldReject: naming a real
// tool is not enough. The arguments are the part an operator would
// approve and the cluster would receive.
func TestProducerContractRefusesArgumentsTheToolWouldReject(t *testing.T) {
	c := catalog()
	p := runReportProbe(t, producerSchema(),
		report(change("scale_deployment", `{"deployment":"api","replicas":"two"}`)),
		producerGate(t, &c))

	if len(p.responses) == 0 {
		t.Fatal("no finish_task response at all")
	}
	if p.responses[0]["error"] != "invalid_proposed_change" {
		t.Fatalf("the model was told %v, want the producer contract's refusal", p.responses[0])
	}
	if got := recordedChangeSet(t, p, "reporter"); got != nil {
		t.Errorf("a change set with unrunnable arguments was recorded: %+v", got)
	}
}

// TestProducerContractAcceptsAnEmptyChangeSet pins the escape hatch. A
// specialist that cannot name an executable call has to have an answer
// that is not a refusal loop — otherwise the pressure the contract
// applies is pressure to invent one.
func TestProducerContractAcceptsAnEmptyChangeSet(t *testing.T) {
	c := catalog()
	p := runReportProbe(t, producerSchema(), report(), producerGate(t, &c))

	if len(p.responses) != 1 {
		t.Fatalf("finish_task responses = %v, want 1", p.responses)
	}
	if _, refused := p.responses[0]["error"]; refused {
		t.Fatalf("an empty change set was refused: %v", p.responses[0])
	}
	if p.rounds != 1 {
		t.Errorf("model was asked %d time(s), want 1 — an empty change set is a finished report", p.rounds)
	}
	// Nothing recorded: no proposal means nothing for the routing
	// decision to find, which is what sends this finding to an operator
	// rather than to the change executor.
	if got := recordedChangeSet(t, p, "reporter"); got != nil {
		t.Errorf("an empty change set was recorded as a proposal: %+v", got)
	}
}

// TestNoCheckerLeavesReportsAlone: a composition with no catalog to
// check against — a library embed with no workload — must not start
// refusing reports. The contract is opt-in with the bundle that defines
// the catalog it checks.
func TestNoCheckerLeavesReportsAlone(t *testing.T) {
	p := runReportProbe(t, producerSchema(),
		report(change("kubectl_scale", `{"deployment":"api"}`)),
		producerGate(t, nil))

	if len(p.responses) != 1 {
		t.Fatalf("finish_task responses = %v, want 1", p.responses)
	}
	if _, refused := p.responses[0]["error"]; refused {
		t.Fatalf("a gate with no checker refused a report: %v", p.responses[0])
	}
	if got := recordedChangeSet(t, p, "reporter"); got != nil {
		t.Errorf("a gate with no checker recorded %+v", got)
	}
}

// TestProducerContractIgnoresReportsWithNoChangeSet covers every roster
// that predates W7.0: a report schema with no proposed_change property
// is untouched, whether or not a catalog is configured.
func TestProducerContractIgnoresReportsWithNoChangeSet(t *testing.T) {
	c := catalog()
	p := runReportProbe(t, &genai.Schema{
		Type:       genai.TypeObject,
		Properties: map[string]*genai.Schema{"detail": {Type: genai.TypeString}},
		Required:   []string{"detail"},
	}, map[string]any{"detail": "api is OOMKilled"}, producerGate(t, &c))

	if len(p.responses) != 1 {
		t.Fatalf("finish_task responses = %v, want 1", p.responses)
	}
	if _, refused := p.responses[0]["error"]; refused {
		t.Fatalf("a report with no change set was refused: %v", p.responses[0])
	}
}
