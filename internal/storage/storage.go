// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"context"
	"database/sql"
	"time"
)

// Storage handles all operations related to the database.
type Storage struct {
	db                *sql.DB
	feedRefreshLocker *feedRefreshLocker
}

// NewStorage returns a new Storage.
func NewStorage(db *sql.DB) *Storage {
	return &Storage{
		db:                db,
		feedRefreshLocker: newFeedRefreshLocker(db),
	}
}

// TryLockFeedRefresh attempts to acquire an exclusive lock on the feed to
// guarantee that only one refresh runs at a time for a given subscription,
// even when several processes share the same database.
//
// The lock is implemented with a PostgreSQL session-level advisory lock: it
// is released automatically by the database if the process crashes or is
// killed, so a feed can never stay locked forever. It returns false when the
// feed is already being refreshed.
func (s *Storage) TryLockFeedRefresh(feedID int64) bool {
	return s.feedRefreshLocker.tryLock(feedID)
}

// UnlockFeedRefresh releases the lock previously acquired with
// TryLockFeedRefresh.
func (s *Storage) UnlockFeedRefresh(feedID int64) {
	s.feedRefreshLocker.unlock(feedID)
}

// DatabaseVersion returns the version of the database which is in use.
func (s *Storage) DatabaseVersion() string {
	var dbVersion string
	err := s.db.QueryRow(`SELECT current_setting('server_version')`).Scan(&dbVersion)
	if err != nil {
		return err.Error()
	}

	return dbVersion
}

// Ping checks if the database connection works.
func (s *Storage) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return s.db.PingContext(ctx)
}

// DBStats returns database statistics.
func (s *Storage) DBStats() sql.DBStats {
	return s.db.Stats()
}

// DBSize returns how much size the database is using in a pretty way.
func (s *Storage) DBSize() (string, error) {
	var size string

	err := s.db.QueryRow("SELECT pg_size_pretty(pg_database_size(current_database()))").Scan(&size)
	if err != nil {
		return "", err
	}

	return size, nil
}
