// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import "testing"

func TestFormatEntryETag(t *testing.T) {
	got := FormatEntryETag(7)
	want := `W/"entry-version-7"`
	if got != want {
		t.Fatalf(`FormatEntryETag(7) = %q, want %q`, got, want)
	}

	entry := &Entry{Version: 42}
	if entry.ETag() != `W/"entry-version-42"` {
		t.Fatalf(`Entry.ETag() = %q, want W/"entry-version-42"`, entry.ETag())
	}
}

func TestParseEntryETag(t *testing.T) {
	testCases := []struct {
		name     string
		etag     string
		version  int64
		expected bool
	}{
		{"weak validator", `W/"entry-version-3"`, 3, true},
		{"strong validator", `"entry-version-3"`, 3, true},
		{"surrounding whitespace", `  W/"entry-version-12"  `, 12, true},
		{"empty", "", 0, false},
		{"unrelated etag", `"abc-1"`, 0, false},
		{"missing quotes", `W/entry-version-1`, 0, false},
		{"non numeric version", `W/"entry-version-x"`, 0, false},
		{"negative version", `W/"entry-version--1"`, 0, false},
		{"wrong prefix", `W/"feed-version-1"`, 0, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			version, ok := ParseEntryETag(tc.etag)
			if ok != tc.expected {
				t.Fatalf(`ParseEntryETag(%q) ok = %v, want %v`, tc.etag, ok, tc.expected)
			}
			if ok && version != tc.version {
				t.Fatalf(`ParseEntryETag(%q) version = %d, want %d`, tc.etag, version, tc.version)
			}
		})
	}
}

func TestEntryETagRoundTrip(t *testing.T) {
	for _, version := range []int64{1, 2, 100, 9223372036854775807} {
		parsed, ok := ParseEntryETag(FormatEntryETag(version))
		if !ok {
			t.Fatalf(`round trip failed for version %d`, version)
		}
		if parsed != version {
			t.Fatalf(`round trip changed version: got %d, want %d`, parsed, version)
		}
	}
}

func TestParseIfMatchHeader(t *testing.T) {
	testCases := []struct {
		name         string
		header       string
		wantVersion  int64
		wantPresent  bool
		wantOK       bool
	}{
		{"absent", "", 0, false, true},
		{"wildcard resolves as present without a version", "*", 0, true, true},
		{"single etag", `W/"entry-version-5"`, 5, true, true},
		{"strong etag", `"entry-version-6"`, 6, true, true},
		{"list of etags", `"entry-version-1", W/"entry-version-9"`, 1, true, true},
		{"malformed", `not-an-etag`, 0, false, false},
		{"list without matching etag", `"other-1", "other-2"`, 0, false, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			version, present, ok := ParseIfMatchHeader(tc.header)
			if ok != tc.wantOK {
				t.Fatalf(`ok = %v, want %v`, ok, tc.wantOK)
			}
			if present != tc.wantPresent {
				t.Fatalf(`present = %v, want %v`, present, tc.wantPresent)
			}
			if version != tc.wantVersion {
				t.Fatalf(`version = %d, want %d`, version, tc.wantVersion)
			}
		})
	}
}
