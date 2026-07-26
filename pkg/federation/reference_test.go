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

package federation

import (
	"errors"
	"testing"
)

func TestParseReference(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    Reference
		wantErr bool
	}{
		{
			name: "a2a name plus skill path (a2a-design grammar)",
			raw:  "a2a://external-triage/investigate-incident",
			want: Reference{Scheme: "a2a", Name: "external-triage", Skill: "investigate-incident"},
		},
		{
			name: "a2a name only",
			raw:  "a2a://external-triage",
			want: Reference{Scheme: "a2a", Name: "external-triage"},
		},
		{
			name: "skill via query (federation-design grammar)",
			raw:  "a2a://external-triage?skill=investigate-incident",
			want: Reference{Scheme: "a2a", Name: "external-triage", Skill: "investigate-incident"},
		},
		{
			name: "trailing slash tolerated",
			raw:  "a2a://external-triage/",
			want: Reference{Scheme: "a2a", Name: "external-triage"},
		},
		{
			name: "scheme and name normalize to lowercase (RFC 3986)",
			raw:  "A2A://External-Triage/Investigate",
			want: Reference{Scheme: "a2a", Name: "external-triage", Skill: "Investigate"},
		},
		{
			name: "future mast scheme parses (adapter resolution is separate)",
			raw:  "mast://cluster-2/incident-triage",
			want: Reference{Scheme: "mast", Name: "cluster-2", Skill: "incident-triage"},
		},
		{name: "skill in both path and query is a conflict", raw: "a2a://x/a?skill=b", wantErr: true},
		{name: "multi-segment skill rejected", raw: "a2a://x/a/b", wantErr: true},
		{name: "missing scheme", raw: "external-triage/skill", wantErr: true},
		{name: "missing name", raw: "a2a:///skill", wantErr: true},
		{name: "userinfo selector rejected in v0.1", raw: "mast://worker@fleet/x", wantErr: true},
		{name: "port rejected in v0.1", raw: "a2a://host:9000/x", wantErr: true},
		{name: "empty string", raw: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseReference(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseReference(%q) = %+v, want error", tc.raw, got)
				}
				if !errors.Is(err, ErrInvalidReference) {
					t.Fatalf("ParseReference(%q) error %v does not wrap ErrInvalidReference", tc.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseReference(%q): %v", tc.raw, err)
			}
			if got.Scheme != tc.want.Scheme || got.Name != tc.want.Name || got.Skill != tc.want.Skill {
				t.Fatalf("ParseReference(%q) = {Scheme:%q Name:%q Skill:%q}, want {Scheme:%q Name:%q Skill:%q}",
					tc.raw, got.Scheme, got.Name, got.Skill, tc.want.Scheme, tc.want.Name, tc.want.Skill)
			}
			if got.Raw != tc.raw {
				t.Errorf("Raw = %q, want %q", got.Raw, tc.raw)
			}
		})
	}
}

func TestReferenceString(t *testing.T) {
	if got := (Reference{Scheme: "a2a", Name: "x", Skill: "s"}).String(); got != "a2a://x/s" {
		t.Errorf("String() = %q, want a2a://x/s", got)
	}
	if got := (Reference{Scheme: "a2a", Name: "x"}).String(); got != "a2a://x" {
		t.Errorf("String() = %q, want a2a://x", got)
	}
}
