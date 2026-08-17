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

// Originally derived from go-steer/core-agent@b1101f99bec3b74ad41f2a938822c9bb9bbca072:pkg/mcp/errbody.go
// Narrowed: the MCP SDK has since closed most of the gap upstream's
// version was written against, so mast's transport only covers the two
// cases the SDK still drops. See the note on jsonRPCErrorTransport.

package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// jsonRPCErrorBodyMax caps how much of a non-2xx response body the
// transport buffers while looking for an error payload. Real payloads
// from Google's MCP surface are well under 4 KiB; the cap keeps a
// misbehaving server from wedging a turn on a large response.
const jsonRPCErrorBodyMax = 32 * 1024

// jsonRPCErrorTransport wraps an http.RoundTripper so that an MCP
// server's own error text reaches the operator instead of a bare HTTP
// status line.
//
// # What the SDK already does, and the two holes it leaves
//
// go-sdk v1.7.0 decodes the body of a non-2xx response and surfaces a
// standard JSON-RPC error object, so most of what upstream's version of
// this file existed for is now the SDK's job. Two cases still lose the
// text, both of which mast hits on its primary MCP surface:
//
//  1. **The MCP tool-result error shape.** Google's
//     container.googleapis.com/mcp answers an IAM denial with HTTP 403
//     and a body of {"result":{"isError":true,"content":[{"type":
//     "text","text":"Permission '...' denied on resource '...'"}]}}.
//     That decodes as a JSON-RPC *response*, not an error, so the SDK
//     falls through to the status line and the operator is told
//     "Forbidden" — the permission name they need is dropped.
//
//  2. **Transient statuses.** For 429/500/502/503/504 the SDK returns
//     early with the status text and never reads the body, so a quota
//     denial arrives as "Too Many Requests" with no quota metric named.
//
// Where the SDK does surface the text (a standard {"error":{"message"}}
// object on a non-transient status), this transport stays out of the
// way — see errorTextFrom. Duplicating the SDK there would cost the
// typed *jsonrpc.Error the SDK wraps into the chain, and would have to
// be deleted twice when the SDK closes the remaining holes.
//
// When it does intervene it returns
//
//	<HTTP status>: <extracted text>
//
// as an error from RoundTrip. http.Client surfaces that as the error
// from Do, which the SDK wraps as jsonrpc2.ErrRejected — the same
// classification its own transient path uses, so the logical MCP
// session is not torn down and the text propagates to the model and the
// operator.
//
// # What it deliberately leaves alone
//
//   - Anything but POST. The SSE reconnect (GET) and the session
//     teardown (DELETE) are not tool calls; failing them from the
//     transport would push the reconnect loop through its retry budget
//     for a status the SDK would have handled directly.
//   - 404, which the SDK translates to ErrSessionMissing so it can skip
//     a redundant DELETE. That sentinel is worth more than the body.
//   - Non-JSON bodies, oversized bodies, and bodies with no extractable
//     text: all replayed intact so the SDK sees exactly the response it
//     would have seen.
type jsonRPCErrorTransport struct {
	base http.RoundTripper
}

func (t *jsonRPCErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if !worthInspecting(req, resp) {
		return resp, nil
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, jsonRPCErrorBodyMax+1))
	_ = resp.Body.Close()
	if readErr != nil {
		// The body is spent and cannot be replayed, so there is no
		// response left to hand back. Surface the status plus the read
		// failure rather than a truncated body.
		return nil, fmt.Errorf("%s: reading the error body: %w", statusLine(resp), readErr)
	}
	if len(body) > jsonRPCErrorBodyMax {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	text := errorTextFrom(body, resp.StatusCode)
	if text == "" {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	return nil, fmt.Errorf("%s: %s", statusLine(resp), text)
}

// worthInspecting reports whether this exchange is one where the SDK
// might drop the server's error text. See the type comment for why each
// exclusion is there.
func worthInspecting(req *http.Request, resp *http.Response) bool {
	if req == nil || req.Method != http.MethodPost {
		return false
	}
	if resp.StatusCode < 400 || resp.StatusCode == http.StatusNotFound {
		return false
	}
	return isJSONContentType(resp.Header.Get("Content-Type"))
}

// statusLine returns the response's status line, reconstructing it when
// the round tripper under test left the field empty. net/http always
// populates Status on a real response.
func statusLine(resp *http.Response) string {
	if resp.Status != "" {
		return resp.Status
	}
	return fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
}

func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	// Content-Type may carry parameters (charset, boundary, ...).
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	return ct == "application/json" || strings.HasSuffix(ct, "+json")
}

// errorTextFrom pulls the human-readable message out of a JSON-RPC
// response body, or returns "" to mean "leave this response alone".
//
// It returns "" for a standard {"error":{"message":..}} object on a
// non-transient status, because the SDK surfaces that one itself. On a
// transient status the SDK never reads the body at all, so the same
// object is worth extracting.
func errorTextFrom(body []byte, status int) string {
	var env struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
		Result *struct {
			IsError bool `json:"isError,omitempty"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content,omitempty"`
		} `json:"result,omitempty"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	if env.Error != nil && sdkSkipsTheBody(status) {
		if msg := strings.TrimSpace(env.Error.Message); msg != "" {
			return msg
		}
	}
	if env.Result != nil && env.Result.IsError {
		for _, c := range env.Result.Content {
			if c.Type != "text" && c.Type != "" {
				continue
			}
			if msg := strings.TrimSpace(c.Text); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// sdkSkipsTheBody mirrors the SDK's isTransientHTTPStatus, the set of
// statuses whose body it returns early without reading.
//
// Copying a private list is a drift risk, and both directions of drift
// are benign. If the SDK adds a status, mast stops extracting a
// standard error object it now drops — no worse than the behavior
// before this file existed. If the SDK removes one, mast extracts text
// the SDK would also have surfaced — a duplicate message, not a lost
// one. TestSDKStillDropsTheseBodies pins the assumption against the
// real client, so a version bump that moves the line fails a test
// rather than quietly widening a hole.
func sdkSkipsTheBody(status int) bool {
	switch status {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}
