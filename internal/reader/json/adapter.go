// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package json // import "miniflux.app/v2/internal/reader/json"

import (
	"html"
	"strings"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/reader/normalize"
)

type JSONAdapter struct {
	jsonFeed *JSONFeed
}

func NewJSONAdapter(jsonFeed *JSONFeed) *JSONAdapter {
	return &JSONAdapter{jsonFeed}
}

func (j *JSONAdapter) BuildFeed(baseURL string) *model.Feed {
	feed := normalize.NewFeed(baseURL)
	feed.Title = strings.TrimSpace(j.jsonFeed.Title)
	feed.FeedURL = strings.TrimSpace(j.jsonFeed.FeedURL)
	feed.FeedURLFallbackToBase = true
	feed.SiteURL = strings.TrimSpace(j.jsonFeed.HomePageURL)
	feed.Description = strings.TrimSpace(j.jsonFeed.Description)
	feed.Language = j.jsonFeed.Language
	feed.IconCandidates = []string{j.jsonFeed.FaviconURL, j.jsonFeed.IconURL}

	for _, item := range j.jsonFeed.Items {
		entry := feed.NewEntry()

		entry.Language = item.Language

		entry.URL = strings.TrimSpace(item.URL)
		entry.FallbackURLs = []string{strings.TrimSpace(item.ExternalURL)}

		entry.Title = strings.TrimSpace(item.Title)
		entry.TitleContentFallback = true
		// The JSON title fallback candidates are truncated rather than used
		// verbatim; they are evaluated in this order until one is non-empty.
		entry.TitleFallbackCandidates = []string{
			item.Summary,
			item.ContentText,
			item.ContentHTML,
		}

		// content_html is HTML, but content_text and summary are plain text
		// (JSON Feed 1.1), so they must be escaped before being stored as
		// HTML, otherwise the sanitizer drops any markup-like characters.
		if contentHTML := strings.TrimSpace(item.ContentHTML); contentHTML != "" {
			entry.Content = contentHTML
		} else if contentText := strings.TrimSpace(item.ContentText); contentText != "" {
			entry.Content = html.EscapeString(contentText)
		} else if summary := strings.TrimSpace(item.Summary); summary != "" {
			entry.Content = html.EscapeString(summary)
		}

		entry.DateCandidates = []string{item.DatePublished, item.DateModified}
		entry.DateFallbackNow = true

		entry.Authors = j.collectAuthors(item)
		entry.SortAndDedupAuthors = true

		for _, attachment := range item.Attachments {
			entry.Enclosures = append(entry.Enclosures, normalize.Enclosure{
				URL:      attachment.URL,
				MimeType: attachment.MimeType,
				Size:     attachment.Size,
			})
		}

		entry.Tags = item.Tags
		entry.TagTrimmer = strings.TrimSpace

		entry.HashCandidates = []string{
			strings.TrimSpace(item.ID),
			strings.TrimSpace(item.URL),
			strings.TrimSpace(item.ExternalURL),
			item.ContentText + item.ContentHTML + item.Summary,
		}
	}

	return feed.Build()
}

// collectAuthors returns the sorted and deduplicated author names of the item,
// falling back to the feed-level authors.
func (j *JSONAdapter) collectAuthors(item JSONItem) []string {
	names := make([]string, 0, len(j.jsonFeed.Authors)+len(item.Authors)+2)

	for _, author := range j.jsonFeed.Authors {
		names = append(names, author.name())
	}
	for _, author := range item.Authors {
		names = append(names, author.name())
	}
	names = append(names, item.Author.name(), j.jsonFeed.Author.name())

	return names
}
