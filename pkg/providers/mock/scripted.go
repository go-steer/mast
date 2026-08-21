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

// Originally derived from go-steer/core-agent@b8dd225e9ae7fdeb3ff23772cc5be25eed34b818

// Package mock ships a credential-free scripted LLM provider for
// offline testing of agent flows.
//
// The scripted provider replays a JSONL transcript turn-by-turn. Pair
// it with a recording captured against a real provider to exercise
// the agent loop without burning API quota. Construct one with
// NewScripted; there is no registry and no init-time registration —
// callers wire the returned model.LLM explicitly.
//
// One scripted LLM serves a whole roster: internal/compose collapses
// every per-specialist model override back to the root model when the
// root is an offline fake, which is right for the stateless fakes and
// would be wrong here, because a replay is a cursor. Concurrent
// consumers therefore get independent replays — see scriptedLLM.
//
// Tool execution at replay time uses the live environment, so the
// scripted provider faithfully replays the LLM side but not the wider
// tool surface — fine for testing prompt construction and loop shape,
// not for bit-exact session reproduction.
package mock

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"os"
	"slices"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
)

// NewScripted returns a scripted LLM that plays back the JSONL
// transcript at path. strict toggles request-shape validation per
// turn (see scriptedLLM for details).
//
// The transcript is read and parsed here, once. A malformed file
// fails at construction rather than at whichever consumer happens to
// call first, and an edit to the file mid-run cannot change what a
// later consumer replays.
func NewScripted(path string, strict bool) (adkmodel.LLM, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("scripted: open: %w", err)
	}
	defer f.Close()
	src, err := newScript(f, path, strict)
	if err != nil {
		return nil, fmt.Errorf("scripted: %w", err)
	}
	return &scriptedLLM{src: src}, nil
}

// scriptedLLM replays a recorded transcript. Each call to
// GenerateContent advances a cursor and yields the responses that
// were captured for that turn. The script is exhausted when more
// calls arrive than there are recorded turns, and the next yield
// surfaces a clear error rather than a silent empty stream.
//
// In strict mode, each incoming request's Contents must JSON-equal
// the recorded request's Contents — that catches regressions in how
// the agent assembles its prompt without depending on tool decls or
// other Config drift.
//
// # One replay per branch
//
// The cursor is per ADK branch, not per LLM. Fan-out dispatch runs
// its analysts concurrently against the single model instance the
// roster shares, and a single cursor walked by N branches at once is
// a replay whose assignment of recorded turns to branches depends on
// goroutine scheduling. Mutex-guarding one cursor makes that worse
// rather than better: -race stays silent while the run is
// nondeterministic, and the first symptom is a "script exhausted"
// from whichever branch lost the race — or, quieter, a branch that
// succeeds having replayed another branch's turn.
//
// ADK's branch tag is the right key because it is ADK's own
// concurrency-isolation identity: parallelagent runs each sub-agent
// under "<fan>.branch_<name>", workflow.ParallelWorker gives each
// item its own, and every sequential shape inherits its parent's
// unchanged. So a coordinator, a planner and a single-agent replay
// all keep walking one cursor in one order, exactly as before, and
// only the shapes that actually run at the same time get separate
// ones. Each replay decodes the transcript for itself: recorded
// responses are pointers handed straight into the agent loop, so
// sharing one decoded slice between branches would hand the same
// *LLMResponse to two goroutines.
//
// Every replay starts at turn 0 of the same recording. What this does
// not do is let one transcript describe N *different* branches —
// that needs a recorded turn to name the agent it belongs to, which
// is a change to the RecordedTurn wire format mast shares with
// core-agent's pkg/recording and therefore belongs upstream first.
//
// Concurrent *invocations* in one process — a daemon serving two
// incidents at once — still share the unbranched replay. Keying on
// the invocation ID as well would separate them, and would break
// multi-turn replay: a second user turn is a new invocation that must
// go on consuming the same recording, not restart it at turn 0.
type scriptedLLM struct {
	src *script

	mu      sync.Mutex
	replays map[string]*replay
}

func (l *scriptedLLM) Name() string { return "scripted" }

func (l *scriptedLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		r, err := l.replayFor(branchOf(ctx))
		if err != nil {
			yield(nil, err)
			return
		}
		r.next(req, yield)
	}
}

// replayFor returns the replay belonging to a branch, minting one on
// first sight.
func (l *scriptedLLM) replayFor(branch string) (*replay, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r, ok := l.replays[branch]; ok {
		return r, nil
	}
	r, err := l.src.replay(branch)
	if err != nil {
		return nil, err
	}
	if l.replays == nil {
		l.replays = map[string]*replay{}
	}
	l.replays[branch] = r
	return r, nil
}

// branchOf reads the caller's ADK branch tag out of the context the
// model was called with. ADK hands llmagent's model call an
// agent.ReadonlyContext, so this is the identity of the lane the call
// is on without any plumbing through the model interface.
//
// A context that is not one — a direct unit-test call, or a wrapper
// somebody put in between — reads as the unbranched replay, which is
// the sequential behaviour. That is the safe direction to degrade in
// (a shared cursor is what the caller had before), and it is why the
// per-branch guarantee is pinned by a test that runs the real
// fan-out shape rather than by a hand-built context.
func branchOf(ctx context.Context) string {
	if rc, ok := ctx.(adkagent.ReadonlyContext); ok {
		return rc.Branch()
	}
	return ""
}

// replay is one consumer's cursor over its own decode of the
// transcript.
type replay struct {
	branch string
	strict bool

	mu     sync.Mutex
	turns  []RecordedTurn
	cursor int
}

// next plays this replay's turn into yield. It is the whole of the
// scripted provider's per-call behaviour; scriptedLLM only decides
// which replay it belongs to.
func (r *replay) next(req *adkmodel.LLMRequest, yield func(*adkmodel.LLMResponse, error) bool) {
	r.mu.Lock()
	if r.cursor >= len(r.turns) {
		n := r.cursor
		r.mu.Unlock()
		yield(nil, fmt.Errorf("scripted: script exhausted at turn %d%s (no more recorded responses)", n, r.where()))
		return
	}
	turn := r.turns[r.cursor]
	r.cursor++
	idx := r.cursor - 1
	r.mu.Unlock()

	if r.strict {
		if err := compareContents(turn.Request, req); err != nil {
			yield(nil, fmt.Errorf("scripted: strict mismatch on turn %d%s: %w", idx, r.where(), err))
			return
		}
	}
	for _, resp := range turn.Responses {
		if !yield(resp, nil) {
			return
		}
	}
}

// where names the branch in an error, and says nothing at all for the
// unbranched replay — the sequential case, where naming an empty
// branch would be noise in every existing message.
func (r *replay) where() string {
	if r.branch == "" {
		return ""
	}
	return fmt.Sprintf(" of branch %q", r.branch)
}

// script is a parsed transcript held as its raw lines, so that each
// replay can decode its own copy. Parsing once at construction is
// what makes a malformed transcript a construction error; keeping the
// bytes is what keeps the decodes independent.
type script struct {
	lines  []scriptLine
	source string
	strict bool
}

func newScript(r io.Reader, source string, strict bool) (*script, error) {
	lines, err := readScriptLines(r, source)
	if err != nil {
		return nil, err
	}
	// Decode once and throw it away: the point is to fail here rather
	// than at first call. The replays below re-decode for themselves.
	if _, err := decodeLines(lines, source); err != nil {
		return nil, err
	}
	return &script{lines: lines, source: source, strict: strict}, nil
}

func (s *script) replay(branch string) (*replay, error) {
	turns, err := decodeLines(s.lines, s.source)
	if err != nil {
		// Unreachable in practice — newScript already decoded these
		// same bytes — but the alternative is discarding an error.
		return nil, fmt.Errorf("scripted: re-decode for branch %q: %w", branch, err)
	}
	return &replay{branch: branch, strict: s.strict, turns: turns}, nil
}

// loadScript parses a JSONL file where each non-blank line is a
// single RecordedTurn. Comment lines starting with "#" are tolerated
// so consumers can hand-edit fixtures.
func loadScript(path string) ([]RecordedTurn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	return decodeScript(f, path)
}

func decodeScript(r io.Reader, source string) ([]RecordedTurn, error) {
	lines, err := readScriptLines(r, source)
	if err != nil {
		return nil, err
	}
	return decodeLines(lines, source)
}

// scriptLine is one turn's raw JSON, with the file line number it
// came from so a decode error can name it.
type scriptLine struct {
	num int
	raw []byte
}

// readScriptLines splits the transcript into candidate turns without
// decoding any of them. Each line is copied: bufio.Scanner reuses its
// buffer, and these bytes outlive the scan.
func readScriptLines(r io.Reader, source string) ([]scriptLine, error) {
	var out []scriptLine
	sc := bufio.NewScanner(r)
	// Allow long lines — recorded turns can be large.
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 || raw[0] == '#' {
			continue
		}
		out = append(out, scriptLine{num: n, raw: slices.Clone(raw)})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: scan: %w", source, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no turns found", source)
	}
	return out, nil
}

func decodeLines(lines []scriptLine, source string) ([]RecordedTurn, error) {
	out := make([]RecordedTurn, 0, len(lines))
	for _, l := range lines {
		var t RecordedTurn
		if err := json.Unmarshal(l.raw, &t); err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", source, l.num, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// compareContents reports whether the recorded and incoming request
// have the same Contents (the message history). Config is ignored on
// purpose — tool declarations legitimately drift as the agent's tool
// registry evolves.
func compareContents(recorded, incoming *adkmodel.LLMRequest) error {
	var rec, inc []byte
	var err error
	if recorded != nil {
		rec, err = json.Marshal(recorded.Contents)
		if err != nil {
			return fmt.Errorf("marshal recorded: %w", err)
		}
	}
	if incoming != nil {
		inc, err = json.Marshal(incoming.Contents)
		if err != nil {
			return fmt.Errorf("marshal incoming: %w", err)
		}
	}
	if !bytes.Equal(rec, inc) {
		return fmt.Errorf("contents differ:\n  recorded: %s\n  incoming: %s", rec, inc)
	}
	return nil
}
