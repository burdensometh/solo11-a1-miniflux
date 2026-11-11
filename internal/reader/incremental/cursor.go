// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package incremental implements format-agnostic feed refresh cursors.
//
// A cursor is a high-water mark over the normalized (cursor time, entry hash)
// ordering of the entries a feed has ever served. It lets the refresh path
// skip entries that are known to already be processed instead of pushing the
// whole feed through the per-entry storage pipeline.
//
// The cursor is only a performance filter. Correctness (no duplicate entries)
// remains guaranteed by the (feed_id, hash) unique constraint and the entry
// tombstones. An entry wrongly admitted by the cursor is simply re-processed
// and rejected at insert time; an entry wrongly skipped can only happen when
// the cursor state itself is wrong, which the regression guard below prevents.
package incremental

import (
	"cmp"
	"slices"
	"time"

	"miniflux.app/v2/internal/model"
)

// MaxTieHashes bounds the number of hashes remembered at the watermark
// timestamp. Feeds that emit more entries sharing one timestamp fall back to
// a full reprocess once the bound is exceeded, which is always safe.
const MaxTieHashes = 1000

// MaxUndatedHashes bounds the remembered hashes of entries that carry no
// usable timestamp. Undated entries cannot be ordered by time, so they are
// tracked purely by hash. Exceeding the bound disables undated filtering
// (and therefore incremental skipping for that feed) until the backlog drains.
const MaxUndatedHashes = 5000

// FeedCursor is the persisted resume point for a single feed.
//
// The zero value means "no cursor yet": every entry is admitted.
type FeedCursor struct {
	// Watermark is the greatest cursor time that has been fully processed.
	// Entries strictly after it are new. It is the zero time when the feed
	// has never completed a refresh or only contains undated entries.
	Watermark time.Time

	// TieHashes are the hashes of the processed entries whose cursor time
	// equals Watermark. They make the boundary exact when several entries
	// share the same timestamp: entries at the watermark time whose hash is
	// not in this set are still admitted (they are new), known ones are skipped.
	TieHashes []string

	// UndatedHashes are the hashes of processed entries whose source feed
	// provided no usable timestamp at all.
	UndatedHashes []string
}

// HasState reports whether a cursor has ever been persisted for the feed.
func (c *FeedCursor) HasState() bool {
	return c != nil && (!c.Watermark.IsZero() || len(c.UndatedHashes) > 0)
}

// Admission describes what the filter decided for one entry.
type Admission int

const (
	// Admit means the entry is past the cursor and must be processed.
	Admit Admission = iota
	// Skip means the entry is at or behind the cursor and was already processed.
	Skip
)

// Classify compares one entry against the cursor.
//
// Entries with a zero cursor time are undated: they can only be recognized by
// hash. Dated entries are admitted when their cursor time is strictly greater
// than the watermark, or when it equals the watermark but their hash was not
// part of the processed tie set.
func (c *FeedCursor) Classify(entry *model.Entry) Admission {
	if !c.HasState() {
		return Admit
	}

	if entry.CursorTime.IsZero() {
		if slices.Contains(c.UndatedHashes, entry.Hash) {
			return Skip
		}
		return Admit
	}

	switch {
	case entry.CursorTime.After(c.Watermark):
		return Admit
	case entry.CursorTime.Before(c.Watermark):
		return Skip
	default:
		if slices.Contains(c.TieHashes, entry.Hash) {
			return Skip
		}
		return Admit
	}
}

// Filter removes the entries that the cursor already covers, preserving the
// original feed order. It returns the entries that still need processing.
func (c *FeedCursor) Filter(entries model.Entries) model.Entries {
	if !c.HasState() {
		return entries
	}

	admitted := make(model.Entries, 0, len(entries))
	for _, entry := range entries {
		if c.Classify(entry) == Admit {
			admitted = append(admitted, entry)
		}
	}
	return admitted
}

// RegressionSkew is the tolerated clock drift between the stored watermark and
// the newest entry observed in a feed document. If every dated entry is older
// than the watermark by more than this, the feed is assumed to have been
// truncated or rewritten, and the cursor is reset rather than trusted.
const RegressionSkew = 24 * time.Hour

// Advance computes the cursor to persist after the admitted entries have been
// committed to the storage layer. allEntries is the full parsed document (it
// defines the newest timestamp observed and feeds the regression guard), while
// processedEntries is the subset that actually reached the storage layer after
// filter rules were applied. An entry filtered out of processedEntries must not
// be marked as seen: a later rule change must be able to ingest it.
//
// Advancement is monotonic: the new watermark is the maximum of the stored one
// and the greatest cursor time observed in this document. A document whose
// every dated entry is older than the stored watermark (beyond RegressionSkew)
// is treated as a regression and resets the cursor so nothing gets silently
// dropped.
func (c *FeedCursor) Advance(allEntries, processedEntries model.Entries) *FeedCursor {
	next := &FeedCursor{}
	if c != nil {
		next.Watermark = c.Watermark
		next.TieHashes = slices.Clone(c.TieHashes)
		next.UndatedHashes = slices.Clone(c.UndatedHashes)
	}

	processed := make(map[string]bool, len(processedEntries))
	for _, entry := range processedEntries {
		processed[entry.Hash] = true
	}

	var newest time.Time
	undatedSeen := make(map[string]bool)
	for _, entry := range allEntries {
		if entry.CursorTime.IsZero() {
			undatedSeen[entry.Hash] = true
			continue
		}
		if entry.CursorTime.After(newest) {
			newest = entry.CursorTime
		}
	}

	// Regression guard: a non-empty feed whose newest entry is far behind the
	// stored watermark is not a normal refresh. Drop all state and reprocess.
	if !next.Watermark.IsZero() && !newest.IsZero() &&
		newest.Add(RegressionSkew).Before(next.Watermark) {
		return &FeedCursor{}
	}

	if newest.After(next.Watermark) {
		// The watermark moves forward: entries at the old boundary are now
		// strictly below it, so their hashes are discarded and the tie set is
		// rebuilt from scratch.
		next.Watermark = newest
		next.TieHashes = next.TieHashes[:0]
	}

	// The tie set is a union: a feed document can be truncated and drop old
	// entries temporarily. If they reappear later with the watermark timestamp,
	// remembering their hashes keeps them classified as already processed. Only
	// entries that actually reached storage are eligible; filtered entries stay
	// admissible even when their timestamp equals the watermark.
	tieSet := make(map[string]bool)
	for _, hash := range next.TieHashes {
		tieSet[hash] = true
	}
	for _, entry := range allEntries {
		if !entry.CursorTime.IsZero() && entry.CursorTime.Equal(next.Watermark) && processed[entry.Hash] {
			tieSet[entry.Hash] = true
		}
	}
	tieHashes, tieOverflow := mergeHashes(nil, tieSet, MaxTieHashes)
	next.TieHashes = tieHashes

	// Undated hashes accumulate until the cap is reached; entries that drop out
	// of the feed document are pruned so the list cannot grow without bound.
	// Filtered undated entries are never remembered.
	pruned := make([]string, 0, len(next.UndatedHashes))
	for _, hash := range next.UndatedHashes {
		if undatedSeen[hash] {
			pruned = append(pruned, hash)
		}
	}
	undatedCandidates := make(map[string]bool, len(undatedSeen))
	for hash := range undatedSeen {
		if processed[hash] {
			undatedCandidates[hash] = true
		}
	}
	undatedHashes, undatedOverflow := mergeHashes(pruned, undatedCandidates, MaxUndatedHashes)
	next.UndatedHashes = undatedHashes

	if tieOverflow || undatedOverflow {
		// A tie/undated set that overflows cannot be filtered safely: return a
		// cursor that admits everything on the next refresh.
		return &FeedCursor{}
	}

	return next
}

// mergeHashes returns the sorted union of the existing list and the candidate
// set. The boolean result reports an overflow: when the union would exceed
// limit, the existing list is returned unchanged so the caller can disable
// filtering instead of persisting an incomplete boundary.
func mergeHashes(existing []string, candidates map[string]bool, limit int) ([]string, bool) {
	union := make(map[string]bool, len(existing)+len(candidates))
	for _, hash := range existing {
		union[hash] = true
	}
	for hash := range candidates {
		union[hash] = true
	}

	if len(union) > limit {
		return existing, true
	}

	out := make([]string, 0, len(union))
	for hash := range union {
		out = append(out, hash)
	}
	slices.SortFunc(out, cmp.Compare)
	return out, false
}
