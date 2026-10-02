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

package cli

import (
	"flag"
	"strings"
	"time"
)

// run parses the flag surface shared by serve and one-shot modes and
// dispatches: a positional prompt runs one turn to completion and
// prints the result (oneshot.go); no prompt serves the daemon —
// exactly the pre-one-shot behavior, so scripts/demo-spike2.sh's
// flag-only invocations pass unchanged.
// runFlags holds the serve/one-shot flag surface. Registering it
// through a function against a caller-supplied FlagSet — rather than
// inline in run() — is what lets cli_surface_test.go enumerate the
// surface without running the binary. These names are a frozen
// contract (DESIGN.md, "The v1.0 stability promise") and nothing else
// in the tree fails if one is renamed.
type runFlags struct {
	workload         *string
	dispatch         *string
	model            *string
	provider         *string
	task             *string
	listen           *string
	attachListen     *string
	a2aListen        *string
	aguiListen       *string
	notifyURL        *string
	parkNotify       *string
	sessionDB        *string
	sessionDrv       *string
	timeout          *time.Duration
	logLevel         *string
	autoResume       *bool
	autoResumeWindow *time.Duration
	watchdog         *string
	mcpDigest        *bool
	version          *bool
}

// serve's parameters are grouped by concern rather than passed flat
// (#293). The flat list reached sixteen parameters, eleven of them
// consecutive strings, which made a transposed pair at the call site
// compile and be wrong at runtime — swapping --attach-listen and
// --a2a-listen would have bound each server to the other's address with
// no error anywhere. A struct literal names every field at the call
// site, so the same mistake has to be written down to be made.
//
// One struct per concern, deliberately not one struct for all of serve:
// a single settings blob would only relocate the problem, since nothing
// about it says which fields belong together. For the same reason
// --watchdog and --mcp-digest stay positional below: they configure
// unrelated subsystems (safety posture, MCP response digesting), they
// are not the same type, and inventing a group for them to sit in would
// be grouping for the metric rather than for the reader.
//
// core-agent's cmd/core-agent run() has the same shape at a larger
// scale and already applies this convention unevenly (agentCardOpts,
// attachOpts, checkpointOpts and friends alongside ~30 flat
// parameters). The convention is shared; the structs deliberately are
// not — the two binaries' overlapping concerns already carry different
// representations (core-agent takes a --session-db bool plus a path,
// mast takes a path plus a driver; core-agent's digest knob is negated
// and mast's is not), so a shared type would have to be wrong in one
// repo to be right in the other. See docs/sibling-sync.md.

// workloadOpts is what to run and in what shape. The two travel
// together because dispatch is resolved against the workload's own
// bundle when the flag is empty (resolveDispatch).
type workloadOpts struct {
	arg      string // --workload: a discovery name or a directory path
	dispatch string // --dispatch: empty means read it off the bundle
}

// modelOpts is the resolved model selection. Both fields have already
// been through resolveModelSelection by the time serve sees them.
type modelOpts struct {
	provider string // --provider alias, possibly empty
	name     string // --model, always populated
}

// listenOpts is every network address the daemon is configured with.
// Four inbound binds and, deliberately in the same struct, the one
// outbound URL: it is a string that used to sit next to the four in the
// flat list, which is exactly the adjacency that made a transposition
// silent.
type listenOpts struct {
	inject string // --listen: the inject endpoint, the only one on by default
	attach string // --attach-listen: TCP address or unix: path, empty disables
	a2a    string // --a2a-listen: empty disables
	agui   string // --agui-listen: empty disables
	notify string // --notify-url: outbound, where a monitoring cycle posts
	// parkNotify is --park-notify: outbound, the conversation a durable
	// approval park announces itself to. A separate field rather than a
	// reuse of whatever the bundle's monitor block names, because the
	// destination of "mast is asking permission" must not be chosen by
	// the workload being asked about (#451).
	parkNotify string
}

// sessionOpts is where session state lives. Empty db means in-memory
// sessions and no durability.
type sessionOpts struct {
	db     string // --session-db: a SQLite path or a Postgres DSN
	driver string // --session-db-driver: sqlite or postgres
	// implied reports that db is mast's doing rather than the
	// operator's — attach mode cannot run without one, so it gets one
	// (#329). It exists so the startup line can say where the file
	// came from and how to move it: a database appearing in a home
	// directory nobody named is worth one sentence.
	implied bool
}

// resumeOpts is the boot-time auto-resume policy: whether to scan for
// sessions a prior shutdown interrupted, and how stale an interruption
// may be before it is left for an operator instead.
type resumeOpts struct {
	auto   bool          // --auto-resume
	window time.Duration // --auto-resume-window: 0 disables the freshness gate
}

// registerRunFlags declares the serve/one-shot flags on fs.
func registerRunFlags(fs *flag.FlagSet) *runFlags {
	return &runFlags{
		workload:         fs.String("workload", "", "workload to run: a name resolved via .agents/ discovery (see internal/config), or a path to a workload directory (containing workload.yaml + specialists/)"),
		dispatch:         fs.String("dispatch", "", "dispatch shape: `coordinator` (spike-1 SubAgents pattern), `graph` (workflow-graph LLM-as-router), `fanout` (concurrent read-only analysts + a _synthesis merge), `bounded` (one SingleTurn specialist, one model call, a report forced to a schema), or `auto` (read the shape off the roster; never picks `bounded`). Unset takes the workload's own `dispatch:`, then coordinator"),
		model:            fs.String("model", "echo", "model to use: `echo` (fake, for smoke), `scripted` (JSONL replay; path via MAST_SCRIPT), a Gemini model id like `gemini-2.5-flash`, or a Claude model id like `claude-sonnet-4-6`"),
		provider:         fs.String("provider", "", "model provider alias: `echo`, `scripted`, `gemini`, `vertex`, `anthropic`, or `anthropic-vertex`. Validates against --model when both are set; picks the provider's default model (the --task profile's tier via internal/taskclass) when --model is unset. The alias also picks the backend within a family: `vertex` runs gemini-* against Vertex AI (GOOGLE_CLOUD_PROJECT, ADC) without GOOGLE_GENAI_USE_VERTEXAI, and `anthropic` / `anthropic-vertex` pick first-party or Vertex for claude-*"),
		task:             fs.String("task", "", "one-shot task class: `chat`, `debug`, `implement`, `research`, `review`, or `orchestrate` (requires a positional prompt; defaults to chat when a prompt is given without --task)"),
		listen:           fs.String("listen", ":7777", "HTTP inject endpoint bind address"),
		attachListen:     fs.String("attach-listen", "", "operator attach surface bind address: a TCP address (e.g. `127.0.0.1:8484`) or a Unix socket path prefixed `unix:`; empty disables the surface. Implies a durable --session-db at ~/.mast/sessions.db when you name none (live-tail pumps from the eventlog). Non-loopback TCP binds are refused without auth — set MAST_ATTACH_TOKEN"),
		a2aListen:        fs.String("a2a-listen", "", "A2A server bind address (e.g. `127.0.0.1:7780`); empty disables the surface. Publishes an agent card and a JSON-RPC endpoint for workloads that opt in via the bundle's a2a.expose. Authenticated when MAST_A2A_TOKEN is set. Non-loopback binds are refused without auth (tasks/cancel is destructive) — set MAST_A2A_TOKEN or bind loopback"),
		aguiListen:       fs.String("agui-listen", "", "AG-UI server bind address (e.g. `127.0.0.1:7781`); empty disables the surface. Serves an HTTP+SSE run endpoint and a /agui/agents.json discovery doc for workloads that opt in via the bundle's agui.expose. Authenticated when MAST_AGUI_TOKEN is set (rate limits via MAST_AGUI_RATE/MAST_AGUI_BURST). Non-loopback binds are refused without auth (a run drives a budgeted turn) — set MAST_AGUI_TOKEN or bind loopback"),
		notifyURL:        fs.String("notify-url", "", "serve mode: switchboard's outbound message ingress (e.g. `http://switchboard:8080`), where a monitoring cycle posts what it found. Required by any workload whose bundle declares a `monitor.notify` block; the bearer comes from MAST_NOTIFY_TOKEN, which must not be one of this daemon's own inbound tokens"),
		parkNotify:       fs.String("park-notify", "", "serve mode: the `conversation` a durable approval park announces itself to, through the same ingress as --notify-url (which is then required, along with MAST_NOTIFY_TOKEN). Without it a park is discoverable only by pulling — GET /parks, the attach stream, `mast sessions show` — so an unattended workload can park at 03:00 and wait until somebody looks. Announced once per park, never repeated, capped at 3 then 1 per 5m"),
		sessionDB:        fs.String("session-db", "", "session store location: a SQLite file path (default driver) or a Postgres DSN/URL with --session-db-driver=postgres; empty = in-memory sessions (no durability), except under --attach-listen, which implies ~/.mast/sessions.db"),
		sessionDrv:       fs.String("session-db-driver", "sqlite", "session DB driver: `sqlite` (--session-db is a file path) or `postgres` (--session-db is a DSN or postgres:// URL)"),
		timeout:          fs.Duration("timeout", 5*time.Minute, "one-shot turn deadline (e.g. 2m, 90s); 0 disables. One-shot only — serve-mode ceilings come from workload budgets"),
		logLevel:         fs.String("log-level", "info", "log level: debug|info|warn|error"),
		autoResume:       fs.Bool("auto-resume", true, "serve mode: on boot, scan for sessions a prior shutdown interrupted and drive a continuation turn for each eligible one (coordinator dispatch only in v0.2). --auto-resume=false disables"),
		autoResumeWindow: fs.Duration("auto-resume-window", time.Hour, "serve mode: only auto-resume sessions interrupted within this window; older interruptions are left for an operator (0 disables the freshness gate)"),
		watchdog:         fs.String("watchdog", "", "behavioral watchdog posture, a ladder where each rung includes the one before it: `warn` (log a detected tool loop and let the turn run), `feedback` (also tell the model, on its next turn, what it is doing), or `enforce` (also cancel the turn in flight on a Critical alert and refuse the session's next turn until POST /sessions/{id}/guardrails/reset). Detection is identical in all three. Unset takes the workload's own safety.watchdog, then mast's default (feedback) — the startup line says which"),
		mcpDigest:        fs.Bool("mcp-digest", true, "route MCP tool responses through the structural digest (internal/digest) before they reach the model: JSON is pruned deterministically (identifier keys kept, long strings truncated, long arrays collapsed head+tail), prose is passed through bounded. Responses under 8000 bytes are untouched. Also registers `retrieve_raw` so a specialist can fetch the un-digested payload back when a digest dropped something it needs. --mcp-digest=false is the kill switch; per-server opt-out is `no_digest: true` in mcp.json"),
		version:          fs.Bool("version", false, "print version and exit"),
	}
}

// misplacedFlag returns the first positional argument that names a
// defined flag (leading dashes stripped, =value ignored), or "" when
// none do. defined reports whether a flag name exists — injected so
// tests don't depend on package-level flag registration order.
func misplacedFlag(args []string, defined func(string) bool) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if name != "" && defined(name) {
			return a
		}
	}
	return ""
}
