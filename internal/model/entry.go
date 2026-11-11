// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model // import "miniflux.app/v2/internal/model"

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Entry statuses and default sorting order.
const (
	EntryStatusUnread       = "unread"
	EntryStatusRead         = "read"
	DefaultSortingOrder     = "published_at"
	DefaultSortingDirection = "asc"
)

// MaxEntryLimit is the maximum allowed value for the "limit" query parameter
// and for the user "entries_per_page" preference.
const MaxEntryLimit = 1000

// MaxEntryIDsLimit is the maximum allowed value for the "limit" query parameter
// for the entry ID list endpoints.
const MaxEntryIDsLimit = 10000

// Entry represents a feed item in the system.
type Entry struct {
	ID          int64         `json:"id"`
	UserID      int64         `json:"user_id"`
	FeedID      int64         `json:"feed_id"`
	Status      string        `json:"status"`
	Hash        string        `json:"hash"`
	Title       string        `json:"title"`
	URL         string        `json:"url"`
	CommentsURL string        `json:"comments_url"`
	Language    string        `json:"language"`
	Date        time.Time     `json:"published_at"`
	CreatedAt   time.Time     `json:"created_at"`
	ChangedAt   time.Time     `json:"changed_at"`
	Content     string        `json:"content"`
	Author      string        `json:"author"`
	ShareCode   string        `json:"share_code"`
	Starred     bool          `json:"starred"`
	ReadingTime int           `json:"reading_time"`
	// Version is the optimistic concurrency token of the entry. It is
	// incremented on every successful write and exposed to clients as an
	// ETag. A write carrying a stale version is rejected with HTTP 409.
	Version     int64         `json:"version"`
	Enclosures  EnclosureList `json:"enclosures"`
	Feed        *Feed         `json:"feed,omitempty"`
	Tags        []string      `json:"tags"`
}

func NewEntry() *Entry {
	return &Entry{
		Enclosures: make(EnclosureList, 0),
		Tags:       make([]string, 0),
		Feed: &Feed{
			Category: &Category{},
			Icon:     &FeedIcon{},
		},
	}
}

// ShouldMarkAsReadOnView Return whether the entry should be marked as viewed considering all user settings and entry state.
func (e *Entry) ShouldMarkAsReadOnView(user *User) bool {
	// Already read, no need to mark as read again. Removed entries are not marked as read
	if e.Status != EntryStatusUnread {
		return false
	}

	// There is an enclosure, markAsRead will happen at enclosure completion time, no need to mark as read on view
	if user.MarkReadOnMediaPlayerCompletion && e.Enclosures.ContainsAudioOrVideo() {
		return false
	}

	// The user wants to mark as read on view
	return user.MarkReadOnView
}

// Entries represents a list of entries.
type Entries []*Entry

// EntriesStatusUpdateRequest represents a request to change entries status.
type EntriesStatusUpdateRequest struct {
	EntryIDs []int64 `json:"entry_ids"`
	Status   string  `json:"status"`
	Starred  *bool   `json:"starred"`
	// ExpectedVersions maps entry IDs to the version the client last read.
	// When an entry ID is present, the write is rejected with a conflict if
	// the stored version no longer matches. Entries missing from the map
	// are written unconditionally, which keeps pre-optimistic-locking
	// clients working.
	ExpectedVersions map[int64]int64 `json:"expected_versions,omitempty"`
}

// EntryUpdateRequest represents a request to update an entry.
type EntryUpdateRequest struct {
	Title   *string `json:"title"`
	Content *string `json:"content"`
	// ExpectedVersion is the version the client last read. A value greater
	// than zero turns the write into a conditional one: it fails with a
	// conflict when the entry has been modified since then. Zero means the
	// client does not participate in optimistic locking and the write is
	// applied unconditionally.
	ExpectedVersion int64 `json:"expected_version,omitempty"`
}

func (e *EntryUpdateRequest) Patch(entry *Entry) {
	if e.Title != nil && *e.Title != "" {
		entry.Title = *e.Title
	}

	if e.Content != nil && *e.Content != "" {
		entry.Content = *e.Content
	}
}

// EntryETagPrefix is the fixed prefix of entry ETags. It namespaces the
// opaque version token so an ETag can never be confused with a raw number
// and so future changes to the ETag scheme stay distinguishable.
const EntryETagPrefix = "entry-version"

// ETag returns the HTTP ETag representing the entry's current version.
// A weak validator is used because the version changes on every accepted
// write while the representation is byte-for-byte compatible with the
// previous one when nothing visible changed.
func (e *Entry) ETag() string {
	return FormatEntryETag(e.Version)
}

// FormatEntryETag builds the weak ETag corresponding to an entry version.
func FormatEntryETag(version int64) string {
	return fmt.Sprintf(`W/"%s-%d"`, EntryETagPrefix, version)
}

// ParseEntryETag extracts the version encoded in an entry ETag. It accepts
// both weak (W/"...") and strong ("...") validators and tolerates optional
// surrounding whitespace. ok is false when the value does not look like an
// entry ETag produced by this server.
func ParseEntryETag(etag string) (version int64, ok bool) {
	etag = strings.TrimSpace(etag)
	etag = strings.TrimPrefix(etag, "W/")
	etag = strings.TrimSpace(etag)
	if len(etag) < 2 || etag[0] != '"' || etag[len(etag)-1] != '"' {
		return 0, false
	}

	opaque := etag[1 : len(etag)-1]
	prefix := EntryETagPrefix + "-"
	if !strings.HasPrefix(opaque, prefix) {
		return 0, false
	}

	version, err := strconv.ParseInt(opaque[len(prefix):], 10, 64)
	if err != nil || version < 0 {
		return 0, false
	}
	return version, true
}

// ParseIfMatchHeader parses an If-Match header value into the expected entry
// version. The wildcard ("*") means "the resource must exist" and is reported
// as present with a zero version, which the storage layer treats as an
// unconditional write. A malformed, non-wildcard header returns ok=false so
// the caller can answer with 400.
func ParseIfMatchHeader(header string) (version int64, present bool, ok bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false, true
	}
	if header == "*" {
		return 0, true, true
	}

	// If-Match may carry a comma-separated list; an entry only ever has a
	// single current version, so the first parseable entry ETag wins.
	for _, candidate := range strings.Split(header, ",") {
		if version, parsed := ParseEntryETag(candidate); parsed {
			return version, true, true
		}
	}
	return 0, false, false
}
