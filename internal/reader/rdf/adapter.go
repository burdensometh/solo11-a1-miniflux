// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rdf // import "miniflux.app/v2/internal/reader/rdf"

import (
	"html"
	"strings"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/reader/normalize"
	"miniflux.app/v2/internal/reader/sanitizer"
)

type rdfAdapter struct {
	rdf *rdf
}

func (r *rdfAdapter) buildFeed(baseURL string) *model.Feed {
	feed := normalize.NewFeed(baseURL)
	feed.Title = sanitizer.StripTags(r.rdf.Channel.Title)
	feed.FeedURL = baseURL
	feed.SiteURL = strings.TrimSpace(r.rdf.Channel.Link)
	feed.Description = strings.TrimSpace(r.rdf.Channel.Description)
	feed.Language = r.rdf.Channel.DublinCoreLanguage
	feed.TitleFallbackToBase = true

	for _, item := range r.rdf.Items {
		entry := feed.NewEntry()

		itemLink := strings.TrimSpace(item.Link)
		entry.URL = itemLink
		entry.SiteURLFallback = true

		for _, title := range []string{item.Title, item.DublinCoreTitle} {
			title = strings.TrimSpace(title)
			if title != "" {
				entry.Title = html.UnescapeString(title)
				break
			}
		}

		if item.DublinCoreContent != "" {
			entry.Content = item.DublinCoreContent
		} else {
			entry.Content = item.Description
		}

		// The historical RDF fallback hashes the raw, untrimmed title
		// together with the description element (ignoring content:encoded).
		if itemLink != "" {
			entry.HashCandidates = []string{itemLink}
		} else {
			entry.HashCandidates = []string{item.Title + item.Description}
		}

		if item.DublinCoreDate != "" {
			entry.DateCandidates = []string{item.DublinCoreDate}
		}
		entry.DateFallbackNow = true

		switch {
		case item.DublinCoreCreator != "":
			entry.Authors = []string{sanitizer.StripTags(item.DublinCoreCreator)}
		case r.rdf.Channel.DublinCoreCreator != "":
			entry.Authors = []string{sanitizer.StripTags(r.rdf.Channel.DublinCoreCreator)}
		}

		entry.Language = item.DublinCoreLanguage
	}

	return feed.Build()
}
