// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client // import "miniflux.app/v2/client"

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// EntriesV2 fetches one page of entries using the API v2 cursor contract.
//
// The client must have been created with WithAPIVersion(APIVersion2); the
// method returns ErrV2Required otherwise. Pagination is walked by passing the
// NextCursor value of the previous page back as filter.Cursor.
func (c *Client) EntriesV2(filter *EntryPageFilterV2) (*EntryPageV2, error) {
	ctx, cancel := withDefaultTimeout()
	defer cancel()
	return c.EntriesV2Context(ctx, filter)
}

// EntriesV2Context is the context-aware variant of EntriesV2.
func (c *Client) EntriesV2Context(ctx context.Context, filter *EntryPageFilterV2) (*EntryPageV2, error) {
	if c.request.apiVersion != APIVersion2 {
		return nil, ErrV2Required
	}

	path := buildV2FilterQueryString("/v1/entries", filter)

	body, err := c.request.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	var page EntryPageV2
	if err := json.NewDecoder(body).Decode(&page); err != nil {
		return nil, fmt.Errorf("miniflux: response error (%v)", err)
	}

	return &page, nil
}

func buildV2FilterQueryString(path string, filter *EntryPageFilterV2) string {
	if filter == nil {
		return path
	}

	values := url.Values{}

	if filter.Status != "" {
		values.Set("status", filter.Status)
	}
	if filter.Direction != "" {
		values.Set("direction", filter.Direction)
	}
	if filter.Order != "" {
		values.Set("order", filter.Order)
	}
	if filter.Limit > 0 {
		values.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Cursor != "" {
		values.Set("cursor", filter.Cursor)
	}
	if filter.Starred != "" {
		values.Set("starred", filter.Starred)
	}
	if filter.Search != "" {
		values.Set("search", filter.Search)
	}
	if filter.CategoryID > 0 {
		values.Set("category_id", strconv.FormatInt(filter.CategoryID, 10))
	}
	if filter.FeedID > 0 {
		values.Set("feed_id", strconv.FormatInt(filter.FeedID, 10))
	}
	if filter.WithTotal {
		values.Set("with_total", "true")
	}
	for _, status := range filter.Statuses {
		values.Add("status", status)
	}
	for _, tag := range filter.Tags {
		values.Add("tags", tag)
	}

	if len(values) == 0 {
		return path
	}
	return path + "?" + values.Encode()
}
