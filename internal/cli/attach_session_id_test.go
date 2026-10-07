// Copyright 2026 Google LLC
// Licensed under the Apache License, Version 2.0 (the "License");
// see LICENSE for details.

package cli

import (
	"strings"
	"testing"
)

func TestNewAttachSessionID(t *testing.T) {
	a, err := newAttachSessionID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newAttachSessionID()
	if !strings.HasPrefix(a, "attach-") || len(a) != len("attach-")+16 {
		t.Errorf("unexpected id %q", a)
	}
	if a == b {
		t.Errorf("ids should differ, got %q twice", a)
	}
}
