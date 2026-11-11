// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"

	"github.com/lib/pq"
)

// These types back the deterministic concurrency tests in
// entry_concurrency_test.go. They live in a _test.go file, so none of this is
// compiled into production binaries.
//
// Relying on time.Sleep to line concurrent transactions up is inherently
// flaky: on a slow or busy CI runner the window may be missed, so the race
// either never reproduces (false confidence) or the test times out
// intermittently (nobody dares run it). A counting barrier instead parks every
// goroutine the moment it reaches the statement under test and releases them
// all together, making the interleaving exact on every machine and every run.

const barrierDriverName = "postgres-barrier-test"

// barrierDriverSingleton is the instance handed to database/sql at
// registration; tests reconfigure its gate between runs.
var barrierDriverSingleton = &barrierDriver{inner: &pq.Driver{}}

func init() {
	// Registering a duplicate name panics; guard against double registration.
	defer func() { _ = recover() }()
	sql.Register(barrierDriverName, barrierDriverSingleton)
}

// barrierGate coordinates one rendez-vous: a fixed number of goroutines block
// on Enter until Release unblocks them simultaneously.
type barrierGate struct {
	mu          sync.Mutex
	cond        *sync.Cond
	expected    int
	waiters     int
	released    bool
	arrived     chan struct{}
	arrivedOnce sync.Once
}

func newBarrierGate(expected int) *barrierGate {
	g := &barrierGate{expected: expected, arrived: make(chan struct{})}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// Enter blocks until expected goroutines have arrived and Release is called.
// After release it returns immediately, so a late query reusing a connection
// can never deadlock against an already-finished rendez-vous.
func (g *barrierGate) Enter() {
	g.mu.Lock()
	if g.released {
		g.mu.Unlock()
		return
	}
	g.waiters++
	if g.waiters >= g.expected {
		g.arrivedOnce.Do(func() { close(g.arrived) })
	}
	for !g.released {
		g.cond.Wait()
	}
	g.mu.Unlock()
}

// Arrived returns a channel closed once the expected number of goroutines have
// parked. Tests select on it instead of sleeping to "guess" arrival.
func (g *barrierGate) Arrived() <-chan struct{} { return g.arrived }

// WaiterCount reports how many goroutines are currently parked.
func (g *barrierGate) WaiterCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waiters
}

// Release unblocks every parked goroutine. It is idempotent.
func (g *barrierGate) Release() {
	g.mu.Lock()
	g.released = true
	g.cond.Broadcast()
	g.mu.Unlock()
}

type barrierDriver struct {
	inner driver.Driver
	gate  *barrierGate
	// match reports whether a query string should be held at the barrier.
	match func(query string) bool
}

// configure arms the rendez-vous for the next test run.
func (d *barrierDriver) configure(gate *barrierGate, match func(string) bool) {
	d.gate = gate
	d.match = match
}

func (d *barrierDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &barrierConn{inner: c, driver: d}, nil
}

type barrierConn struct {
	inner  driver.Conn
	driver *barrierDriver
}

func (c *barrierConn) Prepare(query string) (driver.Stmt, error) {
	return c.inner.Prepare(query)
}

func (c *barrierConn) Close() error { return c.inner.Close() }

func (c *barrierConn) Begin() (driver.Tx, error) { return c.inner.Begin() }

// BeginTx preserves the underlying transaction semantics; only the statements
// inside it are barriered.
func (c *barrierConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.inner.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.inner.Begin()
}

func (c *barrierConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.inner.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.inner.Prepare(query)
}

func (c *barrierConn) Ping(ctx context.Context) error {
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *barrierConn) maybeBarrier(query string) {
	if c.driver.gate != nil && c.driver.match != nil && c.driver.match(query) {
		c.driver.gate.Enter()
	}
}

// QueryContext is the hook used to stall entry inserts: database/sql routes
// QueryRow/Query here because the Conn implements QueryerContext.
func (c *barrierConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.maybeBarrier(query)
	return c.inner.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *barrierConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.maybeBarrier(query)
	return c.inner.(driver.ExecerContext).ExecContext(ctx, query, args)
}

// CheckNamedValue delegates so pq-specific values such as pq.Array keep working.
func (c *barrierConn) CheckNamedValue(nv *driver.NamedValue) error {
	if chk, ok := c.inner.(driver.NamedValueChecker); ok {
		return chk.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}
