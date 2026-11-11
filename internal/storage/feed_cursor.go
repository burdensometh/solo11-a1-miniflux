// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"

	"miniflux.app/v2/internal/reader/incremental"
)

// FeedCursor returns the incremental parsing cursor for a feed, or nil when
// the feed has never completed a cursor-tracked refresh. A nil cursor is a
// valid input to the incremental filter and means "admit everything".
func (s *Storage) FeedCursor(feedID int64) (*incremental.FeedCursor, error) {
	cursor := &incremental.FeedCursor{}
	var watermark time.Time

	err := s.db.QueryRow(`
		SELECT watermark, tie_hashes, undated_hashes
		FROM feed_cursors
		WHERE feed_id = $1
	`, feedID).Scan(&watermark, pq.Array(&cursor.TieHashes), pq.Array(&cursor.UndatedHashes))
	if err != nil {
		if err == sql.ErrNoRows {
			// No row yet is the normal state for an existing feed: the cursor
			// is lazily created on the first successful incremental refresh.
			return nil, nil
		}
		return nil, fmt.Errorf("store: unable to fetch cursor for feed #%d: %w", feedID, err)
	}

	if !watermark.Equal(time.Unix(0, 0).UTC()) {
		cursor.Watermark = watermark.UTC()
	}

	return cursor, nil
}

// UpsertFeedCursor persists the cursor computed after a successful refresh.
// Passing a zero-valued cursor clears any previously stored state, which is
// how the regression/overflow guards force a full reprocess.
func (s *Storage) UpsertFeedCursor(feedID int64, cursor *incremental.FeedCursor) error {
	watermark := time.Unix(0, 0).UTC()
	if cursor != nil && !cursor.Watermark.IsZero() {
		watermark = cursor.Watermark.UTC()
	}

	var tieHashes, undatedHashes []string
	if cursor != nil {
		tieHashes = cursor.TieHashes
		undatedHashes = cursor.UndatedHashes
	}
	// lib/pq encodes a nil slice as SQL NULL, which the NOT NULL columns
	// reject. Persist empty arrays instead.
	if tieHashes == nil {
		tieHashes = []string{}
	}
	if undatedHashes == nil {
		undatedHashes = []string{}
	}

	_, err := s.db.Exec(`
		INSERT INTO feed_cursors (feed_id, watermark, tie_hashes, undated_hashes)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (feed_id) DO UPDATE
		SET
			watermark = EXCLUDED.watermark,
			tie_hashes = EXCLUDED.tie_hashes,
			undated_hashes = EXCLUDED.undated_hashes
	`, feedID, watermark, pq.Array(tieHashes), pq.Array(undatedHashes))
	if err != nil {
		return fmt.Errorf("store: unable to upsert cursor for feed #%d: %w", feedID, err)
	}

	return nil
}
