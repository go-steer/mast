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

package pricing

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRefreshRejectsOversizedBody pins the body cap added for
// go-steer/core-agent#372's OOM-on-hostile-mirror item: a response
// larger than maxRefreshBodyBytes is refused as a network failure
// instead of being buffered wholesale.
func TestRefreshRejectsOversizedBody(t *testing.T) {
	orig := maxRefreshBodyBytes
	maxRefreshBodyBytes = 1024
	t.Cleanup(func() { maxRefreshBodyBytes = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	t.Cleanup(srv.Close)

	out, err := Refresh(t.Context(), t.TempDir(), RefreshOptions{Source: srv.URL, MinInterval: -1})
	if err != nil {
		t.Fatalf("Refresh returned hard error: %v", err)
	}
	if !out.NetworkFailed || out.NetworkError == nil {
		t.Fatalf("want NetworkFailed outcome for oversized body, got %+v", out)
	}
	if !strings.Contains(out.NetworkError.Error(), "byte cap") {
		t.Fatalf("error should name the cap, got: %v", out.NetworkError)
	}
}
