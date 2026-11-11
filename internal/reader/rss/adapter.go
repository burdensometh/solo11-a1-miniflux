// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rss // import "miniflux.app/v2/internal/reader/rss"

import (
	"path"
	"strconv"
	"strings"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/reader/normalize"
	"miniflux.app/v2/internal/reader/sanitizer"
	"miniflux.app/v2/internal/urllib"
)

type rssAdapter struct {
	rss *rss
}

func (r *rssAdapter) buildFeed(baseURL string) *model.Feed {
	feed := normalize.NewFeed(baseURL)
	feed.Title = htmlUnescapeTrim(r.rss.Channel.Title)
	feed.FeedURL = baseURL
	feed.SiteURL = strings.TrimSpace(r.rss.Channel.Link)
	feed.Description = strings.TrimSpace(r.rss.Channel.Description)

	// Hybrid feeds declare the channel language with <dc:language> instead
	// of <language>.
	feed.Language = firstNonEmpty(r.rss.Channel.Language, r.rss.Channel.DublinCoreLanguage)

	if r.rss.Channel.TTL != "" {
		if ttl, err := strconv.Atoi(r.rss.Channel.TTL); err == nil {
			feed.TTLMinutes = ttl
		}
	}

	if r.rss.Channel.Image != nil {
		feed.IconCandidates = []string{r.rss.Channel.Image.URL}
	}

	// The Atom-style rel="self" channel link overrides the feed URL when it
	// resolves to an absolute URL.
	for _, link := range r.rss.Channel.Links {
		href := strings.TrimSpace(link.Href)
		if href == "" || link.Rel != "self" {
			continue
		}
		feed.FeedURLOverrides = append(feed.FeedURLOverrides, href)
	}

	for i := range r.rss.Channel.Items {
		r.buildEntry(feed, &r.rss.Channel.Items[i])
	}

	return feed.Build()
}

// buildEntry converts one RSS item into the neutral entry.
func (r *rssAdapter) buildEntry(feed *normalize.Feed, item *rssItem) {
	entry := feed.NewEntry()

	entry.Content = findEntryContent(item)
	entry.Enclosures = r.collectEnclosures(item)
	entry.EnclosureURLRewriter = feedburnerEnclosureRewriter(item)

	entryURL := findEntryURL(item)
	entry.URL = entryURL
	entry.SiteURLFallback = true
	entry.EnclosureFallback = true

	entry.Title = findEntryTitle(item)
	entry.TitleContentFallback = true

	if author := findEntryAuthor(item); author != "" {
		entry.Authors = []string{author}
	} else if feedAuthor := findFeedAuthor(&r.rss.Channel); feedAuthor != "" {
		entry.Authors = []string{feedAuthor}
	}

	entry.Language = item.DublinCoreLanguage

	// Date: dc:date wins over pubDate; both fall back to time.Now().
	entry.DateCandidates = []string{firstNonEmpty(item.DublinCoreDate, item.PubDate)}
	entry.DateFallbackNow = true

	// Hash: a present <guid> drives the duplicate-GUID disambiguation.
	// Otherwise the raw entry URL hashes, and the last resort is the
	// normalized title together with the content.
	entry.HashTitleContentFallback = true
	if item.GUID.Data != "" {
		entry.ID = item.GUID.Data
		entry.TrackDuplicateID = true
	} else {
		entry.HashCandidates = []string{entryURL}
	}

	if commentsURL := strings.TrimSpace(item.CommentsURL); commentsURL != "" && urllib.IsAbsoluteURL(commentsURL) {
		entry.CommentsURL = commentsURL
	}

	if item.ItunesDuration != "" {
		if duration, err := getDurationInMinutes(item.ItunesDuration); err == nil {
			entry.ReadingTime = duration
		}
	}

	entry.Tags = findEntryTags(item)
	if len(entry.Tags) == 0 {
		entry.Tags = findFeedTags(&r.rss.Channel)
	}
	entry.TagTrimmer = strings.TrimSpace
}

// collectEnclosures gathers media thumbnails, native enclosures, media
// contents and peer links in the historical order.
func (r *rssAdapter) collectEnclosures(item *rssItem) []normalize.Enclosure {
	mediaThumbnails := item.AllMediaThumbnails()
	mediaContents := item.AllMediaContents()
	mediaPeerLinks := item.AllMediaPeerLinks()
	capacity := len(mediaThumbnails) + len(item.Enclosures) + len(mediaContents) + len(mediaPeerLinks)
	enclosures := make([]normalize.Enclosure, 0, capacity)

	for _, mediaThumbnail := range mediaThumbnails {
		enclosures = append(enclosures, normalize.Enclosure{
			URL:      mediaThumbnail.URL,
			MimeType: mediaThumbnail.MimeType(),
			Size:     mediaThumbnail.Size(),
		})
	}

	for _, enclosure := range item.Enclosures {
		enclosures = append(enclosures, normalize.Enclosure{
			URL:      enclosure.URL,
			MimeType: enclosure.Type,
			Size:     enclosure.Size(),
		})
	}

	for _, mediaContent := range mediaContents {
		enclosures = append(enclosures, normalize.Enclosure{
			URL:      mediaContent.URL,
			MimeType: mediaContent.MimeType(),
			Size:     mediaContent.Size(),
		})
	}

	for _, mediaPeerLink := range mediaPeerLinks {
		enclosures = append(enclosures, normalize.Enclosure{
			URL:      mediaPeerLink.URL,
			MimeType: mediaPeerLink.MimeType(),
			Size:     mediaPeerLink.Size(),
		})
	}

	return enclosures
}

// feedburnerEnclosureRewriter returns the FeedBurner origEnclosureLink
// rewriter for an item, or nil when the element is absent.
func feedburnerEnclosureRewriter(item *rssItem) func(string) string {
	if item.FeedBurnerEnclosureLink == "" {
		return nil
	}

	return func(enclosureURL string) string {
		filename := path.Base(item.FeedBurnerEnclosureLink)
		if strings.HasSuffix(enclosureURL, filename) {
			return item.FeedBurnerEnclosureLink
		}
		return enclosureURL
	}
}

func findFeedAuthor(rssChannel *rssChannel) string {
	var author string
	switch {
	case rssChannel.ItunesAuthor != "":
		author = rssChannel.ItunesAuthor
	case rssChannel.GooglePlayAuthor != "":
		author = rssChannel.GooglePlayAuthor
	case rssChannel.ItunesOwner.String() != "":
		author = rssChannel.ItunesOwner.String()
	case rssChannel.ManagingEditor != "":
		author = rssChannel.ManagingEditor
	case rssChannel.Webmaster != "":
		author = rssChannel.Webmaster
	default:
		return ""
	}

	return sanitizer.StripTags(author)
}

func findFeedTags(rssChannel *rssChannel) []string {
	tags := make([]string, 0, len(rssChannel.Categories)+2*len(rssChannel.ItunesCategories)+1)

	tags = appendSorted(tags, strings.TrimSpace, rssChannel.Categories...)
	tags = appendSortedSeq(tags, strings.TrimSpace, rssChannel.ItunesCategoriesSeq())

	tags = appendSorted(tags, strings.TrimSpace, rssChannel.GooglePlayCategory.Text)

	return tags
}

func findEntryTitle(rssItem *rssItem) string {
	title := rssItem.Title.Content

	if rssItem.DublinCoreTitle != "" {
		title = rssItem.DublinCoreTitle
	}

	return htmlUnescapeTrim(title)
}

func findEntryURL(rssItem *rssItem) string {
	for _, link := range []string{rssItem.FeedBurnerLink, rssItem.Link} {
		if link != "" {
			return strings.TrimSpace(link)
		}
	}

	for _, atomLink := range rssItem.Links {
		if atomLink.Href != "" && (strings.EqualFold(atomLink.Rel, "alternate") || atomLink.Rel == "") {
			return strings.TrimSpace(atomLink.Href)
		}
	}

	// Specs: https://cyber.harvard.edu/rss/rss.html#ltguidgtSubelementOfLtitemgt
	// isPermaLink is optional, its default value is true.
	// If its value is false, the guid may not be assumed to be a url.
	if rssItem.GUID.IsPermaLink == "true" || rssItem.GUID.IsPermaLink == "" {
		return strings.TrimSpace(rssItem.GUID.Data)
	}

	return ""
}

func findEntryContent(rssItem *rssItem) string {
	for _, value := range []string{
		rssItem.DublinCoreContent,
		rssItem.Description,
		rssItem.GooglePlayDescription,
		rssItem.ItunesSummary,
		rssItem.ItunesSubtitle,
	} {
		if value != "" {
			return value
		}
	}
	return ""
}

func findEntryAuthor(rssItem *rssItem) string {
	var author string

	switch {
	case rssItem.GooglePlayAuthor != "":
		author = rssItem.GooglePlayAuthor
	case rssItem.ItunesAuthor != "":
		author = rssItem.ItunesAuthor
	case rssItem.DublinCoreCreator != "":
		author = rssItem.DublinCoreCreator
	case rssItem.PersonName() != "":
		author = rssItem.PersonName()
	case strings.Contains(rssItem.Author.Inner, "<![CDATA["):
		author = rssItem.Author.Data
	case rssItem.Author.Inner != "":
		author = rssItem.Author.Inner
	default:
		return ""
	}

	return sanitizer.StripTags(author)
}

func findEntryTags(rssItem *rssItem) []string {
	tags := make([]string, 0, len(rssItem.Categories)+len(rssItem.MediaCategories))

	tags = appendSorted(tags, strings.TrimSpace, rssItem.Categories...)
	tags = appendSortedSeq(tags, strings.TrimSpace, rssItem.MediaCategories.LabelsSeq())

	return tags
}
