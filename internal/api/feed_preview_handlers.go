// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api // import "miniflux.app/v2/internal/api"

import (
	json_parser "encoding/json"
	"net/http"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/proxyrotator"
	"miniflux.app/v2/internal/reader/fetcher"
	"miniflux.app/v2/internal/reader/preview"
	"miniflux.app/v2/internal/validator"
)

// feedPreviewResult maps each requested feed URL to its preview. A feed that
// cannot be fetched or parsed is still present in the map, with its Preview
// Error field set, so that one broken candidate never blocks the others.
type feedPreviewResult struct {
	FeedURL string           `json:"feed_url"`
	Preview *preview.Preview `json:"preview"`
}

func (h *handler) feedPreviewHandler(w http.ResponseWriter, r *http.Request) {
	var previewRequest model.FeedPreviewRequest
	if err := json_parser.NewDecoder(r.Body).Decode(&previewRequest); err != nil {
		response.JSONBadRequest(w, r, err)
		return
	}

	if validationErr := validator.ValidateFeedPreview(&previewRequest); validationErr != nil {
		response.JSONBadRequest(w, r, validationErr.Error())
		return
	}

	requestBuilder := fetcher.NewRequestBuilder().
		WithTimeout(config.Opts.HTTPClientTimeout()).
		WithProxyRotator(proxyrotator.ProxyRotatorInstance).
		WithCustomFeedProxyURL(previewRequest.ProxyURL).
		WithCustomApplicationProxyURL(config.Opts.HTTPClientProxyURL()).
		UseCustomApplicationProxyURL(previewRequest.FetchViaProxy).
		WithUserAgent(previewRequest.UserAgent, config.Opts.HTTPClientUserAgent()).
		WithUsernameAndPassword(previewRequest.Username, previewRequest.Password).
		IgnoreTLSErrors(previewRequest.AllowSelfSignedCertificates).
		DisableHTTP2(previewRequest.DisableHTTP2)

	previewFetcher := preview.NewFetcher(requestBuilder, previewRequest.Cookie)
	previews := previewFetcher.PreviewsForURLs(previewRequest.FeedURLs)

	// One broken candidate never blocks the others: failures are embedded in
	// the Preview objects and every requested URL gets a result.
	results := make([]feedPreviewResult, 0, len(previewRequest.FeedURLs))
	for i, feedURL := range previewRequest.FeedURLs {
		results = append(results, feedPreviewResult{
			FeedURL: feedURL,
			Preview: previews[i],
		})
	}

	response.JSON(w, r, results)
}
