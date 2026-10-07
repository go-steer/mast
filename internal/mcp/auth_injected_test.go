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

package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGoogleAuthTransport_InjectedSendsPlaceholder(t *testing.T) {
	t.Setenv("MAST_GOOGLE_AUTH", "injected")
	// Point ADC at nothing so a lookup would fail if it happened.
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/nonexistent")

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	rt, err := newGoogleAuthTransport(context.Background(), "gke", []string{"https://www.googleapis.com/auth/cloud-platform"})
	if err != nil {
		t.Fatalf("newGoogleAuthTransport: %v", err)
	}
	resp, err := (&http.Client{Transport: rt}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "Bearer placeholder" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer placeholder")
	}
}
