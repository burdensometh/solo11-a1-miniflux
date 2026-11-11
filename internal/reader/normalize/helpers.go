// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package normalize

import (
	"cmp"
	"slices"
	"strings"
)

// sortedUnique returns a sorted copy of values with zero values and
// duplicates removed. It centralizes the sorted-dedup helper that used to be
// duplicated across every format adapter.
func sortedUnique[O cmp.Ordered](values []O) []O {
	sorted := make([]O, 0, len(values))
	var zero O
	for _, value := range values {
		if value == zero {
			continue
		}
		where, found := slices.BinarySearch(sorted, value)
		if found {
			continue
		}
		sorted = slices.Insert(sorted, where, value)
	}
	return sorted
}

// buildTags applies the format-specific trimming before sorting and
// deduplicating the entry tags.
func buildTags(source *Entry) []string {
	if source.TagTrimmer == nil {
		return sortedUnique(source.Tags)
	}

	trimmed := make([]string, 0, len(source.Tags))
	for _, tag := range source.Tags {
		trimmed = append(trimmed, source.TagTrimmer(tag))
	}
	return sortedUnique(trimmed)
}

// joinAuthors joins the non-empty author names with ", ". When the entry
// requests it, names are sorted and deduplicated first (JSON Feed and
// Atom 1.0 behavior).
func joinAuthors(source *Entry) string {
	authors := source.Authors
	if source.SortAndDedupAuthors {
		authors = sortedUnique(authors)
	}

	names := make([]string, 0, len(authors))
	for _, author := range authors {
		if author != "" {
			names = append(names, author)
		}
	}
	return strings.Join(names, ", ")
}
