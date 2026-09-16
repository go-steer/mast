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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/permissions"
)

// fakePermsSource is a PermsSource a test drives directly: frames go in
// on the channel, answers come back out of answered.
type fakePermsSource struct {
	frames chan PromptFrame

	mu       sync.Mutex
	answered []fakeAnswer
	respErr  error
	approver string
}

type fakeAnswer struct {
	id string
	d  permissions.Decision
}

func newFakePermsSource() *fakePermsSource {
	return &fakePermsSource{frames: make(chan PromptFrame, 4)}
}

func (f *fakePermsSource) SubscribePrompts(ctx context.Context) (<-chan PromptFrame, func()) {
	out := make(chan PromptFrame)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case fr, ok := <-f.frames:
				if !ok {
					return
				}
				select {
				case out <- fr:
				case <-ctx.Done():
					return
				case <-done:
					return
				}
			}
		}
	}()
	return out, sync.OnceFunc(func() { close(done) })
}

func (f *fakePermsSource) RespondPrompt(_ context.Context, id string, d permissions.Decision) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.respErr != nil {
		return "", f.respErr
	}
	f.answered = append(f.answered, fakeAnswer{id: id, d: d})
	return f.approver, nil
}

func (f *fakePermsSource) answers() []fakeAnswer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeAnswer(nil), f.answered...)
}

// sourceRegistrant offers a PermsSource and nothing else — the shape a
// mast daemon registers (cmd/mast/permsource.go).
type sourceRegistrant struct {
	stubRegistrant
	src PermsSource
}

func (s *sourceRegistrant) AttachPermsSource() PermsSource { return s.src }

// bothRegistrant offers both capabilities, to pin which one wins.
type bothRegistrant struct {
	stubRegistrant
	src    PermsSource
	broker *PromptBroker
}

func (b *bothRegistrant) AttachPermsSource() PermsSource    { return b.src }
func (b *bothRegistrant) AttachPromptBroker() *PromptBroker { return b.broker }

// TestBuildFeatures_PermsStreamFollowsTheSource pins the honesty rule
// #364 was filed over: perms_stream is true exactly when the two prompt
// routes will answer with something other than 501.
//
// The interesting row is the third. Before #364 the probe was a type
// assertion — implementing PromptBrokerProvider raised the capability
// whether or not the method returned a broker — so a registrant that
// 501s both routes advertised them as available. A client trusts that
// frame to decide whether to show an approval UI at all.
func TestBuildFeatures_PermsStreamFollowsTheSource(t *testing.T) {
	t.Parallel()
	broker := NewPromptBroker()
	defer broker.Close()

	cases := []struct {
		name  string
		agent Registrant
		want  bool
	}{
		{
			name:  "no capability at all",
			agent: &stubRegistrant{app: "a", user: "u", sid: "s"},
			want:  false,
		},
		{
			name:  "a real broker",
			agent: &promptRegistrant{stubRegistrant: stubRegistrant{app: "a", user: "u", sid: "s"}, broker: broker},
			want:  true,
		},
		{
			name:  "the broker method, returning nil",
			agent: &promptRegistrant{stubRegistrant: stubRegistrant{app: "a", user: "u", sid: "s"}},
			want:  false,
		},
		{
			name:  "a real source",
			agent: &sourceRegistrant{stubRegistrant: stubRegistrant{app: "a", user: "u", sid: "s"}, src: newFakePermsSource()},
			want:  true,
		},
		{
			name:  "the source method, returning nil",
			agent: &sourceRegistrant{stubRegistrant: stubRegistrant{app: "a", user: "u", sid: "s"}},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildFeatures(&Entry{AppName: "a", SessionID: "s", Agent: tc.agent}, nil)
			if got[featurePermsStream] != tc.want {
				t.Errorf("features[%s] = %v, want %v", featurePermsStream, got[featurePermsStream], tc.want)
			}
		})
	}
}

func TestPermsSourceFor_SourceWinsOverBroker(t *testing.T) {
	t.Parallel()
	broker := NewPromptBroker()
	defer broker.Close()
	src := newFakePermsSource()

	// Both offered: the source is the richer answer (it can be durable;
	// a broker's pending map cannot), so it takes the routes.
	got := permsSourceFor(&Entry{Agent: &bothRegistrant{
		stubRegistrant: stubRegistrant{app: "a", user: "u", sid: "s"},
		src:            src,
		broker:         broker,
	}})
	if got != PermsSource(src) {
		t.Errorf("permsSourceFor = %#v, want the PermsSource", got)
	}

	// Source method present but empty: fall through to the broker rather
	// than 501, so a registrant can offer the seam conditionally.
	got = permsSourceFor(&Entry{Agent: &bothRegistrant{
		stubRegistrant: stubRegistrant{app: "a", user: "u", sid: "s"},
		broker:         broker,
	}})
	if bs, ok := got.(brokerSource); !ok || bs.b != broker {
		t.Errorf("permsSourceFor with nil source = %#v, want the broker adapted", got)
	}

	// Neither: nil, which is what makes the routes 501 and the
	// capability false.
	if got := permsSourceFor(&Entry{Agent: &bothRegistrant{
		stubRegistrant: stubRegistrant{app: "a", user: "u", sid: "s"},
	}}); got != nil {
		t.Errorf("permsSourceFor with neither = %#v, want nil", got)
	}
}

// TestIntegration_PermsSource_StreamAndRespond runs the wire contract
// against a source that is not a broker: the same two routes, the same
// frame shape, the same POST body. That equivalence is the deliverable
// — a client written against core-agent's prompt surface must not be
// able to tell which mechanism is underneath.
func TestIntegration_PermsSource_StreamAndRespond(t *testing.T) {
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
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/sessions/core-agent/s1/perms/stream", nil)
	streamResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("subscribe status %d: %s", streamResp.StatusCode, body)
	}

	src.frames <- PromptFrame{
		ID:       "int-1",
		Kind:     "mutation",
		ToolName: "patch_resource",
		Detail:   "patch_resource deployment/api",
		Source:   "sre",
		At:       time.Now(),
	}

	frame := readPromptFrame(t, streamResp.Body)
	if frame.ID != "int-1" || frame.Kind != "mutation" || frame.ToolName != "patch_resource" {
		t.Errorf("frame = %+v, want the source's frame verbatim", frame)
	}

	body, _ := json.Marshal(PromptResponse{ID: frame.ID, Decision: "allow-once"})
	resp, err := http.Post(base+"/sessions/core-agent/s1/perms/respond", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST respond: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST respond status = %d, want 200", resp.StatusCode)
	}

	got := src.answers()
	if len(got) != 1 || got[0].id != "int-1" || got[0].d != permissions.DecisionAllowOnce {
		t.Errorf("source saw %+v, want one allow-once for int-1", got)
	}
}

// TestIntegration_PermsSource_RespondEchoesTheApprover pins the ack
// body. switchboard's client reads "approver" to tell "the daemon
// recorded who approved" from "the daemon recorded nobody", and reports
// them differently in a thread — so the field has to follow what the
// source actually wrote down rather than who held the bearer token.
func TestIntegration_PermsSource_RespondEchoesTheApprover(t *testing.T) {
	t.Parallel()
	for name, approver := range map[string]string{
		"a source that records an identity": "alice@example.com",
		"a source that records nobody":      "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := newFakePermsSource()
			src.approver = approver
			reg := NewSessionRegistry()
			if _, err := reg.Register(&sourceRegistrant{
				stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
				src:            src,
			}); err != nil {
				t.Fatal(err)
			}
			base, cleanup := startTestServer(t, reg)
			defer cleanup()

			body, _ := json.Marshal(PromptResponse{ID: "x", Decision: "allow-once"})
			resp, err := http.Post(base+"/sessions/core-agent/s1/perms/respond", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST respond: %v", err)
			}
			defer resp.Body.Close()
			var ack struct {
				Acknowledged bool   `json:"acknowledged"`
				Approver     string `json:"approver"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
				t.Fatalf("decode ack: %v", err)
			}
			if !ack.Acknowledged {
				t.Error("acknowledged = false on a 200; a client reads this as a self-contradicting reply")
			}
			if ack.Approver != approver {
				t.Errorf("approver = %q, want %q", ack.Approver, approver)
			}
		})
	}
}

// TestIntegration_PermsSource_RespondStatusFromError pins the three-way
// split. A source refusing a decision on principle and a source that
// has lost the prompt are different client bugs and must not arrive as
// the same status.
func TestIntegration_PermsSource_RespondStatusFromError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"prompt is gone", fmt.Errorf("%w: id x", ErrPromptNotFound), http.StatusNotFound},
		{"decision refused on principle", fmt.Errorf("%w: allow-session", ErrDecisionNotAdmissible), http.StatusBadRequest},
		{"anything else", errors.New("the resume turn failed"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := newFakePermsSource()
			src.respErr = tc.err
			reg := NewSessionRegistry()
			if _, err := reg.Register(&sourceRegistrant{
				stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
				src:            src,
			}); err != nil {
				t.Fatal(err)
			}
			base, cleanup := startTestServer(t, reg)
			defer cleanup()

			body, _ := json.Marshal(PromptResponse{ID: "x", Decision: "allow-once"})
			resp, err := http.Post(base+"/sessions/core-agent/s1/perms/respond", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST respond: %v", err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// readPromptFrame pulls the first SSE data frame off an open stream.
func readPromptFrame(t *testing.T, body io.Reader) PromptFrame {
	t.Helper()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var frame PromptFrame
		raw := strings.TrimPrefix(line, "data: ")
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatalf("frame unmarshal: %v: %s", err, raw)
		}
		return frame
	}
	t.Fatal("no prompt frame arrived within the deadline")
	return PromptFrame{}
}
