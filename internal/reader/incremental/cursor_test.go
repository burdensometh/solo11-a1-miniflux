// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package incremental

import (
	"fmt"
	"testing"
	"time"

	"miniflux.app/v2/internal/model"
)

func mustTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed.UTC()
}

func makeEntry(hash string, cursorTime time.Time) *model.Entry {
	return &model.Entry{Hash: hash, CursorTime: cursorTime}
}

func TestNilCursorAdmitsEverything(t *testing.T) {
	var cursor *FeedCursor
	entries := model.Entries{
		makeEntry("a", mustTime("2026-01-01T00:00:00Z")),
		makeEntry("b", time.Time{}),
	}

	got := cursor.Filter(entries)
	if len(got) != 2 {
		t.Fatalf("expected 2 admitted entries, got %d", len(got))
	}
}

func TestClassifyDatedEntriesAgainstWatermark(t *testing.T) {
	cursor := &FeedCursor{
		Watermark: mustTime("2026-01-02T00:00:00Z"),
		TieHashes: []string{"at-watermark"},
	}

	tests := []struct {
		name      string
		entry     *model.Entry
		admission Admission
	}{
		{"after watermark", makeEntry("new", mustTime("2026-01-03T00:00:00Z")), Admit},
		{"before watermark", makeEntry("old", mustTime("2026-01-01T00:00:00Z")), Skip},
		{"at watermark, known hash", makeEntry("at-watermark", mustTime("2026-01-02T00:00:00Z")), Skip},
		{"at watermark, unknown hash", makeEntry("late-arrival", mustTime("2026-01-02T00:00:00Z")), Admit},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cursor.Classify(tc.entry); got != tc.admission {
				t.Fatalf("expected %v, got %v", tc.admission, got)
			}
		})
	}
}

func TestClassifyUndatedEntriesByHash(t *testing.T) {
	cursor := &FeedCursor{
		Watermark:     mustTime("2026-01-02T00:00:00Z"),
		UndatedHashes: []string{"seen-undated"},
	}

	if cursor.Classify(makeEntry("seen-undated", time.Time{})) != Skip {
		t.Fatal("known undated entry must be skipped")
	}
	if cursor.Classify(makeEntry("new-undated", time.Time{})) != Admit {
		t.Fatal("unknown undated entry must be admitted")
	}
}

func TestAdvanceBuildsInitialCursor(t *testing.T) {
	entries := model.Entries{
		makeEntry("a", mustTime("2026-01-01T00:00:00Z")),
		makeEntry("b", mustTime("2026-01-02T00:00:00Z")),
		makeEntry("c", mustTime("2026-01-02T00:00:00Z")),
		makeEntry("undated", time.Time{}),
	}

	cursor := (*FeedCursor)(nil).Advance(entries, entries)

	if !cursor.Watermark.Equal(mustTime("2026-01-02T00:00:00Z")) {
		t.Fatalf("unexpected watermark: %v", cursor.Watermark)
	}
	if len(cursor.TieHashes) != 2 {
		t.Fatalf("expected 2 tie hashes, got %d", len(cursor.TieHashes))
	}
	if len(cursor.UndatedHashes) != 1 || cursor.UndatedHashes[0] != "undated" {
		t.Fatalf("unexpected undated hashes: %v", cursor.UndatedHashes)
	}
}

func TestAdvanceIsMonotonicAndResumesInterruptedRefresh(t *testing.T) {
	// Simulate a crash after the first refresh committed only part of its
	// work: the stored cursor covers entries up to t2.
	stored := &FeedCursor{
		Watermark: mustTime("2026-01-02T00:00:00Z"),
		TieHashes: []string{"b", "c"},
	}

	// The next document re-serves the old entries plus one new entry at t3.
	document := model.Entries{
		makeEntry("a", mustTime("2026-01-01T00:00:00Z")),
		makeEntry("b", mustTime("2026-01-02T00:00:00Z")),
		makeEntry("c", mustTime("2026-01-02T00:00:00Z")),
		makeEntry("d", mustTime("2026-01-03T00:00:00Z")),
	}

	admitted := stored.Filter(document)
	if len(admitted) != 1 || admitted[0].Hash != "d" {
		t.Fatalf("expected only entry d to be admitted, got %v", admitted)
	}

	next := stored.Advance(document, document)
	if !next.Watermark.Equal(mustTime("2026-01-03T00:00:00Z")) {
		t.Fatalf("watermark must move forward, got %v", next.Watermark)
	}
	if len(next.TieHashes) != 1 || next.TieHashes[0] != "d" {
		t.Fatalf("old tie set must be replaced at the new watermark, got %v", next.TieHashes)
	}
}

func TestFilteredEntriesAreNotMarkedAsSeen(t *testing.T) {
	stored := &FeedCursor{}

	// Two new entries share the watermark instant; block/keep rules filter
	// one of them out before it reaches storage.
	document := model.Entries{
		makeEntry("kept", mustTime("2026-01-02T00:00:00Z")),
		makeEntry("blocked", mustTime("2026-01-02T00:00:00Z")),
	}
	processed := model.Entries{document[0]}

	next := stored.Advance(document, processed)
	if !next.Watermark.Equal(mustTime("2026-01-02T00:00:00Z")) {
		t.Fatalf("unexpected watermark: %v", next.Watermark)
	}
	if len(next.TieHashes) != 1 || next.TieHashes[0] != "kept" {
		t.Fatalf("only processed entries may enter the tie set, got %v", next.TieHashes)
	}

	// When the rule is removed the blocked entry is admitted again.
	if next.Classify(document[1]) != Admit {
		t.Fatal("a filtered-out entry must remain admissible after the cursor advances")
	}
}

func TestFilteredUndatedEntriesAreNotRemembered(t *testing.T) {
	document := model.Entries{
		makeEntry("kept-undated", time.Time{}),
		makeEntry("blocked-undated", time.Time{}),
	}
	processed := model.Entries{document[0]}

	next := (*FeedCursor)(nil).Advance(document, processed)
	if len(next.UndatedHashes) != 1 || next.UndatedHashes[0] != "kept-undated" {
		t.Fatalf("only processed undated entries may be remembered, got %v", next.UndatedHashes)
	}
	if next.Classify(document[1]) != Admit {
		t.Fatal("a filtered-out undated entry must remain admissible")
	}
}

func TestTieSetRemembersEntriesThatDisappearAndReappear(t *testing.T) {
	stored := &FeedCursor{
		Watermark: mustTime("2026-01-02T00:00:00Z"),
		TieHashes: []string{"b", "c"},
	}

	// A truncated document drops "c"; it must remain in the tie set so its
	// later reappearance is not mistaken for a new entry.
	truncated := model.Entries{
		makeEntry("b", mustTime("2026-01-02T00:00:00Z")),
	}
	next := stored.Advance(truncated, truncated)
	if len(next.TieHashes) != 2 {
		t.Fatalf("expected tie set union of 2, got %v", next.TieHashes)
	}

	if next.Classify(makeEntry("c", mustTime("2026-01-02T00:00:00Z"))) != Skip {
		t.Fatal("reappearing boundary entry must stay classified as known")
	}
}

func TestRegressionGuardResetsCursor(t *testing.T) {
	stored := &FeedCursor{
		Watermark: mustTime("2026-02-01T00:00:00Z"),
		TieHashes: []string{"x"},
	}

	// The feed now serves only entries far older than the stored watermark:
	// typical of a feed that was rewritten or replaced.
	document := model.Entries{
		makeEntry("a", mustTime("2026-01-01T00:00:00Z")),
	}

	next := stored.Advance(document, document)
	if next.HasState() {
		t.Fatal("cursor must be reset when the document regresses beyond the skew window")
	}
	if len(next.Filter(document)) != 1 {
		t.Fatal("after a reset every entry must be admitted")
	}
}

func TestAdvanceWithAllUndatedDocumentKeepsWatermark(t *testing.T) {
	stored := &FeedCursor{
		Watermark:     mustTime("2026-01-02T00:00:00Z"),
		TieHashes:     []string{"b"},
		UndatedHashes: []string{"u1"},
	}

	document := model.Entries{
		makeEntry("u1", time.Time{}),
		makeEntry("u2", time.Time{}),
	}

	next := stored.Advance(document, document)
	if !next.Watermark.Equal(stored.Watermark) {
		t.Fatal("watermark must not move without dated entries")
	}
	if len(next.UndatedHashes) != 2 {
		t.Fatalf("expected 2 undated hashes, got %v", next.UndatedHashes)
	}
}

func TestUndatedHashPruning(t *testing.T) {
	stored := &FeedCursor{
		UndatedHashes: []string{"gone", "kept"},
	}

	document := model.Entries{
		makeEntry("kept", time.Time{}),
		makeEntry("fresh", time.Time{}),
	}

	next := stored.Advance(document, document)
	for _, hash := range next.UndatedHashes {
		if hash == "gone" {
			t.Fatal("undated hashes that left the document must be pruned")
		}
	}
}

func TestOverflowResetsCursor(t *testing.T) {
	document := make(model.Entries, MaxTieHashes+1)
	sameInstant := mustTime("2026-01-01T00:00:00Z")
	for i := range document {
		document[i] = makeEntry(fmt.Sprintf("hash-%04d", i), sameInstant)
	}

	next := (*FeedCursor)(nil).Advance(document, document)
	if next.HasState() {
		t.Fatal("a tie set overflow must reset the cursor to a safe full-reprocess state")
	}
}

func TestParseCursorTimePrefersFirstUsableCandidate(t *testing.T) {
	got := ParseCursorTime("", "not-a-date", "2026-01-02T03:04:05Z")
	expected := mustTime("2026-01-02T03:04:05Z")
	if !got.Equal(expected) {
		t.Fatalf("expected %v, got %v", expected, got)
	}

	if !ParseCursorTime("", "garbage").IsZero() {
		t.Fatal("unusable candidates must yield the zero time, never time.Now")
	}
}

func TestParseCursorTimeNormalizesToUTC(t *testing.T) {
	got := ParseCursorTime("2026-01-02T11:00:00+03:00")
	if got.Location() != time.UTC {
		t.Fatalf("cursor time must be normalized to UTC, got location %v", got.Location())
	}
	if !got.Equal(mustTime("2026-01-02T08:00:00Z")) {
		t.Fatalf("unexpected normalized instant: %v", got)
	}
}
