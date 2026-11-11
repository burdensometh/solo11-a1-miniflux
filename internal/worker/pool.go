// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package worker // import "miniflux.app/v2/internal/worker"

import (
	"log/slog"
	"sync"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
)

// Pool manages a set of background workers that process feed refresh jobs.
//
// The queue is buffered and Push never blocks: when the queue is full the
// batch is rejected and logged instead of stalling the calling goroutine.
// Previously the queue was unbuffered and Push blocked until a worker picked
// the job up, so a single job that never returned (for example a database
// query waiting on a lock without a timeout) would first consume a worker and
// then, once every worker was stuck, freeze the scheduler goroutine inside
// Push. No further batch would ever be scheduled and all subscriptions stopped
// updating until the process was manually restarted.
type Pool struct {
	queue        chan model.Job
	shutdown     chan struct{}
	shutdownOnce sync.Once
	wg           sync.WaitGroup
}

// Push sends a list of jobs to the queue.
//
// Jobs pushed after Shutdown are discarded. A full queue also discards the
// remaining jobs instead of blocking: those feeds keep an expired next_check_at
// and are picked up by the next scheduled batch, so refresh work is only
// delayed, never lost, and the scheduler keeps running.
func (p *Pool) Push(jobs model.JobList) {
	discarded := 0

	for _, job := range jobs {
		select {
		case <-p.shutdown:
			return
		case p.queue <- job:
		default:
			discarded++
		}
	}

	if discarded > 0 {
		slog.Warn("Worker queue is full, jobs rejected and deferred to the next scheduled batch",
			slog.Int("submitted_jobs", len(jobs)),
			slog.Int("discarded_jobs", discarded),
			slog.Int("queue_capacity", cap(p.queue)),
		)
	}
}

// Shutdown stops accepting new jobs and waits for all workers to finish their current jobs.
func (p *Pool) Shutdown() {
	p.shutdownOnce.Do(func() {
		close(p.shutdown)
	})
	p.wg.Wait()
}

// NewPool creates a pool of background workers.
//
// The queue holds several batches worth of jobs (relative to the number of
// workers) so that slow workers do not immediately cause the scheduler to
// defer work.
func NewPool(store *storage.Storage, nbWorkers int) *Pool {
	queueCapacity := nbWorkers * 4
	if queueCapacity < 1 {
		queueCapacity = 1
	}

	workerPool := &Pool{
		queue:    make(chan model.Job, queueCapacity),
		shutdown: make(chan struct{}),
	}

	for i := range nbWorkers {
		workerPool.wg.Add(1)
		worker := &worker{id: i, store: store}
		go worker.Run(workerPool.queue, workerPool.shutdown, &workerPool.wg)
	}

	return workerPool
}
