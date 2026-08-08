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

package serverauth

import (
	"context"
	"errors"
	"testing"
)

func TestStaticBearerValidator(t *testing.T) {
	p := &Principal{Subject: "svc", Scopes: []string{"triage:invoke"}, Tenant: "acme"}
	v, err := NewStaticBearerValidator(map[string]*Principal{"secret": p})
	if err != nil {
		t.Fatalf("NewStaticBearerValidator: %v", err)
	}
	got, err := v.Validate(context.Background(), "secret")
	if err != nil {
		t.Fatalf("Validate(good): %v", err)
	}
	if got != p {
		t.Fatalf("Validate(good) = %+v, want %+v", got, p)
	}
	if _, err := v.Validate(context.Background(), "wrong"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Validate(bad) = %v, want ErrInvalidToken", err)
	}
	// An empty presented token must not match a configured token.
	if _, err := v.Validate(context.Background(), ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Validate(empty) = %v, want ErrInvalidToken", err)
	}
}

func TestNewStaticBearerValidatorRejectsBadConfig(t *testing.T) {
	p := &Principal{Subject: "svc"}
	if _, err := NewStaticBearerValidator(nil); err == nil {
		t.Error("nil map: want error, got nil")
	}
	if _, err := NewStaticBearerValidator(map[string]*Principal{}); err == nil {
		t.Error("empty map: want error, got nil")
	}
	if _, err := NewStaticBearerValidator(map[string]*Principal{"": p}); err == nil {
		t.Error("empty token: want error, got nil")
	}
	if _, err := NewStaticBearerValidator(map[string]*Principal{"t": nil}); err == nil {
		t.Error("nil principal: want error, got nil")
	}
}

func TestPrincipalHasScope(t *testing.T) {
	p := &Principal{Scopes: []string{"a", "b"}}
	if !p.HasScope("a") || !p.HasScope("b") {
		t.Error("HasScope(a/b): want true")
	}
	if p.HasScope("c") {
		t.Error("HasScope(c): want false")
	}
	// A nil principal carries no scopes and must not panic.
	var nilP *Principal
	if nilP.HasScope("a") {
		t.Error("nil principal HasScope: want false")
	}
}

// TestIsLoopbackAddr pins the conservative bind-policy classification: only a
// loopback IP or "localhost" (with a port) counts as loopback; wildcards,
// routable hosts, and a bare/empty host are NON-loopback so the caller's
// unauthenticated-bind refusal errs toward safety.
func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:7780", true},
		{"localhost:7780", true},
		{"[::1]:7780", true},
		{"127.0.0.1:0", true},
		{":7780", false},        // empty host → all interfaces
		{"0.0.0.0:7780", false}, // IPv4 wildcard
		{"[::]:7780", false},    // IPv6 wildcard
		{"192.168.1.10:7780", false},
		{"example.com:7780", false},
		{"not-an-addr", false}, // SplitHostPort fails → treated as non-loopback
		{"", false},
	}
	for _, c := range cases {
		if got := IsLoopbackAddr(c.addr); got != c.want {
			t.Errorf("IsLoopbackAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
