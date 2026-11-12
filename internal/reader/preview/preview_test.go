// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package preview

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/reader/fetcher"
)

func TestMain(m *testing.M) {
	// httptest serves feeds on loopback addresses, which the fetcher refuses
	// unless FETCHER_ALLOW_PRIVATE_NETWORKS is enabled.
	if err := os.Setenv("FETCHER_ALLOW_PRIVATE_NETWORKS", "1"); err != nil {
		fmt.Fprintf(os.Stderr, "Unable to configure test environment: %v\n", err)
		os.Exit(1)
	}

	options, err := config.NewConfigParser().ParseEnvironmentVariables()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to parse test configuration: %v\n", err)
		os.Exit(1)
	}
	config.Opts = options
	os.Exit(m.Run())
}

const rssFeedTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Example Feed</title>
    <link>https://example.com/</link>
    <description>An example feed</description>
    %s
  </channel>
</rss>`

func rssItem(title, pubDate string) string {
	return fmt.Sprintf(`<item><title>%s</title><link>https://example.com/%s</link><pubDate>%s</pubDate></item>`, title, title, pubDate)
}

// resetDefaultCache gives each test an empty process-wide cache.
func resetDefaultCache() {
	DefaultCache = NewCache()
}

func newPreviewTestServer() *httptest.Server {
	older := rssItem("older", "Mon, 01 Jan 2024 00:00:00 GMT")
	newer := rssItem("newer", "Wed, 03 Jan 2024 00:00:00 GMT")
	middle := rssItem("middle", "Tue, 02 Jan 2024 00:00:00 GMT")

	mux := http.NewServeMux()
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, rssFeedTemplate, older+newer+middle)
	})
	mux.HandleFunc("/broken.xml", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("/not-a-feed.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>not a feed</body></html>")
	})
	return httptest.NewServer(mux)
}

func TestPreviewSuccessSortsAndLimitsEntries(t *testing.T) {
	resetDefaultCache()
	server := newPreviewTestServer()
	defer server.Close()

	// Add more than MaxPreviewEntries entries through a dedicated feed.
	var items strings.Builder
	for i := 0; i < MaxPreviewEntries+3; i++ {
		items.WriteString(rssItem(fmt.Sprintf("item-%02d", i), fmt.Sprintf("Mon, %02d Jan 2024 00:00:00 GMT", i+1)))
	}
	server.Config.Handler.(*http.ServeMux).HandleFunc("/many.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, rssFeedTemplate, items.String())
	})

	f := NewFetcher(fetcher.NewRequestBuilder(), "")

	p := f.Preview(server.URL + "/feed.xml")
	if p.Error != "" {
		t.Fatalf("Unexpected preview error: %s", p.Error)
	}
	if p.FeedTitle != "Example Feed" {
		t.Fatalf(`Unexpected feed title: %q`, p.FeedTitle)
	}
	if len(p.Entries) != 3 {
		t.Fatalf(`Expected 3 entries, got %d`, len(p.Entries))
	}
	// Entries must be newest first.
	if p.Entries[0].Title != "newer" || p.Entries[1].Title != "middle" || p.Entries[2].Title != "older" {
		t.Fatalf("Entries are not sorted newest first: %+v", p.Entries)
	}
	if !p.LastBuild.Equal(p.Entries[0].Published) {
		t.Fatal("LastBuild should match the newest entry date")
	}
	if p.FromCache {
		t.Fatal("A first fetch must not be flagged as cached")
	}

	many := f.Preview(server.URL + "/many.xml")
	if len(many.Entries) != MaxPreviewEntries {
		t.Fatalf(`Expected %d entries after truncation, got %d`, MaxPreviewEntries, len(many.Entries))
	}
	if many.Entries[0].Title != "item-07" {
		t.Fatalf("Newest entry should be first, got %q", many.Entries[0].Title)
	}
}

func TestPreviewCachesSuccessfulFetch(t *testing.T) {
	resetDefaultCache()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, rssFeedTemplate, rssItem("a", "Mon, 01 Jan 2024 00:00:00 GMT"))
	}))
	defer server.Close()

	originalTTL := cacheTTL
	cacheTTL = 100 * time.Millisecond
	defer func() { cacheTTL = originalTTL }()

	f := NewFetcher(fetcher.NewRequestBuilder(), "")
	feedURL := server.URL + "/feed.xml"

	first := f.Preview(feedURL)
	second := f.Preview(feedURL)
	if !second.FromCache {
		t.Fatal("The second preview within the TTL must come from the cache")
	}
	if requests.Load() != 1 {
		t.Fatalf("Expected a single HTTP request, got %d", requests.Load())
	}
	if first.Error != "" || second.Error != "" {
		t.Fatal("Cached preview must keep the original data")
	}

	time.Sleep(150 * time.Millisecond)
	third := f.Preview(feedURL)
	if third.FromCache {
		t.Fatal("An expired cache entry must trigger a new fetch")
	}
	if requests.Load() != 2 {
		t.Fatalf("Expected two HTTP requests after expiration, got %d", requests.Load())
	}
}

func TestPreviewNegativeCacheOnFailure(t *testing.T) {
	resetDefaultCache()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	originalTTL := negativeCacheTTL
	negativeCacheTTL = 100 * time.Millisecond
	defer func() { negativeCacheTTL = originalTTL }()

	f := NewFetcher(fetcher.NewRequestBuilder(), "")
	feedURL := server.URL + "/broken.xml"

	first := f.Preview(feedURL)
	if first.Error == "" {
		t.Fatal("A failed fetch must populate the preview error")
	}
	second := f.Preview(feedURL)
	if second.Error == "" {
		t.Fatal("A negatively cached failure must keep its error")
	}
	if !second.FromCache {
		t.Fatal("The second failure within the negative TTL must come from the cache")
	}
	if requests.Load() != 1 {
		t.Fatalf("Expected a single HTTP request thanks to negative caching, got %d", requests.Load())
	}

	time.Sleep(150 * time.Millisecond)
	third := f.Preview(feedURL)
	if third.FromCache {
		t.Fatal("An expired negative cache entry must be retried")
	}
	if requests.Load() != 2 {
		t.Fatalf("Expected two HTTP requests after negative cache expiration, got %d", requests.Load())
	}
}

func TestPreviewParseFailureIsReportedNotReturned(t *testing.T) {
	resetDefaultCache()
	server := newPreviewTestServer()
	defer server.Close()

	f := NewFetcher(fetcher.NewRequestBuilder(), "")
	p := f.Preview(server.URL + "/not-a-feed.xml")
	if p.Error == "" {
		t.Fatal("An HTML document must produce a preview error, not a hard failure")
	}
	if len(p.Entries) != 0 {
		t.Fatal("A failed preview must not contain entries")
	}
}

type fakeCandidate struct {
	url     string
	preview *Preview
}

func (c *fakeCandidate) FeedURL() string          { return c.url }
func (c *fakeCandidate) AttachPreview(p *Preview) { c.preview = p }

func TestAttachPreviewsIsolatesFailuresAndKeepsOrder(t *testing.T) {
	resetDefaultCache()
	server := newPreviewTestServer()
	defer server.Close()

	f := NewFetcher(fetcher.NewRequestBuilder(), "")
	candidates := []FeedReference{
		&fakeCandidate{url: server.URL + "/broken.xml"},
		&fakeCandidate{url: server.URL + "/feed.xml"},
		&fakeCandidate{url: server.URL + "/not-a-feed.xml"},
	}

	f.AttachPreviews(candidates)

	for i, candidate := range candidates {
		fc := candidate.(*fakeCandidate)
		if fc.preview == nil {
			t.Fatalf("Candidate %d has no preview attached", i)
		}
	}

	if candidates[0].(*fakeCandidate).preview.Error == "" {
		t.Fatal("The broken feed should carry an error")
	}
	if candidates[1].(*fakeCandidate).preview.Error != "" {
		t.Fatal("The valid feed should not carry an error")
	}
	if candidates[2].(*fakeCandidate).preview.Error == "" {
		t.Fatal("The unparseable feed should carry an error")
	}
}

func TestPreviewsForURLsReturnsOneResultPerURL(t *testing.T) {
	resetDefaultCache()
	server := newPreviewTestServer()
	defer server.Close()

	f := NewFetcher(fetcher.NewRequestBuilder(), "")
	urls := []string{server.URL + "/feed.xml", server.URL + "/broken.xml"}

	previews := f.PreviewsForURLs(urls)
	if len(previews) != len(urls) {
		t.Fatalf("Expected %d previews, got %d", len(urls), len(previews))
	}
	if previews[0].Error != "" || previews[1].Error == "" {
		t.Fatal("Previews must keep request order and embed failures")
	}
}

func TestPreviewCacheIsSharedAcrossFetchers(t *testing.T) {
	resetDefaultCache()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, rssFeedTemplate, rssItem("a", "Mon, 01 Jan 2024 00:00:00 GMT"))
	}))
	defer server.Close()

	feedURL := server.URL + "/feed.xml"

	// Two independent fetchers, as created by two discovery requests, share
	// the process-wide cache: the second fetcher must not hit the network.
	firstFetcher := NewFetcher(fetcher.NewRequestBuilder(), "")
	first := firstFetcher.Preview(feedURL)
	if first.FromCache {
		t.Fatal("The first fetch must populate the cache")
	}

	secondFetcher := NewFetcher(fetcher.NewRequestBuilder(), "")
	second := secondFetcher.Preview(feedURL)
	if !second.FromCache {
		t.Fatal("The second fetcher must be served by the shared cache")
	}
	if requests.Load() != 1 {
		t.Fatalf("Expected a single HTTP request across fetchers, got %d", requests.Load())
	}
}

func TestPreviewCachePartitionsByCredentials(t *testing.T) {
	resetDefaultCache()

	var requests atomic.Int32
	var seenAuth []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, rssFeedTemplate, rssItem("a", "Mon, 01 Jan 2024 00:00:00 GMT"))
	}))
	defer server.Close()

	feedURL := server.URL + "/feed.xml"

	anonymous := NewFetcher(fetcher.NewRequestBuilder(), "")
	if p := anonymous.Preview(feedURL); p.FromCache || p.Error != "" {
		t.Fatalf("Unexpected anonymous preview: %+v", p)
	}

	authenticated := NewFetcher(
		fetcher.NewRequestBuilder().WithUsernameAndPassword("alice", "secret"),
		"",
	)
	p := authenticated.Preview(feedURL)
	if p.FromCache {
		t.Fatal("A different credential fingerprint must not reuse the anonymous preview")
	}
	if requests.Load() != 2 {
		t.Fatalf("Expected two HTTP requests for two fingerprints, got %d", requests.Load())
	}
	if len(seenAuth) != 2 || seenAuth[0] != "" || seenAuth[1] == "" {
		t.Fatalf("Authorization headers were not propagated as expected: %v", seenAuth)
	}

	// Repeating the authenticated fetch must hit the cache for that fingerprint.
	again := authenticated.Preview(feedURL)
	if !again.FromCache {
		t.Fatal("The second authenticated fetch should be served from the cache")
	}
	if requests.Load() != 2 {
		t.Fatalf("The cached authenticated preview should not trigger another request, got %d", requests.Load())
	}
}
