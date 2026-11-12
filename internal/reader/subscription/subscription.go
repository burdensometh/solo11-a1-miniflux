// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package subscription // import "miniflux.app/v2/internal/reader/subscription"

import (
	"fmt"

	"miniflux.app/v2/internal/reader/preview"
)

// subscription represents a feed subscription.
type subscription struct {
	Title   string           `json:"title"`
	URL     string           `json:"url"`
	Type    string           `json:"type"`
	Preview *preview.Preview `json:"preview,omitempty"`
}

func NewSubscription(title, url, kind string) *subscription {
	return &subscription{Title: title, URL: url, Type: kind}
}

func (s subscription) String() string {
	return fmt.Sprintf(`Title=%q, URL=%q, Type=%q`, s.Title, s.URL, s.Type)
}

// FeedURL implements preview.FeedReference.
func (s *subscription) FeedURL() string {
	return s.URL
}

// AttachPreview implements preview.FeedReference.
func (s *subscription) AttachPreview(p *preview.Preview) {
	s.Preview = p
}

// Subscriptions represents a list of subscription.
type Subscriptions []*subscription

// FeedReferences returns the candidates as preview feed references so they can
// be passed to preview.Fetcher.AttachPreviews.
func (s Subscriptions) FeedReferences() []preview.FeedReference {
	references := make([]preview.FeedReference, len(s))
	for i, candidate := range s {
		references[i] = candidate
	}
	return references
}
