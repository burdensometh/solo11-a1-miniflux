// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"errors"
	"testing"
)

func TestParseEntryVersionETag(t *testing.T) {
	testCases := []struct {
		name string
		etag string
		want int64
	}{
		{"weak", `W/"entry-version-4"`, 4},
		{"strong", `"entry-version-9"`, 9},
		{"padded", `  W/"entry-version-1"  `, 1},
		{"empty", "", 0},
		{"wrong prefix", `W/"feed-version-1"`, 0},
		{"garbage", "not-an-etag", 0},
		{"non numeric", `W/"entry-version-x"`, 0},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseEntryVersionETag(tc.etag); got != tc.want {
				t.Fatalf(`parseEntryVersionETag(%q) = %d, want %d`, tc.etag, got, tc.want)
			}
		})
	}
}

func TestEntryVersionETagRoundTrip(t *testing.T) {
	entry := &Entry{Version: 15}
	if entry.ETag() != `W/"entry-version-15"` {
		t.Fatalf(`unexpected ETag: %q`, entry.ETag())
	}
	if got := parseEntryVersionETag(EntryVersionETag(15)); got != 15 {
		t.Fatalf(`round trip version = %d, want 15`, got)
	}
}

func TestEntryConflictErrorUnwrap(t *testing.T) {
	err := &EntryConflictError{CurrentETag: `W/"entry-version-8"`, CurrentVersion: 8}

	if !errors.Is(err, ErrConflict) {
		t.Fatal(`EntryConflictError must unwrap to ErrConflict`)
	}
	if err.CurrentVersion != 8 {
		t.Fatalf(`got version %d, want 8`, err.CurrentVersion)
	}
}
