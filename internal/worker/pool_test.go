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

func TestShutdownUnblocksPendingPush(t *testing.T) {
	pool := NewPool(nil, 0)

	pushed := make(chan struct{})
	go func() {
		defer close(pushed)
		pool.Push(model.JobList{{FeedID: 1}})
	}()

	// Give Push time to block on the unbuffered queue before shutting down.
	time.Sleep(10 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Shutdown()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown deadlocked while a Push was pending")
	}

	select {
	case <-pushed:
	case <-time.After(5 * time.Second):
		t.Fatal("Push remained blocked after Shutdown")
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	pool := NewPool(nil, 1)
	pool.Shutdown()
	pool.Shutdown()
}

func TestPushRecordsJobsInHealthState(t *testing.T) {
	pool := NewPool(nil, 2)
	pool.Shutdown()

	// Jobs are discarded after shutdown, but the push itself must still be
	// counted so the health view reflects scheduler activity.
	pool.Push(model.JobList{{FeedID: 1}, {FeedID: 2}, {FeedID: 3}})

	snap := pool.Health().Snapshot()
	if snap.Pushed != 3 {
		t.Fatalf("expected 3 pushed jobs in health state, got %d", snap.Pushed)
	}
	if snap.Completed != 0 || snap.InFlight != 0 {
		t.Fatalf("discarded jobs must not be counted as processed, got %+v", snap)
	}
}

func TestHealthStateIsNeverNilAndExposesPoolSize(t *testing.T) {
	pool := NewPool(nil, 5)
	defer pool.Shutdown()

	if pool.Health() == nil {
		t.Fatal("pool must always expose a health registry")
	}
	if got := pool.Health().Snapshot().Workers; got != 5 {
		t.Fatalf("expected 5 workers in health snapshot, got %d", got)
	}
	// Unbuffered channel with idle workers always reports zero queued jobs.
	if got := pool.QueueLength(); got != 0 {
		t.Fatalf("expected empty queue, got %d", got)
	}
}
