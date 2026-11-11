// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package database // import "miniflux.app/v2/internal/database"

import (
	"strings"
	"testing"
)

func TestWithDefaultKeepalivesAddsParameters(t *testing.T) {
	got, err := withDefaultKeepalives("user=postgres password=postgres dbname=miniflux2 sslmode=disable")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	for _, param := range []string{
		"keepalives=1",
		"keepalives_idle=30",
		"keepalives_interval=10",
		"keepalives_count=3",
	} {
		if !strings.Contains(got, param) {
			t.Errorf("Expected DSN to contain %q, got %q", param, got)
		}
	}
}

func TestWithDefaultKeepalivesPreservesExplicitConfiguration(t *testing.T) {
	dsn := "user=postgres dbname=miniflux2 sslmode=disable keepalives=0"

	got, err := withDefaultKeepalives(dsn)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if got != dsn {
		t.Errorf("Expected explicit keepalive configuration to be preserved, got %q", got)
	}
}

func TestWithDefaultKeepalivesAcceptsURLDSN(t *testing.T) {
	got, err := withDefaultKeepalives("postgres://postgres:postgres@localhost/miniflux2?sslmode=disable")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if !strings.Contains(got, "keepalives=1") {
		t.Errorf("Expected URL DSN to be converted and enriched with keepalives, got %q", got)
	}
}
