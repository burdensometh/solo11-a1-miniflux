// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package normalize

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"miniflux.app/v2/internal/crypto"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/reader/date"
	"miniflux.app/v2/internal/reader/language"
	"miniflux.app/v2/internal/reader/sanitizer"
	"miniflux.app/v2/internal/urllib"
)

// resolveAbsoluteURL resolves rawURL against baseURL. On failure it returns
// the rawURL untouched, mirroring the historical per-format behavior.
func resolveAbsoluteURL(baseURL, rawURL string) (string, bool) {
	absoluteURL, err := urllib.ResolveToAbsoluteURL(baseURL, rawURL)
	if err != nil {
		slog.Debug("Unable to resolve absolute URL",
			slog.String("url", rawURL),
			slog.String("base_url", baseURL),
			slog.Any("error", err),
		)
		return rawURL, false
	}
	return absoluteURL, true
}

// firstAbsoluteURL returns the first candidate that resolves to an absolute
// URL against baseURL; empty candidates are skipped.
func firstAbsoluteURL(baseURL string, candidates []string) string {
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if absoluteURL, ok := resolveAbsoluteURL(baseURL, candidate); ok {
			return absoluteURL
		}
	}
	return ""
}

// Build converts the neutral feed into a [model.Feed], applying every shared
// normalization rule in a single place.
func (f *Feed) Build() *model.Feed {
	feedURL := f.FeedURL
	siteURL := strings.TrimSpace(f.SiteURL)

	if strings.TrimSpace(feedURL) == "" && f.FeedURLFallbackToBase {
		feedURL = f.BaseURL
	}

	// RSS/RDF/JSON fall back to the feed URL; Atom falls back to the base URL.
	if siteURL == "" {
		if f.SiteURLFallbackToBase {
			siteURL = f.BaseURL
		} else {
			siteURL = feedURL
		}
	}

	if trimmedFeedURL := strings.TrimSpace(feedURL); trimmedFeedURL != "" {
		if absoluteFeedURL, ok := resolveAbsoluteURL(f.BaseURL, trimmedFeedURL); ok {
			feedURL = absoluteFeedURL
		}
	}

	// RSS channel-level Atom rel="self" links take precedence over the
	// declared feed URL when they resolve successfully.
	for _, override := range f.FeedURLOverrides {
		if absoluteFeedURL, ok := resolveAbsoluteURL(feedURL, override); ok {
			feedURL = absoluteFeedURL
			break
		}
	}

	if absoluteSiteURL, ok := resolveAbsoluteURL(f.BaseURL, siteURL); ok && siteURL != "" {
		siteURL = absoluteSiteURL
	}

	feed := &model.Feed{
		Title:       f.Title,
		Description: f.Description,
		Language:    language.Normalize(f.Language),
		FeedURL:     feedURL,
		SiteURL:     siteURL,
		IconURL:     firstAbsoluteURL(siteURL, f.IconCandidates),
	}

	// RDF falls back to the base URL; every other format uses the site URL.
	if feed.Title == "" {
		if f.TitleFallbackToBase {
			feed.Title = f.BaseURL
		} else {
			feed.Title = siteURL
		}
	}

	if f.TTLMinutes >= 0 {
		feed.TTL = time.Duration(f.TTLMinutes) * time.Minute
	}

	// Track GUIDs already seen in this feed to disambiguate items from
	// non-conformant feeds that reuse the same identifier for every entry.
	seenIDs := make(map[string]int)

	feed.Entries = make(model.Entries, 0, len(f.Entries))
	for _, source := range f.Entries {
		feed.Entries = append(feed.Entries, buildEntry(feed, source, seenIDs))
	}

	return feed
}

// buildEntry applies every entry-level normalization rule.
func buildEntry(feed *model.Feed, source *Entry, seenIDs map[string]int) *model.Entry {
	entry := model.NewEntry()

	entry.Content = source.Content
	entry.CommentsURL = source.CommentsURL
	entry.ReadingTime = source.ReadingTime
	entry.Language = language.Normalize(source.Language)
	if entry.Language == "" {
		entry.Language = feed.Language
	}

	entry.Tags = buildTags(source)
	entry.Author = joinAuthors(source)

	entry.Enclosures = buildEnclosures(feed.SiteURL, source)
	entry.URL = buildEntryURL(feed, source, entry.Enclosures)
	entry.Title = buildEntryTitle(source, entry)
	entry.Date = buildEntryDate(source)
	entry.Hash = buildEntryHash(source, entry, seenIDs)

	return entry
}

// buildEntryURL resolves the entry URL using the format-declared candidates
// and the format-specific fallback chain.
func buildEntryURL(feed *model.Feed, source *Entry, enclosures model.EnclosureList) string {
	candidates := make([]string, 0, 1+len(source.FallbackURLs))
	if source.URL != "" {
		candidates = append(candidates, source.URL)
	}
	candidates = append(candidates, source.FallbackURLs...)

	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if absoluteURL, ok := resolveAbsoluteURL(feed.SiteURL, candidate); ok {
			return absoluteURL
		}
	}

	if source.AbsoluteIDFallback && urllib.IsAbsoluteURL(source.ID) {
		return source.ID
	}

	if source.SiteURLFallback {
		if source.EnclosureFallback && len(enclosures) > 0 && enclosures[0].URL != "" {
			return enclosures[0].URL
		}
		return feed.SiteURL
	}

	return ""
}

// buildEntryTitle applies the content/URL title fallbacks. The format-specific
// trimming and HTML unescaping must already be done when filling the neutral
// entry.
func buildEntryTitle(source *Entry, entry *model.Entry) string {
	if source.Title != "" {
		return source.Title
	}

	if source.TitleContentFallback {
		if len(source.TitleFallbackCandidates) > 0 {
			for _, candidate := range source.TitleFallbackCandidates {
				if title := sanitizer.TruncateHTML(candidate, 100); title != "" {
					return title
				}
			}
		} else if title := sanitizer.TruncateHTML(entry.Content, 100); title != "" {
			return title
		}
	}

	return entry.URL
}

// buildEntryDate tries the raw date candidates in order and falls back to
// the current time when requested.
func buildEntryDate(source *Entry) time.Time {
	for _, value := range source.DateCandidates {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		parsedDate, err := date.Parse(value)
		if err != nil {
			slog.Debug("Unable to parse entry date",
				slog.String("date", value),
				slog.Any("error", err),
			)
			continue
		}

		return parsedDate
	}

	if source.DateFallbackNow {
		return Now()
	}

	return time.Time{}
}

// buildEntryHash computes the entry hash from the format-declared candidates.
// When TrackDuplicateID is set (RSS), the identifier collisions are
// disambiguated with the entry URL or the entry position.
func buildEntryHash(source *Entry, entry *model.Entry, seenIDs map[string]int) string {
	if source.TrackDuplicateID && source.ID != "" {
		n := seenIDs[source.ID]
		seenIDs[source.ID] = n + 1

		switch {
		case n == 0:
			return crypto.SHA256(source.ID)
		case entry.URL != "":
			return crypto.SHA256(source.ID + "|" + entry.URL)
		default:
			return crypto.SHA256(source.ID + "|" + strconv.Itoa(n))
		}
	}

	// Candidates are used verbatim: adapters handle their own trimming.
	for _, candidate := range source.HashCandidates {
		if candidate != "" {
			return crypto.SHA256(candidate)
		}
	}

	if source.HashTitleContentFallback {
		return crypto.SHA256(entry.Title + entry.Content)
	}

	return ""
}

// buildEnclosures resolves and deduplicates the entry attachments. Dedup is
// performed on the absolute URL.
func buildEnclosures(siteURL string, source *Entry) model.EnclosureList {
	enclosures := make(model.EnclosureList, 0, len(source.Enclosures))
	seen := make(map[string]bool, len(source.Enclosures))

	for _, enclosure := range source.Enclosures {
		rawURL := strings.TrimSpace(enclosure.URL)
		if rawURL == "" {
			continue
		}

		if source.EnclosureURLRewriter != nil {
			rawURL = source.EnclosureURLRewriter(rawURL)
			rawURL = strings.TrimSpace(rawURL)
			if rawURL == "" {
				continue
			}
		}

		absoluteURL, ok := resolveAbsoluteURL(siteURL, rawURL)
		if !ok {
			continue
		}

		if seen[absoluteURL] {
			continue
		}
		seen[absoluteURL] = true

		enclosures = append(enclosures, &model.Enclosure{
			URL:      absoluteURL,
			MimeType: enclosure.MimeType,
			Size:     enclosure.Size,
		})
	}

	return enclosures
}
