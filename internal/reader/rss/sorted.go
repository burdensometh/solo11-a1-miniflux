// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rss

import (
	"cmp"
	"iter"
	"slices"
)

// appendSorted is identical to [appendSortedSeq] except it receives variadic
// values rather than an [iter.Seq].
func appendSorted[I any, O cmp.Ordered](sorted []O, fn func(I) O, values ...I) []O {
	sorted = slices.Grow(sorted, len(values))
	return appendSortedSeq(sorted, fn, slices.Values(values))
}

// appendSortedSeq appends elements from the "values" iterator into "sorted":
//   - "fn" is applied to every element
//   - elements are inserted so the slice stays sorted
//   - duplicate elements are not inserted
func appendSortedSeq[I any, O cmp.Ordered](sorted []O, fn func(I) O, values iter.Seq[I]) []O {
	var zero O

	for in := range values {
		out := fn(in)
		if out == zero {
			continue
		}

		where, found := slices.BinarySearch(sorted, out)
		if found {
			continue
		}

		sorted = slices.Insert(sorted, where, out)
	}

	return sorted
}
