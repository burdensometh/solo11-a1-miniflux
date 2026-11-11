// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"miniflux.app/v2/internal/crypto"
	"miniflux.app/v2/internal/model"

	"github.com/lib/pq"
)

// ErrEntryTombstoned is returned when an entry cannot be created because its
// (feed_id, hash) pair has a tombstone recording a prior deletion.
var ErrEntryTombstoned = errors.New("store: entry is tombstoned")

// CountAllEntries returns the number of entries for each status in the database.
func (s *Storage) CountAllEntries() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT status, count(*) FROM entries GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("storage: unable to count entries: %w", err)
	}
	defer rows.Close()

	results := make(map[string]int64)
	results[model.EntryStatusUnread] = 0
	results[model.EntryStatusRead] = 0

	for rows.Next() {
		var status string
		var count int64

		if err := rows.Scan(&status, &count); err != nil {
			continue
		}

		results[status] = count
	}

	results["total"] = results[model.EntryStatusUnread] + results[model.EntryStatusRead]
	return results, nil
}

// UpdateEntryTitleAndContent updates entry title and content.
func (s *Storage) UpdateEntryTitleAndContent(entry *model.Entry) error {
	truncatedTitle, truncatedContent := truncateTitleAndContentForTSVectorField(entry.Title, entry.Content)
	query := `
		UPDATE
			entries
		SET
			title=$1,
			content=$2,
			reading_time=$3,
			document_vectors = setweight(to_tsvector($4), 'A') || setweight(to_tsvector($5), 'B')
		WHERE
			id=$6 AND user_id=$7
	`

	if _, err := s.db.Exec(
		query,
		entry.Title,
		entry.Content,
		entry.ReadingTime,
		truncatedTitle,
		truncatedContent,
		entry.ID,
		entry.UserID); err != nil {
		return fmt.Errorf(`store: unable to update entry #%d: %v`, entry.ID, err)
	}

	return nil
}

// createEntry add a new entry.
func (s *Storage) createEntry(tx *sql.Tx, entry *model.Entry) error {
	truncatedTitle, truncatedContent := truncateTitleAndContentForTSVectorField(entry.Title, entry.Content)
	// The WHERE NOT EXISTS guard makes the tombstone check atomic with the insert, so a
	// concurrent archive committing between an earlier existence check and this statement
	// cannot bring a deleted entry back as unread.
	query := `
		INSERT INTO entries
			(
				title,
				hash,
				url,
				comments_url,
				published_at,
				content,
				author,
				user_id,
				feed_id,
				reading_time,
				changed_at,
				document_vectors,
				tags,
				language
			)
		SELECT
			$1,
			$2,
			$3,
			$4,
			$5,
			$6,
			$7,
			$8,
			$9,
			$10,
			now(),
			setweight(to_tsvector($11), 'A') || setweight(to_tsvector($12), 'B'),
			$13,
			$14
		WHERE NOT EXISTS (
			SELECT 1 FROM entry_tombstones WHERE feed_id=$9 AND hash=$2
		)
		RETURNING
			id, status, created_at, changed_at
	`
	err := tx.QueryRow(
		query,
		entry.Title,
		entry.Hash,
		entry.URL,
		entry.CommentsURL,
		entry.Date,
		entry.Content,
		entry.Author,
		entry.UserID,
		entry.FeedID,
		entry.ReadingTime,
		truncatedTitle,
		truncatedContent,
		pq.Array(entry.Tags),
		entry.Language,
	).Scan(
		&entry.ID,
		&entry.Status,
		&entry.CreatedAt,
		&entry.ChangedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEntryTombstoned
	}
	if err != nil {
		return fmt.Errorf(`store: unable to create entry %q (feed #%d): %v`, entry.URL, entry.FeedID, err)
	}

	if _, err := tx.Exec(
		`INSERT INTO entry_feeds (entry_id, feed_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		entry.ID,
		entry.FeedID,
	); err != nil {
		return fmt.Errorf(`store: unable to link entry #%d to feed #%d: %v`, entry.ID, entry.FeedID, err)
	}

	for _, enclosure := range entry.Enclosures {
		enclosure.EntryID = entry.ID
		enclosure.UserID = entry.UserID
		err := s.createEnclosure(tx, enclosure)
		if err != nil {
			return err
		}
	}

	return nil
}

// updateEntry updates an entry when a feed is refreshed.
// Note: we do not update the published date because some feeds do not contains any date,
// it default to time.Now() which could change the order of items on the history page.
func (s *Storage) updateEntry(tx *sql.Tx, entry *model.Entry) error {
	truncatedTitle, truncatedContent := truncateTitleAndContentForTSVectorField(entry.Title, entry.Content)
	query := `
		UPDATE
			entries
		SET
			title=$1,
			url=$2,
			comments_url=$3,
			content=$4,
			author=$5,
			reading_time=$6,
			document_vectors = setweight(to_tsvector($7), 'A') || setweight(to_tsvector($8), 'B'),
			tags=$9,
			language=$10
		WHERE
			id=$11 AND user_id=$12
		RETURNING
			id
	`
	err := tx.QueryRow(
		query,
		entry.Title,
		entry.URL,
		entry.CommentsURL,
		entry.Content,
		entry.Author,
		entry.ReadingTime,
		truncatedTitle,
		truncatedContent,
		pq.Array(entry.Tags),
		entry.Language,
		entry.ID,
		entry.UserID,
	).Scan(&entry.ID)
	if err != nil {
		return fmt.Errorf(`store: unable to update entry %q: %v`, entry.URL, err)
	}

	for _, enclosure := range entry.Enclosures {
		enclosure.UserID = entry.UserID
		enclosure.EntryID = entry.ID
	}

	return s.updateEnclosures(tx, entry)
}

// lockEntryHash takes a transaction-scoped advisory lock on the (user, hash)
// pair so that two feeds of the same site refreshed concurrently cannot
// insert a duplicate of the same article.
func (s *Storage) lockEntryHash(tx *sql.Tx, userID int64, entryHash string) error {
	if _, err := tx.Exec(
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		fmt.Sprintf("%d:%s", userID, entryHash),
	); err != nil {
		return fmt.Errorf(`store: unable to lock entry hash: %v`, err)
	}
	return nil
}

// getLinkedEntryID returns the ID of the entry with the given hash carried by
// the feed, or 0 when the feed does not carry such an entry.
func (s *Storage) getLinkedEntryID(tx *sql.Tx, feedID int64, entryHash string) (int64, error) {
	var entryID int64

	err := tx.QueryRow(
		`SELECT e.id
		FROM entries e
			JOIN entry_feeds ef ON ef.entry_id = e.id
		WHERE ef.feed_id=$1 AND e.hash=$2
		LIMIT 1`,
		feedID,
		entryHash,
	).Scan(&entryID)

	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf(`store: unable to fetch entry ID: %v`, err)
	}

	return entryID, nil
}

// findSharableEntryID returns the ID of an entry with the same hash already
// carried by another feed of the same site (cross-feed duplicate), or 0 when
// there is none.
func (s *Storage) findSharableEntryID(tx *sql.Tx, entry *model.Entry) (int64, error) {
	var entryID int64

	err := tx.QueryRow(
		`SELECT e.id
		FROM entries e
			JOIN entry_feeds ef ON ef.entry_id = e.id
			JOIN feeds f ON f.id = ef.feed_id
		WHERE
			e.user_id = $1
			AND e.hash = $2
			AND normalize_site_url(f.site_url) = (
				SELECT normalize_site_url(site_url) FROM feeds WHERE id = $3
			)
		ORDER BY e.id
		LIMIT 1`,
		entry.UserID,
		entry.Hash,
		entry.FeedID,
	).Scan(&entryID)

	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf(`store: unable to find a sharable entry: %v`, err)
	}

	return entryID, nil
}

// linkEntryToFeed associates an existing entry with an additional feed. The
// association is skipped when the (feed, hash) pair is tombstoned, so that an
// entry deleted from a feed does not come back through a sibling feed.
func (s *Storage) linkEntryToFeed(tx *sql.Tx, entryID, feedID int64, entryHash string) (bool, error) {
	result, err := tx.Exec(
		`INSERT INTO entry_feeds (entry_id, feed_id)
		SELECT $1, $2
		WHERE NOT EXISTS (
			SELECT 1 FROM entry_tombstones WHERE feed_id = $2 AND hash = $3
		)
		ON CONFLICT (entry_id, feed_id) DO NOTHING`,
		entryID,
		feedID,
		entryHash,
	)
	if err != nil {
		return false, fmt.Errorf(`store: unable to link entry #%d to feed #%d: %v`, entryID, feedID, err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf(`store: unable to link entry #%d to feed #%d: %v`, entryID, feedID, err)
	}

	return count > 0, nil
}

// upsertFeedEntry stores an entry fetched from a feed:
//   - the entry is already carried by this feed: it is optionally updated;
//   - the same article already exists on a sibling feed of the same site: the
//     existing entry is linked to this feed instead of creating a duplicate;
//   - otherwise a new entry is created (unless it is tombstoned).
//
// It returns true when a new entry row has been created.
func (s *Storage) upsertFeedEntry(tx *sql.Tx, entry *model.Entry, updateExisting bool) (bool, error) {
	if err := s.lockEntryHash(tx, entry.UserID, entry.Hash); err != nil {
		return false, err
	}

	entryID, err := s.getLinkedEntryID(tx, entry.FeedID, entry.Hash)
	if err != nil {
		return false, err
	}

	if entryID > 0 {
		entry.ID = entryID
		if updateExisting {
			if err := s.updateEntry(tx, entry); err != nil {
				return false, err
			}
		}
		return false, nil
	}

	sharedEntryID, err := s.findSharableEntryID(tx, entry)
	if err != nil {
		return false, err
	}

	if sharedEntryID > 0 {
		linked, err := s.linkEntryToFeed(tx, sharedEntryID, entry.FeedID, entry.Hash)
		if err != nil {
			return false, err
		}
		if linked {
			entry.ID = sharedEntryID
		}
		return false, nil
	}

	if err := s.createEntry(tx, entry); err != nil {
		if errors.Is(err, ErrEntryTombstoned) {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

// entryTombstoned returns true when the (feed, hash) pair has been deleted
// previously and must not be re-ingested.
func (s *Storage) entryTombstoned(tx *sql.Tx, feedID int64, entryHash string) (bool, error) {
	var tombstoned bool
	err := tx.QueryRow(
		`SELECT true FROM entry_tombstones WHERE feed_id=$1 AND hash=$2 LIMIT 1`,
		feedID,
		entryHash,
	).Scan(&tombstoned)

	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf(`store: unable to check entry tombstone: %v`, err)
	}

	return tombstoned, nil
}

// InsertEntryForFeed inserts a single entry into a feed, reusing the existing
// entry when the article is already known to this feed or to a sibling feed
// of the same site. Returns true if a new entry was created.
func (s *Storage) InsertEntryForFeed(userID, feedID int64, entry *model.Entry) (bool, error) {
	entry.UserID = userID
	entry.FeedID = feedID

	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: unable to start transaction: %v", err)
	}
	defer tx.Rollback()

	tombstoned, err := s.entryTombstoned(tx, feedID, entry.Hash)
	if err != nil {
		return false, err
	}
	if tombstoned {
		return false, ErrEntryTombstoned
	}

	created, err := s.upsertFeedEntry(tx, entry, false)
	if err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}

	return created, nil
}

// IsNewEntry returns false when the entry is already known to the user:
// carried by this feed or a sibling feed of the same site, or tombstoned.
// Otherwise callers would do expensive work on every refresh for items that
// will be discarded.
func (s *Storage) IsNewEntry(userID, feedID int64, entryHash string) bool {
	query := `
		SELECT
			EXISTS (
				SELECT 1
				FROM entries e
					JOIN entry_feeds ef ON ef.entry_id = e.id
				WHERE ef.feed_id=$2 AND e.hash=$3
			) OR EXISTS (
				SELECT 1 FROM entry_tombstones WHERE feed_id=$2 AND hash=$3
			) OR EXISTS (
				SELECT 1
				FROM entries e
					JOIN entry_feeds ef ON ef.entry_id = e.id
					JOIN feeds f ON f.id = ef.feed_id
				WHERE
					e.user_id=$1
					AND e.hash=$3
					AND normalize_site_url(f.site_url) = (
						SELECT normalize_site_url(site_url) FROM feeds WHERE id = $2
					)
			)
	`
	var known bool
	s.db.QueryRow(query, userID, feedID, entryHash).Scan(&known)
	return !known
}

// GetReadTime returns the reading time of an existing entry with the same
// hash, looking at this feed and sibling feeds of the same site.
func (s *Storage) GetReadTime(userID, feedID int64, entryHash string) int {
	var result int

	s.db.QueryRow(
		`SELECT e.reading_time
		FROM entries e
		WHERE
			e.user_id=$1
			AND e.hash=$3
			AND (
				e.feed_id=$2
				OR EXISTS (
					SELECT 1
					FROM entry_feeds ef
						JOIN feeds f ON f.id = ef.feed_id
					WHERE
						ef.entry_id = e.id
						AND normalize_site_url(f.site_url) = (
							SELECT normalize_site_url(site_url) FROM feeds WHERE id = $2
						)
				)
			)
		ORDER BY e.id
		LIMIT 1`,
		userID,
		feedID,
		entryHash,
	).Scan(&result)
	return result
}

// RefreshFeedEntries updates feed entries while refreshing a feed.
func (s *Storage) RefreshFeedEntries(userID, feedID int64, entries model.Entries, updateExistingEntries bool) (newEntries model.Entries, err error) {
	for _, entry := range entries {
		entry.UserID = userID
		entry.FeedID = feedID

		tx, err := s.db.Begin()
		if err != nil {
			return nil, fmt.Errorf(`store: unable to start transaction: %v`, err)
		}

		created, err := s.upsertFeedEntry(tx, entry, updateExistingEntries)
		if err != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				return nil, fmt.Errorf(`store: unable to rollback transaction: %v (rolled back due to: %v)`, rollbackErr, err)
			}
			return nil, err
		}

		if created {
			newEntries = append(newEntries, entry)
		}

		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf(`store: unable to commit transaction: %v`, err)
		}
	}

	return newEntries, nil
}

// ArchiveEntries deletes entries older than the given interval and records tombstones so they are not re-ingested.
func (s *Storage) ArchiveEntries(status string, interval time.Duration, limit int) (int64, error) {
	if interval < 0 || limit <= 0 {
		return 0, nil
	}

	query := `
		WITH to_delete AS (
			SELECT id
			FROM entries
			WHERE
				status=$1 AND
				starred is false AND
				share_code='' AND
				created_at < now() - $2::interval
			ORDER BY created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT $3
		), deleted AS (
			DELETE FROM entries
			USING to_delete
			WHERE entries.id = to_delete.id
			RETURNING entries.id, entries.hash
		)
		INSERT INTO entry_tombstones (feed_id, hash)
		SELECT ef.feed_id, d.hash
		FROM deleted d
			JOIN entry_feeds ef ON ef.entry_id = d.id
		WHERE d.hash <> ''
		ON CONFLICT (feed_id, hash) DO NOTHING
	`

	days := max(int(interval/(24*time.Hour)), 1)

	result, err := s.db.Exec(query, status, fmt.Sprintf("%d days", days), limit)
	if err != nil {
		return 0, fmt.Errorf(`store: unable to archive %s entries: %v`, status, err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf(`store: unable to get the number of rows affected: %v`, err)
	}

	return count, nil
}

// SetEntriesStatus update the status of the given list of entries.
func (s *Storage) SetEntriesStatus(userID int64, entryIDs []int64, status string) error {
	query := `
		UPDATE
			entries
		SET
			status=$1,
			changed_at=now()
		WHERE
			user_id=$2 AND
			id=ANY($3)
		`
	if _, err := s.db.Exec(query, status, userID, pq.Array(entryIDs)); err != nil {
		return fmt.Errorf(`store: unable to update entries statuses %v: %v`, entryIDs, err)
	}

	return nil
}

// SetEntriesStatusAndCountVisible updates the status of the given entries and returns how many are visible in global views.
func (s *Storage) SetEntriesStatusAndCountVisible(userID int64, entryIDs []int64, status string) (int, error) {
	query := `
		WITH updated AS (
			UPDATE entries
			SET
				status=$1,
				changed_at=now()
			WHERE
				user_id=$2 AND
				id=ANY($3)
			RETURNING id
		)
		SELECT count(*)
		FROM updated u
		WHERE EXISTS (
			SELECT 1
			FROM entry_feeds ef
				JOIN feeds f ON (f.id = ef.feed_id)
				JOIN categories c ON (c.id = f.category_id)
			WHERE
				ef.entry_id = u.id
				AND NOT f.hide_globally
				AND NOT c.hide_globally
		)
	`
	var visible int
	if err := s.db.QueryRow(query, status, userID, pq.Array(entryIDs)).Scan(&visible); err != nil {
		return 0, fmt.Errorf(`store: unable to update entries status %v: %v`, entryIDs, err)
	}
	return visible, nil
}

// SetEntriesStarredState updates the starred state for the given list of entries.
func (s *Storage) SetEntriesStarredState(userID int64, entryIDs []int64, starred bool) error {
	query := `UPDATE entries SET starred=$1, changed_at=now() WHERE user_id=$2 AND id=ANY($3)`
	if _, err := s.db.Exec(query, starred, userID, pq.Array(entryIDs)); err != nil {
		return fmt.Errorf(`store: unable to update the starred state %v: %v`, entryIDs, err)
	}

	return nil
}

// ToggleStarred toggles entry starred value.
func (s *Storage) ToggleStarred(userID int64, entryID int64) error {
	query := `UPDATE entries SET starred = NOT starred, changed_at=now() WHERE user_id=$1 AND id=$2`
	result, err := s.db.Exec(query, userID, entryID)
	if err != nil {
		return fmt.Errorf(`store: unable to toggle starred flag for entry #%d: %v`, entryID, err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(`store: unable to toggle starred flag for entry #%d: %v`, entryID, err)
	}

	if count == 0 {
		return errors.New(`store: nothing has been updated`)
	}

	return nil
}

// FlushHistory deletes all read entries (non-starred, non-shared) and records tombstones to prevent re-ingestion.
func (s *Storage) FlushHistory(userID int64) error {
	query := `
		WITH deleted AS (
			DELETE FROM entries
			WHERE user_id=$1 AND status=$2 AND starred is false AND share_code=''
			RETURNING id, hash
		)
		INSERT INTO entry_tombstones (feed_id, hash)
		SELECT ef.feed_id, d.hash
		FROM deleted d
			JOIN entry_feeds ef ON ef.entry_id = d.id
		WHERE d.hash <> ''
		ON CONFLICT (feed_id, hash) DO NOTHING
	`
	if _, err := s.db.Exec(query, userID, model.EntryStatusRead); err != nil {
		return fmt.Errorf(`store: unable to flush history: %v`, err)
	}

	return nil
}

// MarkAllAsRead updates all user entries to the read status.
func (s *Storage) MarkAllAsRead(userID int64) error {
	query := `UPDATE entries SET status=$1, changed_at=now() WHERE user_id=$2 AND status=$3`
	result, err := s.db.Exec(query, model.EntryStatusRead, userID, model.EntryStatusUnread)
	if err != nil {
		return fmt.Errorf(`store: unable to mark all entries as read: %v`, err)
	}

	count, _ := result.RowsAffected()
	slog.Debug("Marked all entries as read",
		slog.Int64("user_id", userID),
		slog.Int64("nb_entries", count),
	)

	return nil
}

// MarkAllAsReadBeforeDate updates all user entries to the read status before the given date.
func (s *Storage) MarkAllAsReadBeforeDate(userID int64, before time.Time) error {
	query := `
		UPDATE
			entries
		SET
			status=$1,
			changed_at=now()
		WHERE
			user_id=$2 AND status=$3 AND published_at < $4
	`
	result, err := s.db.Exec(query, model.EntryStatusRead, userID, model.EntryStatusUnread, before)
	if err != nil {
		return fmt.Errorf(`store: unable to mark all entries as read before %s: %v`, before.Format(time.RFC3339), err)
	}
	count, _ := result.RowsAffected()
	slog.Debug("Marked all entries as read before date",
		slog.Int64("user_id", userID),
		slog.Int64("nb_entries", count),
		slog.String("before", before.Format(time.RFC3339)),
	)
	return nil
}

// MarkGloballyVisibleFeedsAsRead marks as read the unread entries that are
// visible in the global unread view, i.e. those belonging to a feed and a
// category that are both not hidden globally.
func (s *Storage) MarkGloballyVisibleFeedsAsRead(userID int64) error {
	query := `
		UPDATE
			entries
		SET
			status=$1,
			changed_at=now()
		WHERE
			user_id=$2
			AND status=$3
			AND EXISTS (
				SELECT 1
				FROM entry_feeds ef
					JOIN feeds f ON (f.id = ef.feed_id)
					JOIN categories c ON (c.id = f.category_id)
				WHERE
					ef.entry_id = entries.id
					AND f.hide_globally IS FALSE
					AND c.hide_globally IS FALSE
			)
	`
	result, err := s.db.Exec(query, model.EntryStatusRead, userID, model.EntryStatusUnread)
	if err != nil {
		return fmt.Errorf(`store: unable to mark globally visible feeds as read: %v`, err)
	}

	count, _ := result.RowsAffected()
	slog.Debug("Marked globally visible feed entries as read",
		slog.Int64("user_id", userID),
		slog.Int64("nb_entries", count),
	)

	return nil
}

// MarkFeedAsRead updates all feed entries to the read status.
func (s *Storage) MarkFeedAsRead(userID, feedID int64, before time.Time) error {
	query := `
		UPDATE
			entries
		SET
			status=$1,
			changed_at=now()
		WHERE
			user_id=$2 AND status=$4 AND published_at < $5
			AND EXISTS (
				SELECT 1 FROM entry_feeds ef
				WHERE ef.entry_id = entries.id AND ef.feed_id = $3
			)
	`
	result, err := s.db.Exec(query, model.EntryStatusRead, userID, feedID, model.EntryStatusUnread, before)
	if err != nil {
		return fmt.Errorf(`store: unable to mark feed entries as read: %v`, err)
	}

	count, _ := result.RowsAffected()
	slog.Debug("Marked feed entries as read",
		slog.Int64("user_id", userID),
		slog.Int64("feed_id", feedID),
		slog.Int64("nb_entries", count),
		slog.String("before", before.Format(time.RFC3339)),
	)

	return nil
}

// MarkCategoryAsRead updates all category entries to the read status.
func (s *Storage) MarkCategoryAsRead(userID, categoryID int64, before time.Time) error {
	query := `
		UPDATE
			entries
		SET
			status=$1,
			changed_at=now()
		WHERE
			user_id=$2
		AND
			status=$3
		AND
			published_at < $4
		AND EXISTS (
			SELECT 1
			FROM entry_feeds ef
				JOIN feeds f ON (f.id = ef.feed_id)
			WHERE
				ef.entry_id = entries.id
				AND f.category_id = $5
		)
	`
	result, err := s.db.Exec(query, model.EntryStatusRead, userID, model.EntryStatusUnread, before, categoryID)
	if err != nil {
		return fmt.Errorf(`store: unable to mark category entries as read: %v`, err)
	}

	count, _ := result.RowsAffected()
	slog.Debug("Marked category entries as read",
		slog.Int64("user_id", userID),
		slog.Int64("category_id", categoryID),
		slog.Int64("nb_entries", count),
		slog.String("before", before.Format(time.RFC3339)),
	)

	return nil
}

// EntryShareCode returns the share code of the provided entry.
// It generates a new one if not already defined.
func (s *Storage) EntryShareCode(userID int64, entryID int64) (shareCode string, err error) {
	query := `SELECT share_code FROM entries WHERE user_id=$1 AND id=$2`
	err = s.db.QueryRow(query, userID, entryID).Scan(&shareCode)
	if err != nil {
		err = fmt.Errorf(`store: unable to get share code for entry #%d: %v`, entryID, err)
		return
	}

	if shareCode == "" {
		shareCode = crypto.GenerateRandomStringHex(20)

		query = `UPDATE entries SET share_code = $1 WHERE user_id=$2 AND id=$3`
		_, err = s.db.Exec(query, shareCode, userID, entryID)
		if err != nil {
			err = fmt.Errorf(`store: unable to set share code for entry #%d: %v`, entryID, err)
			return
		}
	}

	return
}

// UnshareEntry removes the share code for the given entry.
func (s *Storage) UnshareEntry(userID int64, entryID int64) (err error) {
	query := `UPDATE entries SET share_code='' WHERE user_id=$1 AND id=$2`
	_, err = s.db.Exec(query, userID, entryID)
	if err != nil {
		err = fmt.Errorf(`store: unable to remove share code for entry #%d: %v`, entryID, err)
	}
	return
}

func truncateTitleAndContentForTSVectorField(title, content string) (string, string) {
	// The length of a tsvector (lexemes + positions) must be less than 1 megabyte.
	// We don't need to index the entire content, and we need to keep a buffer for the positions.
	return truncateStringForTSVectorField(title, 200000), truncateStringForTSVectorField(content, 500000)
}

// truncateStringForTSVectorField truncates a string and don't break UTF-8 characters.
func truncateStringForTSVectorField(s string, maxSize int) string {
	if len(s) < maxSize {
		return s
	}

	// Truncate to fit under the limit, ensuring we don't break UTF-8 characters
	truncated := s[:maxSize-1]

	// Walk backwards to find the last complete UTF-8 character
	for i := len(truncated) - 1; i >= 0; i-- {
		if (truncated[i] & 0x80) == 0 {
			// ASCII character, we can stop here
			return truncated[:i+1]
		}
		if (truncated[i] & 0xC0) == 0xC0 {
			// Start of a multi-byte UTF-8 character
			return truncated[:i]
		}
	}

	// Fallback: return empty string if we can't find a valid UTF-8 boundary
	return ""
}
