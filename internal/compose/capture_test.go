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
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	adkagent "google.golang.org/adk/v2/agent"

	"github.com/go-steer/mast/pkg/workload"
)

// The bundle-to-gate translation for prior-state capture (#296), and the
// two things it refuses at startup rather than at 03:00.

// captureBundle declares one mutating tool with a capture, over a read
// and a revert whose classifications the caller chooses. Both are the
// mistakes a bundle author actually makes, because the natural way to
// write either block is to copy the other one and change a name.
func captureBundle(read string, readMutating bool, revert string, revertMutating bool) *workload.Bundle {
	writes := true
	tools := []workload.ToolPolicy{
		{
			Name:     "scale_deployment",
			Mutating: &writes,
			Capture: &workload.Capture{
				Read:     read,
				ArgsFrom: map[string]string{"name": "deployment"},
				Fields:   []string{"spec.replicas"},
			},
		},
		{Name: read, Mutating: &readMutating},
	}
	if revert != "" {
		tools[0].Capture.Revert = &workload.Revert{
			Call:            revert,
			ArgsFromChange:  map[string]string{"deployment": "deployment"},
			ArgsFromCapture: map[string]string{"replicas": "spec.replicas"},
		}
		if revert != "scale_deployment" && revert != read {
			tools = append(tools, workload.ToolPolicy{Name: revert, Mutating: &revertMutating})
		}
	}
	return &workload.Bundle{Name: "b", ToolCatalog: workload.ToolCatalog{Tools: tools}}
}

func TestPriorStateCaptures_DeclarationsReachTheGate(t *testing.T) {
	b := captureBundle("get_deployment", false, "scale_deployment", true)
	var read []string
	c, err := priorStateCaptures(WriteGateConfig{
		Bundle: b,
		ToolRead: func(_ adkagent.Context, name string, _ map[string]any) (map[string]any, error) {
			read = append(read, name)
			return map[string]any{"ok": true}, nil
		},
		ToolSchemas: func(string) (*jsonschema.Schema, error) { return &jsonschema.Schema{Type: "object"}, nil },
	}, MutationPredicate(*b, nil))
	if err != nil {
		t.Fatalf("priorStateCaptures: %v", err)
	}

	decl, err := c.For("scale_deployment")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if decl == nil {
		t.Fatal("scale_deployment has no capture at the gate, so its calls would run with no record of what they overwrote")
	}
	if decl.Read != "get_deployment" || decl.ArgsFrom["name"] != "deployment" || len(decl.Fields) != 1 {
		t.Errorf("capture = %+v, want the bundle's declaration carried across whole", decl)
	}
	if decl.Revert == nil || decl.Revert.ArgsFromCapture["replicas"] != "spec.replicas" {
		t.Errorf("revert = %+v, want the bundle's mapping carried across", decl.Revert)
	}

	// A tool that declares nothing gets nothing — not an empty
	// declaration, which would read as a capture that records nothing.
	other, err := c.For("get_deployment")
	if err != nil || other != nil {
		t.Errorf("For(get_deployment) = %+v, %v; want nil, nil", other, err)
	}

	if _, err := c.Read(nil, "get_deployment", nil); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(read) != 1 || read[0] != "get_deployment" {
		t.Errorf("reads = %v, want the supplied ToolRead to be the one wired in", read)
	}
}

// TestPriorStateCaptures_RefusesAMutatingRead: same refusal, same
// reason, as a mutating freshness read. A capture read that writes would
// hit the cluster before every gated call — unapproved and unrecorded —
// in the name of making the change reversible.
func TestPriorStateCaptures_RefusesAMutatingRead(t *testing.T) {
	b := captureBundle("drain_node", true, "", false)
	_, err := priorStateCaptures(WriteGateConfig{Bundle: b}, MutationPredicate(*b, nil))
	if err == nil {
		t.Fatal("compose started with a mutating capture read; every gated call would write to the cluster before being approved")
	}
	if !strings.Contains(err.Error(), "a recording must not change what it records") {
		t.Errorf("error = %v, want it to say why", err)
	}
	if !strings.Contains(err.Error(), "drain_node") {
		t.Errorf("error = %v, want it to name the offending read", err)
	}
}

// TestPriorStateCaptures_RefusesAReadOnlyRevert is the mirror image, and
// the likelier mistake: a revert that changes nothing is recorded as an
// undo, offered to an operator during an incident, and does nothing when
// they take it.
func TestPriorStateCaptures_RefusesAReadOnlyRevert(t *testing.T) {
	b := captureBundle("get_deployment", false, "describe_deployment", false)
	_, err := priorStateCaptures(WriteGateConfig{Bundle: b}, MutationPredicate(*b, nil))
	if err == nil {
		t.Fatal("compose started with a read-only revert; the record would offer an undo that cannot undo anything")
	}
	if !strings.Contains(err.Error(), "cannot undo a change") {
		t.Errorf("error = %v, want it to say why", err)
	}
	if !strings.Contains(err.Error(), "describe_deployment") {
		t.Errorf("error = %v, want it to name the offending call", err)
	}
}

// TestPriorStateCaptures_RefusalsReachWriteGate: the checks have to bite
// on the real construction path, not only when called directly. A
// startup refusal that only the unit test sees is not a startup refusal.
func TestPriorStateCaptures_RefusalsReachWriteGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bundle *workload.Bundle
		want   string
	}{
		{"mutating read", captureBundle("drain_node", true, "", false), "must not change what it records"},
		{"read-only revert", captureBundle("get_deployment", false, "describe_deployment", false), "cannot undo a change"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := WriteGate(WriteGateConfig{Bundle: tc.bundle}); err == nil {
				t.Fatalf("WriteGate started with a %s", tc.name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestPriorStateCaptures_WarnsWhenItCannotRecord: a deployment with no
// way to run a read still starts — the declaring tools' calls are
// refused one at a time, with the specific reason — but it must say so
// at startup. The failure mode this closes is a bundle author writing
// captures, seeing a clean start, and believing changes are reversible.
func TestPriorStateCaptures_WarnsWhenItCannotRecord(t *testing.T) {
	b := captureBundle("get_deployment", false, "scale_deployment", true)
	pred := MutationPredicate(*b, nil)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if _, err := priorStateCaptures(WriteGateConfig{Bundle: b, Logger: logger}, pred); err != nil {
		t.Fatalf("priorStateCaptures: %v", err)
	}
	if !strings.Contains(buf.String(), "cannot run a read on its own behalf") {
		t.Errorf("no warning that the declared captures will not be taken:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "cannot look up a tool's arguments") {
		t.Errorf("no warning that the declared revert cannot be checked:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "scale_deployment") {
		t.Errorf("the warnings do not name the affected tool:\n%s", buf.String())
	}

	buf.Reset()
	_, err := priorStateCaptures(WriteGateConfig{
		Bundle: b, Logger: logger,
		ToolRead:    func(adkagent.Context, string, map[string]any) (map[string]any, error) { return nil, nil },
		ToolSchemas: func(string) (*jsonschema.Schema, error) { return nil, nil },
	}, pred)
	if err != nil {
		t.Fatalf("priorStateCaptures: %v", err)
	}
	if strings.Contains(buf.String(), "cannot run a read") || strings.Contains(buf.String(), "cannot look up") {
		t.Errorf("warned about seams this deployment has:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "can_read=true") || !strings.Contains(buf.String(), "can_check_revert=true") {
		t.Errorf("startup log does not record that captures are live:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "with_revert=1") {
		t.Errorf("startup log does not say how many declarations carry an undo:\n%s", buf.String())
	}
}

// TestPriorStateCaptures_SilentWithNoDeclarations: a bundle that declares
// no capture logs nothing and refuses nothing. Whatever this costs a
// workload that has not adopted #296, it must not be a line of startup
// noise suggesting it has.
func TestPriorStateCaptures_SilentWithNoDeclarations(t *testing.T) {
	b := &workload.Bundle{Name: "b"}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c, err := priorStateCaptures(WriteGateConfig{Bundle: b, Logger: logger}, MutationPredicate(*b, nil))
	if err != nil {
		t.Fatalf("priorStateCaptures: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("startup logged something for a bundle with no captures:\n%s", buf.String())
	}
	decl, err := c.For("scale_deployment")
	if err != nil || decl != nil {
		t.Errorf("For = %+v, %v; want nil, nil", decl, err)
	}
}
