// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cli // import "miniflux.app/v2/internal/cli"

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/model"
	feedHandler "miniflux.app/v2/internal/reader/handler"
	"miniflux.app/v2/internal/storage"
)

// batchCounters aggregates the entry-level outcomes of every feed in a
// refresh batch. The workers run concurrently, so all counters are atomic.
type batchCounters struct {
	feedsSucceeded atomic.Int64
	feedsFailed    atomic.Int64
	entriesCreated atomic.Int64
	entriesUpdated atomic.Int64
	entriesSkipped atomic.Int64
	entriesFailed  atomic.Int64
}

// LogInfo emits the per-batch reconciliation line. A single bad entry no
// longer rolls back the whole batch, so operators rely on these numbers
// instead of diffing logs by hand.
func (c *batchCounters) LogInfo(nbJobs int, duration time.Duration) {
	slog.Info("Refreshed a batch of feeds",
		slog.Int("nb_feeds", nbJobs),
		slog.Int64("feeds_succeeded", c.feedsSucceeded.Load()),
		slog.Int64("feeds_failed", c.feedsFailed.Load()),
		slog.Int64("entries_created", c.entriesCreated.Load()),
		slog.Int64("entries_updated", c.entriesUpdated.Load()),
		slog.Int64("entries_skipped", c.entriesSkipped.Load()),
		slog.Int64("entries_failed", c.entriesFailed.Load()),
		slog.String("duration", duration.String()),
	)
}

func (c *batchCounters) addFeedResult(result *model.EntryRefreshResult) {
	if result == nil {
		return
	}
	c.entriesCreated.Add(int64(result.Created))
	c.entriesUpdated.Add(int64(result.Updated))
	c.entriesSkipped.Add(int64(result.Skipped))
	c.entriesFailed.Add(int64(result.Failed))
}

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
	counters := &batchCounters{}

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

				result, localizedError := feedHandler.RefreshFeed(store, job.UserID, job.FeedID, false)
				if localizedError != nil {
					counters.feedsFailed.Add(1)
					slog.Warn("Unable to refresh feed",
						slog.Int64("feed_id", job.FeedID),
						slog.Int64("user_id", job.UserID),
						slog.Any("error", localizedError.Error()),
					)
					continue
				}

				counters.feedsSucceeded.Add(1)
				counters.addFeedResult(result)
			}
		}(i)
	}

	for _, job := range jobs {
		jobQueue <- job
	}
	close(jobQueue)

	wg.Wait()

	counters.LogInfo(nbJobs, time.Since(startTime))
}
