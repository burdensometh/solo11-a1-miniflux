// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package validator // import "miniflux.app/v2/internal/validator"

import (
	"miniflux.app/v2/internal/locale"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/urllib"
)

// ValidateSubscriptionDiscovery validates subscription discovery requests.
func ValidateSubscriptionDiscovery(request *model.SubscriptionDiscoveryRequest) *locale.LocalizedError {
	if !urllib.IsAbsoluteURL(request.URL) {
		return locale.NewLocalizedError("error.invalid_site_url")
	}

	if request.ProxyURL != "" && !urllib.IsValidProxyURL(request.ProxyURL) {
		return locale.NewLocalizedError("error.invalid_feed_proxy_url")
	}

	return nil
}

// MaxFeedPreviewURLs limits the number of candidates a single preview request
// can ask for.
const MaxFeedPreviewURLs = 20

// ValidateFeedPreview validates feed preview requests.
func ValidateFeedPreview(request *model.FeedPreviewRequest) *locale.LocalizedError {
	if len(request.FeedURLs) == 0 {
		return locale.NewLocalizedError("error.invalid_site_url")
	}

	if len(request.FeedURLs) > MaxFeedPreviewURLs {
		return locale.NewLocalizedError("error.invalid_site_url")
	}

	for _, feedURL := range request.FeedURLs {
		if !urllib.IsAbsoluteURL(feedURL) {
			return locale.NewLocalizedError("error.invalid_site_url")
		}
	}

	if request.ProxyURL != "" && !urllib.IsValidProxyURL(request.ProxyURL) {
		return locale.NewLocalizedError("error.invalid_feed_proxy_url")
	}

	return nil
}
