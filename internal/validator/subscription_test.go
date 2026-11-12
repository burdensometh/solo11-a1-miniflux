// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package validator // import "miniflux.app/v2/internal/validator"

import (
	"testing"

	"miniflux.app/v2/internal/model"
)

func TestValidateSubscriptionDiscovery(t *testing.T) {
	tests := []struct {
		name    string
		req     *model.SubscriptionDiscoveryRequest
		wantErr bool
	}{
		{
			name:    "valid site url",
			req:     &model.SubscriptionDiscoveryRequest{URL: "https://example.org"},
			wantErr: false,
		},
		{
			name:    "invalid site url",
			req:     &model.SubscriptionDiscoveryRequest{URL: "example.org"},
			wantErr: true,
		},
		{
			name:    "invalid proxy url",
			req:     &model.SubscriptionDiscoveryRequest{URL: "https://example.org", ProxyURL: "example.org"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSubscriptionDiscovery(tc.req); (err != nil) != tc.wantErr {
				t.Fatalf("expected error %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidateFeedPreview(t *testing.T) {
	tests := []struct {
		name    string
		req     *model.FeedPreviewRequest
		wantErr bool
	}{
		{
			name:    "valid feed url",
			req:     &model.FeedPreviewRequest{FeedURLs: []string{"https://example.org/feed.xml"}},
			wantErr: false,
		},
		{
			name:    "empty url list",
			req:     &model.FeedPreviewRequest{FeedURLs: nil},
			wantErr: true,
		},
		{
			name:    "malformed url",
			req:     &model.FeedPreviewRequest{FeedURLs: []string{"example.org/feed.xml"}},
			wantErr: true,
		},
		{
			name:    "invalid proxy url",
			req:     &model.FeedPreviewRequest{FeedURLs: []string{"https://example.org/feed.xml"}, ProxyURL: "example.org"},
			wantErr: true,
		},
		{
			name:    "too many urls",
			req:     &model.FeedPreviewRequest{FeedURLs: make([]string, MaxFeedPreviewURLs+1)},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "too many urls" {
				for i := range tc.req.FeedURLs {
					tc.req.FeedURLs[i] = "https://example.org/feed.xml"
				}
			}
			if err := ValidateFeedPreview(tc.req); (err != nil) != tc.wantErr {
				t.Fatalf("expected error %v, got %v", tc.wantErr, err)
			}
		})
	}
}
