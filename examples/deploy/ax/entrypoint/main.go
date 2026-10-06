// Copyright 2026 Google LLC
// Licensed under the Apache License, Version 2.0 (the "License");
// see LICENSE for details.

// Command ax-mast-entry runs a one-shot mast turn as an AX task command.
//
// AX's runner starts the task command on every boot of the sandbox, and
// Agent Substrate also runs it once in the template's golden boot, where
// egress is denied and /workspace is empty. This wrapper makes a mast turn
// safe under those rules:
//
//   - With --wait-for-egress it blocks until a TLS handshake to that address
//     succeeds, so no turn starts inside the golden boot.
//   - It skips the run when --state-dir holds a done marker from an earlier
//     boot, so a resumed task does not repeat a finished turn. The marker is
//     checked after the egress wait, because /workspace is only populated
//     once the real actor is running.
//   - It copies mast's stdout to result.txt in --state-dir, since AX has no
//     channel for reading a command's output back.
//
// Everything after "--" is the mast command line, run as given.
//
//	ax-mast-entry --wait-for-egress=generativelanguage.googleapis.com:443 -- \
//	  mast --task=review --session-db=/workspace/.mast/sessions.db "..."
package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	stateDir := flag.String("state-dir", "/workspace/.mast", "durable directory for the done marker and result.txt")
	waitFor := flag.String("wait-for-egress", "", "host:port that must accept a TLS handshake before mast starts; empty skips the wait")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	args := flag.Args()
	if len(args) == 0 {
		logger.Error("no mast command given after --")
		os.Exit(2)
	}

	if *waitFor != "" {
		waitForEgress(logger, *waitFor)
	}

	donePath := filepath.Join(*stateDir, "done")
	if b, err := os.ReadFile(donePath); err == nil {
		logger.Info("turn already completed on an earlier boot; not running it again", "marker", donePath, "completed", string(b))
		return
	}
	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		logger.Error("creating state dir", "error", err)
		os.Exit(1)
	}

	code, err := run(logger, args, filepath.Join(*stateDir, "result.txt"))
	if err != nil {
		logger.Error("mast turn did not complete", "exitCode", code, "error", err)
		os.Exit(code)
	}
	if err := os.WriteFile(donePath, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		logger.Error("writing done marker", "error", err)
		os.Exit(1)
	}
	logger.Info("mast turn completed", "marker", donePath)
}

// waitForEgress blocks until a TLS handshake with addr succeeds. A bare TCP
// connect is not enough: Substrate's egress gateway accepts the connection
// and then drops it for an actor without an egress policy.
func waitForEgress(logger *slog.Logger, addr string) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		logger.Error("invalid --wait-for-egress", "addr", addr, "error", err)
		os.Exit(2)
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	start, lastLog := time.Now(), time.Time{}
	for {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
		if err == nil {
			conn.Close()
			logger.Info("egress available", "addr", addr, "waited", time.Since(start).Round(time.Second))
			return
		}
		if time.Since(lastLog) > time.Minute {
			logger.Info("waiting for egress", "addr", addr, "error", err)
			lastLog = time.Now()
		}
		time.Sleep(5 * time.Second)
	}
}

// run starts the mast command, tees its stdout into resultPath, and returns
// its exit code. SIGTERM and SIGINT are forwarded so mast can shut down
// cleanly; under AX the runner signals the whole process group, so mast may
// see them twice, which it tolerates.
func run(logger *slog.Logger, args []string, resultPath string) (int, error) {
	result, err := os.Create(resultPath)
	if err != nil {
		return 1, fmt.Errorf("creating %s: %w", resultPath, err)
	}
	defer result.Close()

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(os.Stdout, result)
	cmd.Stderr = os.Stderr

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("starting %v: %w", args, err)
	}
	logger.Info("started mast", "pid", cmd.Process.Pid, "command", args)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	for {
		select {
		case sig := <-sigs:
			logger.Info("forwarding signal to mast", "signal", sig)
			_ = cmd.Process.Signal(sig)
		case err := <-exited:
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				if code := exitErr.ExitCode(); code > 0 {
					return code, err
				}
				return 1, err // killed by a signal
			}
			if err != nil {
				return 1, err
			}
			return 0, nil
		}
	}
}
