// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package atom // import "miniflux.app/v2/internal/reader/atom"

import (
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/reader/normalize"
)

type atom03Adapter struct {
	atomFeed *atom03Feed
}

func (a *atom03Adapter) buildFeed(baseURL string) *model.Feed {
	feed := normalize.NewFeed(baseURL)
	feed.FeedURL = a.atomFeed.Links.firstLinkWithRelation("self")
	feed.FeedURLFallbackToBase = true
	feed.SiteURL = a.atomFeed.Links.originalLink()
	feed.SiteURLFallbackToBase = true
	feed.Title = a.atomFeed.Title.content()
	feed.Language = a.atomFeed.Language

	for _, atomEntry := range a.atomFeed.Entries {
		entry := feed.NewEntry()

		entry.Language = atomEntry.Language

		// An Atom 0.3 entry without links keeps an empty URL: there is no
		// site-URL fallback on this format.
		entry.URL = atomEntry.Links.originalLink()

		entry.Content = atomEntry.Content.content()
		if entry.Content == "" {
			entry.Content = atomEntry.Summary.content()
		}

		entry.Title = atomEntry.Title.content()
		entry.TitleContentFallback = true

		if author := atomEntry.Author.PersonName(); author != "" {
			entry.Authors = []string{author}
		} else if feedAuthor := a.atomFeed.Author.PersonName(); feedAuthor != "" {
			entry.Authors = []string{feedAuthor}
		}

		entry.DateCandidates = []string{atomEntry.Issued, atomEntry.Modified, atomEntry.Created}
		entry.DateFallbackNow = true

		// Values are hashed verbatim, including empty ones being skipped.
		entry.HashCandidates = []string{atomEntry.ID, atomEntry.Links.originalLink()}
	}

	return feed.Build()
}
