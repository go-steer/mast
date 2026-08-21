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

// Package notify is the egress client for switchboard's outbound
// message ingress (v0.5 W4.5) — the half of unattended monitoring that
// speaks. A monitoring cycle that found something needs to say so in a
// chat channel where there is no inbound message to reply to, and
// switchboard's `--ingress-addr` surface is exactly that door:
//
//	POST  /v1/messages  {"conversation","text"}        → 200 {"conversation","id"}
//	PATCH /v1/messages  {"conversation","id","text"}   → 204
//	PATCH /v1/messages  {"conversation","id","append"} → 204, or 200 {"conversation","id"}
//
// # What this package is not
//
// It is not a chat library and it holds no opinion about what a message
// should say. It has no notion of a finding, a transition, or a
// severity; the text is the caller's, and every method here is one
// HTTP request. Rendering belongs to whoever is writing the sentence
// (for a monitoring cycle that is the model), and the platform belongs
// to switchboard — an instance bridges the one platform it was started
// with, which is why there is no platform field on the wire.
//
// It is also not durable. There is no queue, no retry loop and no
// spool: a failed post is reported to the caller, which for a
// monitoring cycle is the right answer, because the next cycle is a
// fresher sample and re-sending a stale assessment is worse than
// dropping it. Stdlib only, so the slim-embed dependency gate
// (scripts/check-slim-deps.sh) is unaffected.
//
// # The two answers that are not failures
//
// Both come from append, and a client that treats them as errors is
// broken in a way that only shows up in production:
//
//   - 409 "no remembered text for this message" — the ingress remembers
//     the bodies it posted in memory, bounded and lost on restart, so an
//     append can outlive the memory of what it is appending to. A
//     restart, another replica, or a message posted from elsewhere all
//     land here. The answer is ErrSendFullText, and the caller resends
//     the whole message with Replace. 501 on an append means the same
//     thing permanently (the platform cannot even tell when a message is
//     full), and maps to the same sentinel for the same reason.
//
//   - 200 with a ref, instead of 204 — the append would have overflowed
//     the platform's single-message limit, so switchboard posted a
//     continuation in the same thread and handed back its ref. Append
//     returns that ref, and a caller that keeps appending to the old one
//     will 409 forever afterwards.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultPath is switchboard's ingress route. A BaseURL with no path of
// its own gets it appended, so both "http://switchboard:8080" and the
// full endpoint are accepted — an operator copying the URL out of
// switchboard's own README should not have to know which form mast
// wanted.
const defaultPath = "/v1/messages"

// defaultTimeout bounds one request. Generous next to switchboard's own
// 15s platform timeout, because that timeout is inside this one: a
// chat platform that is merely slow should be waited for, and one that
// is gone should not hold a monitoring cycle open.
const defaultTimeout = 30 * time.Second

// maxErrorBody caps how much of a failed response is read for its
// message. Errors are one JSON object with one string in it; anything
// past this is a proxy's HTML apology.
const maxErrorBody = 64 << 10

// Sentinels a caller acts on. Everything else is an *Error with a
// status, which a caller can inspect but is not expected to branch on.
var (
	// ErrSendFullText says the append could not be applied and the
	// whole message must be sent instead (409, or 501 on an append).
	// It is a routine outcome, not a fault: see the package doc.
	ErrSendFullText = errors.New("notify: send the full text")

	// ErrEditUnsupported says the platform cannot edit messages at all
	// (501 on a replace). A caller that wanted to refine a message in
	// place has to post a new one.
	ErrEditUnsupported = errors.New("notify: this platform cannot edit messages")

	// ErrNoSuchMessage says the conversation or message is gone (404) —
	// a channel that was archived, a message somebody deleted.
	ErrNoSuchMessage = errors.New("notify: no such conversation or message")

	// ErrDenied covers the two configuration failures worth telling
	// apart from a transport blip: a bad bearer token (401) and a
	// conversation outside the ingress allowlist (403). Retrying either
	// is pointless until a human changes something.
	ErrDenied = errors.New("notify: refused")
)

// Error is a non-2xx answer from the ingress. Msg is the ingress's own
// error text when it sent one, which is worth surfacing: it says
// whether a 403 was the allowlist or the chat platform's own refusal.
type Error struct {
	Status int
	Msg    string
	// Sentinel is the errors.Is target for this status, or nil.
	Sentinel error
}

func (e *Error) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("notify: ingress returned %d", e.Status)
	}
	return fmt.Sprintf("notify: ingress returned %d: %s", e.Status, e.Msg)
}

func (e *Error) Unwrap() error { return e.Sentinel }

// Ref addresses one posted message. It is exactly what the ingress
// hands back, and exactly what a later edit or append must present.
type Ref struct {
	Conversation string `json:"conversation"`
	ID           string `json:"id"`
}

// Zero reports whether the ref names nothing — no message posted yet,
// or a timeline that was closed.
func (r Ref) Zero() bool { return r.ID == "" }

// Config constructs a Client.
type Config struct {
	// BaseURL is switchboard's ingress. Either the bare origin
	// ("http://switchboard.agent-triage.svc.cluster.local:8080") or the
	// full endpoint; the route is appended when the URL carries no path.
	BaseURL string

	// Token is the bearer token the ingress requires. It is
	// deliberately not the daemon's own token — different direction,
	// different trust — and cmd/mast refuses to start if the two match.
	Token string

	// HTTP substitutes the transport (tests, proxies, custom TLS). When
	// nil a client with Timeout is built.
	HTTP *http.Client

	// Timeout bounds one request when HTTP is nil. Zero means
	// defaultTimeout.
	Timeout time.Duration
}

// Client posts to one switchboard ingress. Safe for concurrent use;
// it holds no per-message state, because the thing worth remembering
// between calls — which message a timeline is currently appending to —
// belongs to whoever is keeping the timeline.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

// New validates the config and builds a client.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("notify: base URL is required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		// Refused rather than defaulted to unauthenticated: the ingress
		// always requires a token, so a client without one can only
		// produce 401s at 3am on the cycle that had something to say.
		return nil, errors.New("notify: token is required")
	}
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("notify: base URL %q: %w", cfg.BaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("notify: base URL %q must be http or https", cfg.BaseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("notify: base URL %q names no host", cfg.BaseURL)
	}
	if p := strings.TrimSuffix(u.Path, "/"); p == "" {
		u.Path = defaultPath
	}
	hc := cfg.HTTP
	if hc == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		hc = &http.Client{Timeout: timeout}
	}
	return &Client{endpoint: u.String(), token: strings.TrimSpace(cfg.Token), http: hc}, nil
}

// Endpoint is the URL this client posts to, for the log line that says
// where a monitor is talking.
func (c *Client) Endpoint() string { return c.endpoint }

// message is the request body of both verbs. The ingress decodes
// strictly (unknown fields are a 400), so this struct is the whole
// vocabulary and omitempty is load-bearing.
type message struct {
	Conversation string `json:"conversation"`
	ID           string `json:"id,omitempty"`
	Text         string `json:"text,omitempty"`
	Append       string `json:"append,omitempty"`
}

// Post sends a new message and returns its ref.
//
// idem is the caller's replay key (the Idempotency-Key header), empty
// for none. A monitoring cycle should always set one: a post that
// timed out client-side may well have landed, and the retry that
// follows would otherwise double-post the same assessment.
func (c *Client) Post(ctx context.Context, conversation, text, idem string) (Ref, error) {
	if strings.TrimSpace(conversation) == "" {
		return Ref{}, errors.New("notify: conversation is required")
	}
	if strings.TrimSpace(text) == "" {
		return Ref{}, errors.New("notify: text is required")
	}
	status, ref, err := c.do(ctx, http.MethodPost, message{Conversation: conversation, Text: text}, idem)
	if err != nil {
		return Ref{}, err
	}
	if status != http.StatusOK || ref.ID == "" {
		return Ref{}, &Error{Status: status, Msg: "post answered no message ref"}
	}
	return ref, nil
}

// Replace rewrites a message's whole body. It is both the refinement
// verb — an assessment that took minutes edits the message it posted
// rather than littering the channel — and the fallback an append's
// ErrSendFullText asks for.
func (c *Client) Replace(ctx context.Context, ref Ref, text, idem string) error {
	if ref.Zero() {
		return errors.New("notify: replace needs a message ref")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("notify: text is required")
	}
	_, _, err := c.do(ctx, http.MethodPatch,
		message{Conversation: ref.Conversation, ID: ref.ID, Text: text}, idem)
	return err
}

// Append adds a line to a message and returns the ref to address from
// now on — the same one, or the continuation switchboard rolled over
// into. A caller that ignores the returned ref will 409 on every
// subsequent append, which is the failure this signature exists to
// make hard.
//
// ErrSendFullText means the append could not be applied and the caller
// should Replace with the full text it holds.
func (c *Client) Append(ctx context.Context, ref Ref, line, idem string) (Ref, error) {
	if ref.Zero() {
		return Ref{}, errors.New("notify: append needs a message ref")
	}
	if strings.TrimSpace(line) == "" {
		return Ref{}, errors.New("notify: append text is required")
	}
	status, got, err := c.do(ctx, http.MethodPatch,
		message{Conversation: ref.Conversation, ID: ref.ID, Append: line}, idem)
	if err != nil {
		return Ref{}, err
	}
	if status == http.StatusOK && got.ID != "" {
		// Rolled over into a continuation message. The timeline moved.
		return got, nil
	}
	return ref, nil
}

// do performs one request and decodes whatever the ingress answered.
// It returns the status and the ref (zero unless the answer carried
// one), or an error for every non-2xx.
func (c *Client) do(ctx context.Context, method string, body message, idem string) (int, Ref, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, Ref{}, fmt.Errorf("notify: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint, bytes.NewReader(raw))
	if err != nil {
		return 0, Ref{}, fmt.Errorf("notify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if idem = strings.TrimSpace(idem); idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Deliberately not wrapped in *Error: there is no status, and a
		// caller that switches on one would read a transport failure as
		// a 0 from the ingress.
		return 0, Ref{}, fmt.Errorf("notify: %s %s: %w", method, c.endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var ref Ref
		if resp.StatusCode == http.StatusOK {
			// 204 carries nothing; a 200 carries the ref. A body that
			// will not decode is not worth failing an otherwise
			// successful send over — the caller learns the ref did not
			// move, which is the conservative reading.
			_ = json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&ref)
		}
		return resp.StatusCode, ref, nil
	}
	return resp.StatusCode, Ref{}, c.statusError(resp, body.Append != "")
}

// statusError maps a non-2xx onto the sentinel a caller can act on.
//
// The append/replace split on 501 is the one place the same status
// means two different things: on an append it is "this platform cannot
// append, send the whole message" (recoverable, and the recovery is a
// replace), on a replace it is "this platform cannot edit at all"
// (not recoverable by editing anything).
func (c *Client) statusError(resp *http.Response, isAppend bool) error {
	e := &Error{Status: resp.StatusCode, Msg: ingressMessage(resp)}
	switch resp.StatusCode {
	case http.StatusConflict:
		e.Sentinel = ErrSendFullText
	case http.StatusNotImplemented:
		if isAppend {
			e.Sentinel = ErrSendFullText
		} else {
			e.Sentinel = ErrEditUnsupported
		}
	case http.StatusNotFound:
		e.Sentinel = ErrNoSuchMessage
	case http.StatusUnauthorized, http.StatusForbidden:
		e.Sentinel = ErrDenied
	}
	return e
}

// ingressMessage pulls the ingress's own error text out of the body.
// The contract is {"error": "..."}; anything else is reported as the
// raw body, trimmed, because an operator debugging a 502 from a proxy
// in front of switchboard needs to see that it was not switchboard.
func ingressMessage(resp *http.Response) string {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error != "" {
		return body.Error
	}
	return strings.TrimSpace(string(raw))
}
