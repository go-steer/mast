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
	"context"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/agui"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/workload"
)

// gatedModel answers only once released, so a test can hold a turn open and
// stack runs behind it. Unlike blockableModel it does not block on ctx: the
// point here is a turn that is healthy and slow, not one being cancelled.
type gatedModel struct {
	started chan struct{} // one send per generation begun (buffered by the test)
	release chan struct{} // closed to let every held generation answer
}

func (m *gatedModel) Name() string { return "gated" }

func (m *gatedModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		select {
		case m.started <- struct{}{}:
		default:
		}
		<-m.release
		yield(&model.LLMResponse{
			Content: genai.NewContentFromText("done", genai.RoleModel),
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2,
			},
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}, nil)
	}
}

// countingRunMetric records the AG-UI run outcomes the server reports.
type countingRunMetric struct {
	mu sync.Mutex
	by map[string]int
}

func (c *countingRunMetric) AGUIRun(_, outcome string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.by == nil {
		c.by = map[string]int{}
	}
	c.by[outcome]++
}

func (c *countingRunMetric) count(outcome string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.by[outcome]
}

// waitForInFlight polls the backend's queue until sessionID holds want slots.
// Polling the counter rather than sleeping is what makes the ordering in these
// tests deterministic: a run that is merely queued never reaches the model, so
// there is no other signal that it has been admitted.
func waitForInFlight(t *testing.T, b *aguiBackend, sessionID string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b.queue.inFlight(sessionID) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %q held %d run slots, want %d", sessionID, b.queue.inFlight(sessionID), want)
}

// postRun fires one run at the endpoint and returns its response.
func postRun(t *testing.T, ts *httptest.Server, threadID, runID string) *http.Response {
	t.Helper()
	body := `{"threadId":"` + threadID + `","runId":"` + runID + `","messages":[{"role":"user","content":"go"}]}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/agui/w", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("post run %s: %v", runID, err)
	}
	return resp
}

// newQueueHarness wires a real turn stack behind an HTTP server, with the
// workload's run-queue depth set to depth.
func newQueueHarness(t *testing.T, depth int) (*aguiBackend, *httptest.Server, *countingRunMetric, *gatedModel) {
	t.Helper()
	m := &gatedModel{started: make(chan struct{}, 8), release: make(chan struct{})}
	h := newTurnHarness(t, m)
	b := &aguiBackend{
		turnDeps: h.deps(),
		bundle: &workload.Bundle{
			Name: "(test)",
			AGUI: workload.AGUI{Expose: true, RunQueue: workload.AGUIRunQueue{Depth: &depth}},
		},
	}
	metric := &countingRunMetric{}
	srv, err := agui.New(agui.Config{
		Backend: b,
		Metric:  metric,
		Exposed: []agui.ExposedWorkload{{WorkloadName: "(test)", EndpointPath: "/agui/w"}},
	})
	if err != nil {
		t.Fatalf("agui.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return b, ts, metric, m
}

// TestAGUIThreadQueueShedsPastItsDepth is the #384 gate: a run arriving at a
// thread that is already full is refused with a status the caller can act on,
// rather than joining an unbounded wait on the session turn lock whose only
// ceiling is the workload's wallclock budget.
//
// depth 1 means two runs may be in flight on the thread — one executing, one
// waiting — so the third is the one shed.
//
// Neutralize check: drop the admit call in RunAgent and the third run does not
// return at all until the gate opens, at which point it answers 200.
func TestAGUIThreadQueueShedsPastItsDepth(t *testing.T) {
	b, ts, metric, m := newQueueHarness(t, 1)
	sessionID := aguiSessionPrefix + "thread-t-q"

	// Run A executes and parks inside the model, holding the turn lock.
	var wg sync.WaitGroup
	for _, id := range []string{"rA", "rB"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := postRun(t, ts, "t-q", id)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	select {
	case <-m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("no turn ever started; harness is wrong")
	}
	// Run B is admitted but blocked on the turn lock: it never reaches the
	// model, so the slot count is the only thing that says it is in.
	waitForInFlight(t, b, sessionID, 2)

	// Run C finds the thread full.
	resp := postRun(t, ts, "t-q", "rC")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("third run status = %d, want %d (409); body %q", resp.StatusCode, http.StatusConflict, body)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Error("a queue-full refusal carries no Retry-After; a client has nothing to pace itself with")
	}
	// The refusal must be decided before the SSE upgrade, or a client reading
	// the stream gets a truncated event stream instead of a status it can read.
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("refusal opened an event stream (Content-Type %q); it must precede the upgrade", ct)
	}
	if !strings.Contains(string(body), "in flight on this thread") {
		t.Errorf("refusal body %q does not say why the run was refused", body)
	}
	if got := metric.count(observability.AGUIRunQueueFull); got != 1 {
		t.Errorf("mast_agui_runs_total{outcome=queue_full} = %d, want 1", got)
	}
	if got := metric.count(observability.AGUIRunRejected); got != 0 {
		t.Errorf("the shed run was also counted as %q (%d); the two refusals must stay distinguishable",
			observability.AGUIRunRejected, got)
	}

	close(m.release)
	wg.Wait()

	// The slots are given back, so the thread is usable again — a full queue is
	// backpressure, not a latch.
	waitForInFlight(t, b, sessionID, 0)
	after := postRun(t, ts, "t-q", "rD")
	_, _ = io.Copy(io.Discard, after.Body)
	_ = after.Body.Close()
	if after.StatusCode != http.StatusOK {
		t.Errorf("run after the queue drained = %d, want 200", after.StatusCode)
	}
	if got := metric.count(observability.AGUIRunSuccess); got != 3 {
		t.Errorf("successful runs = %d, want 3 (the two queued plus the one after)", got)
	}
}

// A busy thread must not refuse a run addressed to a different one: the bound
// is per conversation, which is the whole reason it is not the 429 the
// arrival-rate limiter already owns.
func TestAGUIThreadQueueIsPerThread(t *testing.T) {
	b, ts, _, m := newQueueHarness(t, 0) // no queueing at all: one run per thread
	defer close(m.release)

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp := postRun(t, ts, "t-busy", "r1")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	select {
	case <-m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("no turn ever started; harness is wrong")
	}
	waitForInFlight(t, b, aguiSessionPrefix+"thread-t-busy", 1)

	// Same thread: refused, because depth 0 admits only the executing run.
	same := postRun(t, ts, "t-busy", "r2")
	_, _ = io.Copy(io.Discard, same.Body)
	_ = same.Body.Close()
	if same.StatusCode != http.StatusConflict {
		t.Errorf("second run on the busy thread = %d, want 409", same.StatusCode)
	}

	// Different thread: admitted. It blocks in the model like the first one,
	// which is fine — what matters is that it was not refused.
	other := make(chan int, 1)
	go func() {
		resp := postRun(t, ts, "t-other", "r3")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		other <- resp.StatusCode
	}()
	waitForInFlight(t, b, aguiSessionPrefix+"thread-t-other", 1)
}

// The accounting itself: the boundary is inclusive, slots come back, and an
// entry is deleted at zero rather than left behind. The deletion matters
// because the keys are client-chosen thread ids, and under session_model:
// per_run there is a fresh one per run forever.
func TestAGUIRunQueueAccounting(t *testing.T) {
	var q aguiRunQueue

	r1, ok := q.admit("s", 2)
	if !ok {
		t.Fatal("first admit refused on an empty queue")
	}
	r2, ok := q.admit("s", 2)
	if !ok {
		t.Fatal("second admit refused below the limit")
	}
	if _, ok := q.admit("s", 2); ok {
		t.Error("third admit accepted at the limit")
	}
	if n := q.inFlight("s"); n != 2 {
		t.Errorf("in flight = %d, want 2 (a refusal must not consume a slot)", n)
	}

	r1()
	if _, ok := q.admit("s", 2); !ok {
		t.Error("a released slot was not reusable")
	} else {
		q.mu.Lock()
		q.n["s"]-- // put it back by hand; this branch only needed the answer
		q.mu.Unlock()
	}
	r2()
	q.mu.Lock()
	_, present := q.n["s"]
	q.mu.Unlock()
	if present {
		t.Error("the entry survived at zero; the map grows one key per thread id forever")
	}

	// A limit of zero admits nothing, which is what a depth below any legal
	// value would mean if one ever reached here.
	if _, ok := q.admit("s2", 0); ok {
		t.Error("admit accepted against a limit of zero")
	}
}

// The depth an operator did not set, and the arithmetic between "runs that may
// wait" and "runs that may be in flight". Off by one here is the difference
// between the documented default and a queue one shorter than it says.
func TestAGUIRunQueueDepthDefaults(t *testing.T) {
	if got := (workload.AGUI{}).RunQueueDepth(); got != workload.AGUIDefaultRunQueueDepth {
		t.Errorf("absent depth = %d, want the default %d", got, workload.AGUIDefaultRunQueueDepth)
	}
	zero := 0
	if got := (workload.AGUI{RunQueue: workload.AGUIRunQueue{Depth: &zero}}).RunQueueDepth(); got != 0 {
		t.Errorf("explicit depth 0 = %d, want 0 — a pointer exists precisely so 0 differs from absent", got)
	}
	if got := (&aguiBackend{}).runQueueLimit(); got != workload.AGUIDefaultRunQueueDepth+1 {
		t.Errorf("nil-bundle limit = %d, want %d (the default depth plus the executing run)",
			got, workload.AGUIDefaultRunQueueDepth+1)
	}
	seven := 7
	b := &aguiBackend{bundle: &workload.Bundle{AGUI: workload.AGUI{RunQueue: workload.AGUIRunQueue{Depth: &seven}}}}
	if got := b.runQueueLimit(); got != 8 {
		t.Errorf("limit for depth 7 = %d, want 8", got)
	}
}

// A negative depth is refused at startup. It can only be a typo or a
// misremembered "-1 means unbounded", and unbounded is what the key exists to
// end — so clamping it would hand that author the default they did not ask for.
func TestBuildAGUIServerRefusesNegativeQueueDepth(t *testing.T) {
	neg := -1
	bundle := &workload.Bundle{
		Name: "w",
		AGUI: workload.AGUI{Expose: true, RunQueue: workload.AGUIRunQueue{Depth: &neg}},
	}
	_, err := buildAGUIServer(discardLogger(), "127.0.0.1:0", bundle, &aguiBackend{}, nil, context.Background())
	if err == nil {
		t.Fatal("a negative run_queue.depth started the server")
	}
	for _, want := range []string{"run_queue.depth", "cannot be negative"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
