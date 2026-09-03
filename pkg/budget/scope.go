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

// Whose ceiling it was.
//
// Through v0.5 that question had no answer a caller could act on: both
// enforcement errors were built with fmt.Errorf, so "specialist %q" was
// prose, and every turn driver treated a specialist's crossed cap the
// same way it treated the workload's — cancel the run, lose the session.
// v0.6 W10.3 makes the two outcomes different, and a difference in
// outcome cannot rest on a substring.
//
// The scope rides the error rather than being returned alongside it
// because the error crosses three package boundaries (meter → driver →
// operator surface) and a second return value would have to be threaded
// through every one of them, including the ones that only pass it along.

package budget

import "errors"

// scopedError is an enforcement error attributed to one specialist's own
// ceiling. Its message is unchanged from what fmt.Errorf produced, so
// prefix-matching classifiers and log-scraping tests see exactly what
// they saw before.
type scopedError struct {
	scope string
	err   error
}

func (e *scopedError) Error() string { return e.err.Error() }
func (e *scopedError) Unwrap() error { return e.err }

// Scope reports the specialist whose own ceiling produced err, and false
// for a session-level ceiling or any error that is not one of ours.
//
// The distinction is the whole of W10.3: a specialist that is out of
// budget is one path closed, and the coordinator can try another; a
// workload that is out of budget has nothing left to try. A caller that
// cannot tell them apart has to assume the harsher one, which is what
// mast did through v0.5.
func Scope(err error) (string, bool) {
	var se *scopedError
	if errors.As(err, &se) {
		return se.scope, true
	}
	return "", false
}

// scopedTo attributes err to a specialist. A caller passing an empty
// name gets err back unwrapped, so "the session's ceiling" stays the
// absence of an attribution rather than an attribution to "".
func scopedTo(name string, err error) error {
	if name == "" || err == nil {
		return err
	}
	return &scopedError{scope: name, err: err}
}
