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

// mast sessions — the v0.1 operator-facing session surface
// (docs/durable-execution-design.md, "Operator-facing surface").
//
//	mast sessions list   --session-db=... [--user=...] [--state=paused|aborted|interrupted|idle]
//	mast sessions show   <session-id> --session-db=...
//	mast sessions resume <session-id> --interrupt=<iid> --response='{"approved":true}' [--ack-effects] [--addr=...]
//	mast sessions abort  <session-id> [--reason=...] [--addr=...]
//	mast sessions ack-effects <session-id> [--reason=...] [--addr=... | --session-db=...]
//
// list/show read the SQLite session DB directly (works with or without
// a running daemon). resume/abort go through a running daemon's
// /resume and /abort endpoints — resume must be executed by the runner
// that owns the workflow, and routing abort through the daemon keeps a
// single SQLite writer.
//
// Design deviation, recorded: durable-execution-design sketches
// `resume --token=<token>`. Resume tokens are the v0.2 programmatic-
// pause surface; the v0.1 pause is a HITL RequestInput, whose verified
// resume contract (docs/spike-findings.md, Q2) is keyed by interrupt
// ID. Hence `--interrupt` + `--response` here.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/transcript"
)

// sessionsCmd is the parsed form of a `mast sessions ...` invocation.
// Parsing is separated from execution so it can be unit-tested.
type sessionsCmd struct {
	verb      string // list | show | resume | abort
	sessionID string

	// list/show (read the DB directly)
	db    string
	app   string
	user  string
	state string // list filter; empty = all

	// resume/abort (POST to a running daemon)
	addr        string
	interruptID string
	response    string // raw JSON
	reason      string
	ackEffects  bool
}

const sessionsUsage = `usage: mast sessions <command> [flags]

commands:
  list                    list sessions (reads --session-db directly)
  show   <session-id>     session detail incl. pending interrupts
  resume <session-id>     resume a paused session via a running daemon
  abort  <session-id>     mark a session aborted via a running daemon
  ack-effects <session-id> acknowledge ambiguous prior effects (lifts the
                          recorded-effect outbox's refusal); via a running
                          daemon by default, or --session-db when none serves
                          this DB (one-shot sessions, stopped daemon)

run 'mast sessions <command> -h' for the command's flags`

// parseSessionsArgs parses the argument vector after "sessions".
func parseSessionsArgs(args []string) (*sessionsCmd, error) {
	if len(args) == 0 {
		return nil, errors.New(sessionsUsage)
	}
	cmd := &sessionsCmd{verb: args[0]}
	rest := args[1:]

	fs := flag.NewFlagSet("mast sessions "+cmd.verb, flag.ContinueOnError)
	switch cmd.verb {
	case "list":
		fs.StringVar(&cmd.db, "session-db", "", "path to the SQLite session DB (required)")
		fs.StringVar(&cmd.app, "app", appName, "app name the sessions were stored under")
		fs.StringVar(&cmd.user, "user", "", "filter to one user ID (empty = all)")
		fs.StringVar(&cmd.state, "state", "", "filter by state: paused|aborted|interrupted|idle (empty = all)")
	case "show":
		fs.StringVar(&cmd.db, "session-db", "", "path to the SQLite session DB (required)")
		fs.StringVar(&cmd.app, "app", appName, "app name the sessions were stored under")
		fs.StringVar(&cmd.user, "user", "", "user ID owning the session (empty = auto-discover)")
	case "resume":
		fs.StringVar(&cmd.addr, "addr", "http://127.0.0.1:7777", "base URL of the running mast daemon")
		fs.StringVar(&cmd.interruptID, "interrupt", "", "pending interrupt ID to resume (required; see `mast sessions show`)")
		fs.StringVar(&cmd.response, "response", "", "JSON response payload for the interrupt (required)")
		fs.BoolVar(&cmd.ackEffects, "ack-effects", false, "acknowledge ambiguous prior effects: assert you checked whether the interrupted turn's mutating tool calls took effect; lifts the recorded-effect outbox's refusal for calls recorded so far")
	case "abort":
		fs.StringVar(&cmd.addr, "addr", "http://127.0.0.1:7777", "base URL of the running mast daemon")
		fs.StringVar(&cmd.reason, "reason", "operator abort", "reason recorded in the abort marker")
	case "ack-effects":
		fs.StringVar(&cmd.addr, "addr", "http://127.0.0.1:7777", "base URL of the running mast daemon")
		fs.StringVar(&cmd.db, "session-db", "", "write the acknowledgement directly to this SQLite session DB instead of going through a daemon — ONLY when no daemon is serving this DB (the daemon path serializes against in-flight turns; the direct path cannot)")
		fs.StringVar(&cmd.app, "app", appName, "app name the sessions were stored under (direct --session-db path only)")
		fs.StringVar(&cmd.reason, "reason", "operator ack", "note recorded in the acknowledgement marker")
	default:
		return nil, fmt.Errorf("unknown sessions command %q\n%s", cmd.verb, sessionsUsage)
	}

	// Accept the session ID either before the flags (documented shape:
	// `mast sessions resume <id> --interrupt=...`) or after them.
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		cmd.sessionID = rest[0]
		rest = rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return nil, err
	}
	if cmd.sessionID == "" && fs.NArg() > 0 {
		cmd.sessionID = fs.Arg(0)
	}

	switch cmd.verb {
	case "list":
		if cmd.db == "" {
			return nil, errors.New("mast sessions list: --session-db is required")
		}
		switch cmd.state {
		case "", transcript.StatePaused, transcript.StateAborted, transcript.StateInterrupted, transcript.StateIdle:
		default:
			return nil, fmt.Errorf("mast sessions list: unknown --state %q (want paused|aborted|interrupted|idle)", cmd.state)
		}
	case "show":
		if cmd.sessionID == "" {
			return nil, errors.New("mast sessions show: <session-id> is required")
		}
		if cmd.db == "" {
			return nil, errors.New("mast sessions show: --session-db is required")
		}
	case "resume":
		if cmd.sessionID == "" {
			return nil, errors.New("mast sessions resume: <session-id> is required")
		}
		if cmd.interruptID == "" {
			return nil, errors.New("mast sessions resume: --interrupt is required")
		}
		if cmd.response == "" {
			return nil, errors.New("mast sessions resume: --response is required")
		}
		if !json.Valid([]byte(cmd.response)) {
			return nil, fmt.Errorf("mast sessions resume: --response is not valid JSON: %q", cmd.response)
		}
	case "abort":
		if cmd.sessionID == "" {
			return nil, errors.New("mast sessions abort: <session-id> is required")
		}
	case "ack-effects":
		if cmd.sessionID == "" {
			return nil, errors.New("mast sessions ack-effects: <session-id> is required")
		}
	}
	return cmd, nil
}

// runSessions executes a `mast sessions ...` invocation and returns
// the process exit code.
func runSessions(args []string) int {
	cmd, err := parseSessionsArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := cmd.run(ctx, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "mast sessions %s: %v\n", cmd.verb, err)
		return 1
	}
	return 0
}

func (c *sessionsCmd) run(ctx context.Context, out io.Writer) error {
	switch c.verb {
	case "list":
		return c.runList(ctx, out)
	case "show":
		return c.runShow(ctx, out)
	case "resume":
		var response any
		if err := json.Unmarshal([]byte(c.response), &response); err != nil {
			return fmt.Errorf("parse --response: %w", err)
		}
		return c.post(ctx, out, "/resume", inject.ResumeRequest{
			SessionID:   c.sessionID,
			InterruptID: c.interruptID,
			Response:    response,
			AckEffects:  c.ackEffects,
		})
	case "abort":
		return c.post(ctx, out, "/abort", inject.AbortRequest{
			SessionID: c.sessionID,
			Reason:    c.reason,
		})
	case "ack-effects":
		if c.db != "" {
			// Direct path for DBs no daemon is serving (one-shot task
			// sessions, a stopped daemon). Against a LIVE daemon's DB,
			// use the default daemon path instead — it serializes the
			// watermark against in-flight turns; this one cannot.
			store, err := transcript.Open(c.db, c.app)
			if err != nil {
				return err
			}
			if err := store.AckEffects(ctx, c.user, c.sessionID, c.reason); err != nil {
				return err
			}
			fmt.Fprintln(out, "acknowledged")
			return nil
		}
		return c.post(ctx, out, "/ack-effects", inject.AckEffectsRequest{
			SessionID: c.sessionID,
			Reason:    c.reason,
		})
	default:
		return fmt.Errorf("unknown sessions command %q", c.verb)
	}
}

func (c *sessionsCmd) runList(ctx context.Context, out io.Writer) error {
	store, err := transcript.Open(c.db, c.app)
	if err != nil {
		return err
	}
	summaries, err := store.List(ctx, c.user)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tUSER\tAPP\tLAST EVENT\tSTATE\tPENDING")
	shown := 0
	for _, s := range summaries {
		if c.state != "" && s.State != c.state {
			continue
		}
		pending := strings.Join(s.PendingInterruptIDs, ",")
		if s.State == transcript.StateAborted {
			pending = "(aborted: " + s.AbortReason + ")"
		}
		if s.State == transcript.StateInterrupted {
			pending = "(interrupted: " + s.InterruptReason + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.ID, s.UserID, s.AppName,
			s.LastEventTime.UTC().Format(time.RFC3339), s.State, pending)
		shown++
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if shown == 0 {
		fmt.Fprintln(out, "(no sessions)")
	}
	return nil
}

func (c *sessionsCmd) runShow(ctx context.Context, out io.Writer) error {
	store, err := transcript.Open(c.db, c.app)
	if err != nil {
		return err
	}
	d, err := store.Get(ctx, c.user, c.sessionID)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Session:    %s\n", d.ID)
	fmt.Fprintf(out, "App:        %s\n", d.AppName)
	fmt.Fprintf(out, "User:       %s\n", d.UserID)
	fmt.Fprintf(out, "State:      %s\n", d.State)
	fmt.Fprintf(out, "Last event: %s\n", d.LastEventTime.UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "Events:     %d\n", d.EventCount)
	if d.State == transcript.StateAborted {
		fmt.Fprintf(out, "Aborted:    %s\n", d.AbortReason)
	}
	if d.State == transcript.StateInterrupted {
		fmt.Fprintf(out, "Interrupted: %s\n", d.InterruptReason)
	}
	for _, p := range d.Pending {
		fmt.Fprintf(out, "\nPending input:\n")
		fmt.Fprintf(out, "  Interrupt: %s\n", p.InterruptID)
		fmt.Fprintf(out, "  Author:    %s\n", p.Author)
		fmt.Fprintf(out, "  Raised:    %s\n", p.RaisedAt.UTC().Format(time.RFC3339))
		fmt.Fprintf(out, "  Message:   %s\n", p.Message)
		if p.ResponseSchema != nil {
			schema, err := json.Marshal(p.ResponseSchema)
			if err != nil {
				return fmt.Errorf("marshal response schema: %w", err)
			}
			fmt.Fprintf(out, "  Schema:    %s\n", schema)
		}
		fmt.Fprintf(out, "\nResume with:\n  mast sessions resume %s --interrupt=%s --response='<json>'\n",
			d.ID, p.InterruptID)
	}
	return nil
}

// post sends a JSON body to the daemon at addr+path, authenticating
// with MAST_INJECT_TOKEN when set (the same bearer the daemon checks).
func (c *sessionsCmd) post(ctx context.Context, out io.Writer, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	url := strings.TrimSuffix(c.addr, "/") + path
	// #nosec G704 -- url derives from the operator's own --addr flag;
	// posting to the operator-chosen daemon address is the feature.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token := os.Getenv("MAST_INJECT_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- see above; operator-chosen --addr
	if err != nil {
		return fmt.Errorf("POST %s: %w (is the daemon running?)", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("POST %s: %s: %s", url, resp.Status, strings.TrimSpace(string(respBody)))
	}
	fmt.Fprintf(out, "%s %s: %s\n", path, c.sessionID, strings.TrimSpace(string(respBody)))
	return nil
}
