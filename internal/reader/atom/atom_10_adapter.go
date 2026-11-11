// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package atom // import "miniflux.app/v2/internal/reader/atom"

import (
	"strconv"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/reader/normalize"
	"miniflux.app/v2/internal/urllib"
)

type atom10Adapter struct {
	atomFeed *atom10Feed
}

func (a *atom10Adapter) buildFeed(baseURL string) *model.Feed {
	feed := normalize.NewFeed(baseURL)
	feed.FeedURL = a.atomFeed.Links.firstLinkWithRelation("self")
	feed.FeedURLFallbackToBase = true
	feed.SiteURL = a.atomFeed.Links.originalLink()
	feed.SiteURLFallbackToBase = true
	feed.Title = a.atomFeed.Title.body()
	feed.Description = a.atomFeed.Subtitle.body()
	feed.Language = a.atomFeed.Language
	feed.IconCandidates = []string{a.atomFeed.Icon, a.atomFeed.Logo}

	for _, atomEntry := range a.atomFeed.Entries {
		entry := feed.NewEntry()

		entry.URL = atomEntry.Links.originalLink()
		entry.AbsoluteIDFallback = true
		entry.ID = atomEntry.ID
		entry.SiteURLFallback = true

		entry.Content = atomEntry.Content.body()
		if entry.Content == "" {
			entry.Content = atomEntry.Summary.body()
			if entry.Content == "" {
				entry.Content = atomEntry.FirstMediaDescription()
			}
		}

		entry.Title = atomEntry.Title.title()
		entry.TitleContentFallback = true

		entry.Language = atomEntry.Language

		authors := atomEntry.Authors.personNames()
		if len(authors) == 0 {
			authors = a.atomFeed.Authors.personNames()
		}
		entry.Authors = authors
		entry.SortAndDedupAuthors = true

		entry.DateCandidates = []string{atomEntry.Published, atomEntry.Updated}
		entry.DateFallbackNow = true

		entry.Tags = atomEntry.Categories.CategoryNames()
		if len(entry.Tags) == 0 {
			entry.Tags = a.atomFeed.Categories.CategoryNames()
		}

		commentsURL := atomEntry.Links.firstLinkWithRelationAndType("replies", "text/html", "application/xhtml+xml")
		if urllib.IsAbsoluteURL(commentsURL) {
			entry.CommentsURL = commentsURL
		}

		entry.HashCandidates = []string{atomEntry.ID, atomEntry.Links.originalLink()}

		entry.Enclosures = a.collectEnclosures(atomEntry)
	}

	return feed.Build()
}

// collectEnclosures gathers media thumbnails, Atom enclosure links, media
// contents and peer links in the exact order expected by the historical
// adapter; absolute URL resolution and deduplication are left to the common
// pipeline.
func (a *atom10Adapter) collectEnclosures(atomEntry atom10Entry) []normalize.Enclosure {
	capacity := len(atomEntry.AllMediaThumbnails()) +
		len(atomEntry.Links.findAllLinksWithRelation("enclosure")) +
		len(atomEntry.AllMediaContents()) +
		len(atomEntry.AllMediaPeerLinks())

	enclosures := make([]normalize.Enclosure, 0, capacity)

	for _, mediaThumbnail := range atomEntry.AllMediaThumbnails() {
		enclosures = append(enclosures, normalize.Enclosure{
			URL:      mediaThumbnail.URL,
			MimeType: mediaThumbnail.MimeType(),
			Size:     mediaThumbnail.Size(),
		})
	}

	for _, link := range atomEntry.Links.findAllLinksWithRelation("enclosure") {
		length, _ := strconv.ParseInt(link.Length, 10, 0)
		enclosures = append(enclosures, normalize.Enclosure{
			URL:      link.Href,
			MimeType: link.Type,
			Size:     length,
		})
	}

	for _, mediaContent := range atomEntry.AllMediaContents() {
		enclosures = append(enclosures, normalize.Enclosure{
			URL:      mediaContent.URL,
			MimeType: mediaContent.MimeType(),
			Size:     mediaContent.Size(),
		})
	}

	for _, mediaPeerLink := range atomEntry.AllMediaPeerLinks() {
		enclosures = append(enclosures, normalize.Enclosure{
			URL:      mediaPeerLink.URL,
			MimeType: mediaPeerLink.MimeType(),
			Size:     mediaPeerLink.Size(),
		})
	}

	return enclosures
}
