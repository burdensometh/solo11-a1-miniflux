// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api // import "miniflux.app/v2/internal/api"

import (
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"miniflux.app/v2/internal/model"
)

// v1Entry is the frozen serialization of an entry for API v1.
//
// It deliberately mirrors the historical model.Entry JSON layout field by
// field. New entry attributes must NOT be added here: they belong to v2Entry,
// so legacy clients that decode into rigid structs or assert on the exact
// payload can never be affected by a server upgrade.
type v1Entry struct {
	ID          int64               `json:"id"`
	UserID      int64               `json:"user_id"`
	FeedID      int64               `json:"feed_id"`
	Status      string              `json:"status"`
	Hash        string              `json:"hash"`
	Title       string              `json:"title"`
	URL         string              `json:"url"`
	CommentsURL string              `json:"comments_url"`
	Language    string              `json:"language"`
	Date        time.Time           `json:"published_at"`
	CreatedAt   time.Time           `json:"created_at"`
	ChangedAt   time.Time           `json:"changed_at"`
	Content     string              `json:"content"`
	Author      string              `json:"author"`
	ShareCode   string              `json:"share_code"`
	Starred     bool                `json:"starred"`
	ReadingTime int                 `json:"reading_time"`
	Enclosures  model.EnclosureList `json:"enclosures"`
	Feed        *model.Feed         `json:"feed,omitempty"`
	Tags        []string            `json:"tags"`
}

// v1EntryResponse keeps the historical list envelope {"total","entries"}.
type v1EntryResponse struct {
	Total   int        `json:"total"`
	Entries []*v1Entry `json:"entries"`
}

func newV1Entry(entry *model.Entry) *v1Entry {
	return &v1Entry{
		ID:          entry.ID,
		UserID:      entry.UserID,
		FeedID:      entry.FeedID,
		Status:      entry.Status,
		Hash:        entry.Hash,
		Title:       entry.Title,
		URL:         entry.URL,
		CommentsURL: entry.CommentsURL,
		Language:    entry.Language,
		Date:        entry.Date,
		CreatedAt:   entry.CreatedAt,
		ChangedAt:   entry.ChangedAt,
		Content:     entry.Content,
		Author:      entry.Author,
		ShareCode:   entry.ShareCode,
		Starred:     entry.Starred,
		ReadingTime: entry.ReadingTime,
		Enclosures:  entry.Enclosures,
		Feed:        entry.Feed,
		Tags:        entry.Tags,
	}
}

func newV1Entries(entries model.Entries) []*v1Entry {
	result := make([]*v1Entry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, newV1Entry(entry))
	}
	return result
}

// excerptLength caps the plain-text preview generated for v2 clients.
const excerptLength = 350

// v2Entry is the API v2 serialization of an entry.
//
// Compatibility guarantees:
//
//   - Every v1 field is preserved with the same name and type, so a lenient
//     v1 client reading a v2 response still works.
//   - New fields are additive and nullable/omitempty, so a lenient v2 client
//     talking to a slightly older server degrades gracefully.
//   - The new fields never appear on v1 responses.
type v2Entry struct {
	ID          int64               `json:"id"`
	UserID      int64               `json:"user_id"`
	FeedID      int64               `json:"feed_id"`
	Status      string              `json:"status"`
	Hash        string              `json:"hash"`
	Title       string              `json:"title"`
	URL         string              `json:"url"`
	CommentsURL string              `json:"comments_url"`
	Language    string              `json:"language"`
	Date        time.Time           `json:"published_at"`
	CreatedAt   time.Time           `json:"created_at"`
	ChangedAt   time.Time           `json:"changed_at"`
	Content     string              `json:"content"`
	Author      string              `json:"author"`
	ShareCode   string              `json:"share_code"`
	Starred     bool                `json:"starred"`
	ReadingTime int                 `json:"reading_time"`
	Enclosures  model.EnclosureList `json:"enclosures"`
	Feed        *model.Feed         `json:"feed,omitempty"`
	Tags        []string            `json:"tags"`

	// EntryID is the v2 naming for the resource identifier; "id" stays as an
	// alias so existing consumers do not break.
	EntryID int64 `json:"entry_id"`
	// ExternalURL names the page URL explicitly; "url" remains as an alias.
	ExternalURL string `json:"external_url"`
	// Excerpt is a plain-text, length-capped preview derived from content.
	Excerpt string `json:"excerpt"`
	// ImageURL is the first usable image found in the entry content, if any.
	ImageURL string `json:"image_url,omitempty"`
	// HasMedia reports whether the entry carries an audio/video enclosure.
	HasMedia bool `json:"has_media"`
}

// v2EntryResponse is the v2 list envelope. "entries" is preserved; "total" is
// only computed and included when the client asks for it, and the cursor
// metadata is purely additive.
type v2EntryResponse struct {
	Entries    []*v2Entry `json:"entries"`
	Total      *int       `json:"total,omitempty"`
	Limit      int        `json:"limit"`
	NextCursor string     `json:"next_cursor,omitempty"`
	HasMore    bool       `json:"has_more"`
}

func newV2Entry(entry *model.Entry) *v2Entry {
	excerpt, imageURL := extractPreview(entry.Content)
	return &v2Entry{
		ID:          entry.ID,
		UserID:      entry.UserID,
		FeedID:      entry.FeedID,
		Status:      entry.Status,
		Hash:        entry.Hash,
		Title:       entry.Title,
		URL:         entry.URL,
		CommentsURL: entry.CommentsURL,
		Language:    entry.Language,
		Date:        entry.Date,
		CreatedAt:   entry.CreatedAt,
		ChangedAt:   entry.ChangedAt,
		Content:     entry.Content,
		Author:      entry.Author,
		ShareCode:   entry.ShareCode,
		Starred:     entry.Starred,
		ReadingTime: entry.ReadingTime,
		Enclosures:  entry.Enclosures,
		Feed:        entry.Feed,
		Tags:        entry.Tags,
		EntryID:     entry.ID,
		ExternalURL: entry.URL,
		Excerpt:     excerpt,
		ImageURL:    imageURL,
		HasMedia:    entry.Enclosures.ContainsAudioOrVideo(),
	}
}

func newV2Entries(entries model.Entries) []*v2Entry {
	result := make([]*v2Entry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, newV2Entry(entry))
	}
	return result
}

// extractPreview derives a plain-text excerpt and the first image URL from
// HTML content. It never fails on malformed HTML: goquery parses best-effort
// and the zero values are returned when nothing usable is found.
func extractPreview(htmlContent string) (excerpt string, imageURL string) {
	if htmlContent == "" {
		return "", ""
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		return "", ""
	}

	doc.Find("img").EachWithBreak(func(_ int, img *goquery.Selection) bool {
		if src, exists := img.Attr("src"); exists && strings.TrimSpace(src) != "" {
			imageURL = strings.TrimSpace(src)
			return false
		}
		return true
	})

	// Scripts and styles are not human-readable content: drop them before
	// turning the document into a plain-text preview. Entry content reaching
	// this handler has already been sanitized upstream; this is purely a
	// presentation cleanup.
	doc.Find("script, style, noscript").Remove()

	text := strings.Join(strings.Fields(doc.Text()), " ")
	if len(text) > excerptLength {
		text = strings.TrimSpace(text[:excerptLength]) + "…"
	}
	return text, imageURL
}
