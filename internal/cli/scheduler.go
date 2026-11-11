// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cli // import "miniflux.app/v2/internal/cli"

import (
	"log/slog"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/storage"
	"miniflux.app/v2/internal/worker"
)

func runScheduler(store *storage.Storage, pool *worker.Pool) (stop func()) {
	slog.Debug(`Starting background scheduler...`)

	stopFeedScheduler := make(chan struct{})
	stopCleanupScheduler := make(chan struct{})

	go feedScheduler(
		store,
		pool,
		config.Opts.PollingFrequency(),
		config.Opts.BatchSize(),
		config.Opts.PollingParsingErrorLimit(),
		config.Opts.PollingLimitPerHost(),
		stopFeedScheduler,
	)

	go cleanupScheduler(
		store,
		config.Opts.CleanupFrequency(),
		stopCleanupScheduler,
	)

	return func() {
		close(stopFeedScheduler)
		close(stopCleanupScheduler)
	}
}

func feedScheduler(store *storage.Storage, pool *worker.Pool, frequency time.Duration, batchSize, errorLimit, limitPerHost int, stop <-chan struct{}) {
	// time.NewTicker is used instead of time.Tick so that the ticker can be
	// stopped and its resources released. time.Tick leaks the underlying ticker
	// because there is no way to stop it.
	ticker := time.NewTicker(frequency)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			slog.Debug("Stopping feed scheduler")
			return
		case <-ticker.C:
		}

		// Generate a batch of feeds for any user that has feeds to refresh.
		jobs, err := store.NewBatchBuilder().
			WithBatchSize(batchSize).
			WithErrorLimit(errorLimit).
			WithoutDisabledFeeds().
			WithNextCheckExpired().
			WithLimitPerHost(limitPerHost).
			FetchJobs()

		if err != nil {
			slog.Error("Unable to fetch jobs from database", slog.Any("error", err))
			continue
		}

		if len(jobs) > 0 {
			slog.Debug("Feed URLs in this batch", slog.Any("feed_urls", jobs.FeedURLs()))
			// Push never blocks: if the workers cannot keep up, the surplus
			// jobs are deferred to the next tick instead of freezing the
			// scheduler goroutine.
			pool.Push(jobs)
		}
	}
}

func cleanupScheduler(store *storage.Storage, frequency time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(frequency)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			slog.Debug("Stopping cleanup scheduler")
			return
		case <-ticker.C:
		}

		runCleanupTasks(store)
	}
}
