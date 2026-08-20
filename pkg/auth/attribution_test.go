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

package auth_test

import (
	"context"
	"testing"

	"github.com/go-steer/mast/pkg/auth"
)

func TestAttribution(t *testing.T) {
	t.Parallel()
	const fallback = "mast:internal"
	tests := []struct {
		name string
		ctx  func() context.Context
		want string
	}{
		{
			name: "no caller falls back to the named mechanism",
			ctx:  context.Background,
			want: fallback,
		},
		{
			// A Caller with no identity is a resolution that produced
			// nothing, not an identity of "".
			name: "an empty identity is not an attribution",
			ctx: func() context.Context {
				return auth.WithCaller(context.Background(), auth.Caller{})
			},
			want: fallback,
		},
		{
			name: "a resolved caller is named",
			ctx: func() context.Context {
				return auth.WithCaller(context.Background(), auth.Caller{Identity: "alice@example.com"})
			},
			want: "alice@example.com",
		},
		{
			// Both halves, because either alone answers the wrong
			// question after an incident: the human without the relay
			// that minted the approval, or the relay without the human.
			name: "the proxy path names the human and the credential that asserted them",
			ctx: func() context.Context {
				ctx := auth.WithCaller(context.Background(), auth.Caller{Identity: "alice@example.com"})
				return auth.WithProxyBy(ctx, "sa:switchboard")
			},
			want: "alice@example.com (asserted by sa:switchboard)",
		},
		{
			// WithProxyBy("") means "not proxied"; it must not render a
			// dangling "(asserted by )".
			name: "an empty proxy is not appended",
			ctx: func() context.Context {
				ctx := auth.WithCaller(context.Background(), auth.Caller{Identity: "alice@example.com"})
				return auth.WithProxyBy(ctx, "")
			},
			want: "alice@example.com",
		},
		{
			// A proxy with no effective caller names nobody: there is
			// no human in the record to qualify.
			name: "a proxy without a caller falls back",
			ctx: func() context.Context {
				return auth.WithProxyBy(context.Background(), "sa:switchboard")
			},
			want: fallback,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := auth.Attribution(tc.ctx(), fallback); got != tc.want {
				t.Errorf("Attribution: got %q, want %q", got, tc.want)
			}
		})
	}
}
