// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"os"
	"testing"
	"time"

	"miniflux.app/v2/internal/database"
)

// newFeedRefreshLockTestStorage opens a dedicated connection pool to the
// test database. Each storage instance simulates a separate process because
// advisory locks are held per database session.
func newFeedRefreshLockTestStorage(t *testing.T) *Storage {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Set TEST_DATABASE_URL to run the feed refresh lock tests")
	}

	db, err := database.NewConnectionPool(dsn, 1, 5, time.Minute)
	if err != nil {
		t.Fatalf("unable to connect to the test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return NewStorage(db)
}

func TestFeedRefreshLockExcludesConcurrentRefreshes(t *testing.T) {
	store := newFeedRefreshLockTestStorage(t)
	otherProcess := newFeedRefreshLockTestStorage(t)

	const feedID = 1
	const anotherFeedID = 2

	if !store.TryLockFeedRefresh(feedID) {
		t.Fatal("the first lock acquisition should succeed")
	}

	if store.TryLockFeedRefresh(feedID) {
		store.UnlockFeedRefresh(feedID)
		t.Fatal("the same feed must not be locked twice by the same process")
	}

	if otherProcess.TryLockFeedRefresh(feedID) {
		otherProcess.UnlockFeedRefresh(feedID)
		t.Fatal("the same feed must not be locked by another process while the lock is held")
	}

	if !otherProcess.TryLockFeedRefresh(anotherFeedID) {
		t.Fatal("locking a different feed must not be affected")
	}
	otherProcess.UnlockFeedRefresh(anotherFeedID)

	store.UnlockFeedRefresh(feedID)

	if !otherProcess.TryLockFeedRefresh(feedID) {
		t.Fatal("the lock must be acquirable again once released")
	}
	otherProcess.UnlockFeedRefresh(feedID)
}

func TestFeedRefreshLockUnlockWithoutLock(t *testing.T) {
	store := newFeedRefreshLockTestStorage(t)

	const feedID = 1

	// Unlocking a feed that is not locked must be a no-op.
	store.UnlockFeedRefresh(feedID)

	if !store.TryLockFeedRefresh(feedID) {
		t.Fatal("the lock acquisition should succeed")
	}

	store.UnlockFeedRefresh(feedID)
	store.UnlockFeedRefresh(feedID)

	if !store.TryLockFeedRefresh(feedID) {
		t.Fatal("the lock must be acquirable again once released")
	}
	store.UnlockFeedRefresh(feedID)
}
