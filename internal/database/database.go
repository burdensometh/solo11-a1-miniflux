// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package database // import "miniflux.app/v2/internal/database"

import (
	"database/sql"
	"fmt"
	"log/slog"
)

// Migrate executes database migrations.
//
// Two kinds of migrations are supported, both tracked by the same
// schema_version table so a database always converges to one single
// version number:
//
//   - Transactional migrations run each inside their own transaction
//     together with the schema_version update, which makes them atomic.
//   - Concurrent index migrations cannot run inside a transaction
//     (PostgreSQL rejects CREATE INDEX CONCURRENTLY there). They are
//     idempotent and self-healing: a run that was interrupted, killed
//     or crashed leaves at most an invalid index that the next run
//     detects, drops and rebuilds until the index is valid.
func Migrate(db *sql.DB) error {
	var currentVersion int
	db.QueryRow(`SELECT version FROM schema_version`).Scan(&currentVersion)

	slog.Info("Running database migrations",
		slog.Int("current_version", currentVersion),
		slog.Int("latest_version", schemaVersion),
	)

	for version := currentVersion; version < schemaVersion; version++ {
		newVersion := version + 1

		if spec, ok := concurrentIndexMigrations[version]; ok {
			if err := runConcurrentIndexMigration(db, spec); err != nil {
				return fmt.Errorf("[Migration v%d] %v", newVersion, err)
			}
			if err := bumpSchemaVersion(db, newVersion); err != nil {
				return fmt.Errorf("[Migration v%d] %v", newVersion, err)
			}
			continue
		}

		if version >= len(migrations) {
			return fmt.Errorf("[Migration v%d] no migration is registered for this version", newVersion)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("[Migration v%d] %v", newVersion, err)
		}

		if err := migrations[version](tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("[Migration v%d] %v", newVersion, err)
		}

		if err := bumpSchemaVersionInTx(tx, newVersion); err != nil {
			tx.Rollback()
			return fmt.Errorf("[Migration v%d] %v", newVersion, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("[Migration v%d] %v", newVersion, err)
		}
	}

	return nil
}

func bumpSchemaVersion(db *sql.DB, newVersion int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := bumpSchemaVersionInTx(tx, newVersion); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func bumpSchemaVersionInTx(tx *sql.Tx, newVersion int) error {
	if _, err := tx.Exec(`TRUNCATE schema_version`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES ($1)`, newVersion); err != nil {
		return err
	}
	return nil
}

// IsSchemaUpToDate checks if the database schema is up to date.
func IsSchemaUpToDate(db *sql.DB) error {
	var currentVersion int
	db.QueryRow(`SELECT version FROM schema_version`).Scan(&currentVersion)
	if currentVersion < schemaVersion {
		return fmt.Errorf(`the database schema is not up to date: current=v%d expected=v%d`, currentVersion, schemaVersion)
	}
	return nil
}
