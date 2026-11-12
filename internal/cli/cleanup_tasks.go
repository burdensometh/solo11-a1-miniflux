// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cli // import "miniflux.app/v2/internal/cli"

import (
	"context"
	"log/slog"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/metric"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
)

func runCleanupTasks(ctx context.Context, store *storage.Storage) {
	if nbWebSessions, err := store.CleanOldWebSessions(config.Opts.CleanupRemoveSessionsInterval()); err != nil {
		slog.Error("Unable to clean old web sessions", slog.Any("error", err))
	} else {
		slog.Info("Sessions cleanup completed",
			slog.Int64("web_sessions_removed", nbWebSessions),
		)
	}

	archiveOldEntries(ctx, store, model.EntryStatusRead, config.Opts.CleanupArchiveReadInterval())
	archiveOldEntries(ctx, store, model.EntryStatusUnread, config.Opts.CleanupArchiveUnreadInterval())

	if nbIcons, err := store.CleanupOrphanIcons(); err != nil {
		slog.Error("Unable to clean orphan icons", slog.Any("error", err))
	} else {
		slog.Info("Orphan icons cleanup completed",
			slog.Int64("orphan_icons_removed", nbIcons),
		)
	}
}

// archiveOldEntries deletes old entries for the given status, one small batch
// at a time, so that a large cleanup does not interfere with feed refreshes.
//
// The work only happens inside the configured daily time window (if any) and
// stops as soon as the window closes or the context is canceled. Each batch
// is committed separately and the selection criteria are idempotent, so an
// interrupted run simply resumes where it left off on the next execution.
func archiveOldEntries(ctx context.Context, store *storage.Storage, status string, interval time.Duration) {
	if interval < 0 {
		return
	}

	windowStart, windowEnd, hasWindow := config.Opts.CleanupArchiveWindow()
	windowOpen := func() bool {
		return !hasWindow || withinArchiveWindow(time.Now(), windowStart, windowEnd)
	}

	if !windowOpen() {
		slog.Debug("Entry archiving postponed: outside of the configured cleanup window",
			slog.String("status", status),
			slog.String("window_start", config.Opts.CleanupArchiveWindowStart()),
			slog.String("window_end", config.Opts.CleanupArchiveWindowEnd()),
		)
		return
	}

	userIDs, err := store.UserIDs()
	if err != nil {
		slog.Error("Unable to list users for entry archiving", slog.Any("error", err))
		return
	}

	var (
		batchSize         = config.Opts.CleanupArchiveBatchSize()
		batchDelay        = config.Opts.CleanupArchiveBatchDelay()
		cutoff            = time.Now().Add(-interval)
		startTime         = time.Now()
		total             int64
		interrupted       bool
		consecutiveErrors int
	)

loop:
	for _, userID := range userIDs {
		for {
			if ctx.Err() != nil || !windowOpen() {
				interrupted = true
				break loop
			}

			deleted, err := store.ArchiveEntriesBatch(userID, status, cutoff, batchSize)
			if err != nil {
				slog.Error("Unable to archive a batch of entries, moving to the next user",
					slog.Any("error", err),
					slog.Int64("user_id", userID),
					slog.String("status", status),
				)

				// A persistent failure (e.g. database unreachable) must not
				// turn into a busy loop of failing queries: give up on this
				// run, the next one will resume the work anyway.
				consecutiveErrors++
				if consecutiveErrors >= 5 {
					interrupted = true
					break loop
				}
				if !sleepWithContext(ctx, batchDelay) {
					interrupted = true
					break loop
				}
				break
			}
			consecutiveErrors = 0

			total += deleted
			if deleted < int64(batchSize) {
				break
			}

			if !sleepWithContext(ctx, batchDelay) {
				interrupted = true
				break loop
			}
		}
	}

	elapsed := time.Since(startTime)
	logArgs := []any{
		slog.String("status", status),
		slog.Int64("entries_archived", total),
		slog.Duration("elapsed", elapsed),
	}
	if interrupted {
		slog.Info("Entry archiving interrupted, will resume on the next cleanup run", logArgs...)
	} else {
		slog.Info("Entry archiving completed", logArgs...)
	}

	if config.Opts.HasMetricsCollector() {
		metric.ArchiveEntriesDuration.WithLabelValues(status).Observe(elapsed.Seconds())
	}
}

// withinArchiveWindow reports whether the given time of day falls inside the
// [start, end) window, expressed in minutes since midnight. Windows may wrap
// around midnight (e.g. 22:00-06:00); a zero-length window means "always".
func withinArchiveWindow(now time.Time, startMinutes, endMinutes int) bool {
	if startMinutes == endMinutes {
		return true
	}

	current := now.Hour()*60 + now.Minute()
	if startMinutes < endMinutes {
		return current >= startMinutes && current < endMinutes
	}
	return current >= startMinutes || current < endMinutes
}

// sleepWithContext sleeps for the given duration, returning false if the
// context is canceled first.
func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
