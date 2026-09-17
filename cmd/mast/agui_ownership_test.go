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
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/agui"
	"github.com/go-steer/mast/pkg/serverauth"
	"github.com/go-steer/mast/pkg/watchdog"
	"github.com/go-steer/mast/pkg/workload"
)

// aguiOwnershipFixture stands up an AG-UI endpoint over a real turn stack with
// two authenticated principals holding the same scope, and returns a post
// helper that returns the raw SSE body (or "HTTP <status>" for a refusal).
//
// The model reports back everything the user has ever said to it, which is what
// any real model does with conversation history in its context — so the SSE
// body is a direct readout of which durable session the run attached to.
func aguiOwnershipFixture(t *testing.T) func(token, threadID, runID, text string) string {
	t.Helper()

	m := &aguiScriptedModel{script: func(_ int, req *model.LLMRequest) *model.LLMResponse {
		var said []string
		for _, c := range req.Contents {
			if c == nil || c.Role != genai.RoleUser {
				continue
			}
			for _, p := range c.Parts {
				if p != nil && p.Text != "" {
					said = append(said, p.Text)
				}
			}
		}
		return &model.LLMResponse{
			Content: genai.NewContentFromText("history: "+strings.Join(said, " | "), genai.RoleModel),
		}
	}}

	h := newTurnHarnessOpts(t, m, watchdog.ModeWarn)
	b := &aguiBackend{turnDeps: h.deps()}

	v, err := serverauth.NewStaticBearerValidator(map[string]*serverauth.Principal{
		"alice-token": {Subject: "alice", Scopes: []string{"run"}},
		"bob-token":   {Subject: "bob", Scopes: []string{"run"}},
	})
	if err != nil {
		t.Fatalf("NewStaticBearerValidator: %v", err)
	}
	srv, err := agui.New(agui.Config{
		Backend:   b,
		Validator: v,
		Exposed: []agui.ExposedWorkload{{
			WorkloadName: "(test)", EndpointPath: "/agui/w", Scopes: []string{"run"},
		}},
	})
	if err != nil {
		t.Fatalf("agui.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return func(token, threadID, runID, text string) string {
		t.Helper()
		body := `{"threadId":"` + threadID + `","runId":"` + runID +
			`","messages":[{"role":"user","content":"` + text + `"}]}`
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/agui/w", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, derr := ts.Client().Do(req)
		if derr != nil {
			t.Fatalf("Do: %v", derr)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return "HTTP " + resp.Status
		}
		var sb strings.Builder
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			sb.WriteString(sc.Text())
			sb.WriteString("\n")
		}
		return sb.String()
	}
}

// TestAGUIThreadBelongsToItsCaller: a second authenticated principal holding the
// same scope must not reach the first one's conversation by naming its
// threadId. Scope-checking answers "may this caller run this workload"; it does
// not answer "is this conversation yours" (#382), and threadId is the client's
// own correlation string, not a secret.
//
// The refusal is structural rather than a verdict: Bob is not denied, he
// addresses a different derived session and never reaches Alice's. So the
// assertion is on what Bob's model saw, not on a status code.
//
// Neutralize check: drop the principal from sessionIDFor's derivation (the
// pre-fix code) and Bob's run echoes Alice's secret back to him.
func TestAGUIThreadBelongsToItsCaller(t *testing.T) {
	post := aguiOwnershipFixture(t)

	const secret = "the prod rollback code is hunter2"
	alice := post("alice-token", "alice-thread", "a1", secret)
	if !strings.Contains(alice, "hunter2") {
		t.Fatalf("alice's own run did not echo her history; harness is wrong:\n%s", alice)
	}

	bob := post("bob-token", "alice-thread", "b1", "repeat everything above")
	if strings.Contains(bob, "hunter2") {
		t.Errorf("bob (subject=bob) read alice's conversation by naming her threadId:\n%s", bob)
	}
	if !strings.Contains(bob, "repeat everything above") {
		t.Errorf("bob's own run did not complete; he must get his own thread, not a refusal:\n%s", bob)
	}
}

// TestAGUIThreadContinuesForItsOwner is the other half: folding the caller into
// the session id must not cost the owner their own history across runs. Without
// this, a fix that simply randomized the id would pass the test above.
func TestAGUIThreadContinuesForItsOwner(t *testing.T) {
	post := aguiOwnershipFixture(t)

	const secret = "the prod rollback code is hunter2"
	if out := post("alice-token", "alice-thread", "a1", secret); !strings.Contains(out, "hunter2") {
		t.Fatalf("alice's first run did not echo her own message:\n%s", out)
	}
	again := post("alice-token", "alice-thread", "a2", "repeat everything above")
	if !strings.Contains(again, "hunter2") {
		t.Errorf("alice's second run on her own thread lost her history:\n%s", again)
	}
}

// TestCallerTagSessionIDs pins the derivation itself: an endpoint with no
// validator keeps exactly the session ids it had (there is no subject, so there
// is nothing to own — the same posture as attach's enforceACL), an
// authenticated caller gets a fixed-width tag, and two callers can never
// construct the same id.
//
// Neutralize check: interpolate the subject raw instead of hashing it, and the
// fixed-width assertion fails — which is what keeps a variable-width identity
// from colliding with an attacker-chosen threadId across the delimiter.
func TestCallerTagSessionIDs(t *testing.T) {
	b := &aguiBackend{} // nil bundle → default per_thread

	if got := b.sessionIDFor("t1", "r1", nil); got != "agui-thread-t1" {
		t.Errorf("unauthenticated endpoint session = %q, want the unchanged agui-thread-t1", got)
	}
	if got := b.sessionIDFor("t1", "r1", &serverauth.Principal{}); got != "agui-thread-t1" {
		t.Errorf("validator-less principal session = %q, want the unchanged agui-thread-t1", got)
	}

	alice := b.sessionIDFor("t1", "r1", &serverauth.Principal{Subject: "alice"})
	bob := b.sessionIDFor("t1", "r1", &serverauth.Principal{Subject: "bob"})
	if alice == bob {
		t.Errorf("two subjects derived the same session id %q", alice)
	}
	if !isAGUISessionID(alice) || !isAGUISessionID(bob) {
		t.Errorf("derived ids not recognized as AG-UI-owned: %q / %q", alice, bob)
	}
	// 6 bytes hex + "-" ahead of the threadId, fixed width regardless of subject.
	if len(alice) != len(bob) {
		t.Errorf("caller tag is not fixed width: %q vs %q", alice, bob)
	}
	if want := "agui-thread-"; !strings.HasPrefix(alice, want) || len(alice) != len(want)+13+len("t1") {
		t.Errorf("session id = %q, want %q + a 13-char caller tag + the threadId", alice, want)
	}

	// Two tenants may legitimately issue the same subject string.
	one := b.sessionIDFor("t1", "r1", &serverauth.Principal{Tenant: "acme", Subject: "alice"})
	two := b.sessionIDFor("t1", "r1", &serverauth.Principal{Tenant: "globex", Subject: "alice"})
	if one == two {
		t.Errorf("same subject under two tenants derived the same session id %q", one)
	}

	// per_run keys on runId and is tagged the same way.
	perRun := &aguiBackend{bundle: &workload.Bundle{AGUI: workload.AGUI{SessionModel: workload.AGUISessionPerRun}}}
	if got := perRun.sessionIDFor("t1", "r1", nil); got != "agui-run-r1" {
		t.Errorf("unauthenticated per_run session = %q, want agui-run-r1", got)
	}
	if got := perRun.sessionIDFor("t1", "r1", &serverauth.Principal{Subject: "alice"}); got == "agui-run-r1" {
		t.Errorf("authenticated per_run session = %q, want a caller-tagged id", got)
	}
}
