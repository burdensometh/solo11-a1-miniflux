// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"testing"
	"time"
)

func TestParseShareExpiration(t *testing.T) {
	scenarios := []struct {
		name     string
		input    string
		expected time.Duration
		wantErr  bool
	}{
		{"empty uses default", "", 30 * 24 * time.Hour, false},
		{"never expires", "0", 0, false},
		{"one day", "1", 24 * time.Hour, false},
		{"seven days", "7", 7 * 24 * time.Hour, false},
		{"thirty days", "30", 30 * 24 * time.Hour, false},
		{"ninety days", "90", 90 * 24 * time.Hour, false},
		{"unexpected duration", "2", 0, true},
		{"negative duration", "-1", 0, true},
		{"non-numeric value", "forever", 0, true},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			expiration, err := parseShareExpiration(scenario.input)
			if scenario.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", scenario.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", scenario.input, err)
			}
			if expiration != scenario.expected {
				t.Fatalf("expected %v, got %v", scenario.expected, expiration)
			}
		})
	}
}
