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
//
// Originally derived from go-steer/core-agent@b8dd225e9ae7fdeb3ff23772cc5be25eed34b818

package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestScripted_PlaysTurnsInOrder(t *testing.T) {
	t.Parallel()
	turns := []RecordedTurn{
		{
			Request: &adkmodel.LLMRequest{Model: "m"},
			Responses: []*adkmodel.LLMResponse{
				{Content: textContent(genai.RoleModel, "first"), TurnComplete: true},
			},
		},
		{
			Request: &adkmodel.LLMRequest{Model: "m"},
			Responses: []*adkmodel.LLMResponse{
				{Content: textContent(genai.RoleModel, "second"), TurnComplete: true},
			},
		},
	}
	llm := &scriptedLLM{turns: turns}

	got1 := drain(t, llm, &adkmodel.LLMRequest{})
	if got1[0].Content.Parts[0].Text != "first" {
		t.Errorf("turn 0: got %q", got1[0].Content.Parts[0].Text)
	}
	got2 := drain(t, llm, &adkmodel.LLMRequest{})
	if got2[0].Content.Parts[0].Text != "second" {
		t.Errorf("turn 1: got %q", got2[0].Content.Parts[0].Text)
	}
}

func TestScripted_ExhaustionIsAnError(t *testing.T) {
	t.Parallel()
	llm := &scriptedLLM{turns: []RecordedTurn{
		{Responses: []*adkmodel.LLMResponse{{TurnComplete: true}}},
	}}
	_ = drain(t, llm, &adkmodel.LLMRequest{})

	// Second call must error.
	for _, err := range llm.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		if err == nil {
			t.Fatal("expected exhaustion error on second call")
		}
		if !strings.Contains(err.Error(), "script exhausted") {
			t.Errorf("error %q missing 'script exhausted'", err.Error())
		}
		return // first iteration is enough
	}
	t.Fatal("expected an iteration with an error")
}

func TestScripted_StrictMatch(t *testing.T) {
	t.Parallel()
	contents := []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hello"}}}}
	llm := &scriptedLLM{
		strict: true,
		turns: []RecordedTurn{{
			Request:   &adkmodel.LLMRequest{Contents: contents},
			Responses: []*adkmodel.LLMResponse{{TurnComplete: true}},
		}},
	}
	got := drain(t, llm, &adkmodel.LLMRequest{Contents: contents})
	if len(got) != 1 || !got[0].TurnComplete {
		t.Errorf("expected matching strict turn to play through, got %+v", got)
	}
}

func TestScripted_StrictMismatch(t *testing.T) {
	t.Parallel()
	llm := &scriptedLLM{
		strict: true,
		turns: []RecordedTurn{{
			Request: &adkmodel.LLMRequest{Contents: []*genai.Content{
				{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "recorded"}}},
			}},
			Responses: []*adkmodel.LLMResponse{{TurnComplete: true}},
		}},
	}
	incoming := &adkmodel.LLMRequest{Contents: []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "different"}}},
	}}
	for _, err := range llm.GenerateContent(context.Background(), incoming, false) {
		if err == nil {
			t.Fatal("expected strict mismatch error")
		}
		if !strings.Contains(err.Error(), "strict mismatch") {
			t.Errorf("error %q missing 'strict mismatch'", err.Error())
		}
		return
	}
	t.Fatal("expected an iteration with an error")
}

func TestNewScripted_LoadsAndPlays(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "script.jsonl")
	body := `{"request":{"model":"m"},"responses":[{"content":{"role":"model","parts":[{"text":"hi"}]},"turnComplete":true}]}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	llm, err := NewScripted(path, false)
	if err != nil {
		t.Fatalf("NewScripted: %v", err)
	}
	got := drain(t, llm, &adkmodel.LLMRequest{})
	if len(got) != 1 || got[0].Content.Parts[0].Text != "hi" {
		t.Errorf("expected scripted reply 'hi', got %+v", got)
	}
}

func TestNewScripted_MissingFile(t *testing.T) {
	t.Parallel()
	_, err := NewScripted(filepath.Join(t.TempDir(), "absent.jsonl"), false)
	if err == nil || !strings.Contains(err.Error(), "scripted:") {
		t.Errorf("expected wrapped open error, got %v", err)
	}
}

func TestLoadScript_TolerantOfBlankAndCommentLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "script.jsonl")
	body := "" +
		"# this is a comment\n" +
		"\n" +
		`{"request":{"model":"m"},"responses":[{"turnComplete":true}]}` + "\n" +
		"# trailing comment\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	turns, err := loadScript(path)
	if err != nil {
		t.Fatalf("loadScript: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
}

func TestLoadScript_BadJSONL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadScript(path)
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("expected line-1 parse error, got %v", err)
	}
}

func TestLoadScript_EmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadScript(path)
	if err == nil || !strings.Contains(err.Error(), "no turns found") {
		t.Errorf("expected no-turns error, got %v", err)
	}
}

func textContent(role, s string) *genai.Content {
	return &genai.Content{Role: role, Parts: []*genai.Part{{Text: s}}}
}

// TestScripted_PlaysFromRecording is the wire-format contract check.
// core-agent's recording.NewRecorder emits one json.Encoder-encoded
// RecordedTurn per line; encodeTurns below reproduces that encoding
// byte-for-byte (same type, same field order, same encoder), so the
// round trip through decodeScript pins compatibility with recorded
// transcripts without importing the recorder.
func TestScripted_PlaysFromRecording(t *testing.T) {
	t.Parallel()
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "go"}}},
	}}
	buf := encodeTurns(t, []RecordedTurn{{
		Request: req,
		Responses: []*adkmodel.LLMResponse{
			{Content: textContent(genai.RoleModel, "first"), TurnComplete: true},
		},
	}})

	turns, err := decodeScript(buf, "buf")
	if err != nil {
		t.Fatalf("decodeScript: %v", err)
	}
	scripted := &scriptedLLM{turns: turns}
	got := drain(t, scripted, req)
	if len(got) != 1 || got[0].Content.Parts[0].Text != "first" {
		t.Errorf("scripted replay didn't reproduce recorded response, got %+v", got)
	}
}

// encodeTurns writes turns as JSONL the same way the recorder does:
// one json.Encoder.Encode call per turn.
func encodeTurns(t *testing.T, turns []RecordedTurn) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, turn := range turns {
		if err := enc.Encode(turn); err != nil {
			t.Fatalf("encode turn: %v", err)
		}
	}
	return &buf
}

func drain(t *testing.T, llm adkmodel.LLM, req *adkmodel.LLMRequest) []*adkmodel.LLMResponse {
	t.Helper()
	var out []*adkmodel.LLMResponse
	for resp, err := range llm.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out = append(out, resp)
	}
	return out
}
