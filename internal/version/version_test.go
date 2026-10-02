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

package version

import (
	"runtime/debug"
	"testing"
)

// The shapes debug.ReadBuildInfo actually returns for the builds that
// reach a user. Until rc.2, every one of them that was not a release
// tarball printed "mast dev".
func TestFromBuildInfo(t *testing.T) {
	cases := []struct {
		name string
		info debug.BuildInfo
		want string
	}{
		{"go install mast@version: the main module, from the module cache",
			debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v1.0.0-rc.2"}},
			"1.0.0-rc.2"},
		{"a checkout's go build: VCS-stamped pseudo-version, not a release",
			debug.BuildInfo{
				Main:     debug.Module{Path: modulePath, Version: "v0.6.1-0.20260904220658-603b826132aa+dirty"},
				Settings: []debug.BuildSetting{{Key: "vcs", Value: "git"}},
			},
			""},
		{"a checkout built with -buildvcs=false records (devel)",
			debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "(devel)"}},
			""},
		{"a custom main.go: mast is a dependency",
			debug.BuildInfo{
				Main: debug.Module{Path: "example.com/acme-mast", Version: "(devel)"},
				Deps: []*debug.Module{{Path: "google.golang.org/adk/v2", Version: "v2.4.0"}, {Path: modulePath, Version: "v1.0.0"}},
			},
			"1.0.0"},
		{"a custom main.go with mast replaced by a local directory names no version",
			debug.BuildInfo{
				Main: debug.Module{Path: "example.com/acme-mast"},
				Deps: []*debug.Module{{Path: modulePath, Version: "v1.0.0", Replace: &debug.Module{Path: "../mast"}}},
			},
			""},
		{"a binary that does not use mast at all",
			debug.BuildInfo{Main: debug.Module{Path: "example.com/other"}},
			""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := tc.info
			if got := fromBuildInfo(&info); got != tc.want {
				t.Errorf("fromBuildInfo = %q, want %q", got, tc.want)
			}
		})
	}
}
