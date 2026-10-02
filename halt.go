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

package mast

import "github.com/go-steer/mast/internal/watchdog"

// IsWatchdogHalt reports whether err — from Run, RunWorkload,
// ResumeSession or ResumeByToken — is the behavioral watchdog abandoning
// a runaway turn, which a bundle declaring `safety.watchdog: enforce`
// asks for. It sees through wrapping, so an error you annotate keeps its
// classification.
//
// A halt is not a failure of the work so much as a refusal to keep
// paying for it: the turn was repeating itself. The budget's own stops
// are recognized the same way through pkg/budget's ErrExceeded and
// ErrRefused.
//
// Why a function here rather than the watchdog package's own check: the
// watchdog is internal (#301), and what an embedder needs from it is
// this one answer, not its signals or postures.
func IsWatchdogHalt(err error) bool { return watchdog.IsTripped(err) }
