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

package mcp

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-steer/mast/pkg/effects"
)

// The three shapes a server can publish. The unannotated one is the
// reason this is a three-case fixture and not a two-case one:
// ToolAnnotations.ReadOnlyHint is a plain bool, so "said false" and
// "said nothing" are the same value once decoded, and only the second
// may fall through to default-deny-unknown.
const (
	annotatedReadOnlyTool = "list_clusters"    // readOnlyHint: true
	annotatedMutatingTool = "delete_cluster"   // readOnlyHint: false
	unannotatedTool       = "unclassified_job" // no annotations at all
)

// annotatedServer stands up a streamable-HTTP MCP server publishing one
// tool of each shape, and returns its URL.
func annotatedServer(t *testing.T) string {
	t.Helper()

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "mast-test-annotations", Version: "0.0.1"}, nil)
	run := func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, struct{}, error) {
		return &mcpsdk.CallToolResult{}, struct{}{}, nil
	}
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        annotatedReadOnlyTool,
		Description: "reads",
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true},
	}, run)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        annotatedMutatingTool,
		Description: "writes",
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: false},
	}, run)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        unannotatedTool,
		Description: "says nothing about itself",
	}, run)

	hs := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv }, nil))
	// ADK's connectionRefresher never closes the MCP session it opens,
	// so the SSE stream outlives the test and a plain Close blocks on
	// it — same concession mrtr_test.go's fixture makes.
	t.Cleanup(func() {
		hs.CloseClientConnections()
		hs.Close()
	})
	return hs.URL
}

// listThrough builds a toolset the production way and enumerates it,
// which is what forces the tools/list round trip the capture rides on.
func listThrough(t *testing.T, url string, ann *Annotations) {
	t.Helper()

	ts, err := NewToolset(context.Background(), "annotated", ServerConfig{
		Transport: TransportHTTP,
		URL:       url,
	}, WithAnnotations(ann))
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := ts.Tools(roCtx{Context: ctx})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("enumerated %d tools, want 3 — the fixture is not what this test thinks it is", len(tools))
	}
}

// TestAnnotationsCapturedThroughTheToolset is the claim #447 turns on:
// the annotations ADK's mcptoolset drops are still readable, because
// mast reads them off the tools/list response one layer below — on the
// sending edge of the same client, before the conversion that discards
// them.
//
// It goes through NewToolset and Toolset.Tools rather than calling
// ListTools directly. Calling the SDK directly would prove the server
// sends annotations, which was never in doubt; what was in doubt is
// whether mast can still see them with ADK's adapter in the way.
func TestAnnotationsCapturedThroughTheToolset(t *testing.T) {
	ann := NewAnnotations(slog.New(slog.DiscardHandler))
	listThrough(t, annotatedServer(t), ann)

	cases := []struct {
		tool          string
		wantReadOnly  bool
		wantAnnotated bool
	}{
		{tool: annotatedReadOnlyTool, wantReadOnly: true, wantAnnotated: true},
		{tool: annotatedMutatingTool, wantReadOnly: false, wantAnnotated: true},
		// Absence is carried in the shape: an unannotated tool is not
		// recorded at all. Recording it as false would classify it the
		// same way, and would also make an unclassified tool look
		// classified to anything reading this registry.
		{tool: unannotatedTool, wantReadOnly: false, wantAnnotated: false},
	}
	for _, tc := range cases {
		readOnly, annotated := ann.ReadOnly(tc.tool)
		if readOnly != tc.wantReadOnly || annotated != tc.wantAnnotated {
			t.Errorf("ReadOnly(%q) = (%v, %v), want (%v, %v)",
				tc.tool, readOnly, annotated, tc.wantReadOnly, tc.wantAnnotated)
		}
	}
}

// TestAnnotatedToolClassifiedByThePredicate is the half that matters to
// an operator: a read-only MCP tool stops parking.
//
// Pre-#447 every one of these three came back ClassMutating, because
// the predicate's MCP answer was the default and nothing else. The
// mutating and unannotated rows are here to pin what did NOT change —
// a port that classified everything read-only would pass a
// one-assertion version of this test.
func TestAnnotatedToolClassifiedByThePredicate(t *testing.T) {
	ann := NewAnnotations(slog.New(slog.DiscardHandler))
	listThrough(t, annotatedServer(t), ann)

	pred := effects.NewPredicateWithHints(nil, ann.ReadOnly)
	cases := []struct {
		tool string
		want effects.Class
	}{
		{tool: annotatedReadOnlyTool, want: effects.ClassReadOnly},
		{tool: annotatedMutatingTool, want: effects.ClassMutating},
		{tool: unannotatedTool, want: effects.ClassMutating},
		{tool: "a_tool_no_server_ever_mentioned", want: effects.ClassMutating},
	}
	for _, tc := range cases {
		if got := pred(tc.tool); got != tc.want {
			t.Errorf("predicate(%q) = %v, want %v", tc.tool, got, tc.want)
		}
	}
}

// TestOperatorOverrideBeatsTheAnnotation pins the precedence that makes
// trusting a server's self-declaration defensible at all: the audited
// workload override (tool_catalog.tools[].mutating) is read first, so
// an operator who does not extend that trust to a particular tool can
// pin it and no server can unpin it.
//
// The reverse direction is checked too — an operator may also un-gate a
// tool whose server calls itself mutating — because the override is a
// statement about the tool, not a veto in one direction.
func TestOperatorOverrideBeatsTheAnnotation(t *testing.T) {
	ann := NewAnnotations(slog.New(slog.DiscardHandler))
	listThrough(t, annotatedServer(t), ann)

	pred := effects.NewPredicateWithHints(map[string]bool{
		annotatedReadOnlyTool: true,  // server says read-only; operator says no
		annotatedMutatingTool: false, // server says mutating; operator says it is fine
	}, ann.ReadOnly)

	if got := pred(annotatedReadOnlyTool); got != effects.ClassMutating {
		t.Errorf("predicate(%q) = %v, want ClassMutating — a server's readOnlyHint overrode an audited override", annotatedReadOnlyTool, got)
	}
	if got := pred(annotatedMutatingTool); got != effects.ClassReadOnly {
		t.Errorf("predicate(%q) = %v, want ClassReadOnly — the override is the operator's statement about the tool", annotatedMutatingTool, got)
	}
}

// TestNoAnnotationSinkKeepsTheOldAnswer pins the opt-out: a toolset
// built without WithAnnotations tells the registry nothing, so every
// one of its tools keeps default-deny-unknown. It is what a caller that
// does not want to extend trust to a server gets by leaving the option
// off, and it is also what every pre-#447 caller got.
func TestNoAnnotationSinkKeepsTheOldAnswer(t *testing.T) {
	ann := NewAnnotations(slog.New(slog.DiscardHandler))

	ts, err := NewToolset(context.Background(), "annotated", ServerConfig{
		Transport: TransportHTTP,
		URL:       annotatedServer(t),
	}) // no WithAnnotations
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := ts.Tools(roCtx{Context: ctx}); err != nil {
		t.Fatalf("Tools: %v", err)
	}

	if _, annotated := ann.ReadOnly(annotatedReadOnlyTool); annotated {
		t.Errorf("ReadOnly(%q) reported an annotation from a toolset that was never given the sink", annotatedReadOnlyTool)
	}
	pred := effects.NewPredicateWithHints(nil, ann.ReadOnly)
	if got := pred(annotatedReadOnlyTool); got != effects.ClassMutating {
		t.Errorf("predicate(%q) = %v, want ClassMutating", annotatedReadOnlyTool, got)
	}
}

// TestConflictingAnnotationsResolveToMutating covers the consequence of
// tool names being un-namespaced in mast (#221): two servers can
// publish the same name, and if they disagree the gate has to be wrong
// in the direction that asks a human.
//
// Driven through observe rather than two live servers because the
// disagreement, not the transport, is what is under test.
func TestConflictingAnnotationsResolveToMutating(t *testing.T) {
	ro := &mcpsdk.Tool{Name: "shared_name", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}
	rw := &mcpsdk.Tool{Name: "shared_name", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: false}}

	for _, order := range [][]*mcpsdk.Tool{{ro, rw}, {rw, ro}} {
		ann := NewAnnotations(slog.New(slog.DiscardHandler))
		ann.observe("server-a", order[:1])
		ann.observe("server-b", order[1:])
		readOnly, annotated := ann.ReadOnly("shared_name")
		if !annotated || readOnly {
			t.Errorf("after a disagreement ReadOnly = (%v, %v), want (false, true) whichever server was heard first",
				readOnly, annotated)
		}
	}

	// The same server changing its own mind is not a conflict — it is a
	// server that was restarted or upgraded, and its latest word wins.
	ann := NewAnnotations(slog.New(slog.DiscardHandler))
	ann.observe("server-a", []*mcpsdk.Tool{rw})
	ann.observe("server-a", []*mcpsdk.Tool{ro})
	if readOnly, _ := ann.ReadOnly("shared_name"); !readOnly {
		t.Error("a server's own re-declaration did not take effect")
	}
}

// TestNilAnnotationsIsUsable pins the nil-receiver contract the wiring
// leans on: a daemon that wired no MCP at all still hands the predicate
// a method value, and it must answer rather than panic.
func TestNilAnnotationsIsUsable(t *testing.T) {
	var ann *Annotations
	if readOnly, annotated := ann.ReadOnly("anything"); readOnly || annotated {
		t.Errorf("nil registry answered (%v, %v), want (false, false)", readOnly, annotated)
	}
	ann.observe("server", []*mcpsdk.Tool{{Name: "x", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}})
}
