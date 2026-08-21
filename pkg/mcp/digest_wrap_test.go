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
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/digest"
)

// stubToolContext adapts a plain context.Context into an adkagent.Context
// so a tool can be driven without a runner, via ADK's ContextMock hook.
// Same shape pkg/federation's tests use.
type stubToolContext struct {
	adkagent.ContextMock
	ctx    context.Context
	callID string
}

func newStubToolContext(callID string) *stubToolContext {
	return &stubToolContext{ctx: context.Background(), callID: callID}
}

func (s *stubToolContext) Deadline() (time.Time, bool) { return s.ctx.Deadline() }
func (s *stubToolContext) Done() <-chan struct{}       { return s.ctx.Done() }
func (s *stubToolContext) Err() error                  { return s.ctx.Err() }
func (s *stubToolContext) Value(key any) any           { return s.ctx.Value(key) }
func (s *stubToolContext) FunctionCallID() string      { return s.callID }

// fakeTool is a runnableTool whose response and error the test picks.
type fakeTool struct {
	name     string
	response map[string]any
	err      error
	calls    int
	packed   bool // set when ProcessRequest ran
}

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) Description() string { return "fake" }
func (f *fakeTool) IsLongRunning() bool { return false }
func (f *fakeTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: f.name, Description: "fake"}
}

func (f *fakeTool) Run(_ adkagent.Context, _ any) (map[string]any, error) {
	f.calls++
	return f.response, f.err
}

// inertTool implements only tool.Tool — no Declaration, no Run — which
// is the shape WithDigest must leave alone.
type inertTool struct{ name string }

func (i inertTool) Name() string        { return i.name }
func (i inertTool) Description() string { return "inert" }
func (i inertTool) IsLongRunning() bool { return false }

type fakeToolset struct {
	name  string
	tools []tool.Tool
	err   error
}

func (f *fakeToolset) Name() string { return f.name }
func (f *fakeToolset) Tools(adkagent.ReadonlyContext) ([]tool.Tool, error) {
	return f.tools, f.err
}

// memStore is an in-memory digest.Store for the wrap's CCR assertions.
type memStore struct {
	mu     sync.Mutex
	data   map[string][]byte
	getErr error
}

func (m *memStore) Put(_ context.Context, id string, raw []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	m.data[id] = append([]byte(nil), raw...)
	return nil
}

func (m *memStore) Get(_ context.Context, id string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	v, ok := m.data[id]
	if !ok {
		return nil, digest.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

// bigList builds a JSON-shaped response comfortably over the threshold,
// with a long array the structural pruner will collapse.
func bigList(n int) map[string]any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{
			"name":   "pod-with-a-reasonably-long-name",
			"detail": strings.Repeat("x", 200),
		}
	}
	return map[string]any{"items": items}
}

// runWrapped drives one wrapped tool call and returns the response map.
func runWrapped(t *testing.T, ts tool.Toolset, callID string) (map[string]any, error) {
	t.Helper()
	tools, err := ts.Tools(nil)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	rt, ok := tools[0].(runnableTool)
	if !ok {
		t.Fatalf("wrapped tool is not runnable")
	}
	return rt.Run(newStubToolContext(callID), nil)
}

func TestWithDigestIsInertWhenThereIsNothingToDo(t *testing.T) {
	t.Parallel()
	inner := &fakeToolset{name: "gke"}
	if got := WithDigest(inner, "gke", nil); got != tool.Toolset(inner) {
		t.Errorf("nil options should return the toolset unchanged")
	}
	opts := &DigestOptions{NeverServers: map[string]bool{"gke": true}}
	if got := WithDigest(inner, "gke", opts); got != tool.Toolset(inner) {
		t.Errorf("an opted-out server should return the toolset unchanged")
	}
	if got := WithDigest(inner, "other", opts); got == tool.Toolset(inner) {
		t.Errorf("a server not on the never list should be wrapped")
	}
	if got := WithDigest(nil, "gke", opts); got != nil {
		t.Errorf("a nil toolset should stay nil, got %v", got)
	}
}

func TestTheWrapKeepsTheToolsetsCatalogName(t *testing.T) {
	t.Parallel()
	// specialists.filterToolsets matches a `tools.mcp: - server:`
	// allowlist entry to a toolset by Name(). A wrap that renamed would
	// empty every enumerated allowlist silently.
	inner := &fakeToolset{name: "gke"}
	wrapped := WithDigest(inner, "gke", &DigestOptions{})
	if wrapped.Name() != "gke" {
		t.Errorf("Name() = %q, want the catalog key %q", wrapped.Name(), "gke")
	}
}

func TestANonRunnableToolIsPassedThroughUntouched(t *testing.T) {
	t.Parallel()
	inert := inertTool{name: "declaration_only"}
	wrapped := WithDigest(&fakeToolset{name: "gke", tools: []tool.Tool{inert}}, "gke", &DigestOptions{})
	tools, err := wrapped.Tools(nil)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 || tools[0] != tool.Tool(inert) {
		t.Errorf("a tool with no Run should come back as itself, got %#v", tools)
	}
}

func TestAToolsetErrorIsNotSwallowed(t *testing.T) {
	t.Parallel()
	want := errors.New("server unreachable")
	wrapped := WithDigest(&fakeToolset{name: "gke", err: want}, "gke", &DigestOptions{})
	if _, err := wrapped.Tools(nil); !errors.Is(err, want) {
		t.Errorf("Tools error = %v, want %v", err, want)
	}
}

func TestTheWrapCanBeSeenThroughByAMastSideCaller(t *testing.T) {
	t.Parallel()
	// The write gate's precondition read runs the tool mast wired, not
	// the digest of it (cmd/mast/toolschemas.go). Unwrap is how it gets
	// there, so the wrap must keep announcing it.
	inner := &fakeTool{name: "read_status", response: map[string]any{"state": "steady"}}
	wrapped := WithDigest(&fakeToolset{name: "gke", tools: []tool.Tool{inner}}, "gke", &DigestOptions{})
	tools, err := wrapped.Tools(nil)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	u, ok := tools[0].(interface{ Unwrap() tool.Tool })
	if !ok {
		t.Fatalf("a wrapped tool must be unwrappable, got %T", tools[0])
	}
	if u.Unwrap() != tool.Tool(inner) {
		t.Errorf("Unwrap() = %#v, want the tool that was wrapped", u.Unwrap())
	}
}

func TestASmallResponseKeepsItsOwnShape(t *testing.T) {
	t.Parallel()
	// The headline divergence from upstream: below the threshold the
	// model reads the tool's own map, not a JSON string under a
	// "digest" key.
	want := map[string]any{"kind": "Pod", "phase": "Running"}
	inner := &fakeTool{name: "get_k8s_resource", response: want}
	wrapped := WithDigest(&fakeToolset{name: "gke", tools: []tool.Tool{inner}}, "gke", &DigestOptions{})

	got, err := runWrapped(t, wrapped, "call-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("an undigested response must come back verbatim:\n got %#v\nwant %#v", got, want)
	}
}

func TestAnUndigestedResponseIsIdenticalCallToCall(t *testing.T) {
	t.Parallel()
	// The invariant a sidecar would break. mast compares two reads of
	// the same tool for equality — pkg/approval voids a change-set grant
	// when its precondition read stops returning what it returned at
	// approval time — so anything the wrap adds to a response it does
	// not digest (a wall clock above all) turns a still cluster into a
	// moved one and revokes an approval the operator already gave.
	inner := &fakeTool{name: "read_status", response: map[string]any{"state": "steady"}}
	wrapped := WithDigest(&fakeToolset{name: "gke", tools: []tool.Tool{inner}}, "gke", &DigestOptions{})

	first, err := runWrapped(t, wrapped, "call-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// A second call late enough that any wall-clock field would differ.
	time.Sleep(2 * time.Millisecond)
	second, err := runWrapped(t, wrapped, "call-2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two reads of an unchanged tool must compare equal:\n first %#v\nsecond %#v", first, second)
	}
}

func TestTheOriginalResponseMapIsNotMutated(t *testing.T) {
	t.Parallel()
	// The map belongs to the tool that produced it and an MCP toolset is
	// free to reuse or cache one, so the wrap adds no key to it — not
	// even on the way past.
	original := map[string]any{"kind": "Pod"}
	inner := &fakeTool{name: "get_k8s_resource", response: original}
	wrapped := WithDigest(&fakeToolset{name: "gke", tools: []tool.Tool{inner}}, "gke", &DigestOptions{})

	if _, err := runWrapped(t, wrapped, "call-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(original) != 1 || original["kind"] != "Pod" {
		t.Errorf("the tool's own map was written to: %#v", original)
	}
}

func TestALargeResponseIsPrunedAndKeepsAWayBack(t *testing.T) {
	t.Parallel()
	store := &memStore{}
	inner := &fakeTool{name: "get_k8s_resource", response: bigList(200)}
	wrapped := WithDigest(&fakeToolset{name: "gke", tools: []tool.Tool{inner}}, "gke",
		&DigestOptions{Store: store})

	got, err := runWrapped(t, wrapped, "call-42")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got["method"] != digest.MethodStructuralJSON {
		t.Errorf("method = %v, want %v", got["method"], digest.MethodStructuralJSON)
	}
	body, _ := got["digest"].(string)
	if body == "" {
		t.Fatalf("expected a digest body, got %#v", got)
	}
	rawBytes, _ := got["raw_bytes"].(int)
	if len(body) >= rawBytes {
		t.Errorf("digest (%d bytes) did not reduce the raw payload (%d bytes)", len(body), rawBytes)
	}
	// The escape hatch: the call id came back, and the store can
	// answer it. Without both, the pruning above is unappealable.
	if got["call_id"] != "call-42" {
		t.Errorf("call_id = %v, want call-42", got["call_id"])
	}
	stored, err := store.Get(context.Background(), "call-42")
	if err != nil {
		t.Fatalf("the raw payload was not stored: %v", err)
	}
	if len(stored) != rawBytes {
		t.Errorf("stored %d bytes, response reported raw_bytes = %d", len(stored), rawBytes)
	}
	savings, ok := got["savings"].(map[string]any)
	if !ok {
		t.Fatalf("expected a savings sidecar, got %#v", got["savings"])
	}
	if savings["path"] != digest.MethodStructuralJSON {
		t.Errorf("savings.path = %v, want %v", savings["path"], digest.MethodStructuralJSON)
	}
	if _, ok := savings["subagent_model"]; ok {
		t.Errorf("mast's wrap has no subagent to charge; savings must not claim one: %#v", savings)
	}
}

func TestAFailedToolCallReturnsItsOwnErrorAndItsOwnPartialResponse(t *testing.T) {
	t.Parallel()
	// A failure is the path the wrap has least business editing: the
	// caller is already handling something that went wrong.
	want := errors.New("mcp: 504 from upstream")
	partial := map[string]any{"partial": true}
	inner := &fakeTool{name: "get_k8s_logs", response: partial, err: want}
	wrapped := WithDigest(&fakeToolset{name: "gke", tools: []tool.Tool{inner}}, "gke", &DigestOptions{})

	got, err := runWrapped(t, wrapped, "call-1")
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want the upstream error verbatim", err)
	}
	if !reflect.DeepEqual(got, partial) {
		t.Errorf("the partial response should come back verbatim:\n got %#v\nwant %#v", got, partial)
	}
}

func TestANilResponseStaysNil(t *testing.T) {
	t.Parallel()
	// Some ADK/MCP error paths return (nil, err); the wrap must not
	// conjure an empty map out of one.
	inner := &fakeTool{name: "get_k8s_logs", err: errors.New("boom")}
	wrapped := WithDigest(&fakeToolset{name: "gke", tools: []tool.Tool{inner}}, "gke", &DigestOptions{})

	got, err := runWrapped(t, wrapped, "call-1")
	if err == nil {
		t.Fatal("expected the tool's error")
	}
	if got != nil {
		t.Errorf("a nil response should stay nil, got %#v", got)
	}
}

func TestOnResultSeesRoutedCallsOnly(t *testing.T) {
	t.Parallel()
	var seen []string
	opts := &DigestOptions{OnResult: func(r *digest.Result) { seen = append(seen, r.Method) }}

	small := &fakeTool{name: "small", response: map[string]any{"ok": true}}
	if _, err := runWrapped(t, WithDigest(&fakeToolset{name: "gke", tools: []tool.Tool{small}}, "gke", opts), "c1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(seen) != 0 {
		t.Errorf("a below-threshold call never reaches the router, got %v", seen)
	}

	big := &fakeTool{name: "big", response: bigList(200)}
	if _, err := runWrapped(t, WithDigest(&fakeToolset{name: "gke", tools: []tool.Tool{big}}, "gke", opts), "c2"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(seen) != 1 || seen[0] != digest.MethodStructuralJSON {
		t.Errorf("OnResult = %v, want one structural_json", seen)
	}
}

func TestAThresholdOverrideMovesTheLine(t *testing.T) {
	t.Parallel()
	// Proves the default is a value and not a hard-coded branch: the
	// same tiny response digests once the threshold drops under it.
	inner := &fakeTool{name: "tiny", response: map[string]any{"kind": "Pod"}}
	wrapped := WithDigest(&fakeToolset{name: "gke", tools: []tool.Tool{inner}}, "gke",
		&DigestOptions{Threshold: 1})

	got, err := runWrapped(t, wrapped, "call-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := got["digest"]; !ok {
		t.Errorf("with Threshold=1 even a tiny response routes, got %#v", got)
	}
}

func TestTheWrapperStaysOnTheDispatchPath(t *testing.T) {
	t.Parallel()
	// ADK's flow looks a call up in req.Tools by name. An inner tool
	// that packs itself would put the undigested original back and the
	// wrap would be inert — a declaration from the wrapper and a
	// response from the tool underneath it.
	inner := &selfPackingTool{fakeTool: fakeTool{name: "get_k8s_resource", response: map[string]any{"kind": "Pod"}}}
	wrapped := WithDigest(&fakeToolset{name: "gke", tools: []tool.Tool{inner}}, "gke", &DigestOptions{})
	tools, err := wrapped.Tools(nil)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	rp, ok := tools[0].(interface {
		ProcessRequest(adkagent.Context, *model.LLMRequest) error
	})
	if !ok {
		t.Fatalf("the wrapper must implement ProcessRequest")
	}
	req := &model.LLMRequest{}
	if err := rp.ProcessRequest(newStubToolContext("c1"), req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	if !inner.packed {
		t.Errorf("the inner tool's own ProcessRequest must still run for its side effects")
	}
	if got := req.Tools["get_k8s_resource"]; got != tools[0] {
		t.Errorf("req.Tools holds %#v, want the digesting wrapper", got)
	}
}

func TestAToolThatDoesNotPackItselfIsStillRegistered(t *testing.T) {
	t.Parallel()
	// The fallback branch: no ProcessRequest on the inner tool at all,
	// so the wrapper packs itself via toolutils.
	inner := &fakeTool{name: "get_k8s_resource", response: map[string]any{"kind": "Pod"}}
	wrapped := WithDigest(&fakeToolset{name: "gke", tools: []tool.Tool{inner}}, "gke", &DigestOptions{})
	tools, _ := wrapped.Tools(nil)
	rp := tools[0].(interface {
		ProcessRequest(adkagent.Context, *model.LLMRequest) error
	})
	req := &model.LLMRequest{}
	if err := rp.ProcessRequest(newStubToolContext("c1"), req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	if got := req.Tools["get_k8s_resource"]; got != tools[0] {
		t.Errorf("req.Tools holds %#v, want the digesting wrapper", got)
	}
}

// selfPackingTool is a fakeTool that registers itself the way
// mcptoolset's tool does, so the re-pack branch is exercised.
type selfPackingTool struct{ fakeTool }

func (s *selfPackingTool) ProcessRequest(_ adkagent.Context, req *model.LLMRequest) error {
	s.packed = true
	if req.Tools == nil {
		req.Tools = map[string]any{}
	}
	req.Tools[s.Name()] = &s.fakeTool
	return nil
}
