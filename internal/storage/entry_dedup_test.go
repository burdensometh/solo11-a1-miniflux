// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/database"
	"miniflux.app/v2/internal/model"
)

const skipStorageTestsMessage = `Set TEST_DATABASE_URL to run the storage integration tests`

// newTestStorage returns a Storage connected to a fresh test database with
// all migrations applied. Tests are skipped when TEST_DATABASE_URL is not set.
func newTestStorage(t *testing.T) *Storage {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip(skipStorageTestsMessage)
	}

	if config.Opts == nil {
		var err error
		if config.Opts, err = config.NewConfigParser().ParseEnvironmentVariables(); err != nil {
			t.Fatalf("unable to parse configuration: %v", err)
		}
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("unable to connect to the test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := database.Migrate(db); err != nil {
		t.Fatalf("unable to run migrations: %v", err)
	}

	if _, err := db.Exec(`TRUNCATE entries, entry_feeds, entry_tombstones, enclosures, feeds, categories, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("unable to reset the test database: %v", err)
	}

	return NewStorage(db)
}

func createTestUser(t *testing.T, store *Storage) *model.User {
	t.Helper()

	user, err := store.CreateUser(&model.UserCreationRequest{
		Username: "testuser",
		Password: "secret",
	})
	if err != nil {
		t.Fatalf("unable to create test user: %v", err)
	}

	return user
}

func createTestCategory(t *testing.T, store *Storage, userID int64, title string) *model.Category {
	t.Helper()

	category, err := store.CreateCategory(userID, &model.CategoryCreationRequest{Title: title})
	if err != nil {
		t.Fatalf("unable to create test category: %v", err)
	}

	return category
}

func createTestFeed(t *testing.T, store *Storage, userID, categoryID int64, feedURL, siteURL string) *model.Feed {
	t.Helper()

	feed := &model.Feed{
		UserID:   userID,
		FeedURL:  feedURL,
		SiteURL:  siteURL,
		Title:    feedURL,
		Category: &model.Category{ID: categoryID},
	}
	if err := store.CreateFeed(feed); err != nil {
		t.Fatalf("unable to create test feed: %v", err)
	}

	return feed
}

func newTestEntry(hash, url string) *model.Entry {
	entry := model.NewEntry()
	entry.Hash = hash
	entry.URL = url
	entry.Title = "Entry " + hash
	entry.Date = time.Now()
	entry.Status = model.EntryStatusUnread
	return entry
}

// countLinkedFeeds returns the number of feeds carrying the given entry.
func countLinkedFeeds(t *testing.T, store *Storage, entryID int64) int {
	t.Helper()

	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM entry_feeds WHERE entry_id = $1`, entryID).Scan(&count); err != nil {
		t.Fatalf("unable to count linked feeds: %v", err)
	}
	return count
}

func TestRefreshFeedEntriesDeduplicatesAcrossFeedsOfSameSite(t *testing.T) {
	store := newTestStorage(t)
	user := createTestUser(t, store)
	category := createTestCategory(t, store, user.ID, "Tech")
	feedA := createTestFeed(t, store, user.ID, category.ID, "https://example.com/a.xml", "https://example.com/")
	feedB := createTestFeed(t, store, user.ID, category.ID, "https://example.com/b.xml", "https://example.com/")

	newEntries, err := store.RefreshFeedEntries(user.ID, feedA.ID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false)
	if err != nil {
		t.Fatalf("unable to refresh feed A: %v", err)
	}
	if len(newEntries) != 1 {
		t.Fatalf("expected 1 new entry for feed A, got %d", len(newEntries))
	}

	// The same article fetched from another feed of the same site must not be
	// duplicated.
	newEntries, err = store.RefreshFeedEntries(user.ID, feedB.ID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false)
	if err != nil {
		t.Fatalf("unable to refresh feed B: %v", err)
	}
	if len(newEntries) != 0 {
		t.Fatalf("expected 0 new entries for feed B, got %d", len(newEntries))
	}

	counts, err := store.CountAllEntries()
	if err != nil {
		t.Fatal(err)
	}
	if counts["total"] != 1 {
		t.Fatalf("expected a single entry in total, got %d", counts["total"])
	}

	entryID := newEntriesID(t, store)
	if count := countLinkedFeeds(t, store, entryID); count != 2 {
		t.Fatalf("expected the entry to be linked to 2 feeds, got %d", count)
	}

	// The entry must be visible from both feeds.
	for _, feedID := range []int64{feedA.ID, feedB.ID} {
		entries, err := store.NewEntryQueryBuilder(user.ID).WithFeedID(feedID).GetEntries()
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry for feed #%d, got %d", feedID, len(entries))
		}
		if entries[0].FeedID != feedID {
			t.Fatalf("expected the entry to be described with feed #%d, got feed #%d", feedID, entries[0].FeedID)
		}
	}
}

func newEntriesID(t *testing.T, store *Storage) int64 {
	t.Helper()

	var entryID int64
	if err := store.db.QueryRow(`SELECT id FROM entries LIMIT 1`).Scan(&entryID); err != nil {
		t.Fatalf("unable to fetch entry ID: %v", err)
	}
	return entryID
}

func TestRefreshFeedEntriesDoesNotDeduplicateAcrossDifferentSites(t *testing.T) {
	store := newTestStorage(t)
	user := createTestUser(t, store)
	category := createTestCategory(t, store, user.ID, "Tech")
	feedA := createTestFeed(t, store, user.ID, category.ID, "https://example.com/a.xml", "https://example.com/")
	feedB := createTestFeed(t, store, user.ID, category.ID, "https://other-site.net/b.xml", "https://other-site.net/")

	if _, err := store.RefreshFeedEntries(user.ID, feedA.ID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false); err != nil {
		t.Fatal(err)
	}
	newEntries, err := store.RefreshFeedEntries(user.ID, feedB.ID, model.Entries{newTestEntry("hash-shared", "https://other-site.net/article")}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(newEntries) != 1 {
		t.Fatalf("expected 1 new entry for the feed of the other site, got %d", len(newEntries))
	}

	counts, err := store.CountAllEntries()
	if err != nil {
		t.Fatal(err)
	}
	if counts["total"] != 2 {
		t.Fatalf("expected 2 entries for different sites, got %d", counts["total"])
	}
}

func TestSharedEntryReadStatusIsSynchronized(t *testing.T) {
	store := newTestStorage(t)
	user := createTestUser(t, store)
	category := createTestCategory(t, store, user.ID, "Tech")
	feedA := createTestFeed(t, store, user.ID, category.ID, "https://example.com/a.xml", "https://example.com/")
	feedB := createTestFeed(t, store, user.ID, category.ID, "https://example.com/b.xml", "https://example.com/")

	if _, err := store.RefreshFeedEntries(user.ID, feedA.ID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefreshFeedEntries(user.ID, feedB.ID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false); err != nil {
		t.Fatal(err)
	}

	entryID := newEntriesID(t, store)

	// Marking the entry as read from one feed must be reflected in the other.
	if err := store.SetEntriesStatus(user.ID, []int64{entryID}, model.EntryStatusRead); err != nil {
		t.Fatal(err)
	}

	for _, feedID := range []int64{feedA.ID, feedB.ID} {
		entry, err := store.NewEntryQueryBuilder(user.ID).WithFeedID(feedID).WithEntryIDs(entryID).GetEntry()
		if err != nil {
			t.Fatal(err)
		}
		if entry == nil {
			t.Fatalf("expected the entry to be visible from feed #%d", feedID)
		}
		if entry.Status != model.EntryStatusRead {
			t.Fatalf("expected the entry to be read from feed #%d, got %q", feedID, entry.Status)
		}
	}

	// Marking one feed as read also marks the shared entry in the other feed.
	if _, err := store.RefreshFeedEntries(user.ID, feedA.ID, model.Entries{newTestEntry("hash-other", "https://example.com/other")}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefreshFeedEntries(user.ID, feedB.ID, model.Entries{newTestEntry("hash-other", "https://example.com/other")}, false); err != nil {
		t.Fatal(err)
	}

	if err := store.MarkFeedAsRead(user.ID, feedA.ID, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	unread, err := store.NewEntryQueryBuilder(user.ID).WithFeedID(feedB.ID).WithStatuses(model.EntryStatusUnread).GetEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 0 {
		t.Fatalf("expected no unread entries in feed B after marking feed A as read, got %d", len(unread))
	}
}

func TestRemoveFeedKeepsEntriesSharedWithOtherFeeds(t *testing.T) {
	store := newTestStorage(t)
	user := createTestUser(t, store)
	category := createTestCategory(t, store, user.ID, "Tech")
	feedA := createTestFeed(t, store, user.ID, category.ID, "https://example.com/a.xml", "https://example.com/")
	feedB := createTestFeed(t, store, user.ID, category.ID, "https://example.com/b.xml", "https://example.com/")

	if _, err := store.RefreshFeedEntries(user.ID, feedA.ID, model.Entries{
		newTestEntry("hash-shared", "https://example.com/article"),
		newTestEntry("hash-exclusive", "https://example.com/exclusive"),
	}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefreshFeedEntries(user.ID, feedB.ID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false); err != nil {
		t.Fatal(err)
	}

	// Remove the canonical feed of the shared entry (the first one).
	if err := store.RemoveFeed(user.ID, feedA.ID); err != nil {
		t.Fatal(err)
	}

	// The shared entry must survive and remain attached to feed B.
	entries, err := store.NewEntryQueryBuilder(user.ID).WithFeedID(feedB.ID).GetEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry left in feed B, got %d", len(entries))
	}
	if entries[0].Hash != "hash-shared" {
		t.Fatalf("expected the shared entry to survive, got %q", entries[0].Hash)
	}
	if entries[0].FeedID != feedB.ID {
		t.Fatalf("expected the canonical feed to be reassigned to feed B, got feed #%d", entries[0].FeedID)
	}

	counts, err := store.CountAllEntries()
	if err != nil {
		t.Fatal(err)
	}
	if counts["total"] != 1 {
		t.Fatalf("expected the exclusive entry to be deleted with feed A, got %d entries", counts["total"])
	}

	// Removing the last feed carrying the entry deletes it.
	if err := store.RemoveFeed(user.ID, feedB.ID); err != nil {
		t.Fatal(err)
	}
	counts, err = store.CountAllEntries()
	if err != nil {
		t.Fatal(err)
	}
	if counts["total"] != 0 {
		t.Fatalf("expected no entries left, got %d", counts["total"])
	}
}

func TestRemoveCategoryKeepsEntriesSharedWithOtherCategories(t *testing.T) {
	store := newTestStorage(t)
	user := createTestUser(t, store)
	categoryA := createTestCategory(t, store, user.ID, "Tech")
	categoryB := createTestCategory(t, store, user.ID, "News")
	feedA := createTestFeed(t, store, user.ID, categoryA.ID, "https://example.com/a.xml", "https://example.com/")
	feedB := createTestFeed(t, store, user.ID, categoryB.ID, "https://example.com/b.xml", "https://example.com/")

	if _, err := store.RefreshFeedEntries(user.ID, feedA.ID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefreshFeedEntries(user.ID, feedB.ID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false); err != nil {
		t.Fatal(err)
	}

	// The entry is visible from both categories.
	for _, categoryID := range []int64{categoryA.ID, categoryB.ID} {
		entries, err := store.NewEntryQueryBuilder(user.ID).WithCategoryID(categoryID).GetEntries()
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry in category #%d, got %d", categoryID, len(entries))
		}
		if entries[0].Feed.Category.ID != categoryID {
			t.Fatalf("expected the entry to be described with category #%d, got category #%d", categoryID, entries[0].Feed.Category.ID)
		}
	}

	// Removing the category of the canonical feed must keep the shared entry.
	if err := store.RemoveCategory(user.ID, categoryA.ID); err != nil {
		t.Fatal(err)
	}

	entries, err := store.NewEntryQueryBuilder(user.ID).WithCategoryID(categoryB.ID).GetEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected the shared entry to survive the category removal, got %d entries", len(entries))
	}
}

func TestFeedCountersWithSharedEntries(t *testing.T) {
	store := newTestStorage(t)
	user := createTestUser(t, store)
	category := createTestCategory(t, store, user.ID, "Tech")
	feedA := createTestFeed(t, store, user.ID, category.ID, "https://example.com/a.xml", "https://example.com/")
	feedB := createTestFeed(t, store, user.ID, category.ID, "https://example.com/b.xml", "https://example.com/")

	if _, err := store.RefreshFeedEntries(user.ID, feedA.ID, model.Entries{
		newTestEntry("hash-shared", "https://example.com/article"),
		newTestEntry("hash-a", "https://example.com/a1"),
	}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefreshFeedEntries(user.ID, feedB.ID, model.Entries{
		newTestEntry("hash-shared", "https://example.com/article"),
		newTestEntry("hash-b", "https://example.com/b1"),
	}, false); err != nil {
		t.Fatal(err)
	}

	counters, err := store.FetchCounters(user.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Each feed sees the shared entry in its own counters.
	if got := counters.UnreadCounters[feedA.ID]; got != 2 {
		t.Fatalf("expected 2 unread entries for feed A, got %d", got)
	}
	if got := counters.UnreadCounters[feedB.ID]; got != 2 {
		t.Fatalf("expected 2 unread entries for feed B, got %d", got)
	}

	// Globally, the shared entry is counted only once.
	counts, err := store.CountAllEntries()
	if err != nil {
		t.Fatal(err)
	}
	if counts[model.EntryStatusUnread] != 3 {
		t.Fatalf("expected 3 unread entries in total, got %d", counts[model.EntryStatusUnread])
	}

	navMetadata, err := store.GetNavMetadata(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if navMetadata.CountUnread != 3 {
		t.Fatalf("expected the global unread counter to be 3, got %d", navMetadata.CountUnread)
	}

	// After marking the shared entry as read, all counters are consistent.
	var sharedEntryID int64
	if err := store.db.QueryRow(`SELECT id FROM entries WHERE hash = 'hash-shared'`).Scan(&sharedEntryID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEntriesStatus(user.ID, []int64{sharedEntryID}, model.EntryStatusRead); err != nil {
		t.Fatal(err)
	}

	counters, err = store.FetchCounters(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := counters.UnreadCounters[feedA.ID]; got != 1 {
		t.Fatalf("expected 1 unread entry for feed A, got %d", got)
	}
	if got := counters.UnreadCounters[feedB.ID]; got != 1 {
		t.Fatalf("expected 1 unread entry for feed B, got %d", got)
	}
	if got := counters.ReadCounters[feedA.ID]; got != 1 {
		t.Fatalf("expected 1 read entry for feed A, got %d", got)
	}
	if got := counters.ReadCounters[feedB.ID]; got != 1 {
		t.Fatalf("expected 1 read entry for feed B, got %d", got)
	}

	navMetadata, err = store.GetNavMetadata(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if navMetadata.CountUnread != 2 {
		t.Fatalf("expected the global unread counter to be 2, got %d", navMetadata.CountUnread)
	}
}

func TestFlushHistoryTombstonesSharedEntryForAllFeeds(t *testing.T) {
	store := newTestStorage(t)
	user := createTestUser(t, store)
	category := createTestCategory(t, store, user.ID, "Tech")
	feedA := createTestFeed(t, store, user.ID, category.ID, "https://example.com/a.xml", "https://example.com/")
	feedB := createTestFeed(t, store, user.ID, category.ID, "https://example.com/b.xml", "https://example.com/")

	if _, err := store.RefreshFeedEntries(user.ID, feedA.ID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefreshFeedEntries(user.ID, feedB.ID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false); err != nil {
		t.Fatal(err)
	}

	if err := store.SetEntriesStatus(user.ID, []int64{newEntriesID(t, store)}, model.EntryStatusRead); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushHistory(user.ID); err != nil {
		t.Fatal(err)
	}

	// The tombstone must exist for both feeds.
	var tombstoneCount int
	if err := store.db.QueryRow(`SELECT count(*) FROM entry_tombstones WHERE hash = 'hash-shared'`).Scan(&tombstoneCount); err != nil {
		t.Fatal(err)
	}
	if tombstoneCount != 2 {
		t.Fatalf("expected 2 tombstones for the shared entry, got %d", tombstoneCount)
	}

	// The article must not come back from any of the two feeds.
	for _, feedID := range []int64{feedA.ID, feedB.ID} {
		newEntries, err := store.RefreshFeedEntries(user.ID, feedID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(newEntries) != 0 {
			t.Fatalf("expected the tombstoned entry to stay deleted for feed #%d", feedID)
		}
	}

	counts, err := store.CountAllEntries()
	if err != nil {
		t.Fatal(err)
	}
	if counts["total"] != 0 {
		t.Fatalf("expected no entries, got %d", counts["total"])
	}
}

func TestInsertEntryForFeedDeduplicatesAcrossFeeds(t *testing.T) {
	store := newTestStorage(t)
	user := createTestUser(t, store)
	category := createTestCategory(t, store, user.ID, "Tech")
	feedA := createTestFeed(t, store, user.ID, category.ID, "https://example.com/a.xml", "https://example.com/")
	feedB := createTestFeed(t, store, user.ID, category.ID, "https://example.com/b.xml", "https://example.com/")

	created, err := store.InsertEntryForFeed(user.ID, feedA.ID, newTestEntry("hash-shared", "https://example.com/article"))
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected the first insert to create an entry")
	}

	created, err = store.InsertEntryForFeed(user.ID, feedB.ID, newTestEntry("hash-shared", "https://example.com/article"))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("expected the second insert to reuse the existing entry")
	}

	counts, err := store.CountAllEntries()
	if err != nil {
		t.Fatal(err)
	}
	if counts["total"] != 1 {
		t.Fatalf("expected a single entry, got %d", counts["total"])
	}

	if count := countLinkedFeeds(t, store, newEntriesID(t, store)); count != 2 {
		t.Fatalf("expected the entry to be linked to 2 feeds, got %d", count)
	}
}

func TestIsNewEntryAcrossFeedsOfSameSite(t *testing.T) {
	store := newTestStorage(t)
	user := createTestUser(t, store)
	category := createTestCategory(t, store, user.ID, "Tech")
	feedA := createTestFeed(t, store, user.ID, category.ID, "https://example.com/a.xml", "https://example.com/")
	feedB := createTestFeed(t, store, user.ID, category.ID, "https://example.com/b.xml", "https://example.com/")
	feedOtherSite := createTestFeed(t, store, user.ID, category.ID, "https://other-site.net/c.xml", "https://other-site.net/")

	if !store.IsNewEntry(user.ID, feedA.ID, "hash-shared") {
		t.Fatal("expected the entry to be new before any refresh")
	}

	if _, err := store.RefreshFeedEntries(user.ID, feedA.ID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false); err != nil {
		t.Fatal(err)
	}

	if store.IsNewEntry(user.ID, feedA.ID, "hash-shared") {
		t.Fatal("expected the entry to be known for its own feed")
	}
	if store.IsNewEntry(user.ID, feedB.ID, "hash-shared") {
		t.Fatal("expected the entry to be known for the sibling feed of the same site")
	}
	if !store.IsNewEntry(user.ID, feedOtherSite.ID, "hash-shared") {
		t.Fatal("expected the entry to be new for a feed of a different site")
	}
}

func TestUpdateExistingSharedEntryFromAnyFeed(t *testing.T) {
	store := newTestStorage(t)
	user := createTestUser(t, store)
	category := createTestCategory(t, store, user.ID, "Tech")
	feedA := createTestFeed(t, store, user.ID, category.ID, "https://example.com/a.xml", "https://example.com/")
	feedB := createTestFeed(t, store, user.ID, category.ID, "https://example.com/b.xml", "https://example.com/")

	if _, err := store.RefreshFeedEntries(user.ID, feedA.ID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefreshFeedEntries(user.ID, feedB.ID, model.Entries{newTestEntry("hash-shared", "https://example.com/article")}, false); err != nil {
		t.Fatal(err)
	}

	// An update coming from the second feed updates the shared entry.
	updated := newTestEntry("hash-shared", "https://example.com/article")
	updated.Title = "Updated Title"
	if _, err := store.RefreshFeedEntries(user.ID, feedB.ID, model.Entries{updated}, true); err != nil {
		t.Fatal(err)
	}

	entry, err := store.NewEntryQueryBuilder(user.ID).WithFeedID(feedA.ID).GetEntry()
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected to find the shared entry from feed A")
	}
	if entry.Title != "Updated Title" {
		t.Fatalf("expected the title to be updated, got %q", entry.Title)
	}
}
