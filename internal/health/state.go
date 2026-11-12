// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package health keeps an in-memory snapshot of the background feed refresh
// pipeline health. It is deliberately designed so that health probes only
// read cheap atomic values: every expensive operation (database queries,
// channel introspection) is performed by a single background sampler at a
// fixed cadence, which means a slow database can never slow down a probe.
package health // import "miniflux.app/v2/internal/health"

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Snapshot is an immutable point-in-time view of the refresh pipeline.
type Snapshot struct {
	// Pushed is the cumulative number of jobs the scheduler handed to the pool.
	Pushed uint64

	// Completed is the cumulative number of jobs workers finished (success or failure).
	Completed uint64

	// InFlight is the number of jobs currently being processed by workers.
	InFlight int64

	// Queued is the number of jobs waiting in the in-memory worker channel,
	// as observed by the last sampling tick.
	Queued int

	// DueFeeds is the number of feeds whose next_check_at is in the past,
	// i.e. refresh backlog accumulated at the database level.
	DueFeeds int

	// Workers is the configured worker pool size.
	Workers int

	// LastJobAt is when the last job was picked up by a worker.
	LastJobAt time.Time

	// SampledAt is when the sampler last refreshed the channel/backlog fields.
	SampledAt time.Time

	// SampledOK reports whether the last sampling tick's database query succeeded.
	SampledOK bool

	// SamplerRunning reports whether the background sampler goroutine has ticked at least once.
	SamplerRunning bool
}

// BacklogReader is the storage capability the sampler needs. It is an
// interface so the health package does not depend on the storage package
// (which would otherwise create an import cycle).
type BacklogReader interface {
	// CountDueFeeds counts enabled feeds, below the parsing error limit,
	// whose next_check_at timestamp is in the past.
	CountDueFeeds(errorLimit int) (int, error)
}

// State is the shared, concurrency-safe health registry.
// All counters are updated atomically by workers and the scheduler;
// probes only read, so neither side ever takes a lock.
type State struct {
	pushed    atomic.Uint64
	completed atomic.Uint64
	inFlight  atomic.Int64
	workers   int

	lastJobUnixNano atomic.Int64

	// Fields below are written only by the single sampler goroutine and
	// read by probes. A pointer swap publishes each snapshot atomically.
	snapshot atomic.Pointer[samplerSnapshot]

	samplerHeartbeat atomic.Int64 // unix nano of the last sampler tick
}

type samplerSnapshot struct {
	queued    int
	dueFeeds  int
	sampledAt time.Time
	sampledOK bool
}

// NewState creates a health registry for a pool of the given size.
func NewState(nbWorkers int) *State {
	s := &State{workers: nbWorkers}
	s.snapshot.Store(&samplerSnapshot{sampledAt: time.Now()})
	return s
}

// RecordPushed is called by the scheduler for every job enqueued.
func (s *State) RecordPushed(n int) {
	if s == nil {
		return
	}
	s.pushed.Add(uint64(n))
}

// RecordJobStarted is called by a worker when it picks up a job.
func (s *State) RecordJobStarted() {
	if s == nil {
		return
	}
	s.inFlight.Add(1)
	s.lastJobUnixNano.Store(time.Now().UnixNano())
}

// RecordJobCompleted is called by a worker when a job finishes, whatever the outcome.
func (s *State) RecordJobCompleted() {
	if s == nil {
		return
	}
	s.completed.Add(1)
	s.inFlight.Add(-1)
}

// RunSampler periodically refreshes the parts of the health view that are
// expensive to compute: the channel depth and the database-level backlog.
// It runs in its own goroutine and stops when ctx is cancelled.
//
// The sampler is the *only* component that touches the database for health
// purposes, and it never shares state with probes beyond an atomic pointer
// swap. A failing or slow query only stales the snapshot; it cannot block
// a probe or the refresh workers.
//
// When observe is non-nil it is invoked after every successful or failed
// tick so callers can publish the snapshot to their metrics registry.
func (s *State) RunSampler(ctx context.Context, store BacklogReader, queueLen func() int, interval time.Duration, errorLimit int, observe func(Snapshot)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	sample := func() {
		now := time.Now()
		s.samplerHeartbeat.Store(now.UnixNano())

		var queued int
		if queueLen != nil {
			queued = queueLen()
		}

		// Without a store (e.g. worker pool used standalone) the channel
		// depth is still sampled; the database backlog stays unavailable.
		if store == nil {
			s.snapshot.Store(&samplerSnapshot{
				queued:    queued,
				dueFeeds:  0,
				sampledAt: now,
				sampledOK: false,
			})
			if observe != nil {
				observe(s.Snapshot())
			}
			return
		}

		due, err := store.CountDueFeeds(errorLimit)
		if err != nil {
			slog.Warn("Health sampler unable to count due feeds", slog.Any("error", err))
			// Keep the previous snapshot's values and only stamp a new
			// sampling time with a failure flag, so a probe can detect staleness.
			prev := s.snapshot.Load()
			s.snapshot.Store(&samplerSnapshot{
				queued:    queued,
				dueFeeds:  prev.dueFeeds,
				sampledAt: now,
				sampledOK: false,
			})
			if observe != nil {
				observe(s.Snapshot())
			}
			return
		}

		s.snapshot.Store(&samplerSnapshot{
			queued:    queued,
			dueFeeds:  due,
			sampledAt: now,
			sampledOK: true,
		})
		if observe != nil {
			observe(s.Snapshot())
		}
	}

	// One immediate sample so probes have meaningful data before the first tick.
	sample()

	for {
		select {
		case <-ctx.Done():
			slog.Debug("Stopping health sampler")
			return
		case <-ticker.C:
			sample()
		}
	}
}

// Snapshot returns a consistent, allocation-light view of the current state.
// It never blocks and never performs I/O.
func (s *State) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	snap := s.snapshot.Load()

	var lastJobAt time.Time
	if v := s.lastJobUnixNano.Load(); v > 0 {
		lastJobAt = time.Unix(0, v)
	}

	return Snapshot{
		Pushed:         s.pushed.Load(),
		Completed:      s.completed.Load(),
		InFlight:       s.inFlight.Load(),
		Queued:         snap.queued,
		DueFeeds:       snap.dueFeeds,
		Workers:        s.workers,
		LastJobAt:      lastJobAt,
		SampledAt:      snap.sampledAt,
		SampledOK:      snap.sampledOK,
		SamplerRunning: s.samplerHeartbeat.Load() > 0,
	}
}
