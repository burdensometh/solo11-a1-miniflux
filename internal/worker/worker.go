// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package worker // import "miniflux.app/v2/internal/worker"

import (
	"log/slog"
	"sync"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/metric"
	"miniflux.app/v2/internal/model"
	feedHandler "miniflux.app/v2/internal/reader/handler"
	"miniflux.app/v2/internal/storage"
)

type worker struct {
	id    int
	store *storage.Storage
}

// Run processes feed refresh jobs from the channel until the pool is shut down.
func (w *worker) Run(c <-chan model.Job, shutdown <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()

	slog.Debug("Worker started",
		slog.Int("worker_id", w.id),
	)

	for {
		var job model.Job
		select {
		case <-shutdown:
			return
		case job = <-c:
		}

		slog.Debug("Job received by worker",
			slog.Int("worker_id", w.id),
			slog.Int64("user_id", job.UserID),
			slog.Int64("feed_id", job.FeedID),
			slog.String("feed_url", job.FeedURL),
		)

		// A panic while refreshing a feed must not kill the worker goroutine.
		// A dead worker permanently reduces the pool capacity; if all workers
		// panic the queue stops draining and every subscription freezes until
		// the process is restarted, with no error in the logs explaining why.
		w.runJob(job)
	}
}

func (w *worker) runJob(job model.Job) {
	w.safelyRun(job, func() {
		startTime := time.Now()
		localizedError := feedHandler.RefreshFeed(w.store, job.UserID, job.FeedID, false)

		if config.Opts.HasMetricsCollector() {
			status := metric.StatusSuccess
			if localizedError != nil {
				status = metric.StatusError
			}
			metric.BackgroundFeedRefreshDuration.WithLabelValues(status).Observe(time.Since(startTime).Seconds())
		}
	})
}

// safelyRun executes a job's work and contains any panic so the worker loop
// keeps serving subsequent jobs.
func (w *worker) safelyRun(job model.Job, work func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Panic while refreshing feed, worker recovered",
				slog.Int("worker_id", w.id),
				slog.Int64("user_id", job.UserID),
				slog.Int64("feed_id", job.FeedID),
				slog.String("feed_url", job.FeedURL),
				slog.Any("panic", r),
			)
		}
	}()

	work()
}
