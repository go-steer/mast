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
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// errAttachSessionDBOff is the one case the implication must not
// swallow. An operator who wrote `--session-db=` meant it, and the
// answer cannot be yes — so say why rather than letting a database
// appear anyway.
//
// mast cannot tell "unset" from "explicitly empty" by value the way
// core-agent's bool can, so this is reachable only through the
// explicit-flag map [flag.Visit] builds; without that the refusal
// upstream was careful to keep would be silently swallowed here.
var errAttachSessionDBOff = errors.New("--attach-listen with an empty --session-db: attach live-tail pumps from the eventlog overlay, which an in-memory session store does not have. Drop --session-db to take the implied ~/.mast/sessions.db, or name a path")

// errAttachNeedsPostgresDSN covers the other half. A path can be
// invented; a DSN carries a host, a database and credentials, and
// guessing any of them would be worse than refusing.
var errAttachNeedsPostgresDSN = errors.New("--session-db-driver=postgres with --attach-listen requires --session-db (a DSN): attach implies a durable store only for the default sqlite driver, because a Postgres DSN cannot be invented")

// resolveSessionDB decides whether this run gets a durable session
// store, and reports whether that decision was the operator's or ours
// (#329).
//
// Attach mode is not a preference here, it is a precondition: the
// broadcaster pumps from the eventlog overlay, `/events` replays from
// it, and the guardrail and spend ledgers restore from it. Attach
// without a durable store could never have worked, so demanding the
// flag was demanding a flag that existed only to be mandatory — on the
// product where every shape is a daemon, which made it a paper cut on
// the first run of the thing mast is for.
//
// The implication fires only when the flag was not given at all, so
// nobody's existing database moves; outside attach mode nothing is
// implied, because a run that wanted durability would have asked.
//
// # Where the implied file lands
//
// `~/.mast/sessions.db`, via defaultPath. This is the one part with no
// upstream to copy: core-agent's `--session-db` is a *bool* beside a
// `--session-db-path` that already defaulted to `~/.<binary>/`, so its
// implication routed through a default that existed, while mast takes
// a path-or-DSN string and has no default at all. Matching the
// sibling's shape is what keeps this off `docs/sibling-sync.md` as a
// divergence. `$XDG_STATE_HOME` is arguably the more correct category
// and was rejected on exactly that ground — a cosmetic improvement is
// not worth a permanent ledger row. `os.TempDir()` was rejected
// because a reboot deleting the store is the failure the implication
// exists to prevent; mast's TempDir precedent (the CCR digest store,
// house rule #5) is explicitly scratch, and this is not.
func resolveSessionDB(s sessionOpts, dbFlagSet, attaching bool, defaultPath func() (string, error)) (sessionOpts, error) {
	if !attaching || s.db != "" {
		return s, nil
	}
	if dbFlagSet {
		return s, errAttachSessionDBOff
	}
	if s.driver != "sqlite" {
		return s, errAttachNeedsPostgresDSN
	}
	path, err := defaultPath()
	if err != nil {
		return s, err
	}
	s.db, s.implied = path, true
	return s, nil
}

// defaultSessionDBPath is where an implied store lands.
func defaultSessionDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("--attach-listen needs a durable session db and no home directory is available to put one in (%w); pass --session-db=/some/writable/path/sessions.db", err)
	}
	return filepath.Join(home, "."+appName, "sessions.db"), nil
}
