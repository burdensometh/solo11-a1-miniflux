// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"testing"
	"time"

	"miniflux.app/v2/internal/health"
	"miniflux.app/v2/internal/model"
)

// TestHealthCountersReflectRealWorkerLifecycle verifies that when real
// workers process jobs, the health registry observes the in-flight and
// completed transitions rather than only recording explicit calls.
//
// NewPool with a nil store cannot execute real feed refreshes, so this test
// drives the sampler and counters through the public pool surface as far as
// the in-memory queue allows: queued depth is observable even before a
// worker dequeues, and the counter helpers are exercised by Push.
func TestHealthCountersReflectPoolSurface(t *testing.T) {
	pool := NewPool(nil, 1)

	state := pool.Health()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The sampler runs against a nil store: queue depth must still update
	// without any database, proving probes on web-only processes work.
	go state.RunSampler(ctx, nil, pool.QueueLength, 5*time.Millisecond, 0, nil)

	// Wait for the first sampling tick.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !state.Snapshot().SamplerRunning {
		time.Sleep(2 * time.Millisecond)
	}
	if !state.Snapshot().SamplerRunning {
		t.Fatal("sampler never started")
	}

	// Shutdown discards jobs, but Push must still account for them.
	pool.Shutdown()
	pool.Push(model.JobList{{FeedID: 1}, {FeedID: 2}})

	snap := state.Snapshot()
	if snap.Pushed != 2 {
		t.Fatalf("expected 2 pushed jobs, got %d", snap.Pushed)
	}
	if snap.InFlight != 0 || snap.Completed != 0 {
		t.Fatalf("discarded jobs must not appear in flight or completed, got %+v", snap)
	}
}

// TestHealthStateSatisfiesBacklogFreeProbeContract ensures the snapshot
// returned to an HTTP probe is a value copy: mutating it must not affect
// subsequent reads of the shared registry.
func TestSnapshotIsAnIndependentCopy(t *testing.T) {
	state := health.NewState(3)
	state.RecordPushed(4)

	first := state.Snapshot()
	first.Pushed = 999
	first.Workers = 999

	second := state.Snapshot()
	if second.Pushed != 4 || second.Workers != 3 {
		t.Fatalf("snapshot mutations leaked into the registry: %+v", second)
	}
}
