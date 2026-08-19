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
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
)

// This file covers newMCPClient's refusal of server-initiated input
// requests (SEP-2322 and its classic-protocol equivalent).
//
// A server can ask the client for input in the middle of a call the client
// already made. Answering it is a round trip mast's approval gate never
// sees — the gate is on dispatch — and, on the multi-round-trip path, the
// answer licenses a retry of the original tool call, so one approved
// dispatch runs the server's tool up to ten times.
//
// There are two regimes and mast has to close both, which is why the fix is
// not the one-line MultiRoundTripOptions.Disabled the SDK docs suggest:
//
//   - Protocol >= 2026-07-28 (stdio, and *stateless* streamable HTTP —
//     StreamableServerTransport.SupportsProtocolVersion serves the new
//     version only when stateless). The client-side middleware fulfills and
//     retries. Disabled turns it off and the input-required result comes
//     back to the caller.
//   - Protocol 2025-11-25 (an ordinary *stateful* streamable HTTP server,
//     the common deployment). The server's own middleware sends a real
//     elicitation/sampling/roots request to the client. Disabled has no
//     effect here at all; the receiving-side refusal is what closes it.
//
// The TestSDKStill* cases pin the SDK behavior each half counters, built
// like TestSDKStillDropsTheseBodies in errbody_test.go: when one stops
// holding, the default has moved and the opt-out wants re-reading against
// whatever replaced it.

// inputRequestTool is the name of the tool the fixtures below expose.
const inputRequestTool = "needs-input"

// inputRequestServer stands up a streamable-HTTP MCP server whose only tool
// answers every call with a server-initiated input request and no content,
// and returns its URL plus a counter of how many times the tool actually
// ran. The count is what separates "the client surfaced the request" (one
// run) from "the client answered it and the call went round again".
//
// stateless picks the protocol regime: a stateless server serves
// >= 2026-07-28 and puts the client-side middleware in play, a stateful one
// negotiates 2025-11-25 and puts the server's own middleware in play.
//
// A result may not carry both content and input requests — the SDK rejects
// that shape server-side (mcp/mrtr.go handleMultiRoundTripResult) — so the
// fixture returns input requests alone.
func inputRequestServer(t *testing.T, stateless bool, req mcpsdk.InputRequest) (string, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "mast-test-mrtr", Version: "0.0.1"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: inputRequestTool, Description: "asks the client for input"},
		func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, struct{}, error) {
			calls.Add(1)
			return &mcpsdk.CallToolResult{
				InputRequests: mcpsdk.InputRequestMap{"need": req},
			}, struct{}{}, nil
		})

	hs := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{Stateless: stateless}))
	// ADK's connectionRefresher never closes the MCP session it opens, so
	// the SSE stream outlives the test and a plain Close blocks on it.
	t.Cleanup(func() {
		hs.CloseClientConnections()
		hs.Close()
	})
	return hs.URL, &calls
}

// callInputRequestTool connects c to url and calls the fixture tool once.
func callInputRequestTool(t *testing.T, c *mcpsdk.Client, url string) (*mcpsdk.CallToolResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := c.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = sess.Close() }()

	return sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: inputRequestTool})
}

func defaultClient() *mcpsdk.Client {
	return mcpsdk.NewClient(&mcpsdk.Implementation{Name: "mast-test", Version: "0"}, nil)
}

// rootsRequest and samplingRequest exist to hold the deprecation waiver in
// one place. SEP-2577 deprecated roots, sampling, and logging as of protocol
// 2026-07-28, but deprecated here means live for at least twelve more months
// — and a deprecated-but-live feature is precisely the shape of the gap this
// file covers, since the SDK still answers roots/list out of the box. The
// tests have to speak these to prove mast does not.
func rootsRequest() mcpsdk.InputRequest {
	return &mcpsdk.ListRootsParams{} //nolint:staticcheck // SA1019: deprecated by SEP-2577, functional until at least 2027-07-28
}

func samplingRequest() mcpsdk.InputRequest {
	return &mcpsdk.CreateMessageParams{MaxTokens: 16} //nolint:staticcheck // SA1019: deprecated by SEP-2577, functional until at least 2027-07-28
}

// inputRequestCases are the three requests a server can raise. Roots is
// first because it is the one nothing was stopping: the SDK answers
// roots/list itself, with no handler registered and no capability opted
// into, so a server could complete a round trip with mast today.
//
// stoppedAtTheServer marks the one that never reaches mast's middleware on
// the classic protocol. ServerSession.Elicit checks the client's advertised
// elicitation capability before sending (server.go:1667) and mast advertises
// none; ServerSession.CreateMessageWithTools has no such check, and roots is
// advertised by the SDK's default. Two overlapping controls, and it is worth
// knowing which one caught what — the capability is a claim about mast, the
// middleware is a decision about the request.
var inputRequestCases = []struct {
	name               string
	req                mcpsdk.InputRequest
	stoppedAtTheServer bool
}{
	{name: "roots", req: rootsRequest()},
	{name: "elicitation", req: &mcpsdk.ElicitParams{Mode: "form", Message: "confirm the destructive action"}, stoppedAtTheServer: true},
	{name: "sampling", req: samplingRequest()},
}

// TestMastClientSurfacesInputRequests covers the new-protocol regime: with
// the multi-round-trip middleware off, the input-required result reaches
// the caller and the tool runs exactly once.
func TestMastClientSurfacesInputRequests(t *testing.T) {
	for _, tc := range inputRequestCases {
		t.Run(tc.name, func(t *testing.T) {
			url, calls := inputRequestServer(t, true, tc.req)

			res, err := callInputRequestTool(t, newMCPClient(), url)
			if err != nil {
				t.Fatalf("CallTool = %v, want the input-required result surfaced", err)
			}
			if !res.NeedsInput() {
				t.Errorf("NeedsInput() = false, want true — the caller cannot gate a request it is not told about")
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("the tool ran %d times, want 1 — the client answered the server and the call went round again", got)
			}
		})
	}
}

// TestMastClientRefusesInputRequests covers the classic-protocol regime,
// the one MultiRoundTripOptions.Disabled does not reach. Here the server
// asks the client directly and the refusal has to come from mast.
func TestMastClientRefusesInputRequests(t *testing.T) {
	for _, tc := range inputRequestCases {
		t.Run(tc.name, func(t *testing.T) {
			url, calls := inputRequestServer(t, false, tc.req)

			res, err := callInputRequestTool(t, newMCPClient(), url)
			if err == nil {
				t.Fatalf("CallTool succeeded (needsInput=%v); the server's input request was answered, not refused", res.NeedsInput())
			}
			if !tc.stoppedAtTheServer && !strings.Contains(err.Error(), "refusing server-initiated") {
				t.Errorf("CallTool = %v, want mast's refusal", err)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("the tool ran %d times, want 1 — a refused round trip must not re-dispatch the call", got)
			}
		})
	}
}

// TestSDKStillAutoFulfillsOnTheNewProtocol pins what Disabled counters: a
// default client answers the server and calls again, ten times over for
// roots, which needs no handler to succeed.
func TestSDKStillAutoFulfillsOnTheNewProtocol(t *testing.T) {
	url, calls := inputRequestServer(t, true, rootsRequest())

	res, err := callInputRequestTool(t, defaultClient(), url)
	if err == nil {
		t.Fatalf("the SDK default no longer auto-fulfills roots (needsInput=%v) — re-read newMCPClient against the new default", res.NeedsInput())
	}
	if !strings.Contains(err.Error(), "multi-round-trip") {
		t.Errorf("CallTool = %v, want the multi-round-trip retry cap", err)
	}
	if got := calls.Load(); got < 2 {
		t.Errorf("the tool ran %d times, want the default client to have retried it — the auto-fulfill loop is gone", got)
	}
}

// TestSDKStillAnswersRootsOnTheClassicProtocol pins the half that is not
// about SEP-2322 at all. On 2025-11-25 the server asks the client directly,
// and a default client answers roots/list out of its own (empty) root set —
// no handler, no capability opted into, nothing configured. The round trip
// completes and the call is re-dispatched.
//
// This is why the fix cannot be an absence of handlers, and cannot be
// MultiRoundTripOptions.Disabled alone: Disabled governs the client-side
// middleware, and the client-side middleware is not what ran here.
func TestSDKStillAnswersRootsOnTheClassicProtocol(t *testing.T) {
	url, calls := inputRequestServer(t, false, rootsRequest())

	_, _ = callInputRequestTool(t, defaultClient(), url)
	if got := calls.Load(); got < 2 {
		t.Errorf("the tool ran %d times, want more — nothing answered roots/list, so re-read refuseServerInitiatedInput against the new default", got)
	}
}

// mrtrToolContext drives an ADK tool from a plain context.
type mrtrToolContext struct {
	adkagent.ContextMock
	ctx context.Context
}

func (c *mrtrToolContext) Deadline() (time.Time, bool) { return c.ctx.Deadline() }
func (c *mrtrToolContext) Done() <-chan struct{}       { return c.ctx.Done() }
func (c *mrtrToolContext) Err() error                  { return c.ctx.Err() }
func (c *mrtrToolContext) Value(key any) any           { return c.ctx.Value(key) }

type mrtrToolRunner interface {
	Run(adkagent.Context, any) (map[string]any, error)
}

// runInputRequestTool drives the fixture tool through the whole mast stack —
// NewToolset, ADK's mcptoolset, the client mast built — and returns the
// error the caller sees.
func runInputRequestTool(t *testing.T, cfg ServerConfig) error {
	t.Helper()
	ts, err := NewToolset(context.Background(), "mrtr", cfg)
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tools, err := ts.Tools(roCtx{Context: ctx})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	var target tool.Tool
	for _, tl := range tools {
		if tl.Name() == inputRequestTool {
			target = tl
		}
	}
	if target == nil {
		t.Fatalf("server did not expose %q", inputRequestTool)
	}
	runner, ok := target.(mrtrToolRunner)
	if !ok {
		t.Fatalf("tool %T is not runnable", target)
	}
	if _, err = runner.Run(&mrtrToolContext{ctx: ctx}, map[string]any{}); err == nil {
		t.Fatal("Run succeeded against a tool that only returns input requests")
	}
	return err
}

// TestToolsetDoesNotAnswerInputRequests covers both construction sites in
// toolset.go, because wiring the client at one of them and not the other is
// the obvious way for this fix to half-land. HTTP takes the classic-protocol
// path (httptest servers are stateful, as most deployed ones are) and stdio
// takes the new-protocol one, so the two subtests also happen to cover one
// regime each.
func TestToolsetDoesNotAnswerInputRequests(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		url, calls := inputRequestServer(t, false, rootsRequest())

		err := runInputRequestTool(t, ServerConfig{Transport: TransportHTTP, URL: url})
		if !strings.Contains(err.Error(), "refusing server-initiated") {
			t.Errorf("tool error = %v, want mast's refusal — the HTTP toolset is not using mast's client", err)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("the tool ran %d times, want 1", got)
		}
	})

	// On stdio the request never reaches the refusal: the client-side
	// middleware is off, so the SDK hands the input-required result back and
	// ADK — which has no notion of one — reports a result with no content.
	// That is the correct outcome for a mast that does not support
	// elicitation. The assertion is negative by necessity: what distinguishes
	// fixed from broken is which error comes back, not whether one does.
	t.Run("stdio", func(t *testing.T) {
		err := runInputRequestTool(t, ServerConfig{
			Transport: TransportStdio,
			Command:   buildTestMCPServer(t),
			Env:       map[string]string{"MCP_TEST_INPUT_REQUEST": "1"},
		})
		if strings.Contains(err.Error(), "multi-round-trip") || strings.Contains(err.Error(), "multi round-trip") {
			t.Errorf("the stdio toolset's client answered a server input request instead of surfacing it: %v", err)
		}
		if !strings.Contains(err.Error(), "no text content") {
			t.Errorf("tool error = %v, want ADK's empty-result report for a surfaced input request", err)
		}
	})
}

// buildTestMCPServer compiles testdata/mcpserver and returns the binary.
func buildTestMCPServer(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH; skipping stdio input-request check")
	}
	bin := filepath.Join(t.TempDir(), "mcpserver")
	build := exec.Command(goBin, "build", "-o", bin, "./testdata/mcpserver")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build testdata mcpserver: %v\n%s", err, out)
	}
	return bin
}
