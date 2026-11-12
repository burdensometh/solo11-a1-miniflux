// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cli // import "miniflux.app/v2/internal/cli"

import (
	"context"
	"log/slog"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/storage"
	"miniflux.app/v2/internal/worker"
)

func runScheduler(ctx context.Context, store *storage.Storage, pool *worker.Pool) {
	slog.Debug(`Starting background scheduler...`)

	go feedScheduler(
		store,
		pool,
		config.Opts.PollingFrequency(),
		config.Opts.BatchSize(),
		config.Opts.PollingParsingErrorLimit(),
		config.Opts.PollingLimitPerHost(),
	)

	go cleanupScheduler(
		ctx,
		store,
		config.Opts.CleanupFrequency(),
	)
}

func feedScheduler(store *storage.Storage, pool *worker.Pool, frequency time.Duration, batchSize, errorLimit, limitPerHost int) {
	for range time.Tick(frequency) {
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
		} else if len(jobs) > 0 {
			slog.Debug("Feed URLs in this batch", slog.Any("feed_urls", jobs.FeedURLs()))
			pool.Push(jobs)
		}
	}
}

func cleanupScheduler(ctx context.Context, store *storage.Storage, frequency time.Duration) {
	ticker := time.NewTicker(frequency)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Debug("Stopping cleanup scheduler")
			return
		case <-ticker.C:
			runCleanupTasks(ctx, store)
		}
	}
}
