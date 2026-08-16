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
)

// TestSessionsAckEffects_DirectPathWarnsNoDaemon covers gate finding N4:
// the direct `--session-db` ack path cannot serialize its watermark write
// against a live daemon (mast has no on-disk liveness signal to probe), so
// it must warn the operator before writing. Neutralize the warning line
// and this test fails.
func TestSessionsAckEffects_DirectPathWarnsNoDaemon(t *testing.T) {
	db := filepath.Join(t.TempDir(), "sessions.db")
	// Seed a durable one-shot task session so the DB file exists with a
	// real (non-reserved) session id to ack.
	if err := runOneShot(context.Background(), discardLogger(), oneShotOptions{
		Class:      "debug",
		Model:      "echo",
		SessionDB:  db,
		SessionDrv: "sqlite",
		Prompt:     "seed",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("seed runOneShot: %v", err)
	}

	cmd, err := parseSessionsArgs([]string{
		"ack-effects", oneShotSessionID("debug"),
		"--session-db=" + db, "--reason=checked",
	})
	if err != nil {
		t.Fatalf("parseSessionsArgs: %v", err)
	}
	var out bytes.Buffer
	if err := cmd.run(context.Background(), &out); err != nil {
		t.Fatalf("ack-effects --session-db run: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "NOT serialized") {
		t.Errorf("direct ack output missing the no-daemon warning (gate finding N4):\n%s", got)
	}
	if !strings.Contains(got, "acknowledged") {
		t.Errorf("direct ack output missing the confirmation line:\n%s", got)
	}
}

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
				addr: "http://127.0.0.1:7777", app: appName,
			},
		},
		{
			name: "resume by token",
			args: []string{"resume", "--token=mrt_abc"},
			want: sessionsCmd{
				verb: "resume", token: "mrt_abc",
				addr: "http://127.0.0.1:7777", app: appName,
			},
		},
		{
			name:    "resume token excludes interrupt keying",
			args:    []string{"resume", "incident-123", "--token=mrt_abc"},
			wantErr: "drop <session-id>",
		},
		{
			name:    "resume direct mode is token only",
			args:    []string{"resume", "incident-123", "--interrupt=approve-x", `--response={}`, "--session-db=/tmp/x.db"},
			wantErr: "--token only",
		},
		{
			name: "pause",
			args: []string{"pause", "incident-123", "--reason=maintenance_window", "--message=deploy", "--interrupt", "--ttl=48h"},
			want: sessionsCmd{
				verb: "pause", sessionID: "incident-123",
				reason: "maintenance_window", message: "deploy",
				hardPause: true, ttl: "48h",
				addr: "http://127.0.0.1:7777",
			},
		},
		{
			name:    "pause requires reason",
			args:    []string{"pause", "incident-123"},
			wantErr: "--reason is required",
		},
		{
			name:    "pause rejects both resume-at and resume-after",
			args:    []string{"pause", "incident-123", "--reason=operator", "--resume-at=2026-08-03T00:00:00Z", "--resume-after=15m"},
			wantErr: "mutually exclusive",
		},
		{
			name: "extend-token",
			args: []string{"extend-token", "mrt_abc", "--ttl=168h"},
			want: sessionsCmd{
				verb: "extend-token", token: "mrt_abc", ttl: "168h",
				addr: "http://127.0.0.1:7777",
			},
		},
		{
			name:    "extend-token requires ttl",
			args:    []string{"extend-token", "mrt_abc"},
			wantErr: "--ttl is required",
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
			// The default is redacted, and it is the default because it
			// is what an operator gets without thinking about it.
			name: "export-decisions defaults to a redacted whole-store export",
			args: []string{"export-decisions", "--session-db=/tmp/s.db"},
			want: sessionsCmd{verb: "export-decisions", db: "/tmp/s.db", app: appName},
		},
		{
			name: "export-decisions scoped",
			args: []string{
				"export-decisions", "incident-123", "--session-db=/tmp/s.db",
				"--workload=gke-triage", "--since=2026-08-01T00:00:00Z",
				"--until=2026-09-01T00:00:00Z", "--include-approver", "--out=/tmp/d.jsonl",
			},
			want: sessionsCmd{
				verb: "export-decisions", sessionID: "incident-123", db: "/tmp/s.db", app: appName,
				workload: "gke-triage", since: "2026-08-01T00:00:00Z", until: "2026-09-01T00:00:00Z",
				includeApprover: true, out: "/tmp/d.jsonl",
			},
		},
		{
			name:    "export-decisions requires session-db",
			args:    []string{"export-decisions"},
			wantErr: "--session-db is required",
		},
		{
			// Refused at parse time rather than silently ignored: a
			// window nobody applied would produce an export that quietly
			// covers more than the operator asked for.
			name:    "export-decisions rejects an unparseable window",
			args:    []string{"export-decisions", "--session-db=/tmp/s.db", "--since=yesterday"},
			wantErr: "--since \"yesterday\" is not an RFC3339 time",
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
