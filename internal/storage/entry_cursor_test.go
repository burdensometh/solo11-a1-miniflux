// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"strings"
	"testing"
	"time"

	"miniflux.app/v2/internal/model"
)

func newTestBuilder() *EntryQueryBuilder {
	return &EntryQueryBuilder{
		args:       []any{int64(1)},
		conditions: []string{"e.user_id = $1"},
	}
}

func TestWithKeysetPaginationFirstPageHasNoPositionCondition(t *testing.T) {
	builder := newTestBuilder()
	builder.WithKeysetPagination(EntryCursor{SortColumn: "published_at", Desc: true})

	got := builder.buildCondition()
	if got != "e.user_id = $1" {
		t.Fatalf("first page must not add a position condition, got %q", got)
	}
	if len(builder.args) != 1 {
		t.Fatalf("first page must not add arguments, got %v", builder.args)
	}
	if builder.buildSorting() == "" || !strings.Contains(builder.buildSorting(), `ORDER BY "published_at" DESC, e.id DESC`) {
		t.Fatalf("unexpected sorting clause: %q", builder.buildSorting())
	}
}

func TestWithKeysetPaginationTupleCursorAscending(t *testing.T) {
	builder := newTestBuilder()
	when := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	builder.WithKeysetPagination(EntryCursor{SortColumn: "published_at", SortValue: when, EntryID: 55, Desc: false})

	condition := builder.buildCondition()
	if !strings.Contains(condition, `("published_at", e.id) > ($2, $3)`) {
		t.Fatalf("missing ascending tuple condition: %q", condition)
	}
	if len(builder.args) != 3 {
		t.Fatalf("expected 3 arguments, got %v", builder.args)
	}
	if builder.args[1] != when || builder.args[2] != int64(55) {
		t.Fatalf("cursor arguments misaligned: %v", builder.args)
	}
	if !strings.Contains(builder.buildSorting(), `"published_at" ASC, e.id ASC`) {
		t.Fatalf("unexpected sorting clause: %q", builder.buildSorting())
	}
}

func TestWithKeysetPaginationTupleCursorDescending(t *testing.T) {
	builder := newTestBuilder()
	builder.WithKeysetPagination(EntryCursor{SortColumn: "changed_at", EntryID: 9, Desc: true})

	condition := builder.buildCondition()
	if !strings.Contains(condition, `("changed_at", e.id) < ($2, $3)`) {
		t.Fatalf("missing descending tuple condition: %q", condition)
	}
}

func TestWithKeysetPaginationIDColumnUsesSingleComparison(t *testing.T) {
	builder := newTestBuilder()
	builder.WithKeysetPagination(EntryCursor{SortColumn: "id", EntryID: 42, Desc: true})

	condition := builder.buildCondition()
	if !strings.Contains(condition, "e.id < $2") {
		t.Fatalf("missing id-only condition: %q", condition)
	}
	if len(builder.args) != 2 || builder.args[1] != int64(42) {
		t.Fatalf("id cursor must add exactly one argument, got %v", builder.args)
	}
}

func TestWithKeysetPaginationOverwritesOffsetAndLegacySorting(t *testing.T) {
	builder := newTestBuilder()
	builder.offset = 500
	builder.sortExpressions = []string{`"published_at" ASC`}

	builder.WithKeysetPagination(EntryCursor{SortColumn: "id", EntryID: 1})

	if builder.offset != 0 {
		t.Fatalf("keyset pagination must clear the offset, got %d", builder.offset)
	}
	if len(builder.sortExpressions) != 2 {
		t.Fatalf("legacy sorting must be replaced by the total (column, id) order: %v", builder.sortExpressions)
	}
}

func TestCountEntriesIgnoringPaginationDropsOnlyTheCursorCondition(t *testing.T) {
	builder := newTestBuilder().
		WithStatuses(model.EntryStatusUnread).
		WithKeysetPagination(EntryCursor{SortColumn: "published_at", SortValue: time.Now(), EntryID: 7})

	originalConditionCount := len(builder.conditions)
	copyForCount := builder.withoutPagination()

	if len(copyForCount.conditions) != originalConditionCount-1 {
		t.Fatalf("exactly one condition (the cursor) must be dropped, got %d vs %d", len(copyForCount.conditions), originalConditionCount)
	}
	joined := strings.Join(copyForCount.conditions, " AND ")
	if strings.Contains(joined, ", e.id)") {
		t.Fatalf("cursor condition leaked into the count: %q", joined)
	}
	if !strings.Contains(joined, "e.user_id = $1") || !strings.Contains(joined, "e.status = $2") {
		t.Fatalf("user filters must be preserved in the count: %q", joined)
	}
	if copyForCount.limit != 0 || copyForCount.offset != 0 {
		t.Fatal("counting builder must not carry limit/offset")
	}

	// The original builder must remain untouched so the page query still
	// executes with its cursor in place.
	if len(builder.conditions) != originalConditionCount || builder.offset != 0 {
		t.Fatal("withoutPagination must not mutate the receiver")
	}
}

func TestWithoutPaginationIsANoOpWithoutCursor(t *testing.T) {
	builder := newTestBuilder().WithStatuses(model.EntryStatusRead)

	copyForCount := builder.withoutPagination()

	if len(copyForCount.conditions) != len(builder.conditions) {
		t.Fatal("conditions must be identical when no cursor was applied")
	}
	if len(copyForCount.args) != len(builder.args) {
		t.Fatal("arguments must be identical when no cursor was applied")
	}
}
