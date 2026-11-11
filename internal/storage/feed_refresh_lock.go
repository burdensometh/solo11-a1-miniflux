// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"
)

// feedRefreshLockNamespace is the 32-bit advisory lock namespace used for
// feed refresh locks ("MFLX" in hexadecimal). It avoids collisions with
// other applications using advisory locks on the same database.
const feedRefreshLockNamespace int32 = 0x4d464c58

// feedRefreshLockTimeout is the maximum time to wait for the dedicated lock
// connection or for a lock/unlock round-trip to the database.
const feedRefreshLockTimeout = 5 * time.Second

// feedRefreshLocker provides a per-feed mutex implemented with PostgreSQL
// session-level advisory locks.
//
// All the locks are held on a single dedicated database connection shared by
// the entire process, so the number of locks is not limited by the
// connection pool size. Because advisory locks are bound to the database
// session, PostgreSQL releases them automatically if the process crashes or
// is killed: a feed can never stay locked forever.
type feedRefreshLocker struct {
	db   *sql.DB
	mu   sync.Mutex
	conn *sql.Conn
	held map[int64]struct{}
}

func newFeedRefreshLocker(db *sql.DB) *feedRefreshLocker {
	return &feedRefreshLocker{
		db:   db,
		held: make(map[int64]struct{}),
	}
}

// tryLock attempts to acquire the refresh lock for the given feed without
// blocking. It returns false when the feed is already locked by this process
// or by another one, or when the lock could not be acquired due to a
// database error.
func (l *feedRefreshLocker) tryLock(feedID int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, found := l.held[feedID]; found {
		// Advisory locks are reentrant within the same database session:
		// check the local state first to avoid acquiring the lock twice.
		return false
	}

	for range 2 {
		conn, err := l.connection()
		if err != nil {
			slog.Error("Unable to pin a database connection for the feed refresh lock",
				slog.Int64("feed_id", feedID),
				slog.Any("error", err),
			)
			return false
		}

		var acquired bool
		ctx, cancel := context.WithTimeout(context.Background(), feedRefreshLockTimeout)
		err = conn.QueryRowContext(ctx,
			`SELECT pg_try_advisory_lock($1, $2)`,
			feedRefreshLockNamespace, int32(feedID),
		).Scan(&acquired)
		cancel()

		if err != nil {
			// The dedicated connection is probably broken: drop it, which
			// also releases every lock it was holding, and retry once with
			// a fresh connection.
			slog.Warn("Unable to acquire the feed refresh lock, resetting the dedicated lock connection",
				slog.Int64("feed_id", feedID),
				slog.Any("error", err),
			)
			l.reset()
			continue
		}

		if acquired {
			l.held[feedID] = struct{}{}
		} else if len(l.held) == 0 {
			// Keep the dedicated connection pinned only while it holds locks.
			l.unpin()
		}

		return acquired
	}

	return false
}

// unlock releases the refresh lock for the given feed. It is a no-op when
// the lock is not held by this process.
func (l *feedRefreshLocker) unlock(feedID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, found := l.held[feedID]; !found {
		return
	}
	delete(l.held, feedID)

	if l.conn != nil {
		var released bool
		ctx, cancel := context.WithTimeout(context.Background(), feedRefreshLockTimeout)
		err := l.conn.QueryRowContext(ctx,
			`SELECT pg_advisory_unlock($1, $2)`,
			feedRefreshLockNamespace, int32(feedID),
		).Scan(&released)
		cancel()

		if err != nil {
			// The lock is released anyway when the connection is closed.
			slog.Warn("Unable to release the feed refresh lock, resetting the dedicated lock connection",
				slog.Int64("feed_id", feedID),
				slog.Any("error", err),
			)
			l.reset()
			return
		}

		if !released {
			slog.Warn("The feed refresh lock was not held by the database session",
				slog.Int64("feed_id", feedID),
			)
		}
	}

	if len(l.held) == 0 {
		l.unpin()
	}
}

// connection returns the dedicated lock connection, pinning a new one from
// the pool if necessary. The caller must hold the mutex.
func (l *feedRefreshLocker) connection() (*sql.Conn, error) {
	if l.conn != nil {
		return l.conn, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), feedRefreshLockTimeout)
	defer cancel()

	conn, err := l.db.Conn(ctx)
	if err != nil {
		return nil, err
	}

	l.conn = conn
	return l.conn, nil
}

// reset closes the dedicated lock connection and forgets all the locks it
// was holding. Closing the connection makes PostgreSQL release every
// advisory lock held by the session. The caller must hold the mutex.
func (l *feedRefreshLocker) reset() {
	l.unpin()
	clear(l.held)
}

// unpin returns the dedicated lock connection to the pool.
// The caller must hold the mutex.
func (l *feedRefreshLocker) unpin() {
	if l.conn != nil {
		l.conn.Close()
		l.conn = nil
	}
}
