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
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// theDefault stands in for defaultSessionDBPath so the table never
// depends on the test runner's home directory.
func theDefault() (string, error) { return "/home/op/.mast/sessions.db", nil }

func TestResolveSessionDB(t *testing.T) {
	cases := []struct {
		name        string
		in          sessionOpts
		dbFlagSet   bool
		attaching   bool
		wantDB      string
		wantImplied bool
		wantErr     error
	}{
		{
			// The case #329 is about: the daemon shape mast is for,
			// started without the flag that existed only to be mandatory.
			name:        "attach with no flag implies the default path",
			in:          sessionOpts{driver: "sqlite"},
			attaching:   true,
			wantDB:      "/home/op/.mast/sessions.db",
			wantImplied: true,
		},
		{
			// Nobody's database moves.
			name:      "attach with a named path keeps it",
			in:        sessionOpts{db: "/srv/state/mast.db", driver: "sqlite"},
			dbFlagSet: true,
			attaching: true,
			wantDB:    "/srv/state/mast.db",
		},
		{
			// A path can arrive from a config default or a wrapper
			// script rather than from this process's argv; the value is
			// what counts, not how it got here.
			name:      "a named path without the flag being set still counts",
			in:        sessionOpts{db: "/srv/state/mast.db", driver: "sqlite"},
			attaching: true,
			wantDB:    "/srv/state/mast.db",
		},
		{
			// The implication is scoped to attach. A one-shot or
			// inject-only daemon that wanted durability would have asked.
			name: "no attach implies nothing",
			in:   sessionOpts{driver: "sqlite"},
		},
		{
			name:      "no attach and an explicit empty db is not a conflict",
			in:        sessionOpts{driver: "sqlite"},
			dbFlagSet: true,
		},
		{
			// The case a value comparison cannot see, and the one
			// upstream's bool was careful not to swallow.
			name:      "attach plus an explicitly empty db is refused",
			in:        sessionOpts{driver: "sqlite"},
			dbFlagSet: true,
			attaching: true,
			wantErr:   errAttachSessionDBOff,
		},
		{
			// A DSN carries a host, a database and credentials. None of
			// those can be invented, so this stays an error — but one
			// that says the implication does not apply, rather than
			// repeating the generic "requires --session-db".
			name:      "postgres never implies",
			in:        sessionOpts{driver: "postgres"},
			attaching: true,
			wantErr:   errAttachNeedsPostgresDSN,
		},
		{
			name:      "postgres with a DSN is untouched",
			in:        sessionOpts{db: "postgres://u@h/db", driver: "postgres"},
			attaching: true,
			wantDB:    "postgres://u@h/db",
		},
		{
			// An unknown driver is sessionDialector's error to report,
			// with its list of valid values; resolving is not the place
			// to duplicate that. What matters here is that it does not
			// silently get a sqlite path.
			name:      "an unknown driver does not get an implied sqlite path",
			in:        sessionOpts{driver: "mysql"},
			attaching: true,
			wantErr:   errAttachNeedsPostgresDSN,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSessionDB(tc.in, tc.dbFlagSet, tc.attaching, theDefault)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.db != tc.wantDB {
				t.Errorf("db = %q, want %q", got.db, tc.wantDB)
			}
			if got.implied != tc.wantImplied {
				t.Errorf("implied = %v, want %v", got.implied, tc.wantImplied)
			}
			if got.driver != tc.in.driver {
				t.Errorf("driver = %q, want it unchanged at %q", got.driver, tc.in.driver)
			}
		})
	}
}

// The refusals are what an operator reads instead of watching a
// database appear (or not appear) silently, so assert they say which
// way out applies rather than only that they are non-nil.
func TestResolveSessionDBRefusalsNameTheWayOut(t *testing.T) {
	for _, want := range []string{"--session-db", "~/.mast/sessions.db"} {
		if !strings.Contains(errAttachSessionDBOff.Error(), want) {
			t.Errorf("errAttachSessionDBOff does not mention %q: %v", want, errAttachSessionDBOff)
		}
	}
	for _, want := range []string{"postgres", "sqlite", "DSN"} {
		if !strings.Contains(errAttachNeedsPostgresDSN.Error(), want) {
			t.Errorf("errAttachNeedsPostgresDSN does not mention %q: %v", want, errAttachNeedsPostgresDSN)
		}
	}
}

// A home directory mast cannot find is the one way defaultSessionDBPath
// fails, and the operator's fix is to name a path — so the error has to
// carry it. Reported rather than swallowed into a bare mkdir failure
// later.
func TestResolveSessionDBReportsAnUnavailableHome(t *testing.T) {
	boom := func() (string, error) { return "", errors.New("$HOME is not defined") }
	_, err := resolveSessionDB(sessionOpts{driver: "sqlite"}, false, true, boom)
	if err == nil {
		t.Fatal("want an error when the default path cannot be resolved")
	}
	if !strings.Contains(err.Error(), "$HOME is not defined") {
		t.Errorf("error loses the cause: %v", err)
	}
}

func TestDefaultSessionDBPath(t *testing.T) {
	t.Setenv("HOME", "/tmp/mast-home-test")
	got, err := defaultSessionDBPath()
	if err != nil {
		t.Fatalf("defaultSessionDBPath: %v", err)
	}
	// The shape core-agent already uses (~/.<binary>/sessions.db), which
	// is what keeps this off docs/sibling-sync.md as a divergence.
	if want := filepath.Join("/tmp/mast-home-test", ".mast", "sessions.db"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if err := defaultPathErrorNamesTheFlag(); err != nil {
		t.Error(err)
	}
}

func defaultPathErrorNamesTheFlag() error {
	saved, had := os.LookupEnv("HOME")
	_ = os.Unsetenv("HOME")
	defer func() {
		if had {
			_ = os.Setenv("HOME", saved)
		}
	}()
	if _, err := defaultSessionDBPath(); err != nil && !strings.Contains(err.Error(), "--session-db=") {
		return errors.New("the no-home error does not name --session-db: " + err.Error())
	}
	return nil
}

// The resolution reads "was the flag given?" off flag.Visit, not off the
// value, because mast's --session-db is a string: an explicit empty one
// is indistinguishable downstream from an absent one. This pins the
// mechanism the refusal above depends on.
func TestExplicitFlagMapSeesAnEmptyString(t *testing.T) {
	fs := flag.NewFlagSet("mast", flag.ContinueOnError)
	db := fs.String("session-db", "", "")
	if err := fs.Parse([]string{"--session-db="}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if *db != "" {
		t.Fatalf("db = %q, want empty", *db)
	}
	if !explicit["session-db"] {
		t.Error("an explicitly empty --session-db was not recorded as set; the conflict refusal is unreachable")
	}
}
