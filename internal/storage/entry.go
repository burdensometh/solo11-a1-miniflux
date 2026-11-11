// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"miniflux.app/v2/internal/crypto"
	"miniflux.app/v2/internal/model"

	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
)

// ErrEntryTombstoned is returned when an entry cannot be created because its
// (feed_id, hash) pair has a tombstone recording a prior deletion.
var ErrEntryTombstoned = errors.New("store: entry is tombstoned")

// isUniqueViolation reports whether the given error is a PostgreSQL unique
// constraint violation, typically raised when two entries carry the same
// link/hash despite the pre-insert existence checks.
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == pqerror.UniqueViolation
}

// mergeEntriesByURL collapses entries sharing the same non-empty URL within a
// single feed refresh. The first occurrence is kept, subsequent ones are
// returned separately so the caller can count them as skipped instead of
// letting the database reject them and rolling back the whole refresh.
// Entries without a URL are never merged because they have no dedup key.
func mergeEntriesByURL(entries model.Entries) (deduped model.Entries, duplicated model.Entries) {
	seenURLs := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		entryURL := strings.TrimSpace(entry.URL)
		if entryURL == "" {
			deduped = append(deduped, entry)
			continue
		}
		if _, exists := seenURLs[entryURL]; exists {
			duplicated = append(duplicated, entry)
			continue
		}
		seenURLs[entryURL] = struct{}{}
		deduped = append(deduped, entry)
	}
	return deduped, duplicated
}

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
		// Wrap with %w (not %v) so callers can still detect the underlying
		// PostgreSQL error, for example a unique_violation on a duplicated link.
		return fmt.Errorf(`store: unable to create entry %q (feed #%d): %w`, entry.URL, entry.FeedID, err)
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
			tags=$12,
			language=$13
		WHERE
			user_id=$9 AND feed_id=$10 AND hash=$11
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
		entry.UserID,
		entry.FeedID,
		entry.Hash,
		pq.Array(entry.Tags),
		entry.Language,
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

// entryExists checks if an entry already exists based on its hash when refreshing a feed.
func (s *Storage) entryExists(tx *sql.Tx, entry *model.Entry) (bool, error) {
	var result bool

	// Note: This query uses entries_feed_id_hash_key index (filtering on user_id is not necessary).
	err := tx.QueryRow(`SELECT true FROM entries WHERE feed_id=$1 AND hash=$2 LIMIT 1`, entry.FeedID, entry.Hash).Scan(&result)

	if err != nil && err != sql.ErrNoRows {
		return result, fmt.Errorf(`store: unable to check if entry exists: %v`, err)
	}

	return result, nil
}

func (s *Storage) getEntryIDByHash(tx *sql.Tx, feedID int64, entryHash string) (int64, error) {
	var entryID int64

	err := tx.QueryRow(
		`SELECT id FROM entries WHERE feed_id=$1 AND hash=$2 LIMIT 1`,
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

// InsertEntryForFeed inserts a single entry into a feed, optionally updating if it already exists.
// Returns true if a new entry was created, false if an existing one was reused.
func (s *Storage) InsertEntryForFeed(userID, feedID int64, entry *model.Entry) (bool, error) {
	entry.UserID = userID
	entry.FeedID = feedID

	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: unable to start transaction: %v", err)
	}
	defer tx.Rollback()

	entryID, err := s.getEntryIDByHash(tx, entry.FeedID, entry.Hash)
	if err != nil {
		return false, err
	}
	alreadyExistingEntry := entryID > 0

	if alreadyExistingEntry {
		entry.ID = entryID
	} else {
		if err := s.createEntry(tx, entry); err != nil {
			return false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}

	return !alreadyExistingEntry, nil
}

func (s *Storage) IsNewEntry(feedID int64, entryHash string) bool {
	// An entry is new only if it is neither stored nor tombstoned; otherwise
	// callers would do expensive work on every refresh for items that will be
	// discarded.
	query := `
		SELECT
			EXISTS (
				SELECT 1 FROM entries WHERE feed_id=$1 AND hash=$2
			) OR EXISTS (
				SELECT 1 FROM entry_tombstones WHERE feed_id=$1 AND hash=$2
			)
	`
	var known bool
	s.db.QueryRow(query, feedID, entryHash).Scan(&known)
	return !known
}

func (s *Storage) GetReadTime(feedID int64, entryHash string) int {
	var result int

	// Note: This query uses entries_feed_id_hash_key index
	s.db.QueryRow(
		`SELECT
			reading_time
		FROM
			entries
		WHERE
			feed_id=$1 AND
			hash=$2
		`,
		feedID,
		entryHash,
	).Scan(&result)
	return result
}

// RefreshFeedEntries persists feed entries while refreshing a feed.
//
// Each entry is processed in its own transaction, so a failure on a single
// entry (for example a duplicate link rejected by a unique constraint) only
// discards that entry: the rest of the refresh is preserved instead of being
// rolled back as a whole. Entries sharing a URL inside this refresh are merged
// beforehand, keeping the first occurrence and skipping the others.
//
// The returned EntryRefreshResult counts created, updated, skipped and failed
// entries for reconciliation, while newEntries keeps the historical list of
// freshly created entries so that downstream integrations can be notified.
func (s *Storage) RefreshFeedEntries(userID, feedID int64, entries model.Entries, updateExistingEntries bool) (newEntries model.Entries, result *model.EntryRefreshResult, err error) {
	result = &model.EntryRefreshResult{}

	// Merge entries that share the same link inside this refresh so they can
	// never reach the database as duplicates.
	dedupedEntries, duplicatedEntries := mergeEntriesByURL(entries)
	result.Skipped += len(duplicatedEntries)
	for _, entry := range duplicatedEntries {
		slog.Warn("Skipping entry duplicated by URL in the same feed refresh",
			slog.Int64("user_id", userID),
			slog.Int64("feed_id", feedID),
			slog.String("entry_url", entry.URL),
			slog.String("entry_hash", entry.Hash),
		)
	}

	for _, entry := range dedupedEntries {
		entry.UserID = userID
		entry.FeedID = feedID

		tx, txErr := s.db.Begin()
		if txErr != nil {
			// A failed Begin is a connection-level problem: continuing would
			// only produce the same error for every remaining entry.
			return newEntries, result, fmt.Errorf(`store: unable to start transaction: %v`, txErr)
		}

		entryExists, checkErr := s.entryExists(tx, entry)
		if checkErr != nil {
			result.Failed++
			slog.Error("Unable to check if entry exists",
				slog.Int64("feed_id", feedID),
				slog.String("entry_url", entry.URL),
				slog.Any("error", checkErr),
			)
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				slog.Error("Unable to rollback transaction", slog.Any("error", rollbackErr))
			}
			continue
		}

		var opErr error
		// outcome counts this entry once it has been committed successfully.
		var outcome string
		switch {
		case entryExists && updateExistingEntries:
			opErr = s.updateEntry(tx, entry)
			outcome = "updated"
		case entryExists:
			// Entry already stored and the feed does not allow updates.
			result.Skipped++
			outcome = "skipped"
		default:
			opErr = s.createEntry(tx, entry)
			outcome = "created"
			switch {
			case errors.Is(opErr, ErrEntryTombstoned):
				// No row was inserted (the INSERT ... WHERE NOT EXISTS matched
				// nothing), so the transaction is still valid and only needs a
				// normal commit to close it.
				result.Skipped++
				opErr = nil
				outcome = "skipped"
			case isUniqueViolation(opErr):
				// Another entry with the same link/hash is already stored.
				// The failed statement has aborted this transaction, so it
				// must be rolled back (committing would raise a secondary
				// error). It is a duplicate, not a hard failure: skip it and
				// keep processing the rest of the refresh.
				result.Skipped++
				slog.Warn("Skipping duplicate entry rejected by database constraint",
					slog.Int64("feed_id", feedID),
					slog.String("entry_url", entry.URL),
					slog.String("entry_hash", entry.Hash),
					slog.Any("error", opErr),
				)
				if rollbackErr := tx.Rollback(); rollbackErr != nil {
					slog.Error("Unable to rollback duplicate entry transaction", slog.Any("error", rollbackErr))
				}
				continue
			case opErr == nil:
				newEntries = append(newEntries, entry)
			}
		}

		if opErr != nil {
			result.Failed++
			slog.Error("Unable to persist entry; rolling back this entry only",
				slog.Int64("feed_id", feedID),
				slog.String("entry_url", entry.URL),
				slog.Any("error", opErr),
			)
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				slog.Error("Unable to rollback transaction", slog.Any("error", rollbackErr))
			}
			continue
		}

		if commitErr := tx.Commit(); commitErr != nil {
			if isUniqueViolation(commitErr) {
				// A concurrent refresh committed the same entry in between:
				// treat it as a skipped duplicate rather than a failure. The
				// failed commit already ended the transaction.
				result.Skipped++
				slog.Warn("Duplicate entry detected at commit time",
					slog.Int64("feed_id", feedID),
					slog.String("entry_url", entry.URL),
					slog.Any("error", commitErr),
				)
				continue
			}
			result.Failed++
			slog.Error("Unable to commit entry transaction",
				slog.Int64("feed_id", feedID),
				slog.String("entry_url", entry.URL),
				slog.Any("error", commitErr),
			)
			continue
		}

		switch outcome {
		case "created":
			result.Created++
		case "updated":
			result.Updated++
		}
	}

	return newEntries, result, nil
}

// ArchiveEntries deletes entries older than the given interval and records tombstones so they are not re-ingested.
func (s *Storage) ArchiveEntries(status string, interval time.Duration, limit int) (int64, error) {
	if interval < 0 || limit <= 0 {
		return 0, nil
	}

	query := `
		WITH to_delete AS (
			SELECT id, feed_id, hash
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
			RETURNING entries.feed_id, entries.hash
		)
		INSERT INTO entry_tombstones (feed_id, hash)
		SELECT feed_id, hash FROM deleted WHERE hash <> ''
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
			RETURNING feed_id
		)
		SELECT count(*)
		FROM updated u
			JOIN feeds f ON (f.id = u.feed_id)
			JOIN categories c ON (c.id = f.category_id)
		WHERE NOT f.hide_globally AND NOT c.hide_globally
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
			RETURNING feed_id, hash
		)
		INSERT INTO entry_tombstones (feed_id, hash)
		SELECT feed_id, hash FROM deleted WHERE hash <> ''
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
		FROM
			feeds
			JOIN categories ON (categories.id = feeds.category_id)
		WHERE
			entries.feed_id = feeds.id
			AND entries.user_id=$2
			AND entries.status=$3
			AND feeds.hide_globally IS FALSE
			AND categories.hide_globally IS FALSE
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
			user_id=$2 AND feed_id=$3 AND status=$4 AND published_at < $5
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
		FROM
			feeds
		WHERE
			feed_id=feeds.id
		AND
			feeds.user_id=$2
		AND
			status=$3
		AND
			published_at < $4
		AND
			feeds.category_id=$5
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
