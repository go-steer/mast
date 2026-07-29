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

package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	mastsession "github.com/go-steer/mast/pkg/session"
)

// TestRunOneShot_EchoDebugPersistsSession is the one-shot e2e: echo
// model, --task=debug, temp SQLite. One turn runs to completion, the
// result lands on the out writer, and the session's events are
// durable in the DB (readable the way `mast sessions show` reads
// them).
func TestRunOneShot_EchoDebugPersistsSession(t *testing.T) {
	db := filepath.Join(t.TempDir(), "sessions.db")
	var out bytes.Buffer

	err := runOneShot(context.Background(), discardLogger(), oneShotOptions{
		Class:      "debug",
		Model:      "echo",
		SessionDB:  db,
		SessionDrv: "sqlite",
		Prompt:     "why does the deploy job crashloop?",
	}, &out)
	if err != nil {
		t.Fatalf("runOneShot: %v", err)
	}

	// The echo model finishes Task-mode agents via finish_task with an
	// "[echo triage]" result; that value must be what gets printed.
	got := out.String()
	if !strings.Contains(got, "[echo triage]") {
		t.Errorf("one-shot output = %q, want the echo model's finish_task result", got)
	}

	// Persistence: reopen the SQLite DB read-through and assert the
	// turn's events survived the (simulated) process exit.
	store, err := mastsession.Open(db, appName)
	if err != nil {
		t.Fatalf("reopen session db: %v", err)
	}
	d, err := store.Get(context.Background(), oneShotUserID, oneShotSessionID("debug"))
	if err != nil {
		t.Fatalf("get persisted session: %v", err)
	}
	if d.EventCount < 2 {
		t.Errorf("persisted EventCount = %d, want >= 2 (user turn + agent events)", d.EventCount)
	}

	// A second turn against the same DB continues the same session
	// (the "--session-db persists the session" contract).
	var out2 bytes.Buffer
	if err := runOneShot(context.Background(), discardLogger(), oneShotOptions{
		Class:      "debug",
		Model:      "echo",
		SessionDB:  db,
		SessionDrv: "sqlite",
		Prompt:     "and the previous incident?",
	}, &out2); err != nil {
		t.Fatalf("second runOneShot: %v", err)
	}
	d2, err := store.Get(context.Background(), oneShotUserID, oneShotSessionID("debug"))
	if err != nil {
		t.Fatalf("get session after second turn: %v", err)
	}
	if d2.EventCount <= d.EventCount {
		t.Errorf("EventCount after second turn = %d, want > %d (events append to the same session)", d2.EventCount, d.EventCount)
	}
}

// TestRunOneShot_ChatInMemory covers the Chat-mode class and the
// no-DB path (in-memory sessions).
func TestRunOneShot_ChatInMemory(t *testing.T) {
	var out bytes.Buffer
	err := runOneShot(context.Background(), discardLogger(), oneShotOptions{
		Class:      "chat",
		Model:      "echo",
		SessionDrv: "sqlite",
		Prompt:     "hello there",
	}, &out)
	if err != nil {
		t.Fatalf("runOneShot: %v", err)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Error("one-shot chat output is empty; want the echo model's reply")
	}
}

func TestRunOneShot_UnknownClass(t *testing.T) {
	err := runOneShot(context.Background(), discardLogger(), oneShotOptions{
		Class:      "classify", // SingleTurn is internal, never a public class
		Model:      "echo",
		SessionDrv: "sqlite",
		Prompt:     "x",
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unknown task class") {
		t.Errorf("got err %v, want unknown task class", err)
	}
}

// TestResolveModelSelection pins the --provider alias semantics:
// validation against an explicit --model, and provider-default model
// selection (via the --task profile's tier) when --model is unset.
func TestResolveModelSelection(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
		modelSet bool
		class    string
		want     string
		wantErr  string
	}{
		{name: "no provider passes model through", model: "echo", want: "echo"},
		{name: "echo provider defaults model", provider: "echo", model: "echo", want: "echo"},
		{name: "echo provider accepts explicit echo", provider: "echo", model: "echo", modelSet: true, want: "echo"},
		{name: "echo provider rejects gemini model", provider: "echo", model: "gemini-2.5-flash", modelSet: true, wantErr: "conflicts"},
		{name: "gemini provider accepts gemini model", provider: "gemini", model: "gemini-2.5-flash", modelSet: true, want: "gemini-2.5-flash"},
		{name: "gemini provider rejects echo model", provider: "gemini", model: "echo", modelSet: true, wantErr: "conflicts"},
		// No explicit --model: the --task profile's tier picks the
		// default (debug -> frontier; no class -> mid).
		{name: "gemini provider derives frontier from debug", provider: "gemini", model: "echo", class: "debug", want: "gemini-3.5-pro"},
		{name: "gemini provider derives mid without class", provider: "gemini", model: "echo", want: "gemini-2.5-pro"},
		{name: "anthropic provider accepts claude model", provider: "anthropic", model: "claude-sonnet-4-6", modelSet: true, want: "claude-sonnet-4-6"},
		{name: "anthropic provider rejects gemini model", provider: "anthropic", model: "gemini-2.5-flash", modelSet: true, wantErr: "conflicts"},
		{name: "anthropic provider derives frontier from debug", provider: "anthropic", model: "echo", class: "debug", want: "claude-opus-4-7"},
		{name: "anthropic provider derives mid without class", provider: "anthropic", model: "echo", want: "claude-sonnet-4-6"},
		{name: "anthropic-vertex resolves the same model ids", provider: "anthropic-vertex", model: "claude-haiku-4-5", modelSet: true, want: "claude-haiku-4-5"},
		{name: "anthropic-vertex derives mid without class", provider: "anthropic-vertex", model: "echo", want: "claude-sonnet-4-6"},
		{name: "scripted provider defaults model", provider: "scripted", model: "echo", want: "scripted"},
		{name: "scripted provider rejects other model", provider: "scripted", model: "gemini-2.5-flash", modelSet: true, wantErr: "conflicts"},
		{name: "unknown provider errors", provider: "vertex", model: "echo", wantErr: "unknown --provider"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveModelSelection(tt.provider, tt.model, tt.modelSet, tt.class)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got (%q, %v), want error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
