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

package cli

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/internal/attach"
	"github.com/go-steer/mast/internal/auth"
	"github.com/go-steer/mast/internal/eventlog"
	"github.com/go-steer/mast/internal/transcript"
)

func TestNewAttachSessionID(t *testing.T) {
	a, err := newAttachSessionID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newAttachSessionID()
	if !strings.HasPrefix(a, "attach-") || len(a) != len("attach-")+16 {
		t.Errorf("unexpected id %q", a)
	}
	if a == b {
		t.Errorf("ids should differ, got %q twice", a)
	}
}

// fakeRegistrant is the least a session needs to be registered.
type fakeRegistrant struct{ sid string }

func (f fakeRegistrant) AppName() string                    { return appName }
func (f fakeRegistrant) UserID() string                     { return defaultUserID }
func (f fakeRegistrant) SessionID() string                  { return f.sid }
func (f fakeRegistrant) EventLog() *eventlog.Handle         { return nil }
func (f fakeRegistrant) Inject(string) error                { return nil }
func (f fakeRegistrant) InjectAs(string, auth.Caller) error { return nil }
func (f fakeRegistrant) RequestWake()                       {}

// POST /sessions on serve's attach wiring opens a session backed by the
// workload adapter, rather than answering 501 (#524).
func TestBuildAttachCreatesSessions(t *testing.T) {
	var built []string
	adapterFor := func(sid string) (attach.Registrant, error) {
		built = append(built, sid)
		return fakeRegistrant{sid: sid}, nil
	}
	store := transcript.NewStore(adksession.InMemoryService(), appName)
	att, err := buildAttach(slog.New(slog.DiscardHandler), "127.0.0.1:0", "tok", store, adapterFor)
	if err != nil {
		t.Fatalf("buildAttach: %v", err)
	}
	go func() { _ = att.srv.Serve() }()
	t.Cleanup(func() { _ = att.srv.Close() })

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+att.srv.Addr()+"/sessions", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /sessions = %d %s, want 201", resp.StatusCode, body)
	}
	var got struct {
		App       string `json:"app"`
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if got.App != appName || len(built) != 1 || got.SessionID != built[0] {
		t.Errorf("created %+v, adapters built for %v; want one adapter for the returned session", got, built)
	}
}
