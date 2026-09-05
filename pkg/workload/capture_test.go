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

package workload_test

import (
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/workload"
)

// The bundle half of #296: what a workload declares so that a mutating
// call carries a record of what it overwrote, and the call that puts it
// back.

func TestLoad_Capture(t *testing.T) {
	path := writeBundle(t, "b.yaml", `name: x
specialists: [a]
tool_catalog:
  tools:
    - name: patch_k8s_resource
      mutating: true
      capture:
        read: get_k8s_resource
        args: {output: json}
        args_from: {kind: kind, name: name, namespace: namespace}
        fields: [spec.template.spec.containers]
        revert:
          call: patch_k8s_resource
          args_from_change: {kind: kind, name: name, namespace: namespace}
          args_from_capture: {patch: spec.template.spec.containers}
    - name: get_k8s_resource
      mutating: false
`)
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := b.ToolCatalog.Tools[0].Capture
	if c == nil {
		t.Fatal("patch_k8s_resource has no capture")
	}
	if c.Read != "get_k8s_resource" {
		t.Errorf("read = %q, want get_k8s_resource", c.Read)
	}
	if c.Args["output"] != "json" {
		t.Errorf("args = %v, want the literal output:json", c.Args)
	}
	if c.ArgsFrom["kind"] != "kind" || c.ArgsFrom["namespace"] != "namespace" {
		t.Errorf("args_from = %v, want the change's own arguments mapped onto the read's", c.ArgsFrom)
	}
	if len(c.Fields) != 1 || c.Fields[0] != "spec.template.spec.containers" {
		t.Errorf("fields = %v, want the declared path", c.Fields)
	}
	if c.Revert == nil {
		t.Fatal("the declaration's revert did not load")
	}
	if c.Revert.Call != "patch_k8s_resource" {
		t.Errorf("revert call = %q, want patch_k8s_resource", c.Revert.Call)
	}
	if c.Revert.ArgsFromChange["name"] != "name" {
		t.Errorf("revert args_from_change = %v, want the change's addressing arguments", c.Revert.ArgsFromChange)
	}
	if c.Revert.ArgsFromCapture["patch"] != "spec.template.spec.containers" {
		t.Errorf("revert args_from_capture = %v, want the captured path", c.Revert.ArgsFromCapture)
	}
	// Omitted is nil, not an empty declaration — a tool with no capture
	// behaves exactly as it did before #296.
	if b.ToolCatalog.Tools[1].Capture != nil {
		t.Errorf("get_k8s_resource capture = %+v, want nil", b.ToolCatalog.Tools[1].Capture)
	}
}

// TestLoad_CaptureRevertIsOptional: a workload that can record the prior
// state but cannot name the call that restores it says so by omitting
// revert, and that is a complete declaration rather than a half one. The
// record still carries the old value; only the undo is missing.
func TestLoad_CaptureRevertIsOptional(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      capture:\n        read: r\n        fields: [spec.replicas]\n")
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := b.ToolCatalog.Tools[0].Capture
	if c == nil || c.Revert != nil {
		t.Fatalf("capture = %+v, want a declaration with no revert", c)
	}
}

// TestLoad_CaptureErrors: what the bundle alone can catch. Every one of
// these describes a capture that would load, run, and produce a record
// that looks right — which is the reason to refuse at load rather than
// discover it the first time somebody tries to undo something.
func TestLoad_CaptureErrors(t *testing.T) {
	for _, tc := range []struct{ name, body, wantErr string }{
		{
			"no read tool",
			"name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      capture:\n        fields: [a]\n",
			"names no read tool",
		},
		{
			// A change cannot record its own prior state: by the time the
			// call has run there is no prior state left to read.
			"reads itself",
			"name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      capture:\n        read: t\n",
			"cannot record its own prior state",
		},
		{
			"empty field path",
			"name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      capture:\n        read: r\n        fields: [\"\"]\n",
			"empty path",
		},
		{
			"args_from with an empty side",
			"name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      capture:\n        read: r\n        args_from: {name: \"\"}\n",
			"both sides name an argument",
		},
		{
			"revert names no call",
			"name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      capture:\n        read: r\n        revert:\n          args_from_capture: {replicas: spec.replicas}\n",
			"names no call",
		},
		{
			// The one worth the most: a revert whose arguments all come
			// from the change puts back what the change put in. It is not
			// an undo, and it is shaped closely enough like one to be
			// believed by whoever reads the record during an incident.
			"revert takes nothing from the capture",
			"name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      capture:\n        read: r\n        revert:\n          call: t\n          args_from_change: {name: name}\n",
			"re-apply the change rather than undo it",
		},
		{
			"revert args_from_change with an empty side",
			"name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      capture:\n        read: r\n        revert:\n          call: t\n          args: {force: true}\n          args_from_change: {\"\": name}\n",
			"both sides name an argument",
		},
		{
			"revert args_from_capture with an empty side",
			"name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      capture:\n        read: r\n        revert:\n          call: t\n          args_from_capture: {replicas: \"\"}\n",
			"the right side a path into the capture",
		},
		{
			// fields narrows what the record keeps. A revert drawn from
			// outside that narrowing resolves at capture time and is then
			// absent from the record, so nobody reading the row can see
			// where the value came from.
			"revert reads a path fields does not keep",
			"name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      capture:\n        read: r\n        fields: [spec.replicas]\n        revert:\n          call: t\n          args_from_capture: {image: spec.image}\n",
			"which capture.fields does not record",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeBundle(t, "b.yaml", tc.body)
			_, err := workload.Load(path)
			if err == nil {
				t.Fatalf("Load accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoad_CaptureRevertPathWithoutFieldsIsFine: the narrowing check only
// applies when the declaration narrowed something. With no fields the
// whole read is recorded, so any path in it is in the record.
func TestLoad_CaptureRevertPathWithoutFieldsIsFine(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      capture:\n        read: r\n        revert:\n          call: t\n          args_from_capture: {image: spec.image}\n")
	if _, err := workload.Load(path); err != nil {
		t.Fatalf("Load refused a revert path against an un-narrowed capture: %v", err)
	}
}
