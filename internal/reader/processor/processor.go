// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package processor // import "miniflux.app/v2/internal/reader/processor"

import (
	"log/slog"
	"net/url"
	"slices"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/reader/filter"
	"miniflux.app/v2/internal/reader/rewrite"
	"miniflux.app/v2/internal/reader/sanitizer"
	"miniflux.app/v2/internal/reader/urlcleaner"
	"miniflux.app/v2/internal/storage"
)

// ProcessFeedEntries applies filters, rewrite rules, and sanitization to feed entries.
func ProcessFeedEntries(store *storage.Storage, feed *model.Feed, userID int64) {
	var filteredEntries model.Entries

	user, storeErr := store.UserByID(userID)
	if storeErr != nil {
		slog.Error("Database error", slog.Any("error", storeErr))
		return
	}

	// The errors are handled in RemoveTrackingParameters.
	parsedFeedURL, _ := url.Parse(feed.FeedURL)
	parsedSiteURL, _ := url.Parse(feed.SiteURL)

	blockRules := filter.ParseRules(user.BlockFilterEntryRules, feed.BlockFilterEntryRules)
	allowRules := filter.ParseRules(user.KeepFilterEntryRules, feed.KeepFilterEntryRules)
	slog.Debug("Filter rules",
		slog.String("user_block_filter_rules", user.BlockFilterEntryRules),
		slog.String("feed_block_filter_rules", feed.BlockFilterEntryRules),
		slog.String("user_keep_filter_rules", user.KeepFilterEntryRules),
		slog.String("feed_keep_filter_rules", feed.KeepFilterEntryRules),
		slog.Any("block_rules", blockRules),
		slog.Any("allow_rules", allowRules),
		slog.Int64("user_id", user.ID),
		slog.Int64("feed_id", feed.ID),
	)

	// Processing older entries first ensures that their creation timestamp is lower than newer entries.
	for _, entry := range slices.Backward(feed.Entries) {
		slog.Debug("Processing entry",
			slog.Int64("user_id", user.ID),
			slog.String("entry_url", entry.URL),
			slog.String("entry_hash", entry.Hash),
			slog.String("entry_title", entry.Title),
			slog.Int64("feed_id", feed.ID),
			slog.String("feed_url", feed.FeedURL),
		)

		if filter.IsBlockedEntry(blockRules, allowRules, feed, entry) {
			slog.Debug("Entry is blocked by filter rules",
				slog.Int64("user_id", user.ID),
				slog.String("entry_url", entry.URL),
				slog.String("entry_hash", entry.Hash),
				slog.String("entry_title", entry.Title),
				slog.Int64("feed_id", feed.ID),
				slog.String("feed_url", feed.FeedURL),
			)
			continue
		}

		parsedInputUrl, _ := url.Parse(entry.URL)
		if cleanedURL, err := urlcleaner.RemoveTrackingParameters(parsedFeedURL, parsedSiteURL, parsedInputUrl); err == nil {
			entry.URL = cleanedURL
		}

		entry.URL = rewrite.RewriteEntryURL(feed, entry)
		entryIsNew := store.IsNewEntry(feed.ID, entry.Hash)

		rewrite.ApplyContentRewriteRules(entry, feed.RewriteRules)

		// The sanitizer should always run at the end of the process to make sure unsafe HTML is filtered out.
		entry.Content = sanitizer.SanitizeHTML(entry.URL, entry.Content, &sanitizer.SanitizerOptions{OpenLinksInNewTab: user.OpenExternalLinksInNewTab})

		updateEntryReadingTime(store, feed, entry, entryIsNew, user)

		filteredEntries = append(filteredEntries, entry)
	}

	if user.ShowReadingTime && shouldFetchYouTubeWatchTimeInBulk() {
		fetchYouTubeWatchTimeInBulk(filteredEntries)
	}

	feed.Entries = filteredEntries
}
