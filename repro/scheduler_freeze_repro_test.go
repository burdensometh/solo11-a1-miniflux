// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// This is a standalone reproduction (run with `go test ./repro/...` after
// copying it into the module) that models the original scheduling defect:
//
//   - the old pool used an unbuffered channel and a blocking Push;
//   - when a worker stops making progress (here, a worker that never returns,
//     modeling a database query blocked on a lock with no timeout), the
//     scheduler goroutine eventually blocks forever inside Push;
//   - once blocked, no further batch is scheduled, so every subscription
//     stops updating while the process and its HTTP port stay alive.
//
// The fixed pool uses a buffered channel and a non-blocking Push, so the
// scheduler keeps ticking and surplus jobs are simply deferred.
package repro_test

import (
	"testing"
	"time"
)

type job struct{ id int }

// oldPool reproduces the pre-fix behavior: unbuffered queue, blocking send.
type oldPool struct {
	queue chan job
}

func newOldPool(workers int) *oldPool {
	p := &oldPool{queue: make(chan job)}
	for range workers {
		go func() {
			// A healthy worker would range over the queue. To model a wedged
			// worker (hung DB call), these workers never read from it.
			select {}
		}()
	}
	return p
}

func (p *oldPool) push(j job) {
	p.queue <- j // blocks until a worker receives — and no worker ever will
}

// newPool reproduces the post-fix behavior: buffered queue, non-blocking send.
type newPool struct {
	queue    chan job
	dropped  int
}

func newNewPool(workers int) *newPool {
	return &newPool{queue: make(chan job, workers*4)}
}

func (p *newPool) push(j job) {
	select {
	case p.queue <- j:
	default:
		p.dropped++
	}
}

// TestOldPoolFreezesTheScheduler proves the original design can wedge the
// single scheduler goroutine permanently.
func TestOldPoolFreezesTheScheduler(t *testing.T) {
	pool := newOldPool(1)

	done := make(chan struct{})
	go func() {
		pool.push(job{id: 1})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("The blocking Push returned; the reproduction did not trigger")
	case <-time.After(500 * time.Millisecond):
		t.Log("Scheduler goroutine is stuck in Push: no further batch can be produced (defect reproduced)")
	}
}

// TestNewPoolKeepsTheSchedulerRunning proves the fixed design never blocks and
// only defers work when the workers cannot keep up.
func TestNewPoolKeepsTheSchedulerRunning(t *testing.T) {
	pool := newNewPool(1)

	for range 20 {
		pool.push(job{id: 1})
	}

	if pool.dropped == 0 {
		t.Fatal("Expected surplus jobs to be deferred once the buffered queue was full")
	}

	// The scheduler would immediately be able to run another tick here instead
	// of being trapped inside a channel send.
	t.Logf("Scheduler kept running; %d surplus jobs deferred to the next batch", pool.dropped)
}
