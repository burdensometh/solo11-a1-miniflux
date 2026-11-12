// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBacklogReader struct {
	mu    sync.Mutex
	count int
	err   error
	calls int
}

func (f *fakeBacklogReader) CountDueFeeds(errorLimit int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.count, f.err
}

func (f *fakeBacklogReader) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeBacklogReader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestRecordCounters(t *testing.T) {
	s := NewState(4)

	s.RecordPushed(3)
	s.RecordJobStarted()
	s.RecordJobStarted()

	snap := s.Snapshot()
	if snap.Pushed != 3 {
		t.Fatalf("expected 3 pushed, got %d", snap.Pushed)
	}
	if snap.InFlight != 2 {
		t.Fatalf("expected 2 in flight, got %d", snap.InFlight)
	}
	if snap.Workers != 4 {
		t.Fatalf("expected 4 workers, got %d", snap.Workers)
	}
	if snap.LastJobAt.IsZero() {
		t.Fatal("expected LastJobAt to be recorded")
	}

	s.RecordJobCompleted()
	snap = s.Snapshot()
	if snap.Completed != 1 {
		t.Fatalf("expected 1 completed, got %d", snap.Completed)
	}
	if snap.InFlight != 1 {
		t.Fatalf("expected 1 in flight after completion, got %d", snap.InFlight)
	}
}

func TestNilStateIsSafe(t *testing.T) {
	var s *State
	s.RecordPushed(1)
	s.RecordJobStarted()
	s.RecordJobCompleted()
	if got := s.Snapshot(); got.Pushed != 0 || got.InFlight != 0 {
		t.Fatalf("nil state must return zero values, got %+v", got)
	}
}

func TestSamplerPublishesSnapshotAndStops(t *testing.T) {
	store := &fakeBacklogReader{count: 7}
	s := NewState(2)

	queueDepth := 0
	var observed atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())

	go s.RunSampler(ctx, store, func() int { return queueDepth }, 10*time.Millisecond, 3, func(Snapshot) {
		observed.Add(1)
	})

	// Wait for the immediate sample plus at least one tick.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := s.Snapshot()
		if snap.SamplerRunning && snap.SampledOK && snap.DueFeeds == 7 && observed.Load() > 1 && store.callCount() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	snap := s.Snapshot()
	if !snap.SamplerRunning || !snap.SampledOK {
		t.Fatalf("expected a successful sample, got %+v", snap)
	}
	if snap.DueFeeds != 7 {
		t.Fatalf("expected 7 due feeds, got %d", snap.DueFeeds)
	}
	if store.callCount() < 2 {
		t.Fatalf("expected repeated sampling, got %d calls", store.callCount())
	}

	cancel()
	// After cancellation the goroutine must exit; give it a moment.
	time.Sleep(30 * time.Millisecond)
}

func TestSamplerKeepsPreviousBacklogOnError(t *testing.T) {
	store := &fakeBacklogReader{count: 5}
	s := NewState(1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.RunSampler(ctx, store, func() int { return 0 }, 15*time.Millisecond, 0, nil)

	// Wait for the initial successful sample.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !s.Snapshot().SampledOK {
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.Snapshot().DueFeeds; got != 5 {
		t.Fatalf("expected initial backlog 5, got %d", got)
	}

	// Now make the store fail. A failed tick must flag SampledOK=false but
	// preserve the last known backlog instead of zeroing it.
	store.setErr(errors.New("database is down"))
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !s.Snapshot().SampledOK {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	snap := s.Snapshot()
	if snap.SampledOK {
		t.Fatal("expected SampledOK=false after a failed query")
	}
	if snap.DueFeeds != 5 {
		t.Fatalf("expected previous backlog 5 to be preserved, got %d", snap.DueFeeds)
	}
}

func TestSamplerWithNilStore(t *testing.T) {
	s := NewState(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go s.RunSampler(ctx, nil, func() int { return 3 }, 10*time.Millisecond, 0, nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Snapshot().SamplerRunning {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	snap := s.Snapshot()
	if !snap.SamplerRunning {
		t.Fatal("sampler should run even without a store")
	}
	if snap.Queued != 3 {
		t.Fatalf("expected channel depth 3 to be sampled, got %d", snap.Queued)
	}
}

func TestSamplerPublishesQueueDepth(t *testing.T) {
	store := &fakeBacklogReader{count: 1}
	s := NewState(2)

	depth := 9
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.RunSampler(ctx, store, func() int { return depth }, 10*time.Millisecond, 0, nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Snapshot().Queued == 9 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected sampled queue depth 9, got %d", s.Snapshot().Queued)
}
