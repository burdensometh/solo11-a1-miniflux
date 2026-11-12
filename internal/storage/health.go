// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

// CountDueFeeds counts enabled feeds whose scheduled refresh is overdue
// (next_check_at in the past). When errorLimit is greater than zero, feeds
// that reached the parsing error limit are excluded, matching the criteria
// used by the scheduler's batch builder.
//
// This query is only meant to be called by the periodic health sampler,
// never by an HTTP probe, so a slow database cannot delay a health check.
func (s *Storage) CountDueFeeds(errorLimit int) (int, error) {
	query := `SELECT count(*) FROM feeds WHERE disabled IS false AND next_check_at < now()`
	args := []any{}

	if errorLimit > 0 {
		query += ` AND parsing_error_count < $1`
		args = append(args, errorLimit)
	}

	var count int
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		return 0, err
	}

	return count, nil
}
