// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"errors"
	"fmt"
	"testing"

	"miniflux.app/v2/internal/model"

	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
)

func TestMergeEntriesByURL(t *testing.T) {
	entries := model.Entries{
		{Hash: "h1", URL: "https://example.com/a"},
		{Hash: "h2", URL: "https://example.com/b"},
		{Hash: "h3", URL: "https://example.com/a"},    // duplicate of the first entry
		{Hash: "h4", URL: "  https://example.com/b "}, // whitespace-padded duplicate
		{Hash: "h5", URL: ""},                         // no URL: never deduped
		{Hash: "h6", URL: ""},                         // no URL: never deduped either
	}

	deduped, duplicated := mergeEntriesByURL(entries)

	if got, want := len(deduped), 4; got != want {
		t.Fatalf("deduped length = %d, want %d", got, want)
	}
	if got, want := len(duplicated), 2; got != want {
		t.Fatalf("duplicated length = %d, want %d", got, want)
	}

	// First occurrence wins and ordering is preserved.
	wantURLs := []string{
		"https://example.com/a",
		"https://example.com/b",
		"",
		"",
	}
	for i, want := range wantURLs {
		if deduped[i].URL != want {
			t.Errorf("deduped[%d].URL = %q, want %q", i, deduped[i].URL, want)
		}
	}

	wantSkippedHashes := map[string]bool{"h3": false, "h4": false}
	for _, entry := range duplicated {
		if _, ok := wantSkippedHashes[entry.Hash]; !ok {
			t.Errorf("unexpected duplicated entry hash %q", entry.Hash)
		}
		wantSkippedHashes[entry.Hash] = true
	}
	for hash, found := range wantSkippedHashes {
		if !found {
			t.Errorf("expected duplicated entry hash %q was not reported", hash)
		}
	}

	// No input means two empty slices, never nil maps panics.
	d, u := mergeEntriesByURL(nil)
	if len(d) != 0 || len(u) != 0 {
		t.Errorf("nil input should produce empty slices, got %d/%d", len(d), len(u))
	}
}

func TestIsUniqueViolation(t *testing.T) {
	t.Run("pq unique violation", func(t *testing.T) {
		err := &pq.Error{Code: pqerror.UniqueViolation, Constraint: "entries_feed_id_hash_key"}
		if !isUniqueViolation(err) {
			t.Error("expected pq unique violation to be detected")
		}
	})

	t.Run("wrapped pq unique violation", func(t *testing.T) {
		err := fmt.Errorf("unable to create entry: %w", &pq.Error{Code: pqerror.UniqueViolation})
		if !isUniqueViolation(err) {
			t.Error("expected wrapped pq unique violation to be detected")
		}
	})

	t.Run("other pq error", func(t *testing.T) {
		err := &pq.Error{Code: pqerror.ForeignKeyViolation}
		if isUniqueViolation(err) {
			t.Error("foreign key violation must not be reported as unique violation")
		}
	})

	t.Run("plain error", func(t *testing.T) {
		if isUniqueViolation(errors.New("something else")) {
			t.Error("plain error must not be reported as unique violation")
		}
	})

	t.Run("nil error", func(t *testing.T) {
		if isUniqueViolation(nil) {
			t.Error("nil must not be reported as unique violation")
		}
	})
}

func TestEntryRefreshResultSucceeded(t *testing.T) {
	result := model.EntryRefreshResult{Created: 3, Updated: 2, Skipped: 4, Failed: 1}
	if got, want := result.Succeeded(), 5; got != want {
		t.Errorf("Succeeded() = %d, want %d", got, want)
	}
}
