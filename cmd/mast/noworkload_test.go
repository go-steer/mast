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
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

// TestServeWithoutAWorkloadBoots holds the mode the CLI reference
// documents for an empty --workload: the trivial single-agent
// coordinator, "inject-endpoint smoke only". serve read the bundle's
// monitor, cadence and HITL blocks off a nil bundle and panicked before
// any listener bound, so the documented smoke path was the one way to
// start the daemon that could not start.
//
// It drives serve itself, because serve's only exit is a signal: the
// test raises SIGTERM against its own process once the inject endpoint
// answers, which signal.NotifyContext inside serve catches.
func TestServeWithoutAWorkloadBoots(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("serve panicked: %v", r)
			}
		}()
		done <- serve(slog.New(slog.NewTextHandler(io.Discard, nil)),
			workloadOpts{},
			modelOpts{name: "echo"},
			listenOpts{inject: addr},
			sessionOpts{driver: "sqlite"},
			resumeOpts{},
			"", false)
	}()

	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("serve returned before the inject endpoint answered: %v", err)
		default:
		}
		resp, err := http.Get("http://" + addr + "/")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("inject endpoint never answered on %s: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal self: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve after SIGTERM: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not drain after SIGTERM")
	}
}
