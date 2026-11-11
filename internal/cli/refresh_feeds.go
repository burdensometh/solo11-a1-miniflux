// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cli // import "miniflux.app/v2/internal/cli"

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/locale"
	"miniflux.app/v2/internal/metric"
	"miniflux.app/v2/internal/model"
	feedHandler "miniflux.app/v2/internal/reader/handler"
	"miniflux.app/v2/internal/storage"
	"miniflux.app/v2/internal/throttle"
)

func refreshFeeds(store *storage.Storage) {
	var wg sync.WaitGroup

	startTime := time.Now()

	// Generate a batch of feeds for any user that has feeds to refresh.
	jobs, err := store.NewBatchBuilder().
		WithBatchSize(config.Opts.BatchSize()).
		WithErrorLimit(config.Opts.PollingParsingErrorLimit()).
		WithoutDisabledFeeds().
		WithNextCheckExpired().
		WithLimitPerHost(config.Opts.PollingLimitPerHost()).
		FetchJobs()
	if err != nil {
		slog.Error("Unable to fetch jobs from database", slog.Any("error", err))
		return
	}

	slog.Debug("Feed URLs in this batch", slog.Any("feed_urls", jobs.FeedURLs()))

	nbJobs := len(jobs)
	jobQueue := make(chan model.Job, nbJobs)
	throttler := newRefreshThrottler()
	shutdown := make(chan struct{})

	slog.Info("Starting a pool of workers",
		slog.Int("nb_workers", config.Opts.WorkerPoolSize()),
	)

	for i := range config.Opts.WorkerPoolSize() {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for job := range jobQueue {
				slog.Info("Refreshing feed",
					slog.Int64("feed_id", job.FeedID),
					slog.Int64("user_id", job.UserID),
					slog.Int("worker_id", workerID),
				)

				release := throttler.Acquire(job.FeedURL, shutdown)
				if release == nil {
					return
				}

				localizedError := feedHandler.RefreshFeed(store, job.UserID, job.FeedID, false)
				release()

				reportRefreshOutcome(throttler, job.FeedURL, localizedError)

				if localizedError != nil {
					slog.Warn("Unable to refresh feed",
						slog.Int64("feed_id", job.FeedID),
						slog.Int64("user_id", job.UserID),
						slog.Any("error", localizedError.Error()),
					)
				}
			}
		}(i)
	}

	for _, job := range jobs {
		jobQueue <- job
	}
	close(jobQueue)

	wg.Wait()
	close(shutdown)

	slog.Info("Refreshed a batch of feeds",
		slog.Int("nb_feeds", nbJobs),
		slog.String("duration", time.Since(startTime).String()),
	)
}

// newRefreshThrottler builds the per-host throttler for the one-shot
// refresh command, using the same configuration as the daemon workers.
func newRefreshThrottler() *throttle.Throttler {
	if !config.Opts.PollingThrottleEnabled() {
		return throttle.New(throttle.Config{})
	}

	minInterval := config.Opts.PollingThrottleMinInterval()
	maxInterval := config.Opts.PollingThrottleMaxInterval()
	if maxInterval < minInterval {
		maxInterval = minInterval
	}

	return throttle.New(throttle.Config{
		MinInterval:   minInterval,
		MaxInterval:   maxInterval,
		MaxConcurrent: config.Opts.PollingThrottleMaxConcurrent(),
		BackoffFactor: 2,
		BackoffMax:    maxInterval,
	})
}

// reportRefreshOutcome adapts the per-host pacing after each refresh.
func reportRefreshOutcome(throttler *throttle.Throttler, feedURL string, localizedError *locale.LocalizedErrorWrapper) {
	if !throttler.Enabled() {
		return
	}

	host := throttle.HostOf(feedURL)

	if localizedError != nil &&
		(localizedError.HTTPStatusCode() == http.StatusTooManyRequests ||
			localizedError.HTTPStatusCode() == http.StatusServiceUnavailable) {
		if config.Opts.HasMetricsCollector() {
			metric.IncHostRateLimitingResponse(host)
		}
		throttler.ReportResult(feedURL, throttle.OutcomeRateLimited, 0)
		return
	}

	throttler.ReportResult(feedURL, throttle.OutcomeSuccess, 0)
}
