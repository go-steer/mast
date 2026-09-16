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

package agui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/serverauth"
)

// capBackend is a fakeBackend that also reports capabilities, answering from a
// per-workload table so a test can assert the document is keyed on the
// workload name rather than on whichever entry happened to be first.
type capBackend struct {
	fakeBackend
	byName map[string]AgentCapabilities
	asked  []string
}

func (b *capBackend) AGUICapabilities(workloadName string) AgentCapabilities {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked = append(b.asked, workloadName)
	return b.byName[workloadName]
}

// mustValidator builds a one-token bearer validator, so a test can assert on
// the document an unauthenticated caller receives from an authenticated
// server.
func mustValidator(t *testing.T) serverauth.TokenValidator {
	t.Helper()
	v, err := serverauth.NewStaticBearerValidator(map[string]*serverauth.Principal{"tok": {Subject: "s"}})
	if err != nil {
		t.Fatalf("NewStaticBearerValidator: %v", err)
	}
	return v
}

// getDiscovery fetches the discovery document and returns both the decoded
// descriptors and the raw body, because two of the assertions below are about
// which keys the JSON contains rather than what they decode to.
func getDiscovery(t *testing.T, ts *httptest.Server) ([]AgentDescriptor, string) {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + DiscoveryPath)
	if err != nil {
		t.Fatalf("GET discovery: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("discovery status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read discovery: %v", err)
	}
	var out []AgentDescriptor
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode discovery %q: %v", raw, err)
	}
	return out, string(raw)
}

// TestDiscoveryCapabilitiesFromReporter is the core of #377: the document
// carries, per workload, which optional frame families a run on it can
// contain, and it gets them from the Backend rather than from the exposed-
// workload config.
func TestDiscoveryCapabilitiesFromReporter(t *testing.T) {
	be := &capBackend{byName: map[string]AgentCapabilities{
		"alpha": {StateDelta: true, StateKeys: []string{"plan", "phase"}, Reasoning: true},
		"beta":  {},
	}}
	ts, _ := testServer(t, Config{
		Backend: be,
		Exposed: []ExposedWorkload{
			{WorkloadName: "alpha", EndpointPath: "/agui/alpha"},
			{WorkloadName: "beta", EndpointPath: "/agui/beta"},
		},
	})
	out, _ := getDiscovery(t, ts)
	if len(out) != 2 {
		t.Fatalf("descriptors = %d, want 2", len(out))
	}
	alpha, beta := out[0], out[1]
	if alpha.Name != "alpha" || beta.Name != "beta" {
		t.Fatalf("unexpected order: %q then %q", alpha.Name, beta.Name)
	}
	if !alpha.Capabilities.StateDelta || !alpha.Capabilities.Reasoning {
		t.Errorf("alpha capabilities = %+v, want both on", alpha.Capabilities)
	}
	// Declared order, not sorted and not map order: a client lays out its
	// panels off this list before the first patch arrives.
	if !slices.Equal(alpha.Capabilities.StateKeys, []string{"plan", "phase"}) {
		t.Errorf("alpha.state_keys = %v, want [plan phase] in declared order", alpha.Capabilities.StateKeys)
	}
	// Two workloads on one daemon and one protocol version differ on every
	// field, which is the reason this cannot be a server-wide statement.
	if beta.Capabilities.StateDelta || beta.Capabilities.Reasoning || beta.Capabilities.StateKeys != nil {
		t.Errorf("beta capabilities = %+v, want the zero value", beta.Capabilities)
	}
	if !slices.Contains(be.asked, "alpha") || !slices.Contains(be.asked, "beta") {
		t.Errorf("reporter was asked about %v, want both workload names", be.asked)
	}
}

// TestDiscoveryCapabilitiesWithoutReporter pins the fallback: a Backend that
// does not implement CapabilityReporter still produces a well-formed
// capabilities object, and it promises nothing.
func TestDiscoveryCapabilitiesWithoutReporter(t *testing.T) {
	ts, _ := testServer(t, Config{Backend: newFakeBackend()})
	out, _ := getDiscovery(t, ts)
	if len(out) != 1 {
		t.Fatalf("descriptors = %d, want 1", len(out))
	}
	if got := out[0].Capabilities; got.StateDelta || got.Reasoning || got.StateKeys != nil {
		t.Errorf("capabilities = %+v, want the zero value for a non-reporting backend", got)
	}
}

// TestDiscoveryCapabilitiesFalseIsExplicit is why the bool fields carry no
// omitempty. A client has to distinguish "this server predates capabilities"
// from "this workload emits no reasoning", and eliding false collapses those
// into the same document — the ambiguity the object exists to remove.
func TestDiscoveryCapabilitiesFalseIsExplicit(t *testing.T) {
	ts, _ := testServer(t, Config{Backend: newFakeBackend()})
	_, raw := getDiscovery(t, ts)
	for _, want := range []string{`"capabilities"`, `"state_delta": false`, `"reasoning": false`} {
		if !strings.Contains(raw, want) {
			t.Errorf("discovery document is missing %s:\n%s", want, raw)
		}
	}
	// The one field that IS omitted when empty: an empty key list would
	// have a client render an empty panel rail.
	if strings.Contains(raw, `"state_keys"`) {
		t.Errorf("state_keys present with no projection:\n%s", raw)
	}
}

// TestDiscoveryDropsStateKeysWithoutStateDelta pins the self-consistency
// clamp. A reporter that names keys while saying no patch can be emitted is
// describing a channel that cannot open, and a client wiring panels off
// state_keys would wait forever for the first one. The document resolves the
// contradiction rather than forwarding it.
func TestDiscoveryDropsStateKeysWithoutStateDelta(t *testing.T) {
	be := &capBackend{byName: map[string]AgentCapabilities{
		"triage": {StateDelta: false, StateKeys: []string{"plan"}, Reasoning: true},
	}}
	ts, _ := testServer(t, Config{Backend: be})
	out, _ := getDiscovery(t, ts)
	if got := out[0].Capabilities.StateKeys; got != nil {
		t.Errorf("state_keys = %v with state_delta false, want nil", got)
	}
	// The clamp is narrow: the unrelated field is untouched.
	if !out[0].Capabilities.Reasoning {
		t.Error("the state_keys clamp cleared reasoning too")
	}
}

// TestDiscoveryStaysPublic keeps the endpoint's answer to #377's second open
// question pinned in code. Adding capabilities did not move the document
// behind the token: it states only what a client permitted to run would
// observe in the stream anyway.
func TestDiscoveryStaysPublic(t *testing.T) {
	be := &capBackend{byName: map[string]AgentCapabilities{
		"triage": {StateDelta: true, StateKeys: []string{"plan"}, Reasoning: true},
	}}
	ts, _ := testServer(t, Config{Backend: be, Validator: mustValidator(t)})
	out, _ := getDiscovery(t, ts) // no Authorization header
	if !out[0].Auth.Required {
		t.Fatal("auth.required = false, want true (validator configured)")
	}
	if !out[0].Capabilities.StateDelta || !out[0].Capabilities.Reasoning {
		t.Errorf("capabilities = %+v, want the reported values on the public document", out[0].Capabilities)
	}
}
