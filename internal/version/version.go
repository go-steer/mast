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

// Package version centralizes build-identity reporting for cmd/mast
// and any surface that advertises the build (attach capabilities
// frames, agent cards). Version is overridable at release time via
// -ldflags; plain `go build` reports the "dev" fallback.
//
// GoReleaser injects the real tag via .goreleaser.yaml's ldflags
// entry — keep that path in sync when moving this variable.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is the semver tag for released builds, or "dev" for
// in-development builds. GoReleaser overrides it with the release
// tag via -ldflags at build time.
//
// A build nobody stamped still knows its version when the Go toolchain
// recorded one, and init reads it: `go install
// github.com/go-steer/mast/cmd/mast@v1.0.0` records v1.0.0 as the main
// module's version, and a custom main.go records the mast module it was
// built against as a dependency. Both printed "dev" until v1.0.0-rc.2 —
// the release tarballs were the only builds that could say what they
// were. A checkout's own `go build` stays "dev" (see fromBuildInfo).
var Version = "dev"

func init() {
	if Version != "dev" {
		return // stamped by -ldflags; that wins
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := fromBuildInfo(info); v != "" {
			Version = v
		}
	}
}

// modulePath is the module whose version Version reports.
const modulePath = "github.com/go-steer/mast"

// fromBuildInfo returns the mast module's recorded version, without
// the leading "v" (GoReleaser stamps "1.0.0", not "v1.0.0"), or "" when
// the toolchain recorded none.
func fromBuildInfo(info *debug.BuildInfo) string {
	v := ""
	if info.Main.Path == modulePath {
		// A build from a checkout is stamped from the repository's own
		// tags since Go 1.24, which yields a pseudo-version off whatever
		// tag git can reach — "0.6.1-0.2026…+dirty" in a worktree, which
		// reads like a release and is not one. Such a build records vcs
		// settings; `go install …@vX.Y.Z` builds from the module cache
		// and records none. Only the second is a version mast can name.
		for _, s := range info.Settings {
			if s.Key == "vcs" {
				return ""
			}
		}
		v = info.Main.Version
	} else {
		for _, d := range info.Deps {
			if d.Path == modulePath {
				v = d.Version
				if d.Replace != nil {
					v = d.Replace.Version
				}
				break
			}
		}
	}
	if v == "" || v == "(devel)" {
		return ""
	}
	return strings.TrimPrefix(v, "v")
}

// Commit and Date are the short commit and commit date of a release
// build, empty for a local one; `mast --version` prints them when set.
// They lived in package main until the binary moved into internal/cli
// (#301), where an `-X main.commit=` flag would have stamped nothing
// and nothing would have failed — so they sit here beside Version, and
// a custom main.go built with the same ldflags reports the same
// identity.
var (
	Commit = ""
	Date   = ""
)
