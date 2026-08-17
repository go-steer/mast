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
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// iamDenialBody is the shape Google's container.googleapis.com/mcp
// surface returns for an IAM denial: HTTP 403 carrying a JSON-RPC
// *result* whose isError flag and text content hold the permission name
// the operator has to grant. Kept verbatim from the wire because the
// whole point of this file is that a shape one character off the
// standard error envelope loses the message.
const iamDenialBody = `{
	"id": 4,
	"jsonrpc": "2.0",
	"result": {
		"content": [{
			"text": "Permission 'mcp.googleapis.com/tools.call' denied on resource '//container.googleapis.com/mcp/projects/X' (or it may not exist).",
			"type": "text"
		}],
		"isError": true
	}
}`

const iamPermission = "mcp.googleapis.com/tools.call"

// canned answers every POST with one status and body, so a full MCP
// connect attempt lands on it.
func canned(t *testing.T, status int, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// connectThroughMast drives the real MCP client the way a workload does
// — NewToolset, then a first Tools() call to trigger the lazy connect —
// and returns the error an operator would see.
func connectThroughMast(t *testing.T, url string) error {
	t.Helper()
	ts, err := NewToolset(context.Background(), "gke", ServerConfig{
		Transport: TransportHTTP,
		URL:       url,
	})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := ts.Tools(roCtx{Context: ctx}); err != nil {
		return err
	}
	t.Fatal("Tools() succeeded against a server that only returns errors")
	return nil
}

// connectBareSDK drives the same exchange with no mast wrapping, so a
// test can pin what the SDK does on its own.
func connectBareSDK(t *testing.T, url string) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "mast-test", Version: "0"}, nil)
	sess, err := c.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: url}, nil)
	if err == nil {
		_ = sess.Close()
		t.Fatal("Connect succeeded against a server that only returns errors")
	}
	return err
}

// TestIAMDenialNamesThePermission is the case this file exists for: a
// 403 from the GKE MCP surface must reach the operator with the
// permission name in it, through the whole real stack.
func TestIAMDenialNamesThePermission(t *testing.T) {
	srv := canned(t, http.StatusForbidden, "application/json; charset=UTF-8", iamDenialBody)

	err := connectThroughMast(t, srv.URL)
	if !strings.Contains(err.Error(), iamPermission) {
		t.Errorf("error does not name the missing permission:\n  got: %v\n  want it to contain %q", err, iamPermission)
	}
	if !strings.Contains(err.Error(), "403 Forbidden") {
		t.Errorf("error dropped the HTTP status: %v", err)
	}
}

// TestSDKStillDropsTheseBodies pins the assumption errbody.go is built
// on: these are the exchanges go-sdk resolves to a bare status line.
// The moment the SDK starts surfacing one of them, this test fails and
// the corresponding branch here can go.
//
// The inverse case is pinned too — the SDK *does* surface a standard
// error object on a non-transient status, which is why errorTextFrom
// deliberately leaves that one alone rather than duplicating it and
// throwing away the typed *jsonrpc.Error the SDK puts in the chain.
func TestSDKStillDropsTheseBodies(t *testing.T) {
	dropped := []struct {
		name   string
		status int
		body   string
		text   string
	}{
		{
			name:   "tool-result error shape on a non-transient status",
			status: http.StatusForbidden,
			body:   iamDenialBody,
			text:   iamPermission,
		},
		{
			name:   "standard error object on a transient status",
			status: http.StatusTooManyRequests,
			body:   `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Quota exceeded for quota metric 'MCP calls'"}}`,
			text:   "Quota exceeded for quota metric",
		},
		{
			name:   "tool-result error shape on a transient status",
			status: http.StatusServiceUnavailable,
			body:   `{"id":1,"jsonrpc":"2.0","result":{"isError":true,"content":[{"type":"text","text":"backend unavailable; retry after the rollout"}]}}`,
			text:   "backend unavailable; retry after the rollout",
		},
	}
	for _, tc := range dropped {
		t.Run(tc.name, func(t *testing.T) {
			srv := canned(t, tc.status, "application/json", tc.body)

			bare := connectBareSDK(t, srv.URL)
			if strings.Contains(bare.Error(), tc.text) {
				t.Fatalf("the SDK now surfaces this body itself (%v) — drop mast's branch for it", bare)
			}
			wrapped := connectThroughMast(t, srv.URL)
			if !strings.Contains(wrapped.Error(), tc.text) {
				t.Errorf("mast did not recover the text the SDK dropped:\n  got: %v\n  want it to contain %q", wrapped, tc.text)
			}
		})
	}

	t.Run("standard error object on a non-transient status is the SDK's job", func(t *testing.T) {
		srv := canned(t, http.StatusBadRequest, "application/json",
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`)

		bare := connectBareSDK(t, srv.URL)
		if !strings.Contains(bare.Error(), "Method not found") {
			t.Fatalf("the SDK stopped surfacing standard error objects (%v) — errorTextFrom must now cover them", bare)
		}
		wrapped := connectThroughMast(t, srv.URL)
		if !strings.Contains(wrapped.Error(), "Method not found") {
			t.Errorf("wrapping lost a message the bare SDK surfaces: %v", wrapped)
		}
	})
}

// TestSessionMissingSentinelSurvives guards the 404 exclusion. The SDK
// translates a 404 to ErrSessionMissing so it can skip a redundant
// DELETE on teardown; extracting the body there would trade a sentinel
// the SDK acts on for a string nobody reads.
func TestSessionMissingSentinelSurvives(t *testing.T) {
	srv := canned(t, http.StatusNotFound, "application/json",
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"no such project"}}`)

	err := connectThroughMast(t, srv.URL)
	if !errors.Is(err, mcpsdk.ErrSessionMissing) {
		t.Errorf("404 no longer resolves to ErrSessionMissing: %v", err)
	}
}

// fakeRT returns a canned response without a network.
type fakeRT struct {
	resp    *http.Response
	err     error
	lastReq *http.Request
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.lastReq = req
	return f.resp, f.err
}

func resp(status int, contentType, body string) *http.Response {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func post(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/mcp", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

// mustPassThrough asserts the transport handed the response back
// untouched, body included.
func mustPassThrough(t *testing.T, req *http.Request, in *http.Response, wantBody string) {
	t.Helper()
	tr := &jsonRPCErrorTransport{base: &fakeRT{resp: in}}
	got, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip = %v, want the response passed through", err)
	}
	if got == nil {
		t.Fatal("RoundTrip returned a nil response")
	}
	b, readErr := io.ReadAll(got.Body)
	if readErr != nil {
		t.Fatalf("reading the passed-through body: %v", readErr)
	}
	if string(b) != wantBody {
		t.Errorf("body altered:\n  got: %q\n want: %q", b, wantBody)
	}
}

func TestTransportPassesThroughSuccess(t *testing.T) {
	mustPassThrough(t, post(t), resp(http.StatusOK, "application/json", `{"result":"ok"}`), `{"result":"ok"}`)
}

func TestTransportPassesThroughNonJSON(t *testing.T) {
	// An nginx-style HTML 502: nothing to extract, and the SDK's own
	// transient handling should see the response it expects.
	body := "<html><body>502 Bad Gateway</body></html>"
	mustPassThrough(t, post(t), resp(http.StatusBadGateway, "text/html", body), body)
}

func TestTransportPassesThroughUnrecognizedJSON(t *testing.T) {
	body := `{"totally":"unrelated"}`
	mustPassThrough(t, post(t), resp(http.StatusForbidden, "application/json", body), body)
}

// TestTransportPassesThroughResultWithoutIsError guards against reading
// a successful tool result as an error just because the status was bad.
func TestTransportPassesThroughResultWithoutIsError(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"all good"}]}}`
	mustPassThrough(t, post(t), resp(http.StatusForbidden, "application/json", body), body)
}

// TestTransportIgnoresNonPOST keeps the SSE reconnect (GET) and the
// session teardown (DELETE) out of the transport's way — failing those
// from here would spend the SDK's reconnect budget on a status it
// handles directly.
func TestTransportIgnoresNonPOST(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, "https://example.invalid/mcp", nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			mustPassThrough(t, req, resp(http.StatusForbidden, "application/json", iamDenialBody), iamDenialBody)
		})
	}
}

// TestTransportOversizedBodyPassesThrough pins the cap: a pathological
// server does not get to hold a turn open, and the response stays
// usable.
func TestTransportOversizedBodyPassesThrough(t *testing.T) {
	big := bytes.Repeat([]byte("x"), jsonRPCErrorBodyMax+16)
	body := `{"result":{"isError":true,"content":[{"type":"text","text":"` + string(big) + `"}]}}`

	tr := &jsonRPCErrorTransport{base: &fakeRT{resp: resp(http.StatusInternalServerError, "application/json", body)}}
	got, err := tr.RoundTrip(post(t))
	if err != nil {
		t.Fatalf("RoundTrip = %v, want pass-through", err)
	}
	b, _ := io.ReadAll(got.Body)
	if len(b) <= jsonRPCErrorBodyMax {
		t.Errorf("buffered body was truncated: %d bytes, want more than the %d cap", len(b), jsonRPCErrorBodyMax)
	}
}

func TestTransportPropagatesTransportError(t *testing.T) {
	sentinel := errors.New("dial tcp: connection refused")
	tr := &jsonRPCErrorTransport{base: &fakeRT{err: sentinel}}
	if _, err := tr.RoundTrip(post(t)); !errors.Is(err, sentinel) {
		t.Errorf("RoundTrip = %v, want the base error unchanged", err)
	}
}

// TestTransportReconstructsAnEmptyStatusLine covers the response a hand
// -built round tripper produces; net/http fills Status in, a fake often
// does not, and the operator-facing string should not read ": denied".
func TestTransportReconstructsAnEmptyStatusLine(t *testing.T) {
	tr := &jsonRPCErrorTransport{base: &fakeRT{resp: resp(http.StatusForbidden, "application/json", iamDenialBody)}}
	_, err := tr.RoundTrip(post(t))
	if err == nil {
		t.Fatal("expected an extracted error")
	}
	if !strings.HasPrefix(err.Error(), "403 Forbidden: ") {
		t.Errorf("error = %q, want it to open with the reconstructed status line", err)
	}
}

func TestTransportReportsAnUnreadableBody(t *testing.T) {
	bad := resp(http.StatusForbidden, "application/json", "")
	bad.Body = io.NopCloser(brokenReader{})

	tr := &jsonRPCErrorTransport{base: &fakeRT{resp: bad}}
	got, err := tr.RoundTrip(post(t))
	if err == nil {
		t.Fatal("expected an error once the body is spent")
	}
	if got != nil {
		t.Errorf("got a response alongside the error: %+v", got)
	}
	if !strings.Contains(err.Error(), "403 Forbidden") {
		t.Errorf("error dropped the status: %v", err)
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("mid-body reset") }

func TestIsJSONContentType(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/json":                 true,
		"application/json; charset=UTF-8":  true,
		"APPLICATION/JSON":                 true,
		"application/problem+json":         true,
		" application/json ;charset=utf-8": true,
		"text/html":                        false,
		"text/plain; charset=utf-8":        false,
		"":                                 false,
	} {
		if got := isJSONContentType(ct); got != want {
			t.Errorf("isJSONContentType(%q) = %v, want %v", ct, got, want)
		}
	}
}
