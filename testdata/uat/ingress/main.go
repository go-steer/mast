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

// Command ingress is a controllable stand-in for switchboard's outbound
// message ingress, used by the v0.5 UAT harness (scripts/uat-v0.5.sh)
// to observe what an unattended monitoring cycle actually says — and,
// more often, that it says nothing.
//
// It implements the three answers pkg/notify is written against:
//
//	POST  /v1/messages  {"conversation","text"}        → 200 {"conversation","id"}
//	PATCH /v1/messages  {"conversation","id","text"}   → 204
//	PATCH /v1/messages  {"conversation","id","append"} → 204, or 200 {…}
//
// It is a stub rather than the real switchboard for the reason every
// other fixture here is: the harness must run offline, with no
// credentials and no chat platform. What it is NOT is a mock that
// agrees with mast — it decodes strictly (unknown fields are a 400) and
// it requires the bearer, so a mast that sent the wrong body shape or
// the wrong token would fail these legs rather than pass them.
//
// Every request is appended to "requests.jsonl" in -dir, one JSON
// object per line, which is what the harness asserts on: the message
// text, the Idempotency-Key, and — the assertion the whole workstream
// is about — how many lines are there at all.
//
// Behaviour is steered by files in -dir, so a leg can change the
// ingress's answer between two ticks of a live cadence:
//
//   - "status" — an HTTP status (e.g. 503) returned for every request
//     while the file exists. The harness's "a failed post does not
//     resurrect the diff" leg turns on this.
//   - "append.409" — appends answer 409 "no remembered text for this
//     message", the answer a restarted switchboard gives. mast must
//     recover by re-sending the whole message as an edit.
//   - "append.roll" — the NEXT append answers 200 with a continuation
//     ref instead of 204, which is what switchboard does when a message
//     is full. The file is consumed, so the roll happens once.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// message is the ingress's request body. Decoded with
// DisallowUnknownFields, like the real one: the vocabulary is exactly
// these four keys, and a mast that invented a fifth should fail here
// rather than in production.
type message struct {
	Conversation string `json:"conversation"`
	ID           string `json:"id,omitempty"`
	Text         string `json:"text,omitempty"`
	Append       string `json:"append,omitempty"`
}

// record is one line of requests.jsonl.
type record struct {
	Method       string `json:"method"`
	Auth         string `json:"auth"`
	Idem         string `json:"idem"`
	Conversation string `json:"conversation"`
	ID           string `json:"id,omitempty"`
	Text         string `json:"text,omitempty"`
	Append       string `json:"append,omitempty"`
	Status       int    `json:"status"`
}

type ingress struct {
	dir   string
	token string

	mu   sync.Mutex
	n    int
	body map[string]string // what each message currently says
}

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "bind address")
	dir := flag.String("dir", "", "coordination directory (required)")
	token := flag.String("token", "", "bearer token this ingress requires (required)")
	flag.Parse()
	if *dir == "" || *token == "" {
		log.Fatal("ingress: -dir and -token are required")
	}
	if err := os.MkdirAll(*dir, 0o750); err != nil {
		log.Fatalf("ingress: %v", err)
	}
	g := &ingress{dir: *dir, token: *token, body: map[string]string{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", g.handle)
	// A readiness probe with no side effects, so the harness can wait for
	// the listener rather than sleeping at it.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("ingress: %v", err)
	}
	// Printed so a leg reading the log knows what it bound, even when the
	// harness picked the port itself.
	fmt.Println(ln.Addr().String())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.Serve(ln); err != nil {
		log.Fatalf("ingress: %v", err)
	}
}

func (g *ingress) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+g.token {
		g.write(w, r, message{}, http.StatusUnauthorized, nil, "bad token")
		return
	}
	var m message
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		g.write(w, r, m, http.StatusBadRequest, nil, err.Error())
		return
	}
	if forced := g.forcedStatus(); forced != 0 {
		g.write(w, r, m, forced, nil, "the harness told me to refuse")
		return
	}
	switch {
	case r.Method == http.MethodPost:
		g.mu.Lock()
		g.n++
		id := fmt.Sprintf("m%d", g.n)
		g.body[id] = m.Text
		g.mu.Unlock()
		g.write(w, r, m, http.StatusOK, &message{Conversation: m.Conversation, ID: id}, "")
	case r.Method == http.MethodPatch && m.Append != "":
		g.appendTo(w, r, m)
	case r.Method == http.MethodPatch:
		g.mu.Lock()
		_, known := g.body[m.ID]
		if known {
			g.body[m.ID] = m.Text
		}
		g.mu.Unlock()
		if !known {
			g.write(w, r, m, http.StatusNotFound, nil, "no such message")
			return
		}
		g.write(w, r, m, http.StatusNoContent, nil, "")
	default:
		g.write(w, r, m, http.StatusMethodNotAllowed, nil, "")
	}
}

func (g *ingress) appendTo(w http.ResponseWriter, r *http.Request, m message) {
	if g.exists("append.409") {
		g.write(w, r, m, http.StatusConflict, nil, "no remembered text for this message; send the full text instead")
		return
	}
	if g.consume("append.roll") {
		g.mu.Lock()
		g.n++
		id := fmt.Sprintf("m%d", g.n)
		g.body[id] = m.Append
		g.mu.Unlock()
		g.write(w, r, m, http.StatusOK, &message{Conversation: m.Conversation, ID: id}, "")
		return
	}
	g.mu.Lock()
	prev, known := g.body[m.ID]
	if known {
		g.body[m.ID] = prev + "\n\n" + m.Append
	}
	g.mu.Unlock()
	if !known {
		g.write(w, r, m, http.StatusConflict, nil, "no remembered text for this message; send the full text instead")
		return
	}
	g.write(w, r, m, http.StatusNoContent, nil, "")
}

// write answers and records, in that order of importance: the harness
// asserts on the ledger, so a response that was sent and not recorded
// would be a leg that passes for the wrong reason.
func (g *ingress) write(w http.ResponseWriter, r *http.Request, m message, status int, ref *message, errMsg string) {
	g.record(record{
		Method:       r.Method,
		Auth:         r.Header.Get("Authorization"),
		Idem:         r.Header.Get("Idempotency-Key"),
		Conversation: m.Conversation,
		ID:           m.ID,
		Text:         m.Text,
		Append:       m.Append,
		Status:       status,
	})
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if ref != nil {
		_ = json.NewEncoder(w).Encode(ref)
		return
	}
	if errMsg != "" {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
	}
}

func (g *ingress) record(rec record) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(g.dir, "requests.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(raw, '\n'))
}

func (g *ingress) exists(name string) bool {
	_, err := os.Stat(filepath.Join(g.dir, name))
	return err == nil
}

// consume reports whether the file was there, and removes it, so a
// one-shot behaviour happens once.
func (g *ingress) consume(name string) bool {
	if !g.exists(name) {
		return false
	}
	_ = os.Remove(filepath.Join(g.dir, name))
	return true
}

func (g *ingress) forcedStatus() int {
	raw, err := os.ReadFile(filepath.Join(g.dir, "status")) // #nosec G304 -- harness-controlled fixture dir
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || n < 400 {
		return 0
	}
	return n
}
