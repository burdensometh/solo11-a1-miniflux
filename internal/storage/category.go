// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"
	"miniflux.app/v2/internal/model"
)

// ErrNoCategory is returned when a user does not have any category.
var ErrNoCategory = errors.New("store: no category found")

// AnotherCategoryExists checks if another category exists with the same title.
func (s *Storage) AnotherCategoryExists(userID, categoryID int64, title string) bool {
	var result bool
	query := `SELECT true FROM categories WHERE user_id=$1 AND id != $2 AND lower(title)=lower($3) LIMIT 1`
	s.db.QueryRow(query, userID, categoryID, title).Scan(&result)
	return result
}

// CategoryTitleExists checks if the given category exists into the database.
func (s *Storage) CategoryTitleExists(userID int64, title string) bool {
	var result bool
	query := `SELECT true FROM categories WHERE user_id=$1 AND lower(title)=lower($2) LIMIT 1`
	s.db.QueryRow(query, userID, title).Scan(&result)
	return result
}

// CategoryIDExists checks if the given category exists into the database.
// The returned error is non-nil only for a genuine query/connection failure;
// a category that doesn't exist is reported as (false, nil), matching the
// underlying sql.ErrNoRows case.
func (s *Storage) CategoryIDExists(userID, categoryID int64) (bool, error) {
	var result bool
	query := `SELECT true FROM categories WHERE user_id=$1 AND id=$2 LIMIT 1`
	if err := s.db.QueryRow(query, userID, categoryID).Scan(&result); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf(`store: unable to check if category exists: %w`, err)
	}
	return result, nil
}

// Category returns a category from the database.
func (s *Storage) Category(userID, categoryID int64) (*model.Category, error) {
	var category model.Category

	query := `SELECT id, user_id, title, hide_globally FROM categories WHERE user_id=$1 AND id=$2`
	err := s.db.QueryRow(query, userID, categoryID).Scan(&category.ID, &category.UserID, &category.Title, &category.HideGlobally)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf(`store: unable to fetch category: %v`, err)
	default:
		return &category, nil
	}
}

// FirstCategory returns the first category for the given user.
// It returns ErrNoCategory if the user does not have any category.
func (s *Storage) FirstCategory(userID int64) (*model.Category, error) {
	query := `SELECT id, user_id, title, hide_globally FROM categories WHERE user_id=$1 ORDER BY title ASC LIMIT 1`

	var category model.Category
	err := s.db.QueryRow(query, userID).Scan(&category.ID, &category.UserID, &category.Title, &category.HideGlobally)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNoCategory
	case err != nil:
		return nil, fmt.Errorf(`store: unable to fetch category: %v`, err)
	default:
		return &category, nil
	}
}

// CategoryByTitle finds a category by its case-insensitive title, preferring an exact match.
func (s *Storage) CategoryByTitle(userID int64, title string) (*model.Category, error) {
	var category model.Category

	query := `SELECT id, user_id, title, hide_globally FROM categories WHERE user_id=$1 AND lower(title)=lower($2) ORDER BY title=$2 DESC, id ASC LIMIT 1`
	err := s.db.QueryRow(query, userID, title).Scan(&category.ID, &category.UserID, &category.Title, &category.HideGlobally)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf(`store: unable to fetch category: %v`, err)
	default:
		return &category, nil
	}
}

// Categories returns all categories that belongs to the given user.
func (s *Storage) Categories(userID int64) (model.Categories, error) {
	query := `SELECT id, user_id, title, hide_globally FROM categories WHERE user_id=$1 ORDER BY title ASC`
	rows, err := s.db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf(`store: unable to fetch categories: %v`, err)
	}
	defer rows.Close()

	categories := make(model.Categories, 0)
	for rows.Next() {
		var category model.Category
		if err := rows.Scan(&category.ID, &category.UserID, &category.Title, &category.HideGlobally); err != nil {
			return nil, fmt.Errorf(`store: unable to fetch category row: %v`, err)
		}

		categories = append(categories, category)
	}

	return categories, nil
}

// CategoriesWithFeedCount returns all categories with the number of feeds, sorted according to sortOrder.
func (s *Storage) CategoriesWithFeedCount(userID int64, sortOrder string) (model.Categories, error) {
	query := `
		SELECT
			c.id,
			c.user_id,
			c.title,
			c.hide_globally,
			coalesce(fc.feed_count, 0),
			coalesce(uc.unread_count, 0)
		FROM categories c
		LEFT JOIN (
			SELECT category_id, count(*) AS feed_count
			FROM feeds
			WHERE user_id = $2
			GROUP BY category_id
		) fc ON fc.category_id = c.id
		LEFT JOIN (
			SELECT f.category_id, count(*) AS unread_count
			FROM entries e
			INNER JOIN entry_feeds ef ON ef.entry_id = e.id
			INNER JOIN feeds f ON f.id = ef.feed_id
			WHERE e.user_id = $2 AND e.status = $1
			GROUP BY f.category_id
		) uc ON uc.category_id = c.id
		WHERE
			c.user_id=$2
	`

	if sortOrder == "alphabetical" {
		query += `
			ORDER BY
				c.title ASC
		`
	} else {
		query += `
			ORDER BY
				coalesce(uc.unread_count, 0) DESC,
				c.title ASC
		`
	}

	rows, err := s.db.Query(query, model.EntryStatusUnread, userID)
	if err != nil {
		return nil, fmt.Errorf(`store: unable to fetch categories: %v`, err)
	}
	defer rows.Close()

	categories := make(model.Categories, 0)
	for rows.Next() {
		var category model.Category
		if err := rows.Scan(&category.ID, &category.UserID, &category.Title, &category.HideGlobally, &category.FeedCount, &category.TotalUnread); err != nil {
			return nil, fmt.Errorf(`store: unable to fetch category row: %v`, err)
		}

		categories = append(categories, category)
	}

	return categories, nil
}

// CreateCategory creates a new category.
func (s *Storage) CreateCategory(userID int64, request *model.CategoryCreationRequest) (*model.Category, error) {
	var category model.Category

	query := `
		INSERT INTO categories
			(user_id, title, hide_globally)
		VALUES
			($1, $2, $3)
		RETURNING
			id,
			user_id,
			title,
			hide_globally
	`
	err := s.db.QueryRow(
		query,
		userID,
		request.Title,
		request.HideGlobally,
	).Scan(
		&category.ID,
		&category.UserID,
		&category.Title,
		&category.HideGlobally,
	)

	if err != nil {
		return nil, fmt.Errorf(`store: unable to create category %q for user ID %d: %v`, request.Title, userID, err)
	}

	return &category, nil
}

// UpdateCategory updates an existing category.
func (s *Storage) UpdateCategory(category *model.Category) error {
	query := `UPDATE categories SET title=$1, hide_globally=$2 WHERE id=$3 AND user_id=$4`
	_, err := s.db.Exec(
		query,
		category.Title,
		category.HideGlobally,
		category.ID,
		category.UserID,
	)

	if err != nil {
		return fmt.Errorf(`store: unable to update category: %v`, err)
	}

	return nil
}

// categoryFeedIDs returns the IDs of the feeds belonging to the category.
func categoryFeedIDs(tx *sql.Tx, userID, categoryID int64) ([]int64, error) {
	rows, err := tx.Query(`SELECT id FROM feeds WHERE category_id = $1 AND user_id = $2`, categoryID, userID)
	if err != nil {
		return nil, fmt.Errorf(`store: unable to fetch feeds of category #%d: %v`, categoryID, err)
	}
	defer rows.Close()

	var feedIDs []int64
	for rows.Next() {
		var feedID int64
		if err := rows.Scan(&feedID); err != nil {
			return nil, fmt.Errorf(`store: unable to fetch feed of category #%d: %v`, categoryID, err)
		}
		feedIDs = append(feedIDs, feedID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(`store: unable to fetch feeds of category #%d: %v`, categoryID, err)
	}

	return feedIDs, nil
}

// RemoveCategory deletes a category. Entries shared with feeds of other
// categories are kept; only the entries carried exclusively by the feeds of
// this category are deleted along with it.
func (s *Storage) RemoveCategory(userID, categoryID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf(`store: unable to start transaction: %v`, err)
	}
	defer tx.Rollback()

	// The feeds of this category are deleted by the ON DELETE CASCADE.
	// Reassign the entries they share with other feeds first.
	feedIDs, err := categoryFeedIDs(tx, userID, categoryID)
	if err != nil {
		return err
	}

	if len(feedIDs) > 0 {
		if err := s.reassignSharedEntries(tx, feedIDs); err != nil {
			return err
		}
	}

	result, err := tx.Exec(`DELETE FROM categories WHERE id = $1 AND user_id = $2`, categoryID, userID)
	if err != nil {
		return fmt.Errorf(`store: unable to remove this category: %v`, err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(`store: unable to remove this category: %v`, err)
	}

	if count == 0 {
		return errors.New(`store: no category has been removed`)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf(`store: unable to remove this category: %v`, err)
	}

	return nil
}

// RemoveAndReplaceCategoriesByName deletes categories with the given titles and
// reassigns affected feeds to the user's first remaining category. It returns
// an error if the deletion would leave the user without any categories.
func (s *Storage) RemoveAndReplaceCategoriesByName(userid int64, titles []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return errors.New("store: unable to begin transaction")
	}

	titleParam := pq.Array(titles)
	var count int
	query := "SELECT count(*) FROM categories WHERE user_id = $1 AND title <> ALL($2)"
	err = tx.QueryRow(query, userid, titleParam).Scan(&count)
	if err != nil {
		tx.Rollback()
		return errors.New("store: unable to retrieve category count")
	}
	if count < 1 {
		tx.Rollback()
		return errors.New("store: at least 1 category must remain after deletion")
	}

	query = `
		WITH d_cats AS (SELECT id FROM categories WHERE user_id = $1 AND title = ANY($2))
		UPDATE feeds
		 SET category_id =
		  (SELECT id
			FROM categories
			WHERE user_id = $1 AND id NOT IN (SELECT id FROM d_cats)
			ORDER BY title ASC
			LIMIT 1)
		WHERE user_id = $1 AND category_id IN (SELECT id FROM d_cats)
	`
	_, err = tx.Exec(query, userid, titleParam)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("store: unable to replace categories: %v", err)
	}

	query = "DELETE FROM categories WHERE user_id = $1 AND title = ANY($2)"
	_, err = tx.Exec(query, userid, titleParam)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("store: unable to delete categories: %v", err)
	}
	tx.Commit()
	return nil
}
