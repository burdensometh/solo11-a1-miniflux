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

		startTime := time.Now()
		refreshResult, localizedError := feedHandler.RefreshFeed(w.store, job.UserID, job.FeedID, false)

		if localizedError != nil {
			slog.Warn("Feed refresh failed",
				slog.Int("worker_id", w.id),
				slog.Int64("feed_id", job.FeedID),
				slog.String("feed_url", job.FeedURL),
				slog.Any("error", localizedError.Error()),
			)
		} else if refreshResult != nil && refreshResult.Failed > 0 {
			slog.Warn("Feed refresh completed with failed entries",
				slog.Int("worker_id", w.id),
				slog.Int64("feed_id", job.FeedID),
				slog.String("feed_url", job.FeedURL),
				slog.Int("entries_created", refreshResult.Created),
				slog.Int("entries_updated", refreshResult.Updated),
				slog.Int("entries_skipped", refreshResult.Skipped),
				slog.Int("entries_failed", refreshResult.Failed),
			)
		}

		if config.Opts.HasMetricsCollector() {
			status := metric.StatusSuccess
			if localizedError != nil {
				status = metric.StatusError
			}
			metric.BackgroundFeedRefreshDuration.WithLabelValues(status).Observe(time.Since(startTime).Seconds())
		}
	}
}
