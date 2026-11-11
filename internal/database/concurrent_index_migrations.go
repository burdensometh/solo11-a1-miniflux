// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package database // import "miniflux.app/v2/internal/database"

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lib/pq"
)

// concurrentIndexMigrationVersion is the schema version whose work is
// performed out of a transaction because CREATE INDEX CONCURRENTLY is
// not allowed inside one. It must stay in sync with the length of the
// transactional migrations array (the first transactional migration
// builds version 1, so this one builds version len(migrations)+1).
const concurrentIndexMigrationVersion = len(migrations)

// indexState describes the current state of an index as observed in
// the catalog.
type indexState int

const (
	// indexAbsent means no index with the target name exists on the
	// target table.
	indexAbsent indexState = iota
	// indexValid means the index exists on the target table and is
	// ready for use.
	indexValid
	// indexInvalid means the index exists but is not usable, typically
	// because a previous CREATE INDEX CONCURRENTLY was interrupted.
	indexInvalid
)

// concurrentIndexSpec describes an index that must end up existing and
// valid. The table is required because index names are only unique per
// schema: an index created on another table must not be mistaken for
// the expected one.
type concurrentIndexSpec struct {
	tableName string
	indexName string
	createDDL string
}

// concurrentIndexMigrations maps a zero-based migration position (the
// same indexing as the migrations array) to a concurrent index
// migration.
var concurrentIndexMigrations = map[int]concurrentIndexSpec{
	// The subscription list page (GET /feeds) computes the per-feed
	// read/unread counters and the global unread count with queries of
	// the shape:
	//
	//	SELECT feed_id, status, count(*) FROM entries
	//	WHERE user_id = $1 AND status IN ('read', 'unread')
	//	GROUP BY feed_id, status
	//
	// On a database with a thousand feeds and well over a million
	// entries, a missing or invalid covering index makes the planner
	// fall back to a sequential scan of the whole entries table, which
	// takes several seconds. This covering index allows an index-only
	// scan for both the counter query and the navigation unread count.
	concurrentIndexMigrationVersion: {
		tableName: "entries",
		indexName: "entries_user_status_feed_idx",
		createDDL: `CREATE INDEX CONCURRENTLY entries_user_status_feed_idx ON entries (user_id, status, feed_id)`,
	},
}

// advisoryLockConcurrentIndex serializes concurrent index work across
// every application instance sharing the database. Arbitrary fixed
// key, chosen once and never reused by another lock.
const advisoryLockConcurrentIndex = 88_341_777

// sessionSetting captures a GUC's value before the migration changes
// it, so the exact previous value can be restored on a connection that
// goes back to the pool.
type sessionSetting struct {
	name  string
	value sql.NullString
}

// runConcurrentIndexMigration makes sure the index described by spec
// exists and is valid, without taking an ACCESS EXCLUSIVE lock and
// without blocking writes for the duration of the index build.
//
// It is idempotent and self-healing:
//   - valid index: nothing is done;
//   - missing index: built with CREATE INDEX CONCURRENTLY;
//   - invalid index (for example left over from a crashed upgrade):
//     dropped with DROP INDEX CONCURRENTLY and rebuilt.
//
// A failure at any point leaves the schema_version untouched, so the
// next startup simply retries and converges to the same final state.
func runConcurrentIndexMigration(db *sql.DB, spec concurrentIndexSpec) error {
	state, err := inspectIndexState(db, spec)
	if err != nil {
		return err
	}
	if state == indexValid {
		return nil
	}

	conn, err := db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("unable to acquire database connection: %w", err)
	}
	// Defers run last-registered first: conn.Close is registered first
	// so it runs last, after the settings have been restored.
	defer conn.Close()

	originalSettings, err := saveSessionSettings(conn)
	if err != nil {
		return err
	}
	defer func() {
		if err := restoreSessionSettings(conn, originalSettings); err != nil {
			slog.Warn("Unable to restore database session settings after index migration",
				slog.Any("error", err),
			)
		}
	}()

	// A large table can legitimately take a long time to index. Disable
	// the relevant timeouts for this connection so a timeout cannot
	// abort the build halfway and leave an invalid index behind. The
	// previous values are restored before the connection returns to the
	// pool.
	for _, name := range []string{"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout"} {
		if _, err := conn.ExecContext(context.Background(), `SET `+name+` = 0`); err != nil {
			return fmt.Errorf("unable to disable %s: %w", name, err)
		}
	}

	// Serialize the actual index work across every application instance
	// sharing the database. pg_try_advisory_lock is used instead of the
	// blocking pg_advisory_lock on purpose: two backends waiting on the
	// same session-level key would otherwise be reported as a deadlock
	// by PostgreSQL. A backend that cannot get the lock simply waits
	// and retries; the lock is released when the connection closes or
	// the process dies, so a crashed upgrade cannot hold it forever.
	const (
		lockRetryInterval = 2 * time.Second
		lockAcquireLimit  = 1000 // ~33 minutes at 2s intervals
	)
	var acquired bool
	for attempt := 0; attempt < lockAcquireLimit; attempt++ {
		if err := conn.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock($1)`, advisoryLockConcurrentIndex).Scan(&acquired); err != nil {
			return fmt.Errorf("unable to acquire advisory lock: %w", err)
		}
		if acquired {
			break
		}
		if attempt == 0 || attempt%15 == 0 {
			slog.Info("Another upgrade process is building database indexes, waiting",
				slog.String("index", spec.indexName),
			)
		}
		time.Sleep(lockRetryInterval)
	}
	if !acquired {
		return fmt.Errorf("timed out waiting for the advisory lock held by another migration process")
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockConcurrentIndex)
	}()

	// Re-check after acquiring the lock: another process may have done
	// the work while we were waiting.
	state, err = inspectIndexStateOnConn(conn, spec)
	if err != nil {
		return err
	}
	if state == indexValid {
		return nil
	}
	if state == indexInvalid {
		if _, err := conn.ExecContext(context.Background(), `DROP INDEX CONCURRENTLY `+pq.QuoteIdentifier(spec.indexName)); err != nil {
			return fmt.Errorf("unable to drop invalid index %q: %w", spec.indexName, err)
		}
	}

	if _, err := conn.ExecContext(context.Background(), spec.createDDL); err != nil {
		return fmt.Errorf("unable to create index %q concurrently: %w", spec.indexName, err)
	}

	// CREATE INDEX CONCURRENTLY that fails leaves the index behind in
	// an invalid state rather than returning an error for every cause.
	// Verify the outcome so a failed build is reported and retried on
	// the next run instead of being silently marked as done.
	state, err = inspectIndexStateOnConn(conn, spec)
	if err != nil {
		return err
	}
	if state != indexValid {
		return fmt.Errorf("index %q is not valid after creation; it will be rebuilt on the next migration run", spec.indexName)
	}

	return nil
}

// saveSessionSettings reads the current value of each timeout GUC.
// current_setting with missing_ok=true returns NULL for a parameter
// that was never explicitly set, which is recorded so restore can
// issue RESET for it.
func saveSessionSettings(conn *sql.Conn) ([]sessionSetting, error) {
	names := []string{"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout"}
	settings := make([]sessionSetting, 0, len(names))
	for _, name := range names {
		var value sql.NullString
		if err := conn.QueryRowContext(context.Background(), `SELECT current_setting($1, true)`, name).Scan(&value); err != nil {
			return nil, fmt.Errorf("unable to read current value of %q: %w", name, err)
		}
		settings = append(settings, sessionSetting{name: name, value: value})
	}
	return settings, nil
}

// restoreSessionSettings puts every GUC back to the value captured by
// saveSessionSettings. An unset parameter is reset to its default.
func restoreSessionSettings(conn *sql.Conn, settings []sessionSetting) error {
	for _, setting := range settings {
		if setting.value.Valid {
			if _, err := conn.ExecContext(context.Background(), `SET `+setting.name+` = `+pq.QuoteLiteral(setting.value.String)); err != nil {
				return fmt.Errorf("unable to restore %q: %w", setting.name, err)
			}
		} else if _, err := conn.ExecContext(context.Background(), `RESET `+setting.name); err != nil {
			return fmt.Errorf("unable to reset %q: %w", setting.name, err)
		}
	}
	return nil
}

func inspectIndexState(db *sql.DB, spec concurrentIndexSpec) (indexState, error) {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return indexAbsent, err
	}
	defer conn.Close()
	return inspectIndexStateOnConn(conn, spec)
}

func inspectIndexStateOnConn(conn *sql.Conn, spec concurrentIndexSpec) (indexState, error) {
	var valid bool
	if err := conn.QueryRowContext(context.Background(), `
		SELECT i.indisvalid
		FROM pg_class idx
		JOIN pg_namespace n ON n.oid = idx.relnamespace
		JOIN pg_index i ON i.indexrelid = idx.oid
		JOIN pg_class tbl ON tbl.oid = i.indrelid
		WHERE idx.relkind = 'i'
			AND idx.relname = $1
			AND tbl.relname = $2
			AND n.nspname = current_schema()`, spec.indexName, spec.tableName).Scan(&valid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return indexAbsent, nil
		}
		return indexAbsent, fmt.Errorf("unable to inspect index %q: %w", spec.indexName, err)
	}

	if valid {
		return indexValid, nil
	}
	return indexInvalid, nil
}
