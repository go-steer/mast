// Copyright 2026 Google LLC
// Licensed under the Apache License, Version 2.0 (the "License");
// see LICENSE for details.

// Package injectedauth supports running mast where Google credentials are
// added to outbound requests by an egress gateway instead of being held by
// the process, as in an Agent Substrate sandbox with credential injection.
//
// With MAST_GOOGLE_AUTH=injected, Google clients (Vertex AI models, MCP
// servers with google_oauth auth) skip Application Default Credentials and
// send "Authorization: Bearer placeholder". The gateway replaces the header
// with a real token, and only replaces headers that are present, so the
// placeholder is required.
package injectedauth

import (
	"context"
	"os"
	"time"

	"cloud.google.com/go/auth"
	"golang.org/x/oauth2"
)

// Placeholder is the token value sent in place of a real credential.
const Placeholder = "placeholder"

// Enabled reports whether MAST_GOOGLE_AUTH=injected is set.
func Enabled() bool { return os.Getenv("MAST_GOOGLE_AUTH") == "injected" }

// farFuture keeps clients from treating the placeholder as expired.
var farFuture = time.Now().Add(100 * 365 * 24 * time.Hour)

// TokenSource returns an oauth2 token source yielding the placeholder.
func TokenSource() oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: Placeholder, TokenType: "Bearer", Expiry: farFuture})
}

type placeholderProvider struct{}

func (placeholderProvider) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: Placeholder, Type: "Bearer", Expiry: farFuture}, nil
}

// Credentials returns cloud.google.com/go/auth credentials yielding the
// placeholder, for clients such as genai that take them directly.
func Credentials() *auth.Credentials {
	return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: placeholderProvider{}})
}
