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
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

// TestRouting smoke-tests the starter offline: a route hit per
// category and the Default fallback for an unclassifiable ticket.
func TestRouting(t *testing.T) {
	root, err := buildRoot()
	if err != nil {
		t.Fatalf("buildRoot: %v", err)
	}
	r, err := newRunner(root)
	if err != nil {
		t.Fatalf("newRunner: %v", err)
	}

	cases := []struct {
		name   string
		ticket string
		want   string // specialist tag expected in the terminal output
	}{
		{"billing", "I was double-charged on my last invoice, please refund the duplicate.", "[billing]"},
		{"outage", "Your API has been returning 500s and our dashboard is unreachable.", "[outage]"},
		{"account", "I am locked out and my password reset never arrives.", "[account]"},
		{"fallback", "How do I export my project data to CSV?", "[general]"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runTicket(context.Background(), r, fmt.Sprintf("test-%d", i+1), tc.ticket, io.Discard)
			if err != nil {
				t.Fatalf("runTicket: %v", err)
			}
			if got := fmt.Sprint(out); !strings.Contains(got, tc.want) {
				t.Fatalf("terminal output = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}
