// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"database/sql"
	"os"
	"strconv"
	"testing"
	"time"

	"miniflux.app/v2/internal/model"
)

// These tests exercise RefreshFeedEntries against a real PostgreSQL instance.
// They are skipped unless TEST_DATABASE_URL points to a migrated database, so
// the default unit-test run stays hermetic.
func testDatabaseURL(t *testing.T) string {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	return dsn
}

func newTestStorage(t *testing.T) *Storage {
	t.Helper()
	db, err := sql.Open("postgres", testDatabaseURL(t))
	if err != nil {
		t.Fatalf("unable to open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	store := NewStorage(db)
	if err := store.Ping(); err != nil {
		t.Fatalf("unable to reach database: %v", err)
	}
	return store
}

// uniqueSuffix keeps parallel manual runs of this test from colliding on the
// unique(username) / unique(user_id, feed_url) constraints.
var uniqueSuffix = strconv.FormatInt(time.Now().UnixNano(), 10)

func seedFeed(t *testing.T, store *Storage) (userID, feedID int64) {
	t.Helper()

	user, err := store.CreateUser(&model.UserCreationRequest{
		Username: "refresh_test_user_" + uniqueSuffix,
		Password: "test-password",
	})
	if err != nil {
		t.Fatalf("unable to create user: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.db.Exec(`DELETE FROM users WHERE id = $1`, user.ID); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})

	category, err := store.CreateCategory(user.ID, &model.CategoryCreationRequest{Title: "test-category"})
	if err != nil {
		t.Fatalf("unable to create category: %v", err)
	}

	feed := &model.Feed{
		UserID:   user.ID,
		Category: &model.Category{ID: category.ID},
		Title:    "test-feed",
		FeedURL:  "https://example.invalid/feed-" + uniqueSuffix + ".xml",
		SiteURL:  "https://example.invalid/",
	}
	feed.CheckedNow()
	if err := store.CreateFeed(feed); err != nil {
		t.Fatalf("unable to create feed: %v", err)
	}

	return user.ID, feed.ID
}

func newTestEntry(hash, url string) *model.Entry {
	return &model.Entry{
		Hash:        hash,
		URL:         url,
		Title:       "title-" + hash,
		Content:     "content-" + hash,
		Author:      "author",
		Date:        time.Now().UTC(),
		Enclosures:  make(model.EnclosureList, 0),
		Tags:        []string{},
		CommentsURL: "",
	}
}

func TestRefreshFeedEntriesIntegration(t *testing.T) {
	store := newTestStorage(t)
	userID, feedID := seedFeed(t, store)

	t.Run("fresh inserts plus in-batch URL merge", func(t *testing.T) {
		entries := model.Entries{
			newTestEntry("hash-a-"+uniqueSuffix, "https://example.invalid/post-a"),
			newTestEntry("hash-b-"+uniqueSuffix, "https://example.invalid/post-b"),
			// Same URL as the first entry but a different hash: must be merged away.
			newTestEntry("hash-a-dup-"+uniqueSuffix, "https://example.invalid/post-a"),
		}

		newEntries, result, err := store.RefreshFeedEntries(userID, feedID, entries, true)
		if err != nil {
			t.Fatalf("RefreshFeedEntries returned an error: %v", err)
		}
		if len(newEntries) != 2 {
			t.Errorf("new entries = %d, want 2", len(newEntries))
		}
		if result.Created != 2 {
			t.Errorf("created = %d, want 2", result.Created)
		}
		if result.Skipped != 1 {
			t.Errorf("skipped = %d, want 1 (in-batch duplicate URL)", result.Skipped)
		}
		if result.Updated != 0 || result.Failed != 0 {
			t.Errorf("updated = %d, failed = %d, want 0/0", result.Updated, result.Failed)
		}
		if total := result.Created + result.Updated + result.Skipped + result.Failed; total != 3 {
			t.Errorf("counters total = %d, want 3 (every input accounted for)", total)
		}

		var stored int
		if err := store.db.QueryRow(`SELECT count(*) FROM entries WHERE feed_id = $1`, feedID).Scan(&stored); err != nil {
			t.Fatalf("count entries: %v", err)
		}
		if stored != 2 {
			t.Errorf("stored entries = %d, want 2: duplicate must not be inserted", stored)
		}
	})

	t.Run("second refresh updates existing entries and skips unchanged ones", func(t *testing.T) {
		entries := model.Entries{
			// Existing hash: updated when updateExistingEntries is true.
			newTestEntry("hash-a-"+uniqueSuffix, "https://example.invalid/post-a"),
			// Existing hash: skipped when updates are disabled.
			newTestEntry("hash-b-"+uniqueSuffix, "https://example.invalid/post-b"),
			// Brand new: created.
			newTestEntry("hash-c-"+uniqueSuffix, "https://example.invalid/post-c"),
		}

		newEntries, result, err := store.RefreshFeedEntries(userID, feedID, entries, false)
		if err != nil {
			t.Fatalf("RefreshFeedEntries returned an error: %v", err)
		}
		if result.Created != 1 {
			t.Errorf("created = %d, want 1", result.Created)
		}
		if result.Updated != 0 {
			t.Errorf("updated = %d, want 0 (updates disabled)", result.Updated)
		}
		if result.Skipped != 2 {
			t.Errorf("skipped = %d, want 2 (one existing, one URL-merged)", result.Skipped)
		}
		if result.Failed != 0 {
			t.Errorf("failed = %d, want 0", result.Failed)
		}
		if len(newEntries) != 1 || newEntries[0].Hash != "hash-c-"+uniqueSuffix {
			t.Errorf("unexpected new entries: %+v", newEntries)
		}
	})

	t.Run("tombstoned entry is skipped not recreated", func(t *testing.T) {
		tombHash := "hash-tomb-" + uniqueSuffix
		if _, err := store.db.Exec(
			`INSERT INTO entry_tombstones (feed_id, hash, deleted_at) VALUES ($1, $2, now())`,
			feedID, tombHash,
		); err != nil {
			t.Fatalf("insert tombstone: %v", err)
		}

		entries := model.Entries{newTestEntry(tombHash, "https://example.invalid/post-tomb")}
		_, result, err := store.RefreshFeedEntries(userID, feedID, entries, true)
		if err != nil {
			t.Fatalf("RefreshFeedEntries returned an error: %v", err)
		}
		if result.Skipped != 1 || result.Created != 0 || result.Failed != 0 {
			t.Errorf("unexpected counters: %+v", result)
		}

		var stored int
		if err := store.db.QueryRow(
			`SELECT count(*) FROM entries WHERE feed_id = $1 AND hash = $2`, feedID, tombHash,
		).Scan(&stored); err != nil {
			t.Fatalf("count tombstoned entry: %v", err)
		}
		if stored != 0 {
			t.Errorf("tombstoned entry was recreated, stored = %d, want 0", stored)
		}
	})

	t.Run("duplicate link rejected at insert time is treated as skipped", func(t *testing.T) {
		// Simulate the production incident: another entry with the same link is
		// already stored while the hash differs, so the (feed_id, hash) check
		// passes but the insert hits a unique constraint. A temporary unique
		// index on (feed_id, url) reproduces the duplicate-link rejection
		// deterministically (the existence check cannot catch it because the
		// hashes differ).
		if _, err := store.db.Exec(
			`CREATE UNIQUE INDEX entries_feed_url_test_idx ON entries (feed_id, url)`,
		); err != nil {
			t.Fatalf("create temporary unique index: %v", err)
		}
		t.Cleanup(func() {
			if _, err := store.db.Exec(`DROP INDEX entries_feed_url_test_idx`); err != nil {
				t.Errorf("drop temporary unique index: %v", err)
			}
		})

		existingHash := "hash-link-existing-" + uniqueSuffix
		if _, err := store.db.Exec(`
			INSERT INTO entries (user_id, feed_id, hash, url, title, published_at, changed_at, created_at)
			VALUES ($1, $2, $3, $4, $5, now(), now(), now())`,
			userID, feedID, existingHash, "https://example.invalid/duplicated-link", "already-there",
		); err != nil {
			t.Fatalf("seed existing entry: %v", err)
		}

		// Different hash, same URL: the hash-based existence check misses it,
		// then the unique index rejects the insert with SQLSTATE 23505.
		incoming := newTestEntry("hash-link-incoming-"+uniqueSuffix, "https://example.invalid/duplicated-link")
		// A second, genuinely new entry proves the refresh continues afterwards.
		survivor := newTestEntry("hash-link-survivor-"+uniqueSuffix, "https://example.invalid/unique-link")

		newEntries, result, err := store.RefreshFeedEntries(userID, feedID, model.Entries{incoming, survivor}, true)
		if err != nil {
			t.Fatalf("a duplicate link must not abort the refresh, got error: %v", err)
		}
		if result.Skipped != 1 {
			t.Errorf("skipped = %d, want 1 (duplicate link)", result.Skipped)
		}
		if result.Failed != 0 {
			t.Errorf("failed = %d, want 0: unique violation is a skip, not a failure", result.Failed)
		}
		if result.Created != 1 {
			t.Errorf("created = %d, want 1 (the refresh kept going after the duplicate)", result.Created)
		}
		if len(newEntries) != 1 || newEntries[0].Hash != survivor.Hash {
			t.Errorf("only the non-duplicate entry should be returned as new, got %+v", newEntries)
		}

		var stored int
		if err := store.db.QueryRow(
			`SELECT count(*) FROM entries WHERE feed_id = $1 AND url = $2`,
			feedID, "https://example.invalid/duplicated-link",
		).Scan(&stored); err != nil {
			t.Fatalf("count duplicated-link entries: %v", err)
		}
		if stored != 1 {
			t.Errorf("duplicated link rows = %d, want 1: the rejected insert must not persist", stored)
		}
	})

	t.Run("one failing entry is counted as failed without aborting the others", func(t *testing.T) {
		// A non-existent feed ID makes createEntry fail with a foreign key
		// violation (SQLSTATE 23503), which is not a duplicate and must be
		// counted as Failed rather than rolling back the whole refresh. Both
		// entries fail here, which proves the loop keeps going after the first
		// failure instead of returning immediately.
		entries := model.Entries{
			newTestEntry("hard-fail-1-"+uniqueSuffix, "https://example.invalid/post-fk-fail-1"),
			newTestEntry("hard-fail-2-"+uniqueSuffix, "https://example.invalid/post-fk-fail-2"),
		}
		_, result, err := store.RefreshFeedEntries(userID, 999999999, entries, true)
		if err != nil {
			t.Fatalf("per-entry failures must not surface as a call error: %v", err)
		}
		if result.Failed != 2 {
			t.Errorf("failed = %d, want 2", result.Failed)
		}
		if result.Created != 0 || result.Skipped != 0 {
			t.Errorf("created = %d, skipped = %d, want 0/0", result.Created, result.Skipped)
		}

		// The real feed still accepts entries afterwards: the failed batch did
		// not leave any poisoned transaction state behind.
		_, surviving, survErr := store.RefreshFeedEntries(
			userID, feedID,
			model.Entries{newTestEntry("hash-survives-"+uniqueSuffix, "https://example.invalid/post-survives")},
			true,
		)
		if survErr != nil {
			t.Fatalf("surviving entry refresh failed: %v", survErr)
		}
		if surviving.Created != 1 || surviving.Failed != 0 {
			t.Errorf("surviving counters = %+v, want 1 created / 0 failed", surviving)
		}
	})
}
