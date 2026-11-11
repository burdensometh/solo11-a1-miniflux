// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package opml // import "miniflux.app/v2/internal/reader/opml"

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"miniflux.app/v2/internal/reader/encoding"
)

// parse reads an OPML file and returns a list of subscription.
func parse(data io.Reader) ([]subcription, error) {
	opmlDocument := &opmlDocument{}
	decoder := xml.NewDecoder(data)
	decoder.Entity = xml.HTMLEntity
	decoder.Strict = false
	decoder.CharsetReader = encoding.CharsetReader

	err := decoder.Decode(opmlDocument)
	if err != nil {
		return nil, fmt.Errorf("opml: unable to parse document: %w", err)
	}

	return getSubscriptionsFromOutlines(opmlDocument.Outlines, ""), nil
}

func getSubscriptionsFromOutlines(outlines opmlOutlineCollection, category string) []subcription {
	subscriptions := make([]subcription, 0, len(outlines))

	for _, outline := range outlines {
		if outline.IsSubscription() {
			categoryName := category
			if outlineCategory := flattenCategoryName(outline.Category); outlineCategory != "" {
				categoryName = outlineCategory
			}
			subscriptions = append(subscriptions, subcription{
				Title:        outline.GetTitle(),
				FeedURL:      outline.FeedURL,
				SiteURL:      outline.GetSiteURL(),
				Description:  outline.Description,
				CategoryName: categoryName,

				RewriteRules:                outline.RewriteRules,
				UrlRewriteRules:             outline.UrlRewriteRules,
				BlocklistRules:              outline.BlocklistRules,
				KeeplistRules:               outline.KeeplistRules,
				BlockFilterEntryRules:       outline.BlockFilterEntryRules,
				KeepFilterEntryRules:        outline.KeepFilterEntryRules,
				UserAgent:                   outline.UserAgent,
				IgnoreHTTPCache:             outline.IgnoreHTTPCache,
				FetchViaProxy:               outline.FetchViaProxy,
				Disabled:                    outline.Disabled,
				NoMediaPlayer:               outline.NoMediaPlayer,
				HideGlobally:                outline.HideGlobally,
				AllowSelfSignedCertificates: outline.AllowSelfSignedCertificates,
				DisableHTTP2:                outline.DisableHTTP2,
				IgnoreEntryUpdates:          outline.IgnoreEntryUpdates,
			})
		} else if outline.Outlines.HasChildren() {
			// Miniflux categories are a single flat level. Flatten nested OPML
			// folders to the nearest named folder (the feed's most specific
			// grouping). Anonymous container outlines are transparent: recurse
			// with the last named ancestor instead of losing the category.
			nextCategory := category
			if outlineCategory := flattenCategoryName(outline.Category); outlineCategory != "" {
				nextCategory = outlineCategory
			} else if title := strings.TrimSpace(outline.GetTitle()); title != "" {
				nextCategory = title
			}
			subscriptions = append(subscriptions, getSubscriptionsFromOutlines(outline.Outlines, nextCategory)...)
		}
	}
	return subscriptions
}

// flattenCategoryName converts an OPML category attribute to a single
// Miniflux category name. The OPML spec allows a comma-separated list of
// slash-delimited paths; Miniflux stores one flat category per feed, so the
// first non-empty entry is used and its most specific (last) path segment is
// retained. This mirrors the behavior used for nested outlines.
func flattenCategoryName(rawCategory string) string {
	for _, entry := range strings.Split(rawCategory, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		parts := strings.Split(entry, "/")
		for index := len(parts) - 1; index >= 0; index-- {
			if part := strings.TrimSpace(parts[index]); part != "" {
				return part
			}
		}
	}

	return ""
}
