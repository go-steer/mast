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

// Command blocker is a controllable stdio MCP server used by the v0.2
// end-to-end UAT harness (scripts/uat-v0.2.sh) to supply a REGISTERED,
// BLOCKING tool — the prerequisite the deferred crash/drain/abort legs
// were waiting on (docs/uat-v0.2-plan.md "Blocking-tool prerequisite").
//
// It exposes three tools. Two have effect classes matching the
// fixture's tool_catalog policies — read_status (read-only) and
// apply_change (mutating) — and the third, findings_diff, answers in
// the TEXT record contract a run-to-run classifier uses, which is what
// the v0.5 monitoring legs (scripts/uat-v0.5.sh) read. A tool call
// blocks until the harness releases it, so the
// harness can hold a turn open across a kill -9 / SIGTERM drain / abort
// and drive deterministic timing. All coordination is via files in the
// directory named by UAT_BLOCKER_DIR:
//
//   - On entry the handler creates "<tool>.started" so the harness can
//     poll for "the tool has dispatched and its intent is persisted"
//     before it kills/aborts (a deterministic crash window, never a sleep).
//   - The handler then blocks until "<tool>.release" appears, polling on a
//     short ticker and honoring context cancellation (an aborted or
//     drain-cancelled turn cancels the MCP call ctx → the handler returns
//     ctx.Err() promptly rather than hanging).
//   - If "<tool>.release" already exists on entry, the handler returns
//     immediately — used for the "turn completes" legs where no blocking
//     is wanted.
//   - Every entry appends a line to "<tool>.calls" naming the arguments
//     the handler was actually called with. The write gate's edit legs
//     (scripts/uat-v0.3.sh) turn on that line: an operator's edit is only
//     observable end to end if the tool records which arguments ran, and
//     apply_change declares one (replicas) precisely so it can.
//   - read_status reports the contents of "state" (default "steady"), so
//     a leg can move the world between an operator's approval and the
//     calls it authorized. The change-set freshness legs
//     (scripts/uat-v0.4.sh) turn on that: a grant is re-checked against
//     this read, and a changed answer voids it.
//   - findings_diff reports the contents of "findings_diff.out" verbatim
//     as text, so a leg can hand the daemon any classification a real
//     classifier might produce — including a malformed one.
//
// A `kill -9` of the launching daemon is the one interruption that does NOT
// cancel the call ctx: the crash legs SIGKILL the daemon mid-call, which
// closes our stdin but leaves an in-flight handler busy-polling for a
// release that will never come. A parent-death watcher (exitWhenOrphaned)
// terminates the process when it is reparented away from the daemon, so the
// fixture never orphans itself even on the un-cancellable SIGKILL path.
//
// Credential-free and offline: it speaks MCP over stdio and needs no
// network. The daemon launches it lazily on first tool use per the
// fixture's mcp.json.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// pollInterval bounds how often a blocked handler re-checks for its
// release file. Small enough that the harness's bounded polls observe
// state transitions promptly; large enough to be cheap.
const pollInterval = 50 * time.Millisecond

func main() {
	dir := os.Getenv("UAT_BLOCKER_DIR")
	if dir == "" {
		fmt.Fprintln(os.Stderr, "blocker: UAT_BLOCKER_DIR is required")
		os.Exit(2)
	}

	// Never outlive the launching daemon. A `kill -9` closes our stdin but
	// the MCP stdio server does not unblock an in-flight handler on EOF, so a
	// blocked call would busy-poll forever; the watcher exits on reparenting.
	go exitWhenOrphaned(os.Getppid())

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "uat-blocker", Version: "0.0.1"}, nil)

	// read_status takes no arguments: nothing about the read-only legs
	// depends on what it was asked for. What it RETURNS is
	// harness-controlled (see clusterState), because the change-set
	// freshness legs (scripts/uat-v0.4.sh) need a world that can move
	// between an operator's approval and the calls it authorized.
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "read_status", Description: "UAT controllable blocking tool (read-only)"},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, statusOut, error) {
			state := clusterState(dir)
			if err := block(ctx, dir, "read_status", "state="+state); err != nil {
				return nil, statusOut{}, err
			}
			return textResult("read_status: ok state=" + state), statusOut{State: state}, nil
		})

	// apply_change declares one argument so the mutating legs can tell
	// WHICH call ran, not merely that one did. The write gate's edit
	// verdict substitutes the operator's arguments for the model's, and
	// the only end-to-end evidence that the substitution took effect is
	// the value this handler receives.
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "apply_change", Description: "UAT controllable blocking tool (mutating)"},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in changeArgs) (*mcpsdk.CallToolResult, struct{}, error) {
			if err := block(ctx, dir, "apply_change", fmt.Sprintf("replicas=%d", in.Replicas)); err != nil {
				return nil, struct{}{}, err
			}
			return okResult("apply_change"), struct{}{}, nil
		})

	// findings_diff stands in for a run-to-run classifier — k8s_lookout's
	// `k8s_findings_diff` is the real one (v0.5 W4.4). Two things about
	// it are deliberate and neither is incidental to what the legs test:
	//
	// REGISTERED LOW-LEVEL, not through the generic mcpsdk.AddTool. The
	// generic form always sends structured content, even for an empty
	// output struct; the real classifier sends TEXT, one logfmt record
	// per line ending in a summary. mast's parser is written against
	// that, so a fixture that answered in structured JSON would exercise
	// a path production never takes.
	//
	// The records come from "<dir>/findings_diff.out", so a leg can hand
	// the daemon whatever a classifier might say — including the
	// escalation whose severities did not move, which is the leg that
	// fails if mast ever starts checking the classification instead of
	// carrying it.
	srv.AddTool(&mcpsdk.Tool{
		Name:        "findings_diff",
		Description: "UAT lookout-shaped classifier: text records, run-to-run transitions",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"transitions": map[string]any{
					"type":        "string",
					"description": "comma-separated transition classes to report",
				},
			},
		},
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		detail := ""
		if req != nil && len(req.Params.Arguments) > 0 {
			detail = "args=" + strings.TrimSpace(string(req.Params.Arguments))
		}
		if err := block(ctx, dir, "findings_diff", detail); err != nil {
			return nil, err
		}
		return textResult(findingsDiff(dir)), nil
	})

	if err := srv.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil {
		os.Exit(1)
	}
}

// defaultFindingsDiff is what findings_diff answers when the harness has
// written no canned output: one quiet cycle, correctly terminated. A
// fixture whose default was empty would make "the summary line is
// mandatory" untestable, because every leg would have to write a file
// before it could see the normal case.
const defaultFindingsDiff = "scanned=1 findings=0 elapsed=1ms\n"

// findingsDiff reads the canned classifier output for this leg.
func findingsDiff(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "findings_diff.out")) // #nosec G304 -- harness fixture, path from the harness's own env
	if err != nil {
		return defaultFindingsDiff
	}
	return string(b)
}

// changeArgs is apply_change's declared input. Required (no omitempty),
// so a call that omits it is refused by the MCP server's own schema
// check — which is what makes "the edit that violates the schema never
// reached the tool" a claim the harness can test.
type changeArgs struct {
	Replicas int `json:"replicas" jsonschema:"the replica count to scale the workload to"`
}

// statusOut is read_status's STRUCTURED result, which is the half a
// precondition can be declared against: ADK's MCP tool returns
// {"output": <structured content>} when a server sends one and falls
// back to the text otherwise, so a fixture whose state lived only in
// the text would compare equal on every read and no freshness check
// could ever fail.
type statusOut struct {
	State string `json:"state" jsonschema:"the fixture's current cluster state"`
}

func okResult(tool string) *mcpsdk.CallToolResult { return textResult(tool + ": ok") }

func textResult(text string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
	}
}

// clusterState is the fixture's stand-in for the piece of the world a
// change-set precondition watches: the contents of "<dir>/state", or
// "steady" when the harness has not written one.
//
// It exists so a leg can move the world between an operator's approval
// and the calls that approval authorized — the case a wall-clock TTL
// cannot catch, and the reason a grant is re-checked against a read
// rather than against a timer alone. Writing the file is the whole
// mechanism: the next read returns different bytes, the digest differs,
// and the remaining calls park again.
func clusterState(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "state")) // #nosec G304 -- harness fixture, path from the harness's own env
	if err != nil {
		return "steady"
	}
	return strings.TrimSpace(string(b))
}

// exitWhenOrphaned terminates the process once its parent pid changes from
// orig — the signal that the launching daemon died and this stdio server was
// reparented to init (or a subreaper). It is the backstop for the SIGKILL
// crash legs, where no ctx cancellation reaches an in-flight handler. Polls
// on the same cheap ticker as block(); never returns.
func exitWhenOrphaned(orig int) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for range t.C {
		if os.Getppid() != orig {
			os.Exit(0)
		}
	}
}

// block signals dispatch (writes "<tool>.started") and then waits for
// "<tool>.release", honoring context cancellation. Returns ctx.Err() if
// the call is cancelled before release — the observable an aborted or
// drain-cancelled turn produces.
//
// detail is the rendering of the arguments this entry was called with,
// appended to the call ledger; empty for a tool that takes none.
func block(ctx context.Context, dir, tool, detail string) error {
	started := filepath.Join(dir, tool+".started")
	release := filepath.Join(dir, tool+".release")

	// Announce dispatch. Best-effort: a failed marker write must not wedge
	// the tool (the harness would time out its poll and fail loudly).
	if f, err := os.Create(started); err == nil {
		_ = f.Close()
	} else {
		fmt.Fprintf(os.Stderr, "blocker: %s: cannot write started marker: %v\n", tool, err)
	}

	// Append one line per ENTRY to "<tool>.calls". The started marker is
	// truncating and so only answers "did it run at all"; the write gate's
	// legs (scripts/uat-v0.3.sh) need "how many times", because approving
	// a parked call twice, or re-running it on a resume, is the failure
	// mode they exist to catch — and, for the edit legs, "with what", so
	// the line carries the arguments too. Also best-effort, for the same
	// reason.
	line := tool
	if detail != "" {
		line += " " + detail
	}
	if f, err := os.OpenFile(filepath.Join(dir, tool+".calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_, _ = fmt.Fprintln(f, line)
		_ = f.Close()
	} else {
		fmt.Fprintf(os.Stderr, "blocker: %s: cannot append call marker: %v\n", tool, err)
	}

	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		if _, err := os.Stat(release); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
