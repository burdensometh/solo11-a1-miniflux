// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package database // import "miniflux.app/v2/internal/database"

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// NewConnectionPool configures the database connection pool.
//
// Every connection handed out by the pool is initialized with a
// statement_timeout and lock_timeout so that a query waiting on a row lock or
// a stalled backend cannot block a goroutine forever. Without these timeouts a
// single hung query permanently ties up a worker, and once all workers are
// stuck the scheduler blocks on the job queue and no feed is refreshed again
// until the process is restarted.
func NewConnectionPool(dsn string, minConnections, maxConnections int, connectionLifetime, queryTimeout, lockTimeout time.Duration) (*sql.DB, error) {
	// Enable TCP keepalives on the driver level as well. This makes the
	// operating system detect a half-open connection (e.g. after a NAT table
	// expiry or a silent network drop) instead of blocking on a socket that
	// will never answer again. Values are only applied when the DSN does not
	// already specify them, so explicit user configuration always wins.
	dsn, err := withDefaultKeepalives(dsn)
	if err != nil {
		return nil, err
	}

	connector, err := pq.NewConnector(dsn)
	if err != nil {
		return nil, err
	}

	db := sql.OpenDB(&timeoutConnector{
		Connector:    connector,
		queryTimeout: queryTimeout,
		lockTimeout:  lockTimeout,
	})

	db.SetMaxOpenConns(maxConnections)
	db.SetMaxIdleConns(minConnections)
	db.SetConnMaxLifetime(connectionLifetime)

	return db, nil
}

// timeoutConnector wraps the PostgreSQL connector and configures a bounded
// statement_timeout and lock_timeout on every newly established connection.
type timeoutConnector struct {
	driver.Connector
	queryTimeout time.Duration
	lockTimeout  time.Duration
}

func (c *timeoutConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}

	// The setup runs on the raw driver connection before it is exposed to the
	// pool. Executing the SET commands through the standard driver interface
	// keeps this independent of the concrete connection type.
	initSQL := fmt.Sprintf(
		"SET statement_timeout = %d; SET lock_timeout = %d;",
		c.queryTimeout.Milliseconds(),
		c.lockTimeout.Milliseconds(),
	)

	switch execer := conn.(type) {
	case driver.ExecerContext:
		if _, err := execer.ExecContext(ctx, initSQL, nil); err != nil {
			conn.Close()
			return nil, fmt.Errorf("database: unable to initialize connection timeouts: %w", err)
		}
	case driver.Execer:
		if _, err := execer.Exec(initSQL, nil); err != nil {
			conn.Close()
			return nil, fmt.Errorf("database: unable to initialize connection timeouts: %w", err)
		}
	default:
		conn.Close()
		return nil, errors.New("database: driver does not support executing statements on raw connections")
	}

	return conn, nil
}

// withDefaultKeepalives makes sure TCP keepalives are enabled unless the DSN
// overrides them. Both keyword/value DSNs and URL DSNs are supported.
func withDefaultKeepalives(dsn string) (string, error) {
	parsed, err := pq.ParseURL(dsn)
	if err == nil {
		dsn = parsed
	}

	for _, part := range splitDSN(dsn) {
		if strings.HasPrefix(part, "keepalives") {
			return dsn, nil
		}
	}

	return dsn + " keepalives=1 keepalives_idle=30 keepalives_interval=10 keepalives_count=3", nil
}

// splitDSN splits a lib/pq keyword/value DSN on whitespace.
func splitDSN(dsn string) []string {
	return strings.FieldsFunc(dsn, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n'
	})
}
