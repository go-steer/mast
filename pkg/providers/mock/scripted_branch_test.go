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

package mock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// branchCtx is the smallest agent.ReadonlyContext that answers
// Branch(), which is the only thing the scripted provider asks a
// context for. ADK's ContextMock supplies the rest.
//
// The end-to-end proof that a real branch tag reaches the model this
// way is pkg/graph's TestFanoutReplaysOneRecordingPerBranch; this type
// exists so the provider's own rules can be stated without standing up
// a runner.
type branchCtx struct {
	*adkagent.ContextMock
	branch string
}

func (c branchCtx) Branch() string { return c.branch }

var _ adkagent.ReadonlyContext = branchCtx{}

func onBranch(name string) context.Context {
	return branchCtx{ContextMock: &adkagent.ContextMock{}, branch: name}
}

// numberedTurns builds a transcript whose turn i replies "turn-i", so
// a caller's position in the script is readable off what it got back.
func numberedTurns(n int) []RecordedTurn {
	out := make([]RecordedTurn, 0, n)
	for i := range n {
		out = append(out, RecordedTurn{
			Request: &adkmodel.LLMRequest{Model: "m"},
			Responses: []*adkmodel.LLMResponse{
				{Content: textContent(genai.RoleModel, fmt.Sprintf("turn-%d", i)), TurnComplete: true},
			},
		})
	}
	return out
}

// oneLine drains one call and returns the text of its first response,
// or the error rendered as "ERR: …". It reports rather than fails so
// the concurrent tests below can call it from their goroutines.
func oneLine(llm adkmodel.LLM, ctx context.Context) string {
	var got string
	for resp, err := range llm.GenerateContent(ctx, &adkmodel.LLMRequest{}, false) {
		if err != nil {
			return "ERR: " + err.Error()
		}
		if got == "" && resp.Content != nil && len(resp.Content.Parts) > 0 {
			got = resp.Content.Parts[0].Text
		}
	}
	return got
}

// say is oneLine for the sequential tests: an error is a test failure
// rather than a value.
func say(t *testing.T, llm adkmodel.LLM, ctx context.Context) string {
	t.Helper()
	got := oneLine(llm, ctx)
	if strings.HasPrefix(got, "ERR: ") {
		t.Fatalf("unexpected error: %s", strings.TrimPrefix(got, "ERR: "))
	}
	return got
}

// firstErr drains one call and returns its first error, or nil.
func firstErr(llm adkmodel.LLM, ctx context.Context) error {
	for _, err := range llm.GenerateContent(ctx, &adkmodel.LLMRequest{}, false) {
		if err != nil {
			return err
		}
	}
	return nil
}

// TestScripted_UnbranchedCallersShareOneCursor is the half of the rule
// that must NOT change: every sequential shape mast has — a
// coordinator, a planner, the single-agent replays the v0.2 UAT
// harness runs — is unbranched, and has to go on consuming one script
// in one order. A context that reports no branch is treated the same
// as a context that cannot be asked at all.
func TestScripted_UnbranchedCallersShareOneCursor(t *testing.T) {
	t.Parallel()
	llm := newScriptedFromTurns(t, false, numberedTurns(3)...)

	if got := say(t, llm, context.Background()); got != "turn-0" {
		t.Errorf("plain context first call = %q, want turn-0", got)
	}
	if got := say(t, llm, onBranch("")); got != "turn-1" {
		t.Errorf("empty-branch context = %q, want turn-1 (it must share the plain context's cursor)", got)
	}
	if got := say(t, llm, context.Background()); got != "turn-2" {
		t.Errorf("plain context third call = %q, want turn-2", got)
	}
}

// TestScripted_LargeTranscriptSurvivesTheScanner is why the scanned
// lines are copied. bufio.Scanner hands out subslices of a buffer it
// refills, and the transcript's raw bytes now outlive the scan —
// they are kept so each replay can decode its own copy. A file that
// fits in the initial 64KiB buffer never shows the difference, and a
// real recording does not fit: this one is deliberately over it.
func TestScripted_LargeTranscriptSurvivesTheScanner(t *testing.T) {
	t.Parallel()
	// ~200KiB: several scanner refills, so an uncopied line from an
	// early read is overwritten before it is decoded.
	const turns = 200
	pad := strings.Repeat("x", 1024)
	recorded := make([]RecordedTurn, 0, turns)
	for i := range turns {
		recorded = append(recorded, RecordedTurn{
			Request: &adkmodel.LLMRequest{Model: "m"},
			Responses: []*adkmodel.LLMResponse{
				{Content: textContent(genai.RoleModel, fmt.Sprintf("turn-%d-%s", i, pad)), TurnComplete: true},
			},
		})
	}
	path := filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(path, encodeTurns(t, recorded).Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	llm, err := NewScripted(path, false)
	if err != nil {
		t.Fatalf("NewScripted: %v", err)
	}
	for i := range turns {
		want := fmt.Sprintf("turn-%d-%s", i, pad)
		if got := say(t, llm, onBranch("f.a")); got != want {
			t.Fatalf("turn %d replayed %.32q…, want %.32q…", i, got, want)
		}
	}
}

// TestScripted_UnbranchedCallersAreSerialized covers the case the
// per-branch key deliberately does NOT separate: two invocations at
// once in one process — a daemon handling two incidents — are both
// unbranched, so they share a replay. Keying on the invocation ID as
// well would give them one each, and would break multi-turn replay,
// where a second user turn is a new invocation that must go on
// consuming the same recording rather than restart it.
//
// Sharing is therefore the documented behaviour, and what this pins is
// that sharing is at least safe: the replay's own lock means the
// turns are handed out once each, in order, with no torn read. Run
// under -race, which is where the presubmit runs it.
func TestScripted_UnbranchedCallersAreSerialized(t *testing.T) {
	t.Parallel()
	const turns = 8
	llm := newScriptedFromTurns(t, false, numberedTurns(turns)...)

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		got []string
	)
	for range turns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			line := oneLine(llm, context.Background())
			mu.Lock()
			got = append(got, line)
			mu.Unlock()
		}()
	}
	wg.Wait()

	seen := map[string]int{}
	for _, line := range got {
		seen[line]++
	}
	if len(seen) != turns {
		t.Fatalf("%d concurrent unbranched calls yielded %d distinct turns, want %d: %v", turns, len(seen), turns, got)
	}
	for i := range turns {
		if n := seen[fmt.Sprintf("turn-%d", i)]; n != 1 {
			t.Errorf("turn-%d was handed out %d times, want exactly 1", i, n)
		}
	}
}

// TestScripted_EachBranchReplaysFromTheTop is the fix: concurrent
// lanes each get the whole recording, not a share of it.
func TestScripted_EachBranchReplaysFromTheTop(t *testing.T) {
	t.Parallel()
	llm := newScriptedFromTurns(t, false, numberedTurns(2)...)

	for _, br := range []string{"w_fan.branch_a", "w_fan.branch_b", "w_fan.branch_c"} {
		if got := say(t, llm, onBranch(br)); got != "turn-0" {
			t.Errorf("branch %q first call = %q, want turn-0", br, got)
		}
	}
	for _, br := range []string{"w_fan.branch_a", "w_fan.branch_b", "w_fan.branch_c"} {
		if got := say(t, llm, onBranch(br)); got != "turn-1" {
			t.Errorf("branch %q second call = %q, want turn-1", br, got)
		}
	}
	// The unbranched replay is untouched by all of that: synthesis runs
	// after the analysts and still starts at the top.
	if got := say(t, llm, context.Background()); got != "turn-0" {
		t.Errorf("unbranched call after three branches = %q, want turn-0", got)
	}
}

// TestScripted_ConcurrentBranchesAreDeterministic is the regression
// test proper. It is the defect's own shape: N lanes at once against
// one instance, each reading the transcript end to end.
//
// The old cursor was mutex-guarded, so this is deliberately not a
// -race test — the race detector was always silent on this bug. What
// fails on the old code is the assertion: with one shared cursor, 4
// branches over a 5-turn script exhaust it after 5 calls total and
// most branches never see turn-0 at all.
func TestScripted_ConcurrentBranchesAreDeterministic(t *testing.T) {
	t.Parallel()
	const branches, turns = 4, 5
	llm := newScriptedFromTurns(t, false, numberedTurns(turns)...)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string][]string{}
	)
	for b := range branches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			br := fmt.Sprintf("w_fan.branch_%d", b)
			ctx := onBranch(br)
			var got []string
			for range turns {
				got = append(got, oneLine(llm, ctx))
			}
			mu.Lock()
			seen[br] = got
			mu.Unlock()
		}()
	}
	wg.Wait()

	want := []string{"turn-0", "turn-1", "turn-2", "turn-3", "turn-4"}
	if len(seen) != branches {
		t.Fatalf("recorded %d branches, want %d", len(seen), branches)
	}
	for br, got := range seen {
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("branch %q replayed %v, want %v", br, got, want)
		}
	}
}

// TestScripted_BranchesDecodeIndependently pins why each replay
// re-decodes the transcript instead of sharing one decoded slice:
// recorded responses are pointers handed straight into the agent
// loop, and two branches holding the same *LLMResponse is a data race
// mast would be manufacturing on purpose.
func TestScripted_BranchesDecodeIndependently(t *testing.T) {
	t.Parallel()
	llm := newScriptedFromTurns(t, false, numberedTurns(1)...)

	var a, b *adkmodel.LLMResponse
	for resp := range llm.GenerateContent(onBranch("f.a"), &adkmodel.LLMRequest{}, false) {
		a = resp
	}
	for resp := range llm.GenerateContent(onBranch("f.b"), &adkmodel.LLMRequest{}, false) {
		b = resp
	}
	if a == nil || b == nil {
		t.Fatal("both branches should have replayed turn 0")
	}
	if a == b {
		t.Fatal("two branches were handed the same *LLMResponse pointer")
	}
	a.Content.Parts[0].Text = "clobbered"
	if b.Content.Parts[0].Text != "turn-0" {
		t.Fatalf("branch b's response changed when branch a's was written to: %q", b.Content.Parts[0].Text)
	}
}

// TestScripted_ExhaustionAndMismatchNameTheBranch: a fan-out failure
// that does not say which lane ran out is a failure somebody has to
// bisect by hand.
func TestScripted_ExhaustionAndMismatchNameTheBranch(t *testing.T) {
	t.Parallel()

	t.Run("exhaustion", func(t *testing.T) {
		t.Parallel()
		llm := newScriptedFromTurns(t, false, numberedTurns(1)...)
		_ = say(t, llm, onBranch("w_fan.branch_alpha"))
		err := firstErr(llm, onBranch("w_fan.branch_alpha"))
		if err == nil {
			t.Fatal("expected exhaustion on the second call")
		}
		if !strings.Contains(err.Error(), "script exhausted") || !strings.Contains(err.Error(), "w_fan.branch_alpha") {
			t.Errorf("error %q should say 'script exhausted' and name the branch", err)
		}
	})

	t.Run("strict mismatch", func(t *testing.T) {
		t.Parallel()
		llm := newScriptedFromTurns(t, true, RecordedTurn{
			Request: &adkmodel.LLMRequest{Contents: []*genai.Content{
				{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "recorded"}}},
			}},
			Responses: []*adkmodel.LLMResponse{{TurnComplete: true}},
		})
		incoming := &adkmodel.LLMRequest{Contents: []*genai.Content{
			{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "different"}}},
		}}
		var err error
		for _, e := range llm.GenerateContent(onBranch("w_fan.branch_beta"), incoming, false) {
			err = e
			break
		}
		if err == nil {
			t.Fatal("expected a strict mismatch")
		}
		if !strings.Contains(err.Error(), "strict mismatch") || !strings.Contains(err.Error(), "w_fan.branch_beta") {
			t.Errorf("error %q should say 'strict mismatch' and name the branch", err)
		}
	})

	t.Run("unbranched says nothing about branches", func(t *testing.T) {
		t.Parallel()
		llm := newScriptedFromTurns(t, false, numberedTurns(1)...)
		_ = say(t, llm, context.Background())
		err := firstErr(llm, context.Background())
		if err == nil {
			t.Fatal("expected exhaustion on the second call")
		}
		if strings.Contains(err.Error(), "branch") {
			t.Errorf("unbranched error %q should not mention a branch", err)
		}
	})
}

// TestNewScripted_ParsesOnceAtConstruction covers both halves of why
// the transcript is read eagerly: a malformed file fails at the
// constructor rather than at whichever branch calls first, and an edit
// to the file after construction cannot change what a branch that has
// not started yet will replay.
func TestNewScripted_ParsesOnceAtConstruction(t *testing.T) {
	t.Parallel()

	t.Run("malformed fails at the constructor", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "bad.jsonl")
		if err := os.WriteFile(path, []byte("{\"responses\":[]}\nnot json\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := NewScripted(path, false); err == nil || !strings.Contains(err.Error(), "line 2") {
			t.Fatalf("want a line-2 parse error from NewScripted, got %v", err)
		}
	})

	t.Run("a later edit cannot change a later branch", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "script.jsonl")
		original := `{"responses":[{"content":{"role":"model","parts":[{"text":"turn-0"}]},"turnComplete":true}]}` + "\n"
		if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
			t.Fatal(err)
		}
		llm, err := NewScripted(path, false)
		if err != nil {
			t.Fatalf("NewScripted: %v", err)
		}
		if got := say(t, llm, onBranch("f.a")); got != "turn-0" {
			t.Fatalf("branch a = %q, want turn-0", got)
		}
		edited := `{"responses":[{"content":{"role":"model","parts":[{"text":"rewritten"}]},"turnComplete":true}]}` + "\n"
		if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := say(t, llm, onBranch("f.b")); got != "turn-0" {
			t.Fatalf("branch b started after the file was rewritten and replayed %q, want turn-0", got)
		}
	})
}
