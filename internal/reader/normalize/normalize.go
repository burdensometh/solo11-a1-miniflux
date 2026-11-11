// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package normalize contains the unified feed normalization pipeline shared
// by every feed format parser (RSS, Atom 0.3/1.0, JSON Feed and RDF).
//
// Each format-specific package only decodes its own syntax into the neutral
// intermediate representation defined here; all normalization rules
// (absolute URL resolution, title/date fallbacks, hash generation, enclosure
// deduplication, and so on) live in this package and are applied exactly once
// per feed/entry.
package normalize

import "time"

// Enclosure is the format-neutral representation of an entry attachment.
type Enclosure struct {
	URL      string
	MimeType string
	Size     int64
}

// Entry is the format-neutral representation of a feed entry.
//
// Fields that are optional in a given format stay at their zero value and the
// common pipeline is in charge of applying the appropriate fallbacks.
type Entry struct {
	// URL is the entry URL as found in the feed, before absolute URL
	// resolution. It may be empty.
	URL string

	// FallbackURLs are additional URL candidates used when URL is empty.
	// They are tried in order and resolved against the site URL.
	FallbackURLs []string

	// SiteURLFallback falls back to the feed site URL when no entry URL can
	// be found. RDF and RSS enable this, Atom uses the entry ID instead.
	SiteURLFallback bool

	// EnclosureFallback makes the first resolved enclosure URL the entry URL
	// when the entry has no URL of its own (RSS behavior).
	EnclosureFallback bool

	// AbsoluteIDFallback uses the entry ID as URL when it is already an
	// absolute URL (Atom 1.0 behavior).
	AbsoluteIDFallback bool

	// ID is the entry identifier as declared by the feed. It is used for hash
	// generation and, on Atom, as an absolute-URL fallback.
	ID string

	// TrackDuplicateID enables the RSS duplicate-GUID disambiguation: the
	// first entry with a given ID hashes the ID alone, later collisions hash
	// the ID together with the entry URL or the entry position.
	TrackDuplicateID bool

	Title   string
	Content string

	// TitleFallbackCandidates are tried in order when Title is empty: each
	// candidate is truncated as HTML and the first non-empty result wins.
	// JSON Feed uses this for its summary/content_text/content_html fallbacks.
	// When the list is empty, Content itself is truncated instead.
	TitleFallbackCandidates []string

	// TitleContentFallback derives the title from a truncated version of the
	// content when no title is present. RDF does not apply this fallback and
	// goes straight to the entry URL; every other format enables it.
	TitleContentFallback bool

	// DateCandidates are raw date strings tried in order. The first one that
	// parses successfully wins; an empty list means "use the current time".
	DateCandidates []string

	// DateFallbackNow returns time.Now() when every date candidate is missing
	// or unparseable.
	DateFallbackNow bool

	// Authors are the entry author names in feed order.
	Authors []string

	// SortAndDedupAuthors sorts and deduplicates the author names (JSON Feed
	// and Atom 1.0). When false, names are kept verbatim and joined in order
	// (RSS, RDF and Atom 0.3).
	SortAndDedupAuthors bool

	// Language is the entry-level language; an empty value inherits the feed
	// language.
	Language string

	// Tags are the raw tag values. TagTrimmer is applied to every value
	// before sorting and deduplication; nil means "use values verbatim".
	Tags       []string
	TagTrimmer func(string) string

	CommentsURL string

	Enclosures []Enclosure

	// EnclosureURLRewriter rewrites a raw enclosure URL before absolute URL
	// resolution and deduplication. RSS uses it for the FeedBurner
	// origEnclosureLink substitution; nil means the URL is used verbatim.
	EnclosureURLRewriter func(rawURL string) string

	// ReadingTime is an explicit reading/listening time in minutes computed
	// by the format parser (for example the iTunes duration of a podcast).
	ReadingTime int

	// HashCandidates are the raw values hashed in order until a non-empty one
	// is found. Values are used verbatim (no trimming); every adapter is
	// responsible for preprocessing them according to its format.
	HashCandidates []string

	// HashTitleContentFallback hashes the normalized title together with the
	// content when every other hash candidate is empty (RSS behavior).
	HashTitleContentFallback bool
}

// Feed is the format-neutral representation of a whole feed document.
type Feed struct {
	Title       string
	Description string
	Language    string

	// FeedURL / SiteURL are the values declared in the feed before absolute
	// URL resolution.
	FeedURL string
	SiteURL string

	// BaseURL is the URL the feed was fetched from; every relative URL is
	// resolved against it.
	BaseURL string

	// SiteURLFallbackToBase makes an empty site URL fall back to the base URL
	// (Atom behavior). RSS/RDF/JSON use the feed URL instead.
	SiteURLFallbackToBase bool

	// FeedURLFallbackToBase makes an empty feed URL fall back to the base URL.
	FeedURLFallbackToBase bool

	// TitleFallbackToBase makes an empty feed title fall back to the base URL
	// (RDF behavior). The other formats fall back to the resolved site URL.
	TitleFallbackToBase bool

	// IconCandidates are tried in order until one resolves to an absolute URL.
	IconCandidates []string

	// FeedURLOverrides are tried in order against the base URL; the first one
	// that resolves to an absolute URL replaces the declared feed URL (RSS
	// Atom-style rel="self" channel link).
	FeedURLOverrides []string

	// TTLMinutes is the feed TTL in minutes. A negative value means "unset".
	TTLMinutes int

	Entries []*Entry
}

// NewFeed returns an initialized neutral feed with the TTL marked as unset.
func NewFeed(baseURL string) *Feed {
	return &Feed{
		BaseURL:    baseURL,
		TTLMinutes: -1,
	}
}

// NewEntry appends and returns a new neutral entry.
func (f *Feed) NewEntry() *Entry {
	entry := &Entry{}
	f.Entries = append(f.Entries, entry)
	return entry
}

// Now is overridable in tests to make the "missing date" fallback deterministic.
var Now = time.Now
