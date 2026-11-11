// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package opml // import "miniflux.app/v2/internal/reader/opml"

import "testing"

func TestDeduplicateSubscriptionsKeepsFirstOccurrence(t *testing.T) {
	input := []subcription{
		{Title: "First", FeedURL: "https://example.org/feed.xml", CategoryName: "Category A"},
		{Title: "Duplicate", FeedURL: "https://example.org/feed.xml", CategoryName: "Category B"},
		{Title: "Second", FeedURL: " https://example.org/other.xml ", CategoryName: "Category B"},
	}

	unique, skipped := deduplicateSubscriptions(input)

	if skipped != 1 {
		t.Fatalf("Wrong number of skipped subscriptions: %d instead of 1", skipped)
	}

	if len(unique) != 2 {
		t.Fatalf("Wrong number of unique subscriptions: %d instead of 2", len(unique))
	}

	// The first occurrence wins, including its category.
	if unique[0].Title != "First" || unique[0].CategoryName != "Category A" {
		t.Errorf("Unexpected first subscription: %+v", unique[0])
	}

	// URLs are trimmed before being stored.
	if unique[1].FeedURL != "https://example.org/other.xml" {
		t.Errorf("Feed URL was not trimmed: %q", unique[1].FeedURL)
	}
}

func TestDeduplicateSubscriptionsIsCaseSensitiveForPaths(t *testing.T) {
	input := []subcription{
		{Title: "Lower", FeedURL: "https://example.org/feed.xml"},
		{Title: "Upper", FeedURL: "https://example.org/Feed.xml"},
	}

	unique, skipped := deduplicateSubscriptions(input)

	if skipped != 0 {
		t.Fatalf("Different paths must not be treated as duplicates, got %d skipped", skipped)
	}

	if len(unique) != 2 {
		t.Fatalf("Wrong number of unique subscriptions: %d instead of 2", len(unique))
	}
}
