// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package worker // import "miniflux.app/v2/internal/worker"

import (
	"sync"

	"miniflux.app/v2/internal/health"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
)

// Pool manages a set of background workers that process feed refresh jobs.
type Pool struct {
	queue        chan model.Job
	shutdown     chan struct{}
	shutdownOnce sync.Once
	wg           sync.WaitGroup
	health       *health.State
}

// Push sends a list of jobs to the queue.
// Jobs pushed after Shutdown are discarded.
func (p *Pool) Push(jobs model.JobList) {
	p.health.RecordPushed(len(jobs))

	for _, job := range jobs {
		select {
		case p.queue <- job:
		case <-p.shutdown:
			return
		}
	}
}

// Shutdown stops accepting new jobs and waits for all workers to finish their current jobs.
func (p *Pool) Shutdown() {
	p.shutdownOnce.Do(func() {
		close(p.shutdown)
	})
	p.wg.Wait()
}

// Health returns the shared health registry populated by this pool.
func (p *Pool) Health() *health.State {
	return p.health
}

// QueueLength returns how many jobs are currently buffered in the queue.
// It reads channel internals without touching any job, and is intended to
// be called by the periodic health sampler rather than by request handlers.
func (p *Pool) QueueLength() int {
	return len(p.queue)
}

// NewPool creates a pool of background workers.
func NewPool(store *storage.Storage, nbWorkers int) *Pool {
	workerPool := &Pool{
		queue:    make(chan model.Job),
		shutdown: make(chan struct{}),
		health:   health.NewState(nbWorkers),
	}

	for i := range nbWorkers {
		workerPool.wg.Add(1)
		worker := &worker{id: i, store: store, health: workerPool.health}
		go worker.Run(workerPool.queue, workerPool.shutdown, &workerPool.wg)
	}

	return workerPool
}
