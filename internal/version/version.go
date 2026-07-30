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

// Version is the semver tag for released builds, or "dev" for
// in-development builds. GoReleaser overrides it with the release
// tag via -ldflags at build time.
var Version = "dev"
