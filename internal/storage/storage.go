// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// dbQuerier is implemented by both *sql.DB and *sql.Tx. It lets the same query
// helpers run either directly or inside a caller-provided transaction.
type dbQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Storage handles all operations related to the database.
type Storage struct {
	db *sql.DB
}

// NewStorage returns a new Storage.
func NewStorage(db *sql.DB) *Storage {
	return &Storage{db}
}

// RunInTransaction executes fn in a database transaction. The transaction is
// committed only when fn returns nil; any error (or panic) rolls everything
// back, so callers never observe a partially applied operation.
func (s *Storage) RunInTransaction(fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: unable to begin transaction: %w", err)
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			_ = tx.Rollback()
			panic(recovered)
		}
		if err != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				err = fmt.Errorf("%w (rollback failed: %v)", err, rollbackErr)
			}
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: unable to commit transaction: %w", err)
	}

	return nil
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
