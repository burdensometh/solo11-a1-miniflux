// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/model"

	"github.com/lib/pq"
)

type byStateAndName struct{ f model.Feeds }

func (l byStateAndName) Len() int      { return len(l.f) }
func (l byStateAndName) Swap(i, j int) { l.f[i], l.f[j] = l.f[j], l.f[i] }
func (l byStateAndName) Less(i, j int) bool {
	// disabled test first, since we don't care about errors if disabled
	if l.f[i].Disabled != l.f[j].Disabled {
		return l.f[j].Disabled
	}
	if l.f[i].ParsingErrorCount != l.f[j].ParsingErrorCount {
		return l.f[i].ParsingErrorCount > l.f[j].ParsingErrorCount
	}
	if l.f[i].UnreadCount != l.f[j].UnreadCount {
		return l.f[i].UnreadCount > l.f[j].UnreadCount
	}
	return l.f[i].Title < l.f[j].Title
}

// FeedExists checks if the given feed exists. The returned error is
// non-nil only for a genuine query/connection failure; a feed that doesn't
// exist is reported as (false, nil), matching the underlying sql.ErrNoRows
// case.
func (s *Storage) FeedExists(userID, feedID int64) (bool, error) {
	var result bool
	query := `SELECT true FROM feeds WHERE user_id=$1 AND id=$2 LIMIT 1`
	if err := s.db.QueryRow(query, userID, feedID).Scan(&result); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf(`store: unable to check if feed exists: %w`, err)
	}
	return result, nil
}

// CheckedAt returns when the feed was last checked.
func (s *Storage) CheckedAt(userID, feedID int64) (time.Time, error) {
	var result time.Time
	query := `SELECT checked_at FROM feeds WHERE user_id=$1 AND id=$2 LIMIT 1`
	err := s.db.QueryRow(query, userID, feedID).Scan(&result)
	if err != nil {
		return time.Now(), err
	}
	return result, nil
}

// CategoryFeedExists returns true if the given feed exists and belongs to
// the given category. The returned error is non-nil only for a genuine
// query/connection failure; see FeedExists above for the same convention.
func (s *Storage) CategoryFeedExists(userID, categoryID, feedID int64) (bool, error) {
	var result bool
	query := `SELECT true FROM feeds WHERE user_id=$1 AND category_id=$2 AND id=$3 LIMIT 1`
	if err := s.db.QueryRow(query, userID, categoryID, feedID).Scan(&result); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf(`store: unable to check if feed exists in category: %w`, err)
	}
	return result, nil
}

// FeedURLExists returns true if the given feed URL already exists for the user.
func (s *Storage) FeedURLExists(userID int64, feedURL string) bool {
	var result bool
	query := `SELECT true FROM feeds WHERE user_id=$1 AND feed_url=$2 LIMIT 1`
	s.db.QueryRow(query, userID, feedURL).Scan(&result)
	return result
}

// AnotherFeedURLExists returns true if another feed with the same URL exists for the user.
func (s *Storage) AnotherFeedURLExists(userID, feedID int64, feedURL string) bool {
	var result bool
	query := `SELECT true FROM feeds WHERE id <> $1 AND user_id=$2 AND feed_url=$3 LIMIT 1`
	s.db.QueryRow(query, feedID, userID, feedURL).Scan(&result)
	return result
}

// CountAllFeeds returns the number of feeds keyed by enabled, disabled, and total.
func (s *Storage) CountAllFeeds() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT disabled, count(*) FROM feeds GROUP BY disabled`)
	if err != nil {
		return nil, fmt.Errorf("storage: unable to count feeds: %w", err)
	}
	defer rows.Close()

	results := map[string]int64{
		"enabled":  0,
		"disabled": 0,
		"total":    0,
	}

	for rows.Next() {
		var disabled bool
		var count int64

		if err := rows.Scan(&disabled, &count); err != nil {
			continue
		}

		if disabled {
			results["disabled"] = count
		} else {
			results["enabled"] = count
		}
	}

	results["total"] = results["disabled"] + results["enabled"]
	return results, nil
}

// CountAllFeedsWithErrors returns the number of feeds with parsing errors.
func (s *Storage) CountAllFeedsWithErrors() (int, error) {
	pollingParsingErrorLimit := config.Opts.PollingParsingErrorLimit()
	if pollingParsingErrorLimit <= 0 {
		pollingParsingErrorLimit = 1
	}
	query := `SELECT count(*) FROM feeds WHERE parsing_error_count >= $1`
	var result int
	err := s.db.QueryRow(query, pollingParsingErrorLimit).Scan(&result)
	if err != nil {
		return 0, fmt.Errorf("storage: unable to count feeds with errors: %w", err)
	}

	return result, nil
}

// Feeds returns all feeds that belong to the given user.
func (s *Storage) Feeds(userID int64) (model.Feeds, error) {
	return s.NewFeedQueryBuilder(userID).
		WithSorting(model.DefaultFeedSorting, model.DefaultFeedSortingDirection).
		GetFeeds()
}

func getFeedsSorted(builder *feedQueryBuilder) (model.Feeds, error) {
	result, err := builder.GetFeeds()
	if err == nil {
		sort.Sort(byStateAndName{result})
		return result, nil
	}
	return result, err
}

// FeedsWithCounters returns all feeds of the given user with read and unread entry counters.
func (s *Storage) FeedsWithCounters(userID int64) (model.Feeds, error) {
	return getFeedsSorted(s.NewFeedQueryBuilder(userID).
		WithCounters().
		WithSorting(model.DefaultFeedSorting, model.DefaultFeedSortingDirection))
}

// FetchCounters returns the per-feed read and unread entry counts for the given user.
func (s *Storage) FetchCounters(userID int64) (model.FeedCounters, error) {
	reads, unreads, err := s.NewFeedQueryBuilder(userID).
		WithCounters().
		fetchFeedCounter()

	return model.FeedCounters{ReadCounters: reads, UnreadCounters: unreads}, err
}

// FeedsByCategoryWithCounters returns all feeds in the given category for the given user with read and unread entry counters.
func (s *Storage) FeedsByCategoryWithCounters(userID, categoryID int64) (model.Feeds, error) {
	return getFeedsSorted(s.NewFeedQueryBuilder(userID).
		WithCategoryID(categoryID).
		WithCounters().
		WithSorting(model.DefaultFeedSorting, model.DefaultFeedSortingDirection))
}

// WeeklyFeedEntryCount returns the weekly entry count for a feed.
func (s *Storage) WeeklyFeedEntryCount(userID, feedID int64) (int, error) {
	// Calculate a virtual weekly count based on the average updating frequency.
	// This helps after just adding a high volume feed.
	// Return 0 when the 'count(*)' is zero(0) or one(1).
	query := `
		SELECT
			COALESCE(CAST(CEIL(
				(EXTRACT(epoch from interval '1 week'))	/
				NULLIF((EXTRACT(epoch from (max(e.published_at)-min(e.published_at))/NULLIF((count(*)-1), 0) )), 0)
			) AS BIGINT), 0)
		FROM
			entries e
			JOIN entry_feeds ef ON ef.entry_id = e.id
		WHERE
			e.user_id=$1 AND
			ef.feed_id=$2 AND
			e.published_at >= now() - interval '1 week';
	`

	var weeklyCount int
	err := s.db.QueryRow(query, userID, feedID).Scan(&weeklyCount)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf(`store: unable to fetch weekly count for feed #%d: %v`, feedID, err)
	}

	return weeklyCount, nil
}

// FeedByID returns the feed with the given ID.
func (s *Storage) FeedByID(userID, feedID int64) (*model.Feed, error) {
	feed, err := s.NewFeedQueryBuilder(userID).
		WithFeedID(feedID).
		GetFeed()

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf(`store: unable to fetch feed #%d: %v`, feedID, err)
	}

	return feed, nil
}

// CreateFeed creates a new feed.
func (s *Storage) CreateFeed(feed *model.Feed) error {
	sql := `
		INSERT INTO feeds (
			feed_url,
			site_url,
			title,
			category_id,
			user_id,
			etag_header,
			last_modified_header,
			user_agent,
			cookie,
			username,
			password,
			disabled,
			rewrite_rules,
			blocklist_rules,
			keeplist_rules,
			block_filter_entry_rules,
			keep_filter_entry_rules,
			ignore_http_cache,
			allow_self_signed_certificates,
			fetch_via_proxy,
			hide_globally,
			url_rewrite_rules,
			no_media_player,
			apprise_service_urls,
			webhook_url,
			disable_http2,
			description,
			proxy_url,
			ignore_entry_updates,
			language
		)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30)
		RETURNING
			id
	`
	err := s.db.QueryRow(
		sql,
		feed.FeedURL,
		feed.SiteURL,
		feed.Title,
		feed.Category.ID,
		feed.UserID,
		feed.EtagHeader,
		feed.LastModifiedHeader,
		feed.UserAgent,
		feed.Cookie,
		feed.Username,
		feed.Password,
		feed.Disabled,
		feed.RewriteRules,
		feed.BlocklistRules,
		feed.KeeplistRules,
		feed.BlockFilterEntryRules,
		feed.KeepFilterEntryRules,
		feed.IgnoreHTTPCache,
		feed.AllowSelfSignedCertificates,
		feed.FetchViaProxy,
		feed.HideGlobally,
		feed.UrlRewriteRules,
		feed.NoMediaPlayer,
		feed.AppriseServiceURLs,
		feed.WebhookURL,
		feed.DisableHTTP2,
		feed.Description,
		feed.ProxyURL,
		feed.IgnoreEntryUpdates,
		feed.Language,
	).Scan(&feed.ID)
	if err != nil {
		return fmt.Errorf(`store: unable to create feed %q: %v`, feed.FeedURL, err)
	}

	for _, entry := range feed.Entries {
		entry.FeedID = feed.ID
		entry.UserID = feed.UserID

		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf(`store: unable to start transaction: %v`, err)
		}

		if _, err := s.upsertFeedEntry(tx, entry, false); err != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				return fmt.Errorf(`store: unable to rollback transaction: %v (rolled back due to: %v)`, rollbackErr, err)
			}
			return err
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf(`store: unable to commit transaction: %v`, err)
		}
	}

	return nil
}

// UpdateFeed updates an existing feed.
func (s *Storage) UpdateFeed(feed *model.Feed) (err error) {
	query := `
		UPDATE
			feeds
		SET
			feed_url=$1,
			site_url=$2,
			title=$3,
			category_id=$4,
			etag_header=$5,
			last_modified_header=$6,
			checked_at=$7,
			parsing_error_msg=$8,
			parsing_error_count=$9,
			rewrite_rules=$10,
			blocklist_rules=$11,
			keeplist_rules=$12,
			block_filter_entry_rules=$13,
			keep_filter_entry_rules=$14,
			user_agent=$15,
			cookie=$16,
			username=$17,
			password=$18,
			disabled=$19,
			next_check_at=$20,
			ignore_http_cache=$21,
			allow_self_signed_certificates=$22,
			fetch_via_proxy=$23,
			hide_globally=$24,
			url_rewrite_rules=$25,
			no_media_player=$26,
			apprise_service_urls=$27,
			webhook_url=$28,
			disable_http2=$29,
			description=$30,
			ntfy_enabled=$31,
			ntfy_priority=$32,
			ntfy_topic=$33,
			pushover_enabled=$34,
			pushover_priority=$35,
			proxy_url=$36,
			ignore_entry_updates=$37,
			language=$38
		WHERE
			id=$39 AND user_id=$40
	`
	_, err = s.db.Exec(query,
		feed.FeedURL,
		feed.SiteURL,
		feed.Title,
		feed.Category.ID,
		feed.EtagHeader,
		feed.LastModifiedHeader,
		feed.CheckedAt,
		feed.ParsingErrorMsg,
		feed.ParsingErrorCount,
		feed.RewriteRules,
		feed.BlocklistRules,
		feed.KeeplistRules,
		feed.BlockFilterEntryRules,
		feed.KeepFilterEntryRules,
		feed.UserAgent,
		feed.Cookie,
		feed.Username,
		feed.Password,
		feed.Disabled,
		feed.NextCheckAt,
		feed.IgnoreHTTPCache,
		feed.AllowSelfSignedCertificates,
		feed.FetchViaProxy,
		feed.HideGlobally,
		feed.UrlRewriteRules,
		feed.NoMediaPlayer,
		feed.AppriseServiceURLs,
		feed.WebhookURL,
		feed.DisableHTTP2,
		feed.Description,
		feed.NtfyEnabled,
		feed.NtfyPriority,
		feed.NtfyTopic,
		feed.PushoverEnabled,
		feed.PushoverPriority,
		feed.ProxyURL,
		feed.IgnoreEntryUpdates,
		feed.Language,
		feed.ID,
		feed.UserID,
	)
	if err != nil {
		return fmt.Errorf(`store: unable to update feed #%d (%s): %v`, feed.ID, feed.FeedURL, err)
	}

	return nil
}

// UpdateFeedError persists the parsing error fields for the given feed.
func (s *Storage) UpdateFeedError(feed *model.Feed) (err error) {
	query := `
		UPDATE
			feeds
		SET
			parsing_error_msg=$1,
			parsing_error_count=$2,
			checked_at=$3,
			next_check_at=$4
		WHERE
			id=$5 AND user_id=$6
	`
	_, err = s.db.Exec(query,
		feed.ParsingErrorMsg,
		feed.ParsingErrorCount,
		feed.CheckedAt,
		feed.NextCheckAt,
		feed.ID,
		feed.UserID,
	)
	if err != nil {
		return fmt.Errorf(`store: unable to update feed error #%d (%s): %v`, feed.ID, feed.FeedURL, err)
	}

	return nil
}

// reassignSharedEntries moves the canonical feed of entries that are still
// carried by feeds other than the ones being removed to one of their
// remaining feeds, so the ON DELETE CASCADE does not delete entries that
// still belong to other feeds.
func (s *Storage) reassignSharedEntries(tx *sql.Tx, feedIDs []int64) error {
	query := `
		UPDATE entries
		SET feed_id = (
			SELECT ef.feed_id
			FROM entry_feeds ef
			WHERE
				ef.entry_id = entries.id
				AND ef.feed_id <> ALL($1)
			ORDER BY ef.feed_id
			LIMIT 1
		)
		WHERE
			feed_id = ANY($1)
			AND EXISTS (
				SELECT 1
				FROM entry_feeds ef
				WHERE
					ef.entry_id = entries.id
					AND ef.feed_id <> ALL($1)
			)
	`
	if _, err := tx.Exec(query, pq.Array(feedIDs)); err != nil {
		return fmt.Errorf(`store: unable to reassign shared entries: %v`, err)
	}
	return nil
}

// RemoveFeed removes the given feed. Entries still carried by other feeds are
// kept; entries carried only by this feed are deleted along with it.
func (s *Storage) RemoveFeed(userID, feedID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf(`store: unable to start transaction: %v`, err)
	}
	defer tx.Rollback()

	if err := s.reassignSharedEntries(tx, []int64{feedID}); err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM feeds WHERE id=$1 AND user_id=$2`, feedID, userID); err != nil {
		return fmt.Errorf(`store: unable to delete feed #%d: %v`, feedID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf(`store: unable to delete feed #%d: %v`, feedID, err)
	}

	return nil
}

// ResetFeedErrors clears the parsing error fields for all feeds.
func (s *Storage) ResetFeedErrors() error {
	_, err := s.db.Exec(`UPDATE feeds SET parsing_error_count=0, parsing_error_msg=''`)
	return err
}

// ResetNextCheckAt schedules all feeds to be checked immediately.
func (s *Storage) ResetNextCheckAt() error {
	_, err := s.db.Exec(`UPDATE feeds SET next_check_at=now()`)
	return err
}
