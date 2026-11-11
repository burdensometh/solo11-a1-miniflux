// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"database/sql"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	_ "github.com/lib/pq"
)

// scratchDatabase is created and destroyed per test run; no production
// database is ever touched.
const scratchDatabase = "miniflux_migration_test"

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("Set DATABASE_URL to run PostgreSQL-backed migration tests")
	}
	return dsn
}

// maintenanceDSN rewrites the DSN so it points at the default
// "postgres" maintenance database, which is needed to create and drop
// the scratch database.
func maintenanceDSN(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("invalid DATABASE_URL: %v", err)
	}
	u.Path = "/postgres"
	return u.String()
}

func openScratch(t *testing.T) *sql.DB {
	t.Helper()
	dsn := testDSN(t)

	admin, err := sql.Open("postgres", maintenanceDSN(t, dsn))
	if err != nil {
		t.Fatalf("unable to open maintenance connection: %v", err)
	}
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + scratchDatabase); err != nil {
		admin.Close()
		t.Fatalf("unable to drop scratch database: %v", err)
	}
	if _, err := admin.Exec(`CREATE DATABASE ` + scratchDatabase); err != nil {
		admin.Close()
		t.Fatalf("unable to create scratch database: %v", err)
	}
	admin.Close()

	t.Cleanup(func() {
		c, err := sql.Open("postgres", maintenanceDSN(t, dsn))
		if err == nil {
			_, _ = c.Exec(`DROP DATABASE IF EXISTS ` + scratchDatabase)
			c.Close()
		}
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("invalid DATABASE_URL: %v", err)
	}
	u.Path = "/" + scratchDatabase

	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatalf("unable to open scratch connection: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("unable to reach scratch database: %v", err)
	}
	return db
}

func currentSchemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("unable to read schema_version: %v", err)
	}
	return version
}

// seedMinimalSchema creates only the objects the index migration
// inspects. It must not rely on the full migrator so that individual
// healing paths can be tested in isolation.
func seedMinimalSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
		CREATE TYPE entry_status AS ENUM ('unread', 'read', 'removed');
		CREATE TABLE schema_version (version text not null);
		CREATE TABLE entries (
			id bigserial primary key,
			user_id int not null,
			feed_id bigint not null,
			hash text not null,
			published_at timestamp with time zone not null,
			title text not null,
			url text not null,
			status entry_status default 'unread'
		);
	`); err != nil {
		t.Fatalf("unable to seed minimal schema: %v", err)
	}
}

func TestMigrateCreatesIndexesConcurrentlyOnFreshDatabase(t *testing.T) {
	db := openScratch(t)

	if err := Migrate(db); err != nil {
		t.Fatalf("initial migration failed: %v", err)
	}
	if got := currentSchemaVersion(t, db); got != schemaVersion {
		t.Fatalf("schema version after migration = %d, want %d", got, schemaVersion)
	}

	spec := concurrentIndexMigrations[concurrentIndexMigrationVersion]
	state, err := inspectIndexState(db, spec)
	if err != nil {
		t.Fatalf("inspectIndexState failed: %v", err)
	}
	if state != indexValid {
		t.Fatalf("index state = %d, want indexValid", state)
	}

	// A second run must be a no-op and converge to the same state.
	if err := Migrate(db); err != nil {
		t.Fatalf("second migration run failed: %v", err)
	}
	if got := currentSchemaVersion(t, db); got != schemaVersion {
		t.Fatalf("schema version after re-run = %d, want %d", got, schemaVersion)
	}
	state, err = inspectIndexState(db, spec)
	if err != nil {
		t.Fatalf("inspectIndexState after re-run failed: %v", err)
	}
	if state != indexValid {
		t.Fatalf("index state after re-run = %d, want indexValid", state)
	}
}

func TestConcurrentIndexMigrationHealsMissingIndex(t *testing.T) {
	db := openScratch(t)

	if err := Migrate(db); err != nil {
		t.Fatalf("initial migration failed: %v", err)
	}
	spec := concurrentIndexMigrations[concurrentIndexMigrationVersion]

	// Simulate an upgrade that bumped the version but lost the index.
	if _, err := db.Exec(`DROP INDEX ` + spec.indexName); err != nil {
		t.Fatalf("unable to drop index: %v", err)
	}
	if _, err := db.Exec(`UPDATE schema_version SET version = $1`, concurrentIndexMigrationVersion); err != nil {
		t.Fatalf("unable to reset schema version: %v", err)
	}

	state, err := inspectIndexState(db, spec)
	if err != nil {
		t.Fatalf("inspectIndexState failed: %v", err)
	}
	if state != indexAbsent {
		t.Fatalf("index state = %d, want indexAbsent", state)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("healing migration failed: %v", err)
	}
	if got := currentSchemaVersion(t, db); got != schemaVersion {
		t.Fatalf("schema version after healing = %d, want %d", got, schemaVersion)
	}
	state, err = inspectIndexState(db, spec)
	if err != nil {
		t.Fatalf("inspectIndexState after healing failed: %v", err)
	}
	if state != indexValid {
		t.Fatalf("index state after healing = %d, want indexValid", state)
	}
}

func TestConcurrentIndexMigrationHealsInvalidIndex(t *testing.T) {
	db := openScratch(t)
	spec := concurrentIndexMigrations[concurrentIndexMigrationVersion]

	seedMinimalSchema(t, db)
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES ($1)`, concurrentIndexMigrationVersion); err != nil {
		t.Fatalf("unable to set schema version: %v", err)
	}
	if _, err := db.Exec(`CREATE INDEX ` + spec.indexName + ` ON entries (user_id)`); err != nil {
		t.Fatalf("unable to create placeholder index: %v", err)
	}
	// An interrupted CREATE INDEX CONCURRENTLY leaves exactly this state.
	if _, err := db.Exec(`UPDATE pg_index SET indisvalid = false WHERE indexrelid = $1::regclass`, spec.indexName); err != nil {
		t.Fatalf("unable to mark index invalid: %v", err)
	}

	state, err := inspectIndexState(db, spec)
	if err != nil {
		t.Fatalf("inspectIndexState failed: %v", err)
	}
	if state != indexInvalid {
		t.Fatalf("index state = %d, want indexInvalid", state)
	}

	if err := runConcurrentIndexMigration(db, spec); err != nil {
		t.Fatalf("healing migration failed: %v", err)
	}

	state, err = inspectIndexState(db, spec)
	if err != nil {
		t.Fatalf("inspectIndexState after healing failed: %v", err)
	}
	if state != indexValid {
		t.Fatalf("index state after healing = %d, want indexValid", state)
	}

	// The rebuilt index must carry the expected definition rather than
	// the placeholder's single column.
	var indexDef string
	if err := db.QueryRow(`SELECT indexdef FROM pg_indexes WHERE indexname = $1`, spec.indexName).Scan(&indexDef); err != nil {
		t.Fatalf("unable to read index definition: %v", err)
	}
	for _, want := range []string{"user_id", "status", "feed_id"} {
		if !strings.Contains(indexDef, want) {
			t.Fatalf("index definition %q is missing column %q", indexDef, want)
		}
	}
}

func TestInspectIndexStateDistinguishesIndexesOnOtherTables(t *testing.T) {
	db := openScratch(t)
	seedMinimalSchema(t, db)
	spec := concurrentIndexMigrations[concurrentIndexMigrationVersion]

	if _, err := db.Exec(`CREATE TABLE other_table (id bigserial primary key, user_id int)`); err != nil {
		t.Fatalf("unable to create other_table: %v", err)
	}
	// Same index name, different table: must not count as the target.
	if _, err := db.Exec(`CREATE INDEX ` + spec.indexName + ` ON other_table (user_id)`); err != nil {
		t.Fatalf("unable to create decoy index: %v", err)
	}

	state, err := inspectIndexState(db, spec)
	if err != nil {
		t.Fatalf("inspectIndexState failed: %v", err)
	}
	if state != indexAbsent {
		t.Fatalf("index state = %d, want indexAbsent for an index on another table", state)
	}
}

func TestConcurrentIndexMigrationIsSafeToRunInParallel(t *testing.T) {
	db := openScratch(t)
	seedMinimalSchema(t, db)
	spec := concurrentIndexMigrations[concurrentIndexMigrationVersion]
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES ($1)`, concurrentIndexMigrationVersion); err != nil {
		t.Fatalf("unable to set schema version: %v", err)
	}

	const runners = 3
	var wg sync.WaitGroup
	errs := make(chan error, runners)
	for range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- runConcurrentIndexMigration(db, spec)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("parallel migration run failed: %v", err)
		}
	}

	state, err := inspectIndexState(db, spec)
	if err != nil {
		t.Fatalf("inspectIndexState failed: %v", err)
	}
	if state != indexValid {
		t.Fatalf("index state = %d, want indexValid", state)
	}
}

func TestHealedIndexServesTheFeedCounterQuery(t *testing.T) {
	db := openScratch(t)

	if err := Migrate(db); err != nil {
		t.Fatalf("initial migration failed: %v", err)
	}
	spec := concurrentIndexMigrations[concurrentIndexMigrationVersion]

	// Populate enough rows that a sequential scan would be the default
	// fallback plan without a usable covering index.
	if _, err := db.Exec(`
		INSERT INTO users (username, password) VALUES ('counter-user', 'x');
		INSERT INTO categories (user_id, title) VALUES (1, 'cat');
		INSERT INTO feeds (user_id, category_id, title, feed_url, site_url)
			SELECT 1, 1, 'feed-' || i, 'https://example.com/' || i, 'https://example.com/' || i
			FROM generate_series(1, 200) AS i;
		INSERT INTO entries (user_id, feed_id, hash, published_at, changed_at, title, url, status)
			SELECT 1, f.id, 'h-' || f.id || '-' || e, now(), now(), 't', 'u',
				CASE WHEN e % 5 = 0 THEN 'unread'::entry_status ELSE 'read'::entry_status END
			FROM feeds f CROSS JOIN generate_series(1, 500) AS e;
	`); err != nil {
		t.Fatalf("unable to seed entries: %v", err)
	}
	if _, err := db.Exec(`VACUUM (ANALYZE) entries`); err != nil {
		t.Fatalf("vacuum failed: %v", err)
	}

	// Break and heal the index to exercise the exact production path.
	if _, err := db.Exec(`DROP INDEX ` + spec.indexName); err != nil {
		t.Fatalf("unable to drop index: %v", err)
	}
	if err := runConcurrentIndexMigration(db, spec); err != nil {
		t.Fatalf("healing migration failed: %v", err)
	}
	if _, err := db.Exec(`VACUUM (ANALYZE) entries`); err != nil {
		t.Fatalf("vacuum failed: %v", err)
	}

	rows, err := db.Query(`
		EXPLAIN
		SELECT e.feed_id, e.status, count(*)
		FROM entries e
		WHERE e.user_id = 1 AND e.status IN ('read', 'unread')
		GROUP BY e.feed_id, e.status
	`)
	if err != nil {
		t.Fatalf("EXPLAIN failed: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if !strings.Contains(plan.String(), spec.indexName) {
		t.Fatalf("counter query does not use the healed index:\n%s", plan.String())
	}

	// And the query must still return correct counts.
	var totalGroups int
	if err := db.QueryRow(`
		SELECT count(*) FROM (
			SELECT e.feed_id, e.status, count(*)
			FROM entries e
			WHERE e.user_id = 1 AND e.status IN ('read', 'unread')
			GROUP BY e.feed_id, e.status
		) g
	`).Scan(&totalGroups); err != nil {
		t.Fatalf("counter query failed: %v", err)
	}
	if totalGroups == 0 {
		t.Fatal("counter query returned no groups")
	}
}

func TestAdvisoryLockIsReleasedAfterMigration(t *testing.T) {
	db := openScratch(t)
	seedMinimalSchema(t, db)
	spec := concurrentIndexMigrations[concurrentIndexMigrationVersion]
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES ($1)`, concurrentIndexMigrationVersion); err != nil {
		t.Fatalf("unable to set schema version: %v", err)
	}

	if err := runConcurrentIndexMigration(db, spec); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	var held bool
	if err := db.QueryRow(`SELECT pg_try_advisory_lock($1)`, advisoryLockConcurrentIndex).Scan(&held); err != nil {
		t.Fatalf("unable to try advisory lock: %v", err)
	}
	if !held {
		t.Fatal("advisory lock is still held after the migration finished")
	}
	if _, err := db.Exec(`SELECT pg_advisory_unlock($1)`, advisoryLockConcurrentIndex); err != nil {
		t.Fatalf("unable to release test lock: %v", err)
	}
}

func TestMigrationDoesNotLeakDisabledTimeoutsIntoTheConnectionPool(t *testing.T) {
	db := openScratch(t)
	// Force every statement onto the single physical connection the
	// migration used, otherwise the pool could hand out a fresh
	// connection and the test could pass while GUCs still leak.
	db.SetMaxOpenConns(1)
	seedMinimalSchema(t, db)
	spec := concurrentIndexMigrations[concurrentIndexMigrationVersion]
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES ($1)`, concurrentIndexMigrationVersion); err != nil {
		t.Fatalf("unable to set schema version: %v", err)
	}

	// Establish known non-zero session values and capture how the
	// server normalizes them through SHOW.
	names := []string{"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout"}
	rawValues := map[string]string{
		"statement_timeout":                   "30000",
		"lock_timeout":                        "20000",
		"idle_in_transaction_session_timeout": "40000",
	}
	baseline := make(map[string]string, len(names))
	for _, name := range names {
		if _, err := db.Exec(`SET ` + name + ` = ` + rawValues[name]); err != nil {
			t.Fatalf("unable to set default for %s: %v", name, err)
		}
		var shown string
		if err := db.QueryRow(`SHOW ` + name).Scan(&shown); err != nil {
			t.Fatalf("unable to read %s: %v", name, err)
		}
		if shown == "0" || shown == "0ms" {
			t.Fatalf("test setup for %s did not take effect", name)
		}
		baseline[name] = shown
	}

	if err := runConcurrentIndexMigration(db, spec); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// The migration temporarily sets every timeout to zero; after it
	// returns, the pooled connection must show the exact previous
	// values again.
	for _, name := range names {
		var got string
		if err := db.QueryRow(`SHOW ` + name).Scan(&got); err != nil {
			t.Fatalf("unable to read %s: %v", name, err)
		}
		if got != baseline[name] {
			t.Fatalf("%s = %q after migration, want %q (disabled timeout leaked into the pooled connection)", name, got, baseline[name])
		}
	}
}
