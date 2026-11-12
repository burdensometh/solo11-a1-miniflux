// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package preview fetches a few recent entries of discovered feed
// candidates so that users can preview a feed before subscribing.
//
// Previews are served from a process-wide, bounded TTL cache: the first
// request for a feed URL fetches the feed live, subsequent requests within the
// time-to-live are answered from the cache. A failed fetch is also cached for
// a shorter period (negative caching) so that a dead feed does not slow down
// every discovery attempt. Cache entries are partitioned by a fingerprint of
// the request credentials and TLS settings, so one user's authenticated
// preview can never be served to another user. Fetching one candidate must
// never prevent the other candidates from being previewed, so every error is
// attached to the candidate itself instead of being returned to the caller.
package preview // import "miniflux.app/v2/internal/reader/preview"

import (
	"bytes"
	"encoding/hex"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/reader/fetcher"
	"miniflux.app/v2/internal/reader/parser"
)

const (
	// MaxPreviewEntries is the number of recent entries exposed per preview.
	MaxPreviewEntries = 5

	// fetchTimeout bounds the time spent fetching a single feed for previewing.
	// It is intentionally shorter than the regular HTTP client timeout so that
	// several dead candidates cannot stall the discovery flow.
	fetchTimeout = 10 * time.Second
)

// Cache durations are variables so tests can shorten them.
var (
	// cacheTTL is how long a successful preview is reused.
	cacheTTL = 5 * time.Minute

	// negativeCacheTTL is how long a failed preview is remembered to avoid
	// hammering unreachable feeds on every discovery.
	negativeCacheTTL = 1 * time.Minute

	// maxCachedPreviews bounds the memory used by the process-wide cache.
	maxCachedPreviews = 1024
)

// PreviewEntry is a lightweight, serializable view of a feed entry.
type PreviewEntry struct {
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Author    string    `json:"author,omitempty"`
	Published time.Time `json:"published_at"`
}

// Preview contains the information shown next to a feed candidate.
type Preview struct {
	// FeedTitle is the title advertised inside the feed document itself.
	FeedTitle string `json:"feed_title,omitempty"`
	// SiteURL is the website the feed belongs to, when advertised.
	SiteURL string `json:"site_url,omitempty"`
	// Entries holds the most recent entries, newest first.
	Entries []*PreviewEntry `json:"entries"`
	// LastBuild is the publication date of the newest entry. It is the zero
	// value when the feed has no entries with a date.
	LastBuild time.Time `json:"last_build,omitempty"`
	// Error is set when the feed could not be fetched or parsed. When set,
	// Entries is empty and the candidate can still be subscribed manually.
	Error string `json:"error,omitempty"`
	// FromCache reports whether the preview was served from the cache instead
	// of a live fetch.
	FromCache bool `json:"from_cache"`
	// FetchedAt is when the underlying feed document was fetched.
	FetchedAt time.Time `json:"fetched_at"`
}

// FeedReference is the subset of the subscription model that the preview
// package needs to know about. It avoids an import cycle with the subscription
// package, which attaches previews back onto its candidates.
type FeedReference interface {
	FeedURL() string
	AttachPreview(*Preview)
}

// cacheEntry wraps a preview with its expiration time.
type cacheEntry struct {
	preview  *Preview
	expireAt time.Time
}

// Cache is the process-wide, bounded TTL cache of feed previews. It is safe
// for concurrent use. Keys combine the feed URL and a fingerprint of the
// request settings (credentials, proxy, TLS) so that previews cannot leak
// across users with different access to a feed.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry
}

// NewCache creates an empty preview cache.
func NewCache() *Cache {
	return &Cache{entries: make(map[string]*cacheEntry)}
}

// DefaultCache is the process-wide cache shared by every discovery flow.
var (
	DefaultCache     *Cache
	defaultCacheOnce sync.Once
)

func defaultCache() *Cache {
	defaultCacheOnce.Do(func() {
		DefaultCache = NewCache()
	})
	return DefaultCache
}

func cacheKey(feedURL, fingerprint string) string {
	return fingerprint + "|" + feedURL
}

func (c *Cache) lookup(key string) (*Preview, bool) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(entry.expireAt) {
		return nil, false
	}
	// Return a copy so callers cannot mutate the cached value.
	clone := *entry.preview
	return &clone, true
}

func (c *Cache) store(key string, p *Preview) {
	ttl := cacheTTL
	if p.Error != "" {
		ttl = negativeCacheTTL
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Simple FIFO eviction once the cache grows beyond its bound. Previews are
	// short-lived, so an approximate bound is enough.
	if len(c.entries) >= maxCachedPreviews {
		for existingKey := range c.entries {
			delete(c.entries, existingKey)
			break
		}
	}

	c.entries[key] = &cacheEntry{
		preview:  p,
		expireAt: time.Now().Add(ttl),
	}
}

// Fetcher fetches feed previews, storing the results in the shared cache.
type Fetcher struct {
	requestBuilder *fetcher.RequestBuilder
	// cookie is attached per request, scoped to the feed URL's host, matching
	// the behavior of the regular feed fetch flow.
	cookie string
	cache  *Cache
}

// NewFetcher creates a preview Fetcher backed by the process-wide cache. The
// request builder carries the discovery request's credentials, proxy and TLS
// settings; it is cloned for every request so the preview timeout does not
// leak into other fetches. The cookie, when non-empty, is attached per request
// and scoped to the feed URL's host.
func NewFetcher(requestBuilder *fetcher.RequestBuilder, cookie string) *Fetcher {
	return &Fetcher{
		requestBuilder: requestBuilder,
		cookie:         cookie,
		cache:          defaultCache(),
	}
}

// fingerprint summarizes the request settings that can change the response of
// a feed URL, so previews fetched with different credentials or transport
// settings are never mixed.
func (f *Fetcher) fingerprint() string {
	hasher := fnv.New64a()
	headers := f.requestBuilder.Headers()
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		hasher.Write([]byte(key))
		hasher.Write([]byte(headers.Get(key)))
	}
	hasher.Write([]byte(f.cookie))
	hasher.Write([]byte(f.requestBuilder.TransportFingerprint()))
	return hex.EncodeToString(hasher.Sum(nil))
}

// AttachPreviews fetches a preview for every candidate and attaches it.
// Candidates are fetched concurrently; each failure is recorded on the
// candidate's preview and never aborts the others. The returned slice keeps
// the caller's ordering, which reflects the discovery order on the page.
func (f *Fetcher) AttachPreviews(candidates []FeedReference) {
	var wg sync.WaitGroup
	for _, candidate := range candidates {
		wg.Add(1)
		go func(c FeedReference) {
			defer wg.Done()
			c.AttachPreview(f.Preview(c.FeedURL()))
		}(candidate)
	}
	wg.Wait()
}

// PreviewsForURLs fetches a preview for every feed URL concurrently and
// returns them in the same order. Failures are embedded in the Preview
// objects, so the result always contains one entry per requested URL.
func (f *Fetcher) PreviewsForURLs(feedURLs []string) []*Preview {
	previews := make([]*Preview, len(feedURLs))
	var wg sync.WaitGroup
	for i, feedURL := range feedURLs {
		wg.Add(1)
		go func(index int, url string) {
			defer wg.Done()
			previews[index] = f.Preview(url)
		}(i, feedURL)
	}
	wg.Wait()
	return previews
}

// Preview returns a preview for the given feed URL, using the cache when a
// fresh entry for the same request fingerprint is available. The error is
// never returned to the caller: it is embedded in the returned Preview so that
// one broken feed cannot block the whole discovery flow.
func (f *Fetcher) Preview(feedURL string) *Preview {
	key := cacheKey(feedURL, f.fingerprint())
	if p, ok := f.cache.lookup(key); ok {
		p.FromCache = true
		return p
	}

	p := f.fetch(feedURL)
	f.cache.store(key, p)
	return p
}

func (f *Fetcher) fetch(feedURL string) *Preview {
	slog.Debug("Fetching feed preview", slog.String("feed_url", feedURL))

	requestBuilder := f.requestBuilder.Clone().WithTimeout(fetchTimeout)
	if f.cookie != "" {
		requestBuilder = requestBuilder.WithCookie(f.cookie, feedURL)
	}
	responseHandler := fetcher.NewResponseHandler(requestBuilder.ExecuteRequest(feedURL))
	defer responseHandler.Close()

	result := &Preview{FetchedAt: time.Now()}

	if localizedError := responseHandler.LocalizedError(); localizedError != nil {
		slog.Debug("Unable to fetch feed preview",
			slog.String("feed_url", feedURL),
			slog.Any("error", localizedError.Error()),
		)
		result.Error = localizedError.Error().Error()
		return result
	}

	responseBody, localizedError := responseHandler.ReadBody(config.Opts.HTTPClientMaxBodySize())
	if localizedError != nil {
		slog.Debug("Unable to read feed preview",
			slog.String("feed_url", feedURL),
			slog.Any("error", localizedError.Error()),
		)
		result.Error = localizedError.Error().Error()
		return result
	}

	feed, parseErr := parser.ParseFeed(responseHandler.EffectiveURL(), bytes.NewReader(responseBody))
	if parseErr != nil {
		slog.Debug("Unable to parse feed preview",
			slog.String("feed_url", feedURL),
			slog.Any("error", parseErr),
		)
		result.Error = parseErr.Error()
		return result
	}

	result.FeedTitle = feed.Title
	result.SiteURL = feed.SiteURL
	result.Entries, result.LastBuild = buildPreviewEntries(feed.Entries)
	return result
}

// buildPreviewEntries returns at most MaxPreviewEntries entries, newest first,
// along with the publication date of the newest entry.
func buildPreviewEntries(entries model.Entries) ([]*PreviewEntry, time.Time) {
	sorted := make(model.Entries, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Date.After(sorted[j].Date)
	})

	limit := MaxPreviewEntries
	if len(sorted) < limit {
		limit = len(sorted)
	}

	previewEntries := make([]*PreviewEntry, 0, limit)
	var lastBuild time.Time
	for _, entry := range sorted[:limit] {
		previewEntries = append(previewEntries, &PreviewEntry{
			Title:     entry.Title,
			URL:       entry.URL,
			Author:    entry.Author,
			Published: entry.Date,
		})
		if entry.Date.After(lastBuild) {
			lastBuild = entry.Date
		}
	}
	return previewEntries, lastBuild
}
