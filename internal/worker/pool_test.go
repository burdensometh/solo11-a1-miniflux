// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package worker // import "miniflux.app/v2/internal/worker"

import (
	"testing"
	"time"

	"miniflux.app/v2/internal/model"
)

func TestPushAfterShutdownDiscardsJobs(t *testing.T) {
	pool := NewPool(nil, 2)
	pool.Shutdown()

	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Push(model.JobList{{FeedID: 1}, {FeedID: 2}})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Push blocked after Shutdown instead of discarding jobs")
	}
}

func TestShutdownDoesNotDeadlockWithoutWorkers(t *testing.T) {
	pool := NewPool(nil, 0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Shutdown()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown deadlocked")
	}
}

// TestPushNeverBlocks guards against the original defect: with an unbuffered
// queue, Push blocked forever once every worker was stuck, which froze the
// scheduler goroutine and stopped all subscription refreshes. Push must now
// reject surplus jobs and return promptly even when nothing drains the queue.
func TestPushNeverBlocksWhenQueueIsFull(t *testing.T) {
	// No workers means nothing drains the queue, so it deterministically fills.
	pool := NewPool(nil, 0)
	defer pool.Shutdown()

	jobs := make(model.JobList, cap(pool.queue)+10)
	for i := range jobs {
		jobs[i] = model.Job{FeedID: int64(i + 1)}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Push(jobs)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Push blocked when the queue was full instead of deferring jobs")
	}
}

// TestWorkerRecoversFromPanic ensures a panic raised while running a job is
// contained and does not kill the worker goroutine.
func TestWorkerRecoversFromPanic(t *testing.T) {
	w := &worker{id: 0}

	panicked := false
	w.safelyRun(model.Job{FeedID: 1, FeedURL: "http://example.invalid/feed.xml"}, func() {
		panicked = true
		panic("boom")
	})

	if !panicked {
		t.Fatal("Expected the job function to run")
	}

	// The worker is still usable after the recovered panic.
	ranAgain := false
	w.safelyRun(model.Job{FeedID: 2, FeedURL: "http://example.invalid/feed.xml"}, func() {
		ranAgain = true
	})

	if !ranAgain {
		t.Fatal("Expected the worker to keep processing jobs after a panic")
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	pool := NewPool(nil, 1)
	pool.Shutdown()
	pool.Shutdown()
}
