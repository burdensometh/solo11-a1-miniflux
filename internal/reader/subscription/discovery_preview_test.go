// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package subscription

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/reader/fetcher"
	"miniflux.app/v2/internal/reader/preview"
)

// TestDiscoveryThenPreviewEndToEnd serves an HTML page advertising two feeds,
// runs the subscription finder against it and previews the discovered
// candidates, including a broken one. It exercises the whole
// discovery -> preview -> subscribe selection chain without a database.
func TestDiscoveryThenPreviewEndToEnd(t *testing.T) {
	t.Setenv("FETCHER_ALLOW_PRIVATE_NETWORKS", "1")
	parsedOptions, err := config.NewConfigParser().ParseEnvironmentVariables()
	if err != nil {
		t.Fatalf("Unable to parse test options: %v", err)
	}
	previousOptions := config.Opts
	config.Opts = parsedOptions
	t.Cleanup(func() { config.Opts = previousOptions })

	mux := http.NewServeMux()
	mux.HandleFunc("/index.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html>
<html>
  <head>
    <link rel="alternate" type="application/rss+xml" title="Main feed" href="/feed.xml">
    <link rel="alternate" type="application/atom+xml" title="Comments feed" href="/comments.xml">
  </head>
  <body><a href="/broken.xml">broken</a></body>
</html>`)
	})
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Main feed</title><link>http://example.com/</link><description>d</description>
<item><title>Fresh entry</title><link>http://example.com/fresh</link><pubDate>Wed, 03 Jan 2024 00:00:00 GMT</pubDate></item>
<item><title>Old entry</title><link>http://example.com/old</link><pubDate>Mon, 01 Jan 2024 00:00:00 GMT</pubDate></item>
</channel></rss>`)
	})
	mux.HandleFunc("/comments.xml", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "feed gone", http.StatusGone)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	// Step 1: discover candidates from the home page.
	finder := NewSubscriptionFinder(fetcher.NewRequestBuilder())
	subscriptions, localizedError := finder.FindSubscriptions(server.URL+"/index.html", "", "")
	if localizedError != nil {
		t.Fatalf("Discovery failed: %v", localizedError)
	}
	if len(subscriptions) != 2 {
		t.Fatalf("Expected 2 candidates, got %d", len(subscriptions))
	}

	// Candidates keep the page order, which drives the selection page order.
	if subscriptions[0].Title != "Main feed" || subscriptions[1].Title != "Comments feed" {
		t.Fatalf("Candidates are not in page order: %q, %q", subscriptions[0].Title, subscriptions[1].Title)
	}

	// Step 2: preview every candidate concurrently. The broken one must not
	// prevent the valid one from being previewed.
	previewFetcher := preview.NewFetcher(fetcher.NewRequestBuilder(), "")
	previewFetcher.AttachPreviews(subscriptions.FeedReferences())

	if subscriptions[0].Preview == nil || subscriptions[1].Preview == nil {
		t.Fatal("Every candidate must have a preview attached")
	}
	if subscriptions[0].Preview.Error != "" {
		t.Fatalf("The main feed preview should succeed: %s", subscriptions[0].Preview.Error)
	}
	if len(subscriptions[0].Preview.Entries) != 2 {
		t.Fatalf("Expected 2 preview entries, got %d", len(subscriptions[0].Preview.Entries))
	}
	if subscriptions[0].Preview.Entries[0].Title != "Fresh entry" {
		t.Fatal("Preview entries must be sorted newest first")
	}
	if subscriptions[1].Preview.Error == "" {
		t.Fatal("The broken comments feed should carry a preview error")
	}

	// Step 3: a second preview of the valid feed must be served from the cache.
	cached := previewFetcher.Preview(subscriptions[0].URL)
	if !cached.FromCache {
		t.Fatal("The second preview should be served from the cache")
	}
}
