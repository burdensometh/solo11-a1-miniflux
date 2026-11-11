// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"miniflux.app/v2/internal/database"
	"miniflux.app/v2/internal/model"
)

// These tests exercise the entry write path under real concurrency against a
// PostgreSQL database. They need a running database, exactly like the API
// integration tests; DATABASE_URL follows the same convention used by the
// daemon and the Makefile. When it is absent (plain `go test ./...` on a
// machine without Postgres) the tests skip instead of failing, so they stay
// safe to run unconditionally in CI stages that provision no database.
const testDatabaseURLEnv = "DATABASE_URL"

// raceParameters is sized so the race window is hit on every run without
// making the suite slow: enough goroutines and entries to overlap
// transactions, few enough to finish in well under a second on Postgres.
const (
	raceGoroutines = 8
	raceEntries    = 12
	// arrivalTimeout is only a failure watchdog: if the harness itself breaks
	// and goroutines never reach the rendez-vous, fail loudly instead of
	// hanging the whole suite forever. It does not influence scheduling.
	arrivalTimeout = 30 * time.Second
)

var (
	testStore  *Storage
	testDB     *sql.DB
	testUserID int64
	testFeedID int64
	testCatID  int64
)

func TestMain(m *testing.M) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		// No database configured: every test skips on its own.
		os.Exit(m.Run())
	}

	db, err := sql.Open(barrierDriverName, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "concurrency test: unable to open database: %v\n", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(raceGoroutines + 2)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "concurrency test: unable to reach database %q: %v\n", dsn, err)
		os.Exit(1)
	}

	if err := database.Migrate(db); err != nil {
		fmt.Fprintf(os.Stderr, "concurrency test: migration failed: %v\n", err)
		os.Exit(1)
	}

	testDB = db
	testStore = NewStorage(db)
	code := m.Run()
	_ = db.Close()
	os.Exit(code)
}

// requireDB skips the test when no database is available.
func requireDB(t *testing.T) *Storage {
	t.Helper()
	if testStore == nil {
		t.Skipf("set %s to a PostgreSQL DSN to run concurrency tests", testDatabaseURLEnv)
	}
	return testStore
}

// resetFixtures wipes entry/feed/category/user state and creates one user,
// category and feed that the tests share. TRUNCATE ... RESTART IDENTITY makes
// each run independent and keeps it fast.
func resetFixtures(t *testing.T) {
	t.Helper()
	_, err := testDB.Exec(`
		TRUNCATE TABLE entry_tombstones, entries, enclosures, feeds, categories, users
		RESTART IDENTITY CASCADE
	`)
	if err != nil {
		t.Fatalf("unable to reset fixtures: %v", err)
	}

	err = testDB.QueryRow(
		`INSERT INTO users (username, password) VALUES ($1, $2) RETURNING id`,
		"race_user", "secret",
	).Scan(&testUserID)
	if err != nil {
		t.Fatalf("unable to create user: %v", err)
	}

	err = testDB.QueryRow(
		`INSERT INTO categories (user_id, title) VALUES ($1, $2) RETURNING id`,
		testUserID, "race_category",
	).Scan(&testCatID)
	if err != nil {
		t.Fatalf("unable to create category: %v", err)
	}

	err = testDB.QueryRow(`
		INSERT INTO feeds (user_id, category_id, title, feed_url, site_url)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		testUserID, testCatID, "race_feed",
		"https://example.invalid/feed.xml", "https://example.invalid/",
	).Scan(&testFeedID)
	if err != nil {
		t.Fatalf("unable to create feed: %v", err)
	}
}

// makeEntries builds n distinct, valid entries for the fixture feed.
func makeEntries(n int) model.Entries {
	now := time.Now().UTC()
	entries := make(model.Entries, 0, n)
	for i := range n {
		entries = append(entries, &model.Entry{
			Hash:       fmt.Sprintf("race-hash-%d", i),
			Title:      fmt.Sprintf("race title %d", i),
			URL:        fmt.Sprintf("https://example.invalid/entry/%d", i),
			Date:       now.Add(time.Duration(i) * time.Minute),
			Content:    fmt.Sprintf("race content %d", i),
			Author:     "race author",
			Language:   "en",
			Tags:       []string{},
			Enclosures: model.EnclosureList{},
		})
	}
	return entries
}

// countRows is an independent source of truth used to reconcile the counters
// reported by the storage layer against the physical table contents.
func countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := testDB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query failed (%s): %v", query, err)
	}
	return n
}

// isEntryInsert matches the atomic entry INSERT issued during a refresh.
func isEntryInsert(query string) bool {
	return strings.Contains(query, "INSERT INTO entries")
}

// runConcurrentRefreshes launches goroutines that all refresh the same feed at
// the same time, optionally funnelling them through a deterministic barrier. It
// returns the number of goroutines that reported an error.
func runConcurrentRefreshes(t *testing.T, entries model.Entries, useBarrier bool) (errorCount int) {
	t.Helper()

	var gate *barrierGate
	if useBarrier {
		gate = newBarrierGate(raceGoroutines)
		barrierDriverSingleton.configure(gate, isEntryInsert)
		t.Cleanup(func() { barrierDriverSingleton.configure(nil, nil) })
	}

	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
		start = make(chan struct{})
	)

	for range raceGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// All goroutines wait for the same release so none can get ahead
			// before the barrier is armed.
			<-start
			_, err := testStore.RefreshFeedEntries(testUserID, testFeedID, cloneEntries(entries), true)
			if err != nil {
				errMu.Lock()
				errorCount++
				errMu.Unlock()
				t.Logf("concurrent refresh returned error: %v", err)
			}
		}()
	}

	close(start)

	if useBarrier {
		// Deterministic rendez-vous: block until every goroutine is parked inside
		// the entry INSERT, observed via the gate's arrival channel rather than a
		// sleep, then release them simultaneously.
		select {
		case <-gate.Arrived():
		case <-time.After(arrivalTimeout):
			gate.Release()
			wg.Wait()
			t.Fatalf("barrier: only %d/%d goroutines reached the entry insert within %s",
				gate.WaiterCount(), raceGoroutines, arrivalTimeout)
		}
		gate.Release()
	}

	wg.Wait()
	return errorCount
}

func cloneEntries(in model.Entries) model.Entries {
	out := make(model.Entries, len(in))
	for i, e := range in {
		cp := *e
		out[i] = &cp
	}
	return out
}

// TestConcurrentRefreshNoLostOrDuplicatedEntries is the regression test for the
// "entry count does not add up" incident. Several workers refresh one feed at
// once; regardless of interleaving the database must end with exactly one row
// per distinct entry, every refresh must succeed, and the storage counters must
// reconcile with the physical rows.
func TestConcurrentRefreshNoLostOrDuplicatedEntries(t *testing.T) {
	store := requireDB(t)
	resetFixtures(t)
	entries := makeEntries(raceEntries)

	errorCount := runConcurrentRefreshes(t, entries, true)

	if errorCount != 0 {
		t.Fatalf("expected zero refresh errors under concurrency, got %d (a unique-constraint race aborts whole refreshes)", errorCount)
	}

	physicalRows := countRows(t, `SELECT count(*) FROM entries WHERE feed_id=$1`, testFeedID)
	if physicalRows != raceEntries {
		t.Fatalf("entry count mismatch: expected exactly %d rows after %d concurrent refreshes, found %d (lost or duplicated entries)",
			raceEntries, raceGoroutines, physicalRows)
	}

	distinctHashes := countRows(t, `SELECT count(DISTINCT hash) FROM entries WHERE feed_id=$1`, testFeedID)
	if distinctHashes != raceEntries {
		t.Fatalf("duplicate hashes detected: %d distinct hashes for %d expected entries", distinctHashes, raceEntries)
	}

	// Reconcile the counters surfaced to the UI with the table contents.
	counters, err := store.FetchCounters(testUserID)
	if err != nil {
		t.Fatalf("FetchCounters failed: %v", err)
	}
	var counterTotal int
	for _, n := range counters.UnreadCounters {
		counterTotal += n
	}
	unreadRows := countRows(t, `SELECT count(*) FROM entries WHERE feed_id=$1 AND status='unread'`, testFeedID)
	if counterTotal != unreadRows {
		t.Fatalf("unread counter (%d) does not match actual unread rows (%d)", counterTotal, unreadRows)
	}
}

// TestConcurrentRefreshWithoutBarrier hammers the same path without the
// scheduling hook. It complements the deterministic test: the barrier test
// proves the exact worst-case interleaving is safe on every run, while this one
// covers the broader set of interleavings. It asserts invariants, not timing,
// so it cannot flake.
func TestConcurrentRefreshWithoutBarrier(t *testing.T) {
	requireDB(t)
	resetFixtures(t)
	entries := makeEntries(raceEntries)

	if errorCount := runConcurrentRefreshes(t, entries, false); errorCount != 0 {
		t.Fatalf("expected zero refresh errors, got %d", errorCount)
	}
	if got := countRows(t, `SELECT count(*) FROM entries WHERE feed_id=$1`, testFeedID); got != raceEntries {
		t.Fatalf("expected %d rows, found %d", raceEntries, got)
	}

	// A second, identical wave must be idempotent: rows already present are
	// updated rather than inserted, so the count must stay constant.
	runConcurrentRefreshes(t, entries, false)
	if got := countRows(t, `SELECT count(*) FROM entries WHERE feed_id=$1`, testFeedID); got != raceEntries {
		t.Fatalf("second refresh wave changed row count: expected %d, found %d", raceEntries, got)
	}
}

// TestConcurrentRefreshTombstoneRespected makes sure the atomic path preserves
// the tombstone guarantee when racing: a deleted entry must never come back as
// a new unread row, even if many refreshes insert it concurrently.
func TestConcurrentRefreshTombstoneRespected(t *testing.T) {
	requireDB(t)
	resetFixtures(t)
	entries := makeEntries(raceEntries)

	// Persist every entry first, then tombstone one of them by deleting it and
	// recording the tombstone the way ArchiveEntries/FlushHistory do.
	if _, err := testStore.RefreshFeedEntries(testUserID, testFeedID, cloneEntries(entries), true); err != nil {
		t.Fatalf("seed refresh failed: %v", err)
	}
	victimHash := entries[0].Hash
	if _, err := testDB.Exec(`DELETE FROM entries WHERE feed_id=$1 AND hash=$2`, testFeedID, victimHash); err != nil {
		t.Fatalf("failed to remove victim entry: %v", err)
	}
	if _, err := testDB.Exec(
		`INSERT INTO entry_tombstones (feed_id, hash) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		testFeedID, victimHash,
	); err != nil {
		t.Fatalf("failed to create tombstone: %v", err)
	}

	if errorCount := runConcurrentRefreshes(t, entries, true); errorCount != 0 {
		t.Fatalf("tombstone race produced errors: %d", errorCount)
	}

	if n := countRows(t, `SELECT count(*) FROM entries WHERE feed_id=$1 AND hash=$2`, testFeedID, victimHash); n != 0 {
		t.Fatalf("tombstoned entry was re-inserted %d times", n)
	}
	// The tombstoned entry stays gone; all others remain present exactly once.
	if got := countRows(t, `SELECT count(*) FROM entries WHERE feed_id=$1`, testFeedID); got != raceEntries-1 {
		t.Fatalf("expected %d surviving entries, found %d", raceEntries-1, got)
	}
}

// TestConcurrentRefreshPreservesEnclosures ensures the atomic insert path does
// not regress attachment handling: an entry carrying an enclosure, inserted by
// exactly one of the racing goroutines, must end with exactly one enclosure
// row. The other goroutines must not duplicate or drop it.
func TestConcurrentRefreshPreservesEnclosures(t *testing.T) {
	requireDB(t)
	resetFixtures(t)

	entries := makeEntries(raceEntries)
	for _, e := range entries {
		e.Enclosures = model.EnclosureList{
			{URL: e.URL + "/audio.mp3", MimeType: "audio/mpeg", Size: 1024},
		}
	}

	if errorCount := runConcurrentRefreshes(t, entries, true); errorCount != 0 {
		t.Fatalf("concurrent refresh with enclosures produced errors: %d", errorCount)
	}

	entryRows := countRows(t, `SELECT count(*) FROM entries WHERE feed_id=$1`, testFeedID)
	if entryRows != raceEntries {
		t.Fatalf("expected %d entries, found %d", raceEntries, entryRows)
	}
	enclosureRows := countRows(t, `SELECT count(*) FROM enclosures e JOIN entries n ON n.id = e.entry_id WHERE n.feed_id=$1`, testFeedID)
	if enclosureRows != raceEntries {
		t.Fatalf("expected exactly %d enclosures (one per entry), found %d", raceEntries, enclosureRows)
	}
}

// TestBarrierDriverActuallyParks is a self-check that the harness has teeth:
// without a release, a barriered query must block; after release it completes.
// This guards against a future refactor silently disabling the rendez-vous and
// turning the deterministic test into a no-op.
func TestBarrierDriverActuallyParks(t *testing.T) {
	requireDB(t)
	resetFixtures(t)

	gate := newBarrierGate(1)
	barrierDriverSingleton.configure(gate, isEntryInsert)
	t.Cleanup(func() { barrierDriverSingleton.configure(nil, nil) })

	completed := make(chan error, 1)
	go func() {
		_, err := testStore.RefreshFeedEntries(testUserID, testFeedID, makeEntries(1), true)
		completed <- err
	}()

	select {
	case err := <-completed:
		t.Fatalf("barriered insert completed before release (err=%v); harness is not stalling statements", err)
	case <-gate.Arrived():
		// Expected: the goroutine is parked at the entry insert.
	}

	gate.Release()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("refresh after barrier release failed: %v", err)
		}
	case <-time.After(arrivalTimeout):
		t.Fatal("refresh did not complete after barrier release")
	}
}
