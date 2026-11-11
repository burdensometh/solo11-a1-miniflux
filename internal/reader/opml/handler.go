// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package opml // import "miniflux.app/v2/internal/reader/opml"

import (
	"database/sql"
	"fmt"
	"io"
	"strings"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
	"miniflux.app/v2/internal/validator"
)

// ImportResult describes the outcome of an OPML import.
type ImportResult struct {
	// Created is the number of feeds inserted by this import.
	Created int `json:"created"`
	// Skipped is the number of OPML entries that were not inserted because
	// their URL was already present for the user or appeared earlier in the
	// same file.
	Skipped int `json:"skipped"`
}

// Handler handles the logic for OPML import/export.
type Handler struct {
	store *storage.Storage
}

// Export exports user feeds to OPML.
func (h *Handler) Export(userID int64) (string, error) {
	feeds, err := h.store.Feeds(userID)
	if err != nil {
		return "", err
	}

	subscriptions := make([]subcription, 0, len(feeds))
	for _, feed := range feeds {
		subscriptions = append(subscriptions, subcription{
			Title:        feed.Title,
			FeedURL:      feed.FeedURL,
			SiteURL:      feed.SiteURL,
			Description:  feed.Description,
			CategoryName: feed.Category.Title,

			RewriteRules:                feed.RewriteRules,
			UrlRewriteRules:             feed.UrlRewriteRules,
			BlocklistRules:              feed.BlocklistRules,
			KeeplistRules:               feed.KeeplistRules,
			BlockFilterEntryRules:       feed.BlockFilterEntryRules,
			KeepFilterEntryRules:        feed.KeepFilterEntryRules,
			UserAgent:                   feed.UserAgent,
			IgnoreHTTPCache:             feed.IgnoreHTTPCache,
			FetchViaProxy:               feed.FetchViaProxy,
			Disabled:                    feed.Disabled,
			NoMediaPlayer:               feed.NoMediaPlayer,
			HideGlobally:                feed.HideGlobally,
			AllowSelfSignedCertificates: feed.AllowSelfSignedCertificates,
			DisableHTTP2:                feed.DisableHTTP2,
			IgnoreEntryUpdates:          feed.IgnoreEntryUpdates,
		})
	}

	return serialize(subscriptions), nil
}

// Import parses and creates feeds from an OPML import. It discards the
// per-entry counters; callers that need them should use ImportWithResult.
func (h *Handler) Import(userID int64, data io.Reader) error {
	_, err := h.ImportWithResult(userID, data)
	return err
}

// ImportWithResult parses and creates feeds from an OPML import atomically.
// Either every feed and category is persisted, or none is. Duplicate URLs
// (already present for the user, or repeated within the file) are skipped
// rather than updated, preserving the existing feed's state and settings.
func (h *Handler) ImportWithResult(userID int64, data io.Reader) (ImportResult, error) {
	subscriptions, err := parse(data)
	if err != nil {
		return ImportResult{}, err
	}

	subscriptions, duplicates := deduplicateSubscriptions(subscriptions)
	result := ImportResult{Skipped: duplicates}

	err = h.store.RunInTransaction(func(tx *sql.Tx) error {
		for _, subscription := range subscriptions {
			exists, err := h.store.FeedURLExistsTx(tx, userID, subscription.FeedURL)
			if err != nil {
				return err
			}
			if exists {
				result.Skipped++
				continue
			}

			category, err := h.resolveCategoryTx(tx, userID, subscription.CategoryName)
			if err != nil {
				return err
			}

			if validationErr := validateSubscription(category.ID, subscription); validationErr != nil {
				return fmt.Errorf(`opml: invalid feed settings for %q: %w`, subscription.FeedURL, validationErr)
			}

			feed := &model.Feed{
				UserID:      userID,
				Title:       subscription.Title,
				FeedURL:     subscription.FeedURL,
				SiteURL:     subscription.SiteURL,
				Description: subscription.Description,
				Category:    category,
			}
			applySubscriptionSettings(feed, subscription)
			if err := h.store.CreateFeedTx(tx, feed); err != nil {
				return fmt.Errorf(`opml: unable to create this feed: %q: %w`, subscription.FeedURL, err)
			}

			result.Created++
		}

		return nil
	})
	if err != nil {
		// The transaction was rolled back: do not report partial counters.
		return ImportResult{}, err
	}

	return result, nil
}

// deduplicateSubscriptions keeps the first occurrence of each feed URL and
// returns how many entries were dropped. The first occurrence wins so that
// import order (and therefore the OPML document) determines the category and
// settings for a feed listed more than once.
func deduplicateSubscriptions(subscriptions []subcription) ([]subcription, int) {
	seen := make(map[string]struct{}, len(subscriptions))
	unique := make([]subcription, 0, len(subscriptions))
	duplicates := 0

	for _, subscription := range subscriptions {
		feedURL := strings.TrimSpace(subscription.FeedURL)
		if feedURL == "" {
			duplicates++
			continue
		}

		if _, found := seen[feedURL]; found {
			duplicates++
			continue
		}

		seen[feedURL] = struct{}{}
		subscription.FeedURL = feedURL
		subscription.SiteURL = strings.TrimSpace(subscription.SiteURL)
		unique = append(unique, subscription)
	}

	return unique, duplicates
}

func (h *Handler) resolveCategoryTx(tx *sql.Tx, userID int64, categoryName string) (*model.Category, error) {
	categoryName = strings.TrimSpace(categoryName)
	if categoryName == "" {
		category, err := h.store.FirstCategoryTx(tx, userID)
		if err != nil {
			return nil, fmt.Errorf("opml: unable to find first category: %w", err)
		}
		if category == nil {
			return nil, storage.ErrNoCategory
		}
		return category, nil
	}

	category, err := h.store.CategoryByTitleTx(tx, userID, categoryName)
	if err != nil {
		return nil, fmt.Errorf("opml: unable to search category by title: %w", err)
	}

	if category == nil {
		category, err = h.store.CreateCategoryTx(tx, userID, &model.CategoryCreationRequest{Title: categoryName})
		if err != nil {
			return nil, fmt.Errorf(`opml: unable to create this category: %q`, categoryName)
		}
	}

	return category, nil
}

func applySubscriptionSettings(feed *model.Feed, s subcription) {
	feed.RewriteRules = s.RewriteRules
	feed.UrlRewriteRules = s.UrlRewriteRules
	feed.BlocklistRules = s.BlocklistRules
	feed.KeeplistRules = s.KeeplistRules
	feed.BlockFilterEntryRules = s.BlockFilterEntryRules
	feed.KeepFilterEntryRules = s.KeepFilterEntryRules
	feed.UserAgent = s.UserAgent
	feed.IgnoreHTTPCache = s.IgnoreHTTPCache
	feed.FetchViaProxy = s.FetchViaProxy
	feed.Disabled = s.Disabled
	feed.NoMediaPlayer = s.NoMediaPlayer
	feed.HideGlobally = s.HideGlobally
	feed.AllowSelfSignedCertificates = s.AllowSelfSignedCertificates
	feed.DisableHTTP2 = s.DisableHTTP2
	feed.IgnoreEntryUpdates = s.IgnoreEntryUpdates
}

func validateSubscription(categoryID int64, s subcription) error {
	feedCreationRequest := &model.FeedCreationRequest{
		FeedURL:                     s.FeedURL,
		CategoryID:                  categoryID,
		UserAgent:                   s.UserAgent,
		IgnoreEntryUpdates:          s.IgnoreEntryUpdates,
		Disabled:                    s.Disabled,
		NoMediaPlayer:               s.NoMediaPlayer,
		IgnoreHTTPCache:             s.IgnoreHTTPCache,
		AllowSelfSignedCertificates: s.AllowSelfSignedCertificates,
		FetchViaProxy:               s.FetchViaProxy,
		HideGlobally:                s.HideGlobally,
		DisableHTTP2:                s.DisableHTTP2,
		RewriteRules:                s.RewriteRules,
		BlocklistRules:              s.BlocklistRules,
		KeeplistRules:               s.KeeplistRules,
		BlockFilterEntryRules:       s.BlockFilterEntryRules,
		KeepFilterEntryRules:        s.KeepFilterEntryRules,
		UrlRewriteRules:             s.UrlRewriteRules,
	}

	if validationErr := validator.ValidateFeedCreationRequest(feedCreationRequest); validationErr != nil {
		return validationErr.Error()
	}

	return nil
}

// NewHandler creates a new handler for OPML files.
func NewHandler(store *storage.Storage) *Handler {
	return &Handler{store: store}
}
