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

package attach

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/permissions"
)

// The /perms routes as a wire contract (#364).
//
// This file exists because of a specific, recorded failure. #248 filed a
// wire-contract test as this repo's accountability for parity row 16 —
// and pinned `POST /resume`, which the client on the other end never
// calls. switchboard's approval client builds
// /sessions/<app>/<id>/perms/{stream,respond} and nothing else
// (pkg/approval/wire.go). Both repos wrote a coherent half, each half
// passed its own tests, and the halves did not meet for five weeks.
//
// So: same discipline, aimed at the endpoint the other side actually
// dials. Every value below is a literal rather than the constant that
// produces it, on purpose — a Go rename is free and a string change is
// a break in a repo this compiler cannot see, and a constant-to-constant
// assertion would stay green through exactly that break.
//
// Read against switchboard's client on 2026-09-16. What is NOT claimed
// here: that a live press has been run end to end. This pins the shape;
// the first real press is the confirmation.

// The two paths, spelled the way the client builds them.
func TestPermsWire_RoutePaths(t *testing.T) {
	t.Parallel()
	src := newFakePermsSource()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&sourceRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		src:            src,
	}); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/sessions/core-agent/s1/perms/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET perms/stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /sessions/core-agent/s1/perms/stream = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	resp.Body.Close()

	body, _ := json.Marshal(map[string]string{"id": "x", "decision": "allow-once"})
	resp, err = http.Post(base+"/sessions/core-agent/s1/perms/respond", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST perms/respond: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /sessions/core-agent/s1/perms/respond = %d, want 200", resp.StatusCode)
	}
}

// The SSE event name and the frame's JSON keys. The client reads the
// tool under "tool" — not "tool_name", which is what the Go field is
// called, and the kind of mismatch this file is for.
func TestPermsWire_FrameEnvelopeAndKeys(t *testing.T) {
	t.Parallel()
	src := newFakePermsSource()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&sourceRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		src:            src,
	}); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/sessions/core-agent/s1/perms/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()

	src.frames <- PromptFrame{
		ID:          "p1",
		Kind:        "control_plane_write",
		ToolName:    "patch_resource",
		Detail:      "patch_resource deployment/api",
		Verb:        "patch",
		Source:      "remediator",
		PersistTool: "patch_resource",
		PersistKey:  "deployment/api",
		Access:      "write",
		At:          time.Now(),
	}

	event, data := readSSEEvent(t, resp.Body)
	if event != "prompt" {
		t.Errorf("SSE event name = %q, want prompt (the only name the stream defines)", event)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		t.Fatalf("frame is not an object: %v: %s", err, data)
	}
	want := []string{"access", "at", "detail", "id", "kind", "persist_key", "persist_tool", "source", "tool", "verb"}
	var got []string
	for k := range raw {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("frame keys = %v, want %v", got, want)
	}
}

// The respond body the client sends, and the ack it reads back. The
// client refuses a 2xx whose body does not acknowledge, and reports
// "recorded nobody" when approver is absent — so both keys are part of
// the contract, not decoration.
func TestPermsWire_RespondBodyAndAckKeys(t *testing.T) {
	t.Parallel()
	src := newFakePermsSource()
	src.approver = "alice@example.com"
	reg := NewSessionRegistry()
	if _, err := reg.Register(&sourceRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		src:            src,
	}); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	// Hand-built body: the client marshals these two keys and nothing
	// else, and deliberately has no approver field, so the route must
	// not require one.
	resp, err := http.Post(base+"/sessions/core-agent/s1/perms/respond", "application/json",
		strings.NewReader(`{"id":"p1","decision":"allow-once"}`))
	if err != nil {
		t.Fatalf("POST respond: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var ack map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if ack["acknowledged"] != true {
		t.Errorf("ack[acknowledged] = %v, want true", ack["acknowledged"])
	}
	if ack["approver"] != "alice@example.com" {
		t.Errorf("ack[approver] = %v, want the identity the source recorded", ack["approver"])
	}
}

// The decision vocabulary the route parses. All six are understood at
// the door; which of them a given source will honour is the source's
// call, and a refusal is a 400 naming the reason rather than an
// "unknown decision" the client would read as its own typo.
func TestPermsWire_DecisionVocabulary(t *testing.T) {
	t.Parallel()
	for _, wire := range []string{
		"deny", "allow-once", "allow-session",
		"allow-session-verb", "allow-session-tool", "allow-always",
	} {
		if _, ok := DecisionFromWire(wire); !ok {
			t.Errorf("DecisionFromWire(%q) not recognised; the client offers this string", wire)
		}
	}
	if _, ok := DecisionFromWire("allow_once"); ok {
		t.Error("DecisionFromWire accepted an underscored spelling; the wire form is hyphenated")
	}
}

// The kind vocabulary. kind is not display text: switchboard derives the
// button set from it, and its rule for an unrecognised kind is to offer
// all six decisions (pkg/approval/approval.go). Renaming any of these in
// Go is free; renaming one on the wire puts four buttons in front of an
// operator that this daemon answers 400, so they are pinned as literals.
func TestPermsWire_KindVocabulary(t *testing.T) {
	t.Parallel()
	cases := map[permissions.PromptKind]string{
		permissions.PromptKindBash:              "bash",
		permissions.PromptKindFileWrite:         "file_write",
		permissions.PromptKindPathScope:         "path_scope",
		permissions.PromptKindControlPlaneWrite: "control_plane_write",
	}
	for kind, want := range cases {
		if got := kindToWire(kind); got != want {
			t.Errorf("kindToWire(%v) = %q, want %q", kind, got, want)
		}
	}
	// An unmapped kind falls back rather than emitting the enum's name —
	// "generic" is a kind the client knows, and a leaked Go identifier
	// would be one it does not.
	if got := kindToWire(permissions.PromptKind(99)); got != "generic" {
		t.Errorf("kindToWire(unmapped) = %q, want generic", got)
	}
}

// readSSEEvent returns the first (event name, data) pair on a stream.
func readSSEEvent(t *testing.T, body io.Reader) (string, string) {
	t.Helper()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	deadline := time.Now().Add(3 * time.Second)
	var event string
	for time.Now().Before(deadline) && scanner.Scan() {
		line := scanner.Text()
		if rest, ok := strings.CutPrefix(line, "event: "); ok {
			event = rest
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data: "); ok {
			return event, rest
		}
	}
	t.Fatal("no complete SSE event arrived within the deadline")
	return "", ""
}
