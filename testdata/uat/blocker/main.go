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
// It exposes two tools whose effect classes match the fixture's
// tool_catalog policies: read_status (read-only) and apply_change
// (mutating). A tool call blocks until the harness releases it, so the
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
	for _, name := range []string{"read_status", "apply_change"} {
		tool := name // capture
		mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: tool, Description: "UAT controllable blocking tool"},
			func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, struct{}, error) {
				if err := block(ctx, dir, tool); err != nil {
					return nil, struct{}{}, err
				}
				return &mcpsdk.CallToolResult{
					Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: tool + ": ok"}},
				}, struct{}{}, nil
			})
	}

	if err := srv.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil {
		os.Exit(1)
	}
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
func block(ctx context.Context, dir, tool string) error {
	started := filepath.Join(dir, tool+".started")
	release := filepath.Join(dir, tool+".release")

	// Announce dispatch. Best-effort: a failed marker write must not wedge
	// the tool (the harness would time out its poll and fail loudly).
	if f, err := os.Create(started); err == nil {
		_ = f.Close()
	} else {
		fmt.Fprintf(os.Stderr, "blocker: %s: cannot write started marker: %v\n", tool, err)
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
