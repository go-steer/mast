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
	"net/http"
	"testing"
	"time"
)

// GET /perms as a read that is either true or a refusal (#375).
//
// The route answered 200 with the zero PermsInfo for every registrant
// that did not implement PermsProvider, which was every mast daemon
// ever shipped. That is the #133 shape — a read that looks like an
// answer — with an aggravating factor the tool catalog did not have:
// an empty tool list is visibly empty, whereas `{"mode":""}` is a
// description of a daemon that permits everything and has adjudicated
// nothing. A client cannot tell it from the truth.

// permsRefusingRegistrant implements PermsProvider *and* reports the
// capability off — the attachadapter shape, where every optional
// interface is satisfied structurally and only the report knows which
// ones are wired (#490).
type permsRefusingRegistrant struct {
	stubRegistrant
	report CapabilityReport
}

func (r *permsRefusingRegistrant) AttachPerms() PermsInfo { return PermsInfo{Mode: "yolo"} }

func (r *permsRefusingRegistrant) AttachCapabilities() CapabilityReport { return r.report }

// TestPermsRead_RefusesWhenNoProvider is the regression. On pre-#375
// code this gets 200 and a body a client renders as "no rules".
func TestPermsRead_RefusesWhenNoProvider(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	// stubRegistrant satisfies Registrant and nothing else.
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "s1"}); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/perms")
	if err != nil {
		t.Fatalf("GET /perms: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: a registrant with no permissions source must refuse the read, not describe itself as ungoverned", resp.StatusCode)
	}
}

// TestPermsRead_RefusesWhenTheReportSaysUnwired covers the case the
// whole capability-report mechanism exists for: the type satisfies
// PermsProvider, so presence probing would say yes, and only the
// registrant knows it has nothing behind it.
func TestPermsRead_RefusesWhenTheReportSaysUnwired(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&permsRefusingRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		report:         CapabilityReport{Perms: false},
	}); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/perms")
	if err != nil {
		t.Fatalf("GET /perms: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: the report beats the method set, or an adapter advertises every route it merely compiles against", resp.StatusCode)
	}
}

// TestPermsRead_AnswersWhenTheReportSaysWired is the other leg: same
// type, report flipped on.
func TestPermsRead_AnswersWhenTheReportSaysWired(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&permsRefusingRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		report:         CapabilityReport{Perms: true},
	}); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/perms")
	if err != nil {
		t.Fatalf("GET /perms: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got PermsInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Mode != "yolo" {
		t.Errorf("mode = %q, want the provider's answer", got.Mode)
	}
}

// TestPermsRead_AdvertisedBitMatchesTheRoute is the invariant rather
// than the cases: whatever the capabilities frame says about `perms`,
// the route must agree. Both are computed from entryFeature for
// exactly this reason — #490 was two code paths disagreeing about the
// same question, and the fix is not to answer it twice.
func TestPermsRead_AdvertisedBitMatchesTheRoute(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		make func() Registrant
	}{
		{"bare registrant", func() Registrant {
			return &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
		}},
		{"provider by interface", func() Registrant {
			return &operatorMutationsRegistrant{
				stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
				perms:          PermsInfo{Mode: "ask"},
			}
		}},
		{"reporter says off", func() Registrant {
			return &permsRefusingRegistrant{
				stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
				report:         CapabilityReport{Perms: false},
			}
		}},
		{"reporter says on", func() Registrant {
			return &permsRefusingRegistrant{
				stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
				report:         CapabilityReport{Perms: true},
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ag := tc.make()
			reg := NewSessionRegistry()
			entry, err := reg.Register(ag)
			if err != nil {
				t.Fatal(err)
			}
			base, cleanup := startTestServer(t, reg)
			defer cleanup()

			advertised := buildFeatures(entry, nil)[featurePerms]

			resp, err := http.Get(base + "/sessions/core-agent/s1/perms")
			if err != nil {
				t.Fatalf("GET /perms: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			served := resp.StatusCode == http.StatusOK
			if advertised != served {
				t.Errorf("capabilities say perms=%v but GET /perms answered %d; a client that gates its permissions view on the frame would render a dead affordance", advertised, resp.StatusCode)
			}
		})
	}
}

// TestPermsWire_FieldNames pins the JSON spelling of everything #375
// added, as literals. Same discipline as permswire_test.go and the
// same reason: a Go rename is free and a wire rename is a break in a
// repo this compiler cannot see.
func TestPermsWire_FieldNames(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(PermsInfo{
		Mode:       "ask",
		OnMutation: "require_approval",
		Allow:      []string{"a"},
		Deny:       []string{"d"},
		Approvals: []ApprovalInfo{{
			Tool:      "scale_deployment",
			Key:       "scale_deployment:api",
			Decision:  "deny",
			At:        time.Unix(0, 0).UTC(),
			Approver:  "alice@example.com",
			Refusal:   "denied_by_operator",
			ChangeSet: "cs-1",
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"mode", "on_mutation", "allow", "deny", "approvals"} {
		if _, ok := got[k]; !ok {
			t.Errorf("PermsInfo is missing wire key %q; got %s", k, b)
		}
	}
	row, ok := got["approvals"].([]any)
	if !ok || len(row) != 1 {
		t.Fatalf("approvals did not marshal as a one-element array: %s", b)
	}
	first, _ := row[0].(map[string]any)
	for _, k := range []string{"tool", "key", "decision", "at", "approver", "refusal", "change_set"} {
		if _, ok := first[k]; !ok {
			t.Errorf("ApprovalInfo is missing wire key %q; got %s", k, b)
		}
	}
}

// TestPermsWire_ModeIsOmittedNotEmptied: a daemon with no permissions
// gate must leave the key out. Emitting `"mode":""` would hand a
// client a value it has to special-case, and the whole point of this
// change is that an absent answer is distinguishable from a permissive
// one.
func TestPermsWire_ModeIsOmittedNotEmptied(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(PermsInfo{OnMutation: "apply"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["mode"]; ok {
		t.Errorf("mode is present on a gate-less PermsInfo: %s", b)
	}
	if got["on_mutation"] != "apply" {
		t.Errorf("on_mutation = %v, want it carried on its own axis independent of mode", got["on_mutation"])
	}
}
