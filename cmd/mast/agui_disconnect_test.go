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
	"sync/atomic"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/agui"
	"github.com/go-steer/mast/pkg/watchdog"
)

// aguiCtxModel is a scripted model whose script sees the turn's context, so a
// test can tell a turn that was cancelled apart from one that merely finished:
// a round that blocks does so on ctx rather than on a timer.
type aguiCtxModel struct {
	gen func(context.Context, *model.LLMRequest) (*model.LLMResponse, error)
}

func (m *aguiCtxModel) Name() string { return "agui-ctx" }

func (m *aguiCtxModel) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		resp, err := m.gen(ctx, req)
		if err != nil {
			yield(nil, err)
			return
		}
		if resp.UsageMetadata == nil {
			resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2,
			}
		}
		resp.TurnComplete = true
		resp.FinishReason = genai.FinishReasonStop
		yield(resp, nil)
	}
}

// sawApplyChangeResult reports whether the request's history already carries a
// result for apply_change — that is, whether a well-behaved model would know
// the change had been applied.
func sawApplyChangeResult(req *model.LLMRequest) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == "apply_change" {
				return true
			}
		}
	}
	return false
}

// TestAGUIDisconnectMidToolDoesNotRefireOnRetry is the reason #383 is a
// correctness fix and not the reconnect feature it was filed under.
//
// The client's connection dies while a mutating tool is still executing: the
// side effect has certainly happened, and its FunctionResponse has certainly
// not been persisted. Before the fix the turn ctx descended from
// r.Context(), so the reset cancelled the turn there — leaving the session
// with no record of the call — and the client's retry re-applied the change.
//
// The scripted model is deliberately WELL BEHAVED: it calls the tool only when
// no prior result for it appears in the request's history. So a second call is
// not a misbehaving model, it is a correct model reacting to a history in
// which the first call left no trace.
//
// Neutralize check: derive streamCtx from ctx instead of
// context.WithoutCancel(ctx) and apply_change fires twice.
func TestAGUIDisconnectMidToolDoesNotRefireOnRetry(t *testing.T) {
	var calls atomic.Int32
	fired := make(chan struct{}, 4)
	release := make(chan struct{})

	applyChange, err := functiontool.New(functiontool.Config{
		Name:        "apply_change",
		Description: "apply a configuration change (mutating)",
	}, func(adkagent.Context, struct {
		Target string `json:"target"`
	},
	) (map[string]any, error) {
		n := calls.Add(1)
		select {
		case fired <- struct{}{}:
		default:
		}
		if n == 1 {
			// The effect has happened. Hold here so the client can vanish
			// before the result is ever written back.
			<-release
		}
		return map[string]any{"applied": true}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	answered := make(chan struct{})
	var answeredOnce sync.Once
	m := &aguiCtxModel{gen: func(_ context.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		if !sawApplyChangeResult(req) {
			return aguiCallResponse("apply_change", map[string]any{"target": "prod"}), nil
		}
		answeredOnce.Do(func() { close(answered) })
		return &model.LLMResponse{
			Content: genai.NewContentFromText("change applied", genai.RoleModel),
		}, nil
	}}

	h := newTurnHarnessOpts(t, m, watchdog.ModeWarn, []tool.Tool{applyChange}...)
	b := &aguiBackend{turnDeps: h.deps()}
	srv, err := agui.New(agui.Config{
		Backend: b,
		Exposed: []agui.ExposedWorkload{{WorkloadName: "(test)", EndpointPath: "/agui/w"}},
	})
	if err != nil {
		t.Fatalf("agui.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		body := `{"threadId":"t-disc","runId":"r1","messages":[{"role":"user","content":"apply the change"}]}`
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/agui/w", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, derr := ts.Client().Do(req)
		if derr != nil {
			return
		}
		// Hold the stream open like a real consumer until the "network" dies.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	select {
	case <-fired:
	case <-time.After(10 * time.Second):
		t.Fatal("tool never fired; harness is wrong")
	}
	cancel()                           // the network blips, mid-tool
	time.Sleep(200 * time.Millisecond) // let the reset reach the server
	close(release)                     // the effect completes against a consumerless turn
	<-done

	// The turn must run to completion without its consumer: the model is
	// called again and sees the tool result, which is what persists the
	// FunctionResponse a retry will read.
	// Not Fatal: when this fails, the re-fire below is the consequence worth
	// seeing in the same run, and it is the half a caller actually pays for.
	select {
	case <-answered:
	case <-time.After(10 * time.Second):
		t.Error("the turn did not finish after its client vanished; it was cancelled with the request")
	}

	// The client retries the same work on the same thread.
	retry := `{"threadId":"t-disc","runId":"r2","messages":[{"role":"user","content":"apply the change"}]}`
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/agui/w", strings.NewReader(retry))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := ts.Client().Do(req2)
	if err != nil {
		t.Fatalf("retry Do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	_ = resp2.Body.Close()

	if total := calls.Load(); total != 1 {
		t.Errorf("apply_change fired %d times for one logical request, want 1", total)
	}
}
