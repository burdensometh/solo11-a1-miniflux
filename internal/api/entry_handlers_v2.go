// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api // import "miniflux.app/v2/internal/api"

import (
	"errors"
	"net/http"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/mediaproxy"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
	"miniflux.app/v2/internal/validator"
)

// findEntriesV2 implements the API v2 list contract:
//
//   - Pagination is cursor-based (keyset). "offset" and the positional
//     before/after parameters are rejected so the two pagination models can
//     never be mixed within one request.
//   - The sort order is restricted to columns that support a total, stable
//     keyset ordering.
//   - The expensive total count is opt-in via "with_total=true"; clients that
//     only need to walk pages rely on "has_more" instead.
//   - Every filter available on v1 (status, tags, search, feed/category,
//     dates, …) keeps working unchanged.
func (h *handler) findEntriesV2(w http.ResponseWriter, r *http.Request, feedID int64, categoryID int64) {
	// The two pagination models must not be mixed: positional id parameters
	// on a v2 request would otherwise silently fight with the cursor. Date
	// filters such as "before"/"after" are plain WHERE conditions and stay
	// available; only the id-based positional pagination is rejected.
	if request.HasQueryParam(r, "offset") {
		response.JSONBadRequest(w, r, errors.New(`API v2 uses cursor pagination: remove "offset" and use "cursor"`))
		return
	}
	for _, param := range []string{"before_entry_id", "after_entry_id"} {
		if request.HasQueryParam(r, param) {
			response.JSONBadRequest(w, r, errors.New(`API v2 uses cursor pagination: the "`+param+`" parameter is not supported, use "cursor"`))
			return
		}
	}

	statuses := request.QueryStringParamList(r, "status")
	for _, status := range statuses {
		if err := validator.ValidateEntryStatus(status); err != nil {
			response.JSONBadRequest(w, r, err)
			return
		}
	}

	order := request.QueryStringParam(r, "order", model.DefaultSortingOrder)
	if !cursorColumns[order] {
		response.JSONBadRequest(w, r, errors.New(`API v2 cursor pagination supports only the following "order" values: "id", "published_at", "changed_at", "created_at"`))
		return
	}

	direction := request.QueryStringParam(r, "direction", model.DefaultSortingDirection)
	if err := validator.ValidateDirection(direction); err != nil {
		response.JSONBadRequest(w, r, err)
		return
	}

	limit := request.QueryIntParam(r, "limit", 100)
	if limit <= 0 {
		// Normalize instead of letting the storage layer clamp zero to its
		// internal maximum: the response "limit" field must describe the
		// actual page size and has_more must remain computable.
		limit = 100
	}
	if err := validator.ValidateRange(0, limit); err != nil {
		response.JSONBadRequest(w, r, err)
		return
	}

	userID := request.UserID(r)
	categoryID = request.QueryInt64Param(r, "category_id", categoryID)
	if categoryID > 0 {
		exists, err := h.store.CategoryIDExists(userID, categoryID)
		if err != nil {
			response.JSONServerError(w, r, err)
			return
		}
		if !exists {
			response.JSONBadRequest(w, r, errors.New("invalid category ID"))
			return
		}
	}

	feedID = request.QueryInt64Param(r, "feed_id", feedID)
	if feedID > 0 {
		exists, err := h.store.FeedExists(userID, feedID)
		if err != nil {
			response.JSONServerError(w, r, err)
			return
		}
		if !exists {
			response.JSONBadRequest(w, r, errors.New("invalid feed ID"))
			return
		}
	}

	now := time.Now()
	cursor := storage.EntryCursor{
		SortColumn: order,
		Desc:       direction == "desc",
	}

	if token := r.URL.Query().Get("cursor"); token != "" {
		decoded, err := decodeCursor(token, now)
		if err != nil {
			response.JSONBadRequest(w, r, err)
			return
		}
		if decoded.SortColumn != order || decoded.Desc != cursor.Desc {
			response.JSONBadRequest(w, r, errors.New(`invalid "cursor" parameter: cursor order/direction does not match the request parameters`))
			return
		}
		cursor = decoded
	}

	tags := request.QueryStringParamList(r, "tags")

	builder := h.store.NewEntryQueryBuilder(userID).
		WithFeedID(feedID).
		WithCategoryID(categoryID).
		WithStatuses(statuses...).
		WithLimit(limit).
		WithTags(tags...).
		WithEnclosures().
		WithKeysetPagination(cursor)

	if request.HasQueryParam(r, "globally_visible") {
		globallyVisible := request.QueryBoolParam(r, "globally_visible", true)

		if globallyVisible {
			builder = builder.WithGloballyVisible()
		}
	}

	builder = configureFilters(builder, r)

	entries, err := builder.GetEntries()
	if err != nil {
		response.JSONServerError(w, r, err)
		return
	}

	for i := range entries {
		entries[i].Content = mediaproxy.RewriteDocumentWithAbsoluteProxyURL(entries[i].Content)
		entries[i].Enclosures.ProxifyEnclosureURL(config.Opts.MediaProxyMode(), config.Opts.MediaProxyResourceTypes())
	}

	// Fetch one extra row semantics are avoided to keep a single query shape:
	// a full page means another page may exist, and the caller learns it
	// authoritatively on the next request.
	hasMore := len(entries) == limit && limit > 0

	pageResponse := v2EntryResponse{
		Entries: newV2Entries(entries),
		Limit:   limit,
		HasMore: hasMore,
	}

	if request.QueryBoolParam(r, "with_total", false) {
		total, err := builder.CountEntriesIgnoringPagination()
		if err != nil {
			response.JSONServerError(w, r, err)
			return
		}
		pageResponse.Total = &total
	}

	if hasMore {
		if next, ok := builder.KeysetPosition(entries, cursor); ok {
			token, err := encodeCursor(next, now)
			if err != nil {
				response.JSONServerError(w, r, err)
				return
			}
			pageResponse.NextCursor = token
		}
	}

	response.JSON(w, r, &pageResponse)
}
