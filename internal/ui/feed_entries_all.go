// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"log/slog"
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/ui/view"
)

func (h *handler) showFeedEntriesAllPage(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	feedID := request.RouteInt64Param(r, "feedID")
	feed, err := h.store.FeedByID(user.ID, feedID)
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	if feed == nil {
		response.HTMLNotFound(w, r)
		return
	}

	if err := h.store.PreloadFeedIcon(feed); err != nil {
		slog.Debug("Unable to preload feed icon", slog.Any("error", err))
	}

	offset := request.QueryIntParam(r, "offset", 0)

	entries, count, err := h.store.NewEntryQueryBuilder(user.ID).
		WithFeedID(feed.ID).
		WithSorting(user.EntryOrder, user.EntryDirection).
		WithSorting("id", user.EntryDirection).
		WithoutContent().
		WithOffset(offset).
		WithLimit(user.EntriesPerPage).
		WithFeedIconsPreload().
		GetEntriesWithCount()
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	view := view.New(h.tpl, r)
	view.Set("feed", feed)
	view.Set("entries", entries)
	view.Set("total", count)
	view.Set("pagination", getPagination(h.routePath("/feed/%d/entries/all", feed.ID), count, offset, user.EntriesPerPage))
	view.Set("menu", "feeds")
	view.Set("user", user)
	navMetadata, _ := h.store.GetNavMetadata(user.ID)
	view.Set("countUnread", navMetadata.CountUnread)
	view.Set("countErrorFeeds", navMetadata.CountErrorFeeds)
	view.Set("hasSaveEntry", navMetadata.HasSaveEntry)
	view.Set("showOnlyUnreadEntries", false)

	response.HTML(w, r, view.Render("feed_entries"))
}
