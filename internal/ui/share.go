// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"

	"miniflux.app/v2/internal/ui/view"
)

// defaultShareExpirationDays is used when the share form does not specify a
// lifetime. Links created before this feature existed remain permanent; new
// links get a finite lifetime by default.
const defaultShareExpirationDays = 30

// allowedShareExpirations maps accepted form values to a lifetime in days.
// Zero means the link never expires.
var allowedShareExpirations = map[int]bool{0: true, 1: true, 7: true, 30: true, 90: true}

func parseShareExpiration(value string) (time.Duration, error) {
	if value == "" {
		value = strconv.Itoa(defaultShareExpirationDays)
	}

	days, err := strconv.Atoi(value)
	if err != nil || !allowedShareExpirations[days] {
		return 0, fmt.Errorf("invalid share link expiration: %q", value)
	}

	return time.Duration(days) * 24 * time.Hour, nil
}

func (h *handler) createSharedEntry(w http.ResponseWriter, r *http.Request) {
	entryID := request.RouteInt64Param(r, "entryID")

	expiration, err := parseShareExpiration(r.FormValue("expiration"))
	if err != nil {
		response.HTMLBadRequest(w, r, err)
		return
	}

	shareCode, err := h.store.EntryShareCode(request.UserID(r), entryID, expiration)
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	response.HTMLRedirect(w, r, h.routePath("/share/%s", shareCode))
}

func (h *handler) unshareEntry(w http.ResponseWriter, r *http.Request) {
	entryID := request.RouteInt64Param(r, "entryID")
	if err := h.store.UnshareEntry(request.UserID(r), entryID); err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	response.HTMLRedirect(w, r, h.routePath("/shares"))
}

func (h *handler) sharedEntry(w http.ResponseWriter, r *http.Request) {
	shareCode := request.RouteStringParam(r, "shareCode")
	if shareCode == "" {
		response.HTMLNotFound(w, r)
		return
	}

	entry, err := h.store.NewAnonymousQueryBuilder().
		WithShareCode(shareCode).
		GetEntry()
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}
	if entry == nil {
		response.HTMLNotFound(w, r)
		return
	}

	// The page still depends on the visitor's theme and language, so it must
	// never be stored by shared caches.
	view := view.NewPublic(h.tpl, r)
	view.Set("entry", entry)
	response.HTML(w, r, view.Render("entry"))
}
