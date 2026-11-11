// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"fmt"
	"time"

	"github.com/lib/pq"

	"miniflux.app/v2/internal/model"
)

// EntryCursor is the decoded, position part of an API v2 pagination token.
//
// It carries the composite sort position (sort column value plus the entry ID
// tiebreaker) of the last entry returned by the previous page. The API layer
// is responsible for turning it into an opaque token; the storage layer only
// consumes the decoded values.
type EntryCursor struct {
	// SortColumn is the entries table column the result set is ordered by.
	// It must match the "order" query parameter of the request.
	SortColumn string

	// SortValue is the value of SortColumn for the last entry of the
	// previous page. It is left untouched when the sort column is the
	// numeric "id" column, in which case EntryID alone carries the position.
	SortValue time.Time

	// EntryID is the tiebreaker value (entries.id) of the last entry of the
	// previous page. Every supported sort order is completed by an ID
	// tiebreaker so the ordering is total and stable.
	EntryID int64

	// Desc reports whether the request sorts in descending direction.
	Desc bool
}

// WithKeysetPagination restricts the query to the rows positioned strictly
// after the given cursor within the requested sort order and replaces offset
// pagination with a deterministic composite ordering.
//
// The ordering is always (sortColumn, id), which is total because ids are
// unique: cursors therefore remain stable even when several rows share the
// same published_at, changed_at or created_at value.
//
// WithOffset must not be combined with keyset pagination; v2 rejects that
// combination at the API layer.
func (e *EntryQueryBuilder) WithKeysetPagination(cursor EntryCursor) *EntryQueryBuilder {
	e.offset = 0
	e.sortExpressions = nil
	e.cursorConditionStart = 0

	column := pq.QuoteIdentifier(cursor.SortColumn)
	if cursor.Desc {
		e.sortExpressions = append(e.sortExpressions, column+" DESC", "e.id DESC")
	} else {
		e.sortExpressions = append(e.sortExpressions, column+" ASC", "e.id ASC")
	}

	if cursor.EntryID <= 0 {
		// No position yet: this is the first page.
		return e
	}

	e.cursorConditionStart = len(e.conditions)

	if cursor.SortColumn == "id" {
		// The id column is numeric, no row-value comparison is needed.
		argID := len(e.args) + 1
		operator := ">"
		if cursor.Desc {
			operator = "<"
		}
		e.conditions = append(e.conditions, fmt.Sprintf("e.id %s $%d", operator, argID))
		e.args = append(e.args, cursor.EntryID)
		e.cursorArgCount = 1
		return e
	}

	// Row-value comparison on (sortColumn, id). The direction is applied to
	// both sides of the tuple, so descending pages use "<" on the whole
	// composite position.
	argColumn := len(e.args) + 1
	argID := len(e.args) + 2
	operator := ">"
	if cursor.Desc {
		operator = "<"
	}
	e.conditions = append(e.conditions,
		fmt.Sprintf("(%s, e.id) %s ($%d, $%d)", column, operator, argColumn, argID))
	e.args = append(e.args, cursor.SortValue, cursor.EntryID)
	e.cursorArgCount = 2

	return e
}

// KeysetPosition returns the sort position of the last entry of the result
// set so the API layer can encode the next-page cursor. It returns false when
// the page is empty.
func (e *EntryQueryBuilder) KeysetPosition(entries model.Entries, cursor EntryCursor) (EntryCursor, bool) {
	if len(entries) == 0 {
		return EntryCursor{}, false
	}

	last := entries[len(entries)-1]
	next := EntryCursor{SortColumn: cursor.SortColumn, Desc: cursor.Desc, EntryID: last.ID}

	switch cursor.SortColumn {
	case "published_at":
		next.SortValue = last.Date
	case "changed_at":
		next.SortValue = last.ChangedAt
	case "created_at":
		next.SortValue = last.CreatedAt
	}

	return next, true
}

// CountEntriesIgnoringPagination returns the total number of matching rows
// across all pages, keeping every filter (status, feed, search, tags, …) but
// dropping the keyset position condition.
func (e *EntryQueryBuilder) CountEntriesIgnoringPagination() (int, error) {
	return e.withoutPagination().CountEntries()
}

// withoutPagination returns a shallow copy of the builder with the keyset
// position condition and its arguments removed, while preserving every
// user-supplied filter. The receiver is never mutated.
func (e *EntryQueryBuilder) withoutPagination() *EntryQueryBuilder {
	countBuilder := *e
	countBuilder.limit = 0
	countBuilder.offset = 0
	countBuilder.cursorConditionStart = 0
	countBuilder.cursorArgCount = 0

	if e.cursorConditionStart > 0 {
		countBuilder.conditions = append([]string(nil), e.conditions[:e.cursorConditionStart]...)
		countBuilder.args = append([]any(nil), e.args[:len(e.args)-e.cursorArgCount]...)
	}

	return &countBuilder
}
