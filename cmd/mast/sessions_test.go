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
	"strings"
	"testing"
)

func TestParseSessionsArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    sessionsCmd
		wantErr string
	}{
		{
			name: "list",
			args: []string{"list", "--session-db=/tmp/s.db"},
			want: sessionsCmd{verb: "list", db: "/tmp/s.db", app: appName},
		},
		{
			name: "list with filters",
			args: []string{"list", "--session-db=/tmp/s.db", "--state=paused", "--user=op", "--app=other"},
			want: sessionsCmd{verb: "list", db: "/tmp/s.db", app: "other", user: "op", state: "paused"},
		},
		{
			name:    "list requires session-db",
			args:    []string{"list"},
			wantErr: "--session-db is required",
		},
		{
			name:    "list rejects unknown state",
			args:    []string{"list", "--session-db=/tmp/s.db", "--state=running"},
			wantErr: "unknown --state",
		},
		{
			name: "show id before flags",
			args: []string{"show", "incident-123", "--session-db=/tmp/s.db"},
			want: sessionsCmd{verb: "show", sessionID: "incident-123", db: "/tmp/s.db", app: appName},
		},
		{
			name: "show id after flags",
			args: []string{"show", "--session-db=/tmp/s.db", "incident-123"},
			want: sessionsCmd{verb: "show", sessionID: "incident-123", db: "/tmp/s.db", app: appName},
		},
		{
			name:    "show requires id",
			args:    []string{"show", "--session-db=/tmp/s.db"},
			wantErr: "<session-id> is required",
		},
		{
			name: "resume",
			args: []string{"resume", "incident-123", "--interrupt=approve-x", `--response={"approved":true}`},
			want: sessionsCmd{
				verb: "resume", sessionID: "incident-123",
				interruptID: "approve-x", response: `{"approved":true}`,
				addr: "http://127.0.0.1:7777",
			},
		},
		{
			name:    "resume requires interrupt",
			args:    []string{"resume", "incident-123", `--response={"approved":true}`},
			wantErr: "--interrupt is required",
		},
		{
			name:    "resume requires response",
			args:    []string{"resume", "incident-123", "--interrupt=approve-x"},
			wantErr: "--response is required",
		},
		{
			name:    "resume rejects invalid response JSON",
			args:    []string{"resume", "incident-123", "--interrupt=approve-x", "--response={nope"},
			wantErr: "not valid JSON",
		},
		{
			name: "abort",
			args: []string{"abort", "incident-123", "--reason=operator cancelled", "--addr=http://mast:9999"},
			want: sessionsCmd{
				verb: "abort", sessionID: "incident-123",
				reason: "operator cancelled", addr: "http://mast:9999",
			},
		},
		{
			name: "abort default reason",
			args: []string{"abort", "incident-123"},
			want: sessionsCmd{
				verb: "abort", sessionID: "incident-123",
				reason: "operator abort", addr: "http://127.0.0.1:7777",
			},
		},
		{
			name:    "no command",
			args:    nil,
			wantErr: "usage: mast sessions",
		},
		{
			name:    "unknown command",
			args:    []string{"destroy"},
			wantErr: `unknown sessions command "destroy"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSessionsArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parseSessionsArgs(%v) = %+v, want error containing %q", tt.args, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSessionsArgs(%v): %v", tt.args, err)
			}
			if *got != tt.want {
				t.Errorf("parseSessionsArgs(%v)\n got %+v\nwant %+v", tt.args, *got, tt.want)
			}
		})
	}
}
