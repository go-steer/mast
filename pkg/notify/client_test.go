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

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The egress client (v0.5 W4.5). The contract under test is
// switchboard's ingress as shipped (cmd/switchboard/ingress.go), not a
// notification abstraction: these tests are written against the status
// codes and bodies that file produces.

// capture is one request the stub ingress saw.
type capture struct {
	method string
	path   string
	auth   string
	ctype  string
	idem   string
	body   message
}

// stub is a switchboard ingress that answers however the test says and
// records what it was asked.
type stub struct {
	t    *testing.T
	saw  []capture
	next func(w http.ResponseWriter, r *http.Request, body message)
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.t.Helper()
	raw, _ := io.ReadAll(r.Body)
	var body message
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			s.t.Errorf("stub ingress: undecodable body %q: %v", raw, err)
		}
	}
	s.saw = append(s.saw, capture{
		method: r.Method,
		path:   r.URL.Path,
		auth:   r.Header.Get("Authorization"),
		ctype:  r.Header.Get("Content-Type"),
		idem:   r.Header.Get("Idempotency-Key"),
		body:   body,
	})
	s.next(w, r, body)
}

func newStub(t *testing.T, next func(http.ResponseWriter, *http.Request, message)) (*Client, *stub) {
	t.Helper()
	s := &stub{t: t, next: next}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Token: "ingress-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, s
}

// writeRef answers the way a POST (or a rolled-over append) does.
func writeRef(w http.ResponseWriter, conv, id string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(Ref{Conversation: conv, ID: id})
}

// writeErr answers the way the ingress reports a failure.
func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// TestPostSendsWhatTheIngressDecodes: the ingress rejects unknown
// fields, so the body has to be exactly the four it knows and no more —
// a posted message must not carry an empty "id" or "append".
func TestPostSendsWhatTheIngressDecodes(t *testing.T) {
	c, s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ message) {
		writeRef(w, "C0123", "1723742401.001900")
	})

	ref, err := c.Post(context.Background(), "C0123", "3 findings escalated", "uat-transitions/2026-08-21T10:00:00Z")
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if ref.Conversation != "C0123" || ref.ID != "1723742401.001900" {
		t.Errorf("ref = %+v, want the ingress's own ref", ref)
	}
	if len(s.saw) != 1 {
		t.Fatalf("saw %d requests, want 1", len(s.saw))
	}
	got := s.saw[0]
	if got.method != http.MethodPost || got.path != defaultPath {
		t.Errorf("%s %s, want POST %s", got.method, got.path, defaultPath)
	}
	if got.auth != "Bearer ingress-token" {
		t.Errorf("Authorization = %q, want the configured bearer token", got.auth)
	}
	if got.ctype != "application/json" {
		t.Errorf("Content-Type = %q; the ingress 415s anything else", got.ctype)
	}
	if got.idem != "uat-transitions/2026-08-21T10:00:00Z" {
		t.Errorf("Idempotency-Key = %q, want the caller's replay key", got.idem)
	}
	if got.body.ID != "" || got.body.Append != "" {
		t.Errorf("body = %+v, want no id and no append on a post", got.body)
	}
}

// TestPostWithoutAReplayKeySendsNoHeader: the key is optional on the
// wire, and an empty one would be a key.
func TestPostWithoutAReplayKeySendsNoHeader(t *testing.T) {
	c, s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ message) {
		writeRef(w, "C0123", "1.1")
	})
	if _, err := c.Post(context.Background(), "C0123", "text", ""); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if s.saw[0].idem != "" {
		t.Errorf("Idempotency-Key = %q, want no header", s.saw[0].idem)
	}
}

// TestAppendKeepsTheRefOn204: the ordinary case — the line landed on
// the message we already had.
func TestAppendKeepsTheRefOn204(t *testing.T) {
	c, s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ message) {
		w.WriteHeader(http.StatusNoContent)
	})

	ref := Ref{Conversation: "C0123", ID: "1.1"}
	got, err := c.Append(context.Background(), ref, "and now a fourth", "k1")
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got != ref {
		t.Errorf("ref = %+v, want the unchanged %+v", got, ref)
	}
	if s.saw[0].method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", s.saw[0].method)
	}
	if s.saw[0].body.Append == "" || s.saw[0].body.Text != "" {
		t.Errorf("body = %+v, want append set and text empty", s.saw[0].body)
	}
}

// TestAppendRetargetsOnRollover is the failure the Append signature
// exists to prevent: switchboard answers 200 with a continuation's ref
// when the message filled up, and a caller that keeps the old ref 409s
// forever afterwards.
func TestAppendRetargetsOnRollover(t *testing.T) {
	c, _ := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ message) {
		writeRef(w, "C0123:1.1", "2.2")
	})

	got, err := c.Append(context.Background(), Ref{Conversation: "C0123", ID: "1.1"}, "one more line", "k1")
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	want := Ref{Conversation: "C0123:1.1", ID: "2.2"}
	if got != want {
		t.Errorf("ref = %+v, want the continuation %+v", got, want)
	}
}

// TestAppendAsksForTheFullTextOn409: the ingress's append memory is
// in-process and lossy by design, so this is a routine answer and has
// to be distinguishable from a fault.
func TestAppendAsksForTheFullTextOn409(t *testing.T) {
	c, _ := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ message) {
		writeErr(w, http.StatusConflict, "no remembered text for this message; send the full text instead")
	})

	_, err := c.Append(context.Background(), Ref{Conversation: "C0123", ID: "1.1"}, "line", "k1")
	if !errors.Is(err, ErrSendFullText) {
		t.Fatalf("Append error = %v, want ErrSendFullText", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusConflict {
		t.Fatalf("error = %v, want an *Error carrying 409", err)
	}
	if !strings.Contains(e.Msg, "send the full text") {
		t.Errorf("Msg = %q, want the ingress's own text", e.Msg)
	}
}

// TestAppendOn501IsAlsoSendTheFullText: a platform with no append at
// all answers 501, and the recovery is identical. The same status on a
// replace is not recoverable, which is the next test.
func TestAppendOn501IsAlsoSendTheFullText(t *testing.T) {
	c, _ := newStub(t, func(w http.ResponseWriter, _ *http.Request, body message) {
		if body.Append != "" {
			writeErr(w, http.StatusNotImplemented, "this platform does not support append; send the full text")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if _, err := c.Append(context.Background(), Ref{Conversation: "C", ID: "1"}, "line", ""); !errors.Is(err, ErrSendFullText) {
		t.Fatalf("Append error = %v, want ErrSendFullText", err)
	}
	if err := c.Replace(context.Background(), Ref{Conversation: "C", ID: "1"}, "whole thing", ""); err != nil {
		t.Fatalf("Replace after a 501 append: %v", err)
	}
}

// TestReplaceOn501IsUnsupported: the platform cannot edit at all, so
// there is nothing to fall back to except posting anew.
func TestReplaceOn501IsUnsupported(t *testing.T) {
	c, _ := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ message) {
		writeErr(w, http.StatusNotImplemented, "this platform cannot edit messages")
	})

	err := c.Replace(context.Background(), Ref{Conversation: "C", ID: "1"}, "text", "")
	if !errors.Is(err, ErrEditUnsupported) {
		t.Fatalf("Replace error = %v, want ErrEditUnsupported", err)
	}
	if errors.Is(err, ErrSendFullText) {
		t.Error("a replace that cannot be applied must not ask for the full text; it just got it")
	}
}

// TestStatusSentinels covers the rest of the map in one table.
func TestStatusSentinels(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, ErrDenied},
		{"not in the allowlist", http.StatusForbidden, ErrDenied},
		{"channel is gone", http.StatusNotFound, ErrNoSuchMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ message) {
				writeErr(w, tc.status, "refused")
			})
			_, err := c.Post(context.Background(), "C", "text", "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Post error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestServerErrorHasNoSentinel: a 502 from the platform (or from a
// proxy in front of switchboard) is a transient nobody should branch
// on — it is retried by the next cycle, not by a special case here.
func TestServerErrorHasNoSentinel(t *testing.T) {
	c, _ := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ message) {
		writeErr(w, http.StatusBadGateway, "the chat platform rejected the message")
	})

	_, err := c.Post(context.Background(), "C", "text", "")
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("Post error = %v, want an *Error", err)
	}
	if e.Status != http.StatusBadGateway || e.Sentinel != nil {
		t.Errorf("error = %+v, want a bare 502", e)
	}
	for _, sentinel := range []error{ErrSendFullText, ErrEditUnsupported, ErrDenied, ErrNoSuchMessage} {
		if errors.Is(err, sentinel) {
			t.Errorf("a 502 matched %v", sentinel)
		}
	}
}

// TestNonJSONErrorBodyIsSurfaced: an HTML apology from a proxy is the
// single most useful thing to see when switchboard is not the one
// answering.
func TestNonJSONErrorBodyIsSurfaced(t *testing.T) {
	c, _ := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ message) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>upstream connect error</html>")
	})

	_, err := c.Post(context.Background(), "C", "text", "")
	if err == nil || !strings.Contains(err.Error(), "upstream connect error") {
		t.Fatalf("Post error = %v, want the proxy's own body", err)
	}
}

// TestPostRefusesAnAnswerWithNoRef: a 200 with nothing in it leaves the
// caller with a message it cannot edit, which is worse than a failure.
func TestPostRefusesAnAnswerWithNoRef(t *testing.T) {
	c, _ := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ message) {
		writeRef(w, "C0123", "")
	})

	if _, err := c.Post(context.Background(), "C0123", "text", ""); err == nil {
		t.Fatal("Post accepted a ref with no message id")
	}
}

// TestTransportFailureIsNotAStatus: a connection refused has no status,
// and reporting it as one would read as a 0 from the ingress.
func TestTransportFailureIsNotAStatus(t *testing.T) {
	c, err := New(Config{BaseURL: "http://127.0.0.1:1/v1/messages", Token: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Post(context.Background(), "C", "text", "")
	if err == nil {
		t.Fatal("Post against a closed port succeeded")
	}
	var e *Error
	if errors.As(err, &e) {
		t.Errorf("transport failure reported as status %d", e.Status)
	}
}

// TestNoRequestWithoutTheBasics: the arguments the ingress would 400 on
// are refused here, so a monitoring cycle's log names the bundle
// problem rather than a status code.
func TestNoRequestWithoutTheBasics(t *testing.T) {
	c, s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ message) {
		w.WriteHeader(http.StatusNoContent)
	})
	ctx := context.Background()

	if _, err := c.Post(ctx, "  ", "text", ""); err == nil {
		t.Error("Post accepted an empty conversation")
	}
	if _, err := c.Post(ctx, "C", " \n", ""); err == nil {
		t.Error("Post accepted empty text")
	}
	if _, err := c.Append(ctx, Ref{}, "line", ""); err == nil {
		t.Error("Append accepted a zero ref")
	}
	if err := c.Replace(ctx, Ref{Conversation: "C"}, "text", ""); err == nil {
		t.Error("Replace accepted a ref with no message id")
	}
	if len(s.saw) != 0 {
		t.Errorf("the ingress saw %d requests; want none", len(s.saw))
	}
}

// TestBaseURLForms: an operator should be able to paste either the
// origin or the endpoint out of switchboard's own README.
func TestBaseURLForms(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://switchboard:8080", "http://switchboard:8080/v1/messages"},
		{"http://switchboard:8080/", "http://switchboard:8080/v1/messages"},
		{"http://switchboard:8080/v1/messages", "http://switchboard:8080/v1/messages"},
		{"https://sb.example.com/chat/v1/messages", "https://sb.example.com/chat/v1/messages"},
	}
	for _, tc := range cases {
		c, err := New(Config{BaseURL: tc.in, Token: "t"})
		if err != nil {
			t.Fatalf("New(%q): %v", tc.in, err)
		}
		if c.Endpoint() != tc.want {
			t.Errorf("New(%q).Endpoint() = %q, want %q", tc.in, c.Endpoint(), tc.want)
		}
	}
}

// TestNewRejects: a client that cannot possibly work is refused at
// construction, where the daemon can still say so at startup.
func TestNewRejects(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no url", Config{Token: "t"}},
		{"no token", Config{BaseURL: "http://switchboard:8080"}},
		{"not a url", Config{BaseURL: "://switchboard", Token: "t"}},
		{"no scheme", Config{BaseURL: "switchboard:8080", Token: "t"}},
		{"no host", Config{BaseURL: "http:///v1/messages", Token: "t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatalf("New(%+v) succeeded", tc.cfg)
			}
		})
	}
}
