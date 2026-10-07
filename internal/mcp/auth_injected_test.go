// Copyright 2026 Google LLC
// Licensed under the Apache License, Version 2.0 (the "License");
// see LICENSE for details.

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
