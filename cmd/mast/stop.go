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

// mast stop — the planned-stop verb (issue #42; docs/durable-execution-
// design.md, "Planned stop"). Asks a running daemon to drain and exit:
// exactly the SIGTERM path, with the interruption markers classified
// "operator stop" instead of "daemon shutdown".
//
//	mast stop [--addr=...] [--reason=...] [--pause-sessions]
//
// Exit-code contract (the daemon's, not this command's): 0 = clean
// drain, 3 = drain window expired with interrupted survivors, 1 =
// error. The exit code encodes work-cut-short, not who initiated —
// Restart=on-failure supervision revives the daemon exactly when
// boot-time repair has work to do.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-steer/mast/pkg/inject"
)

// stopFlagSet builds `mast stop`'s flag surface. Split out of runStop
// so the CLI-surface test can enumerate it without issuing a request:
// flag names are a frozen contract (see DESIGN.md, "The v1.0 stability
// promise").
func stopFlagSet() (fs *flag.FlagSet, addr, reason *string, pauseSessions *bool) {
	fs = flag.NewFlagSet("mast stop", flag.ContinueOnError)
	addr = fs.String("addr", "http://127.0.0.1:7777", "base URL of the running mast daemon")
	reason = fs.String("reason", "", "reason recorded in the stop classification (appended to the interruption markers)")
	pauseSessions = fs.Bool("pause-sessions", false, "gate-pause every session the drain marks, so boot-time auto-resume hands them back to the operator instead of continuing them")
	return fs, addr, reason, pauseSessions
}

// runStop executes `mast stop` and returns the process exit code (of
// this CLI invocation — the daemon's own exit code follows the drain).
func runStop(args []string) int {
	fs, addr, reason, pauseSessions := stopFlagSet()
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "mast stop: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	payload, err := json.Marshal(inject.StopRequest{Reason: *reason, PauseSessions: *pauseSessions})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mast stop: %v\n", err)
		return exitFailure
	}
	url := strings.TrimSuffix(*addr, "/") + "/stop"
	// #nosec G704 -- url derives from the operator's own --addr flag.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "mast stop: %v\n", err)
		return exitFailure
	}
	req.Header.Set("Content-Type", "application/json")
	if token := os.Getenv("MAST_INJECT_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- operator-chosen --addr
	if err != nil {
		fmt.Fprintf(os.Stderr, "mast stop: POST %s: %v (is the daemon running?)\n", url, err)
		return exitFailure
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintf(os.Stderr, "mast stop: POST %s: %s: %s\n", url, resp.Status, strings.TrimSpace(string(body)))
		return exitFailure
	}
	var res inject.StopResult
	if err := json.Unmarshal(body, &res); err == nil && res.DrainBound != "" {
		fmt.Printf("stop accepted; daemon draining (bound %s). Daemon exits 0 on a clean drain, 3 if the window expires with interrupted sessions.\n", res.DrainBound)
	} else {
		fmt.Println("stop accepted; daemon draining")
	}
	return exitOK
}
