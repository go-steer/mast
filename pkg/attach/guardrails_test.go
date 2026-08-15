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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// guardrailRegistrant is a stubRegistrant wired for the guardrail
// surface. resetErr, when set, is what the reset capability returns —
// the 409 and 400 paths are entirely about which error comes back.
type guardrailRegistrant struct {
	stubRegistrant
	info     GuardrailInfo
	resetErr error
	gotReq   GuardrailResetRequest
}

func (r *guardrailRegistrant) AttachGuardrails() GuardrailInfo { return r.info }

func (r *guardrailRegistrant) AttachResetGuardrail(req GuardrailResetRequest) (GuardrailResetResponse, error) {
	r.gotReq = req
	if r.resetErr != nil {
		// Post-refusal state rides along, which is what makes the 409
		// body actionable without a follow-up GET.
		return GuardrailResetResponse{Guardrails: r.info}, r.resetErr
	}
	return GuardrailResetResponse{
		Reset:          []string{GuardrailCostCeiling},
		BudgetAddedUSD: req.AdditionalBudgetUSD,
		Guardrails:     r.info,
		Message:        "cleared cost_ceiling",
	}, nil
}

func trippedInfo() GuardrailInfo {
	return GuardrailInfo{
		Watchdog: WatchdogInfo{Mode: "warn", Advisory: true, Alerts: 2, Reason: "tool loop"},
		CostCeiling: CostCeilingInfo{
			MaxSessionUSD:  10,
			SessionCostUSD: 12.5,
			MaxTurns:       40,
			Turns:          41,
			Tripped:        true,
			WouldRetrip:    true,
			Reason:         "$12.5000 > cap $10.0000 (30600 tokens over 41 calls)",
			Scopes: []ScopeCeilingInfo{
				{Name: "log-analyst", MaxTurns: 5, Turns: 6, Tripped: true, Reason: "6 model calls (turns) > cap 5"},
			},
		},
		Halted: true,
	}
}

func guardrailServer(t *testing.T, ag Registrant) string {
	t.Helper()
	reg := NewSessionRegistry()
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	t.Cleanup(cleanup)
	return base
}

func postReset(t *testing.T, base, body string) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest("POST", base+"/sessions/core-agent/s1/guardrails/reset", rdr)
	if err != nil {
		t.Fatal(err)
	}
	// Required even on an empty body — see the 415 test below.
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /guardrails/reset: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, raw
}

func TestIntegration_GuardrailsEndpoint(t *testing.T) {
	t.Parallel()
	base := guardrailServer(t, &guardrailRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		info:           trippedInfo(),
	})

	resp, err := http.Get(base + "/sessions/core-agent/s1/guardrails")
	if err != nil {
		t.Fatalf("GET /guardrails: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var got GuardrailInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Halted || !got.CostCeiling.Tripped || !got.CostCeiling.WouldRetrip {
		t.Errorf("state = %+v, want a halted session", got)
	}
	// The three dimensions and the per-specialist breakdown are the
	// mast-native half: a dollars-only view of this session would
	// report a $12.50 overspend and hide the specialist that is also
	// over, which is the one an operator has to raise separately.
	if got.CostCeiling.MaxTurns != 40 || got.CostCeiling.Turns != 41 {
		t.Errorf("turn dimension lost: %+v", got.CostCeiling)
	}
	if len(got.CostCeiling.Scopes) != 1 || !got.CostCeiling.Scopes[0].Tripped {
		t.Errorf("scopes = %+v, want the tripped specialist", got.CostCeiling.Scopes)
	}
}

func TestIntegration_GuardrailsEndpoint_NoProvider_ZeroState(t *testing.T) {
	t.Parallel()
	base := guardrailServer(t, &stubRegistrant{app: "core-agent", user: "u", sid: "s1"})

	resp, err := http.Get(base + "/sessions/core-agent/s1/guardrails")
	if err != nil {
		t.Fatalf("GET /guardrails: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 (reads answer zero-value, not 501)", resp.StatusCode)
	}
	var got GuardrailInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Halted || got.CostCeiling.Tripped {
		t.Errorf("state = %+v, want nothing armed", got)
	}
	// A client iterating scopes must not have to nil-check first.
	if got.CostCeiling.Scopes == nil {
		t.Error("scopes decoded as nil; want an empty list")
	}
}

// The read answers for everyone; the write must not. An operator whose
// POST vanished into a 200 would believe the session was unwedged.
func TestIntegration_GuardrailsReset_NoCapability_501(t *testing.T) {
	t.Parallel()
	base := guardrailServer(t, &stubRegistrant{app: "core-agent", user: "u", sid: "s1"})
	resp, _ := postReset(t, base, "")
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", resp.StatusCode)
	}
}

// "Clear whatever tripped" is the common request, so an empty body is
// legal — and the caller is stamped from the authenticated context,
// never read off the wire.
func TestIntegration_GuardrailsReset_EmptyBodyAndCallerStamping(t *testing.T) {
	t.Parallel()
	ag := &guardrailRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		info:           trippedInfo(),
	}
	base := guardrailServer(t, ag)

	resp, raw := postReset(t, base, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var got GuardrailResetResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Reset) != 1 || got.Reset[0] != GuardrailCostCeiling {
		t.Errorf("reset = %v", got.Reset)
	}
	if !got.Guardrails.Halted {
		t.Error("response carries no post-reset state; a client would need a follow-up GET")
	}
	if ag.gotReq.Caller == "" {
		t.Error("caller not stamped from the auth context")
	}

	// A client-supplied caller is ignored — attribution a caller
	// writes about themselves is not attribution.
	_, _ = postReset(t, base, `{"caller":"someone-else","guardrail":"cost_ceiling"}`)
	if ag.gotReq.Caller == "someone-else" {
		t.Error("caller was read off the wire")
	}
}

// Raising a budget ceiling from a page the operator merely visited is
// exactly the cross-site write the content-type guard exists to stop,
// and the empty-body path is where a route can quietly opt out of it.
func TestIntegration_GuardrailsReset_RequiresJSONContentType(t *testing.T) {
	t.Parallel()
	base := guardrailServer(t, &guardrailRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		info:           trippedInfo(),
	})

	resp, err := http.Post(base+"/sessions/core-agent/s1/guardrails/reset", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d, want 415", resp.StatusCode)
	}
}

func TestIntegration_GuardrailsReset_BadRequests(t *testing.T) {
	t.Parallel()
	ag := &guardrailRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		info:           trippedInfo(),
	}
	base := guardrailServer(t, ag)

	for _, tc := range []struct{ name, body string }{
		{"unknown guardrail", `{"guardrail":"vibes"}`},
		{"negative dollars", `{"additional_budget_usd":-5}`},
		{"negative tokens", `{"additional_tokens":-5}`},
		{"negative turns", `{"additional_turns":-5}`},
		{"malformed json", `{"guardrail":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := postReset(t, base, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", resp.StatusCode, raw)
			}
		})
	}
}

// The heart of the contract: a reset that provably would not survive
// the next turn is refused, and the refusal carries the numbers the
// operator needs to size the grant.
func TestIntegration_GuardrailsReset_WouldRetrip_409(t *testing.T) {
	t.Parallel()
	ag := &guardrailRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		info:           trippedInfo(),
		resetErr:       fmt.Errorf("%w: spent $12.5000 of $10.0000", ErrGuardrailRetrip),
	}
	base := guardrailServer(t, ag)

	resp, raw := postReset(t, base, `{"guardrail":"cost_ceiling"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", resp.StatusCode, raw)
	}
	var got GuardrailResetResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Reset) != 0 {
		t.Errorf("reset = %v on a refusal, want empty", got.Reset)
	}
	if !strings.Contains(got.Message, "$12.5000") {
		t.Errorf("message = %q, want the spend and the ceiling", got.Message)
	}
	if !got.Guardrails.Halted {
		t.Error("409 body carries no state; the client can't render 'spent $X of $Y'")
	}
}

// A registrant that can service the reset advertises it, so a client
// knows whether to offer the button. cost_ceiling is a separate claim:
// the surface can be wired on a session that has no budget at all.
func TestGuardrailCapabilityFlags(t *testing.T) {
	t.Parallel()
	entry := &Entry{AppName: "core-agent", SessionID: "s1", Agent: &guardrailRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}}
	got := buildFeatures(entry, nil)
	if !got[featureGuardrails] {
		t.Errorf("guardrails feature = false for a registrant that implements the resetter")
	}
	if got[featureCostCeiling] {
		t.Errorf("cost_ceiling advertised without a report claiming one")
	}

	bare := &Entry{AppName: "core-agent", SessionID: "s1", Agent: &stubRegistrant{}}
	if got := buildFeatures(bare, nil); got[featureGuardrails] {
		t.Error("guardrails advertised on a registrant with no reset capability")
	}
}

// Configured is what the cost_ceiling flag is derived from: any of the
// three dimensions counts, because any of them can halt the session.
func TestCostCeilingConfigured(t *testing.T) {
	t.Parallel()
	if (CostCeilingInfo{}).Configured() {
		t.Error("an unbounded session reports a ceiling")
	}
	for _, c := range []CostCeilingInfo{
		{MaxSessionUSD: 10},
		{MaxTokens: 1000},
		{MaxTurns: 40},
	} {
		if !c.Configured() {
			t.Errorf("%+v reports no ceiling", c)
		}
	}
	// Usage alone is not a ceiling.
	if (CostCeilingInfo{SessionCostUSD: 5, Turns: 3}).Configured() {
		t.Error("spend without a cap reports a ceiling")
	}
}
