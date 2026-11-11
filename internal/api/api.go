// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api // import "miniflux.app/v2/internal/api"

import (
	"net/http"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
	"miniflux.app/v2/internal/worker"
)

type handler struct {
	store *storage.Storage
	pool  *worker.Pool
}

// NewHandler returns an http.Handler that handles API v1 calls.
// The returned handler expects the base path to be stripped from the request URL.
func NewHandler(store *storage.Storage, pool *worker.Pool) http.Handler {
	handler := &handler{store: store, pool: pool}
	middleware := newMiddleware(store)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/users", requireScope(model.ScopeUsersWrite, handler.createUserHandler))
	mux.HandleFunc("GET /v1/users", requireScope(model.ScopeUsersRead, handler.usersHandler))
	mux.HandleFunc("GET /v1/users/{identifier}", requireScope(model.ScopeUsersRead, handler.dispatchUserLookupHandler))
	mux.HandleFunc("PUT /v1/users/{userID}", requireScope(model.ScopeUsersWrite, handler.updateUserHandler))
	mux.HandleFunc("DELETE /v1/users/{userID}", requireScope(model.ScopeUsersWrite, handler.removeUserHandler))
	mux.HandleFunc("PUT /v1/users/{userID}/mark-all-as-read", requireScope(model.ScopeEntriesWrite, handler.markUserAsReadHandler))
	mux.HandleFunc("GET /v1/me", handler.currentUserHandler)
	mux.HandleFunc("POST /v1/categories", requireScope(model.ScopeCategoriesWrite, handler.createCategoryHandler))
	mux.HandleFunc("GET /v1/categories", requireScope(model.ScopeCategoriesRead, handler.getCategoriesHandler))
	mux.HandleFunc("PUT /v1/categories/{categoryID}", requireScope(model.ScopeCategoriesWrite, handler.updateCategoryHandler))
	mux.HandleFunc("DELETE /v1/categories/{categoryID}", requireScope(model.ScopeCategoriesWrite, handler.removeCategoryHandler))
	mux.HandleFunc("PUT /v1/categories/{categoryID}/mark-all-as-read", requireScope(model.ScopeEntriesWrite, handler.markCategoryAsReadHandler))
	mux.HandleFunc("GET /v1/categories/{categoryID}/feeds", requireScope(model.ScopeFeedsRead, handler.getCategoryFeedsHandler))
	mux.HandleFunc("PUT /v1/categories/{categoryID}/refresh", requireScope(model.ScopeFeedsWrite, handler.refreshCategoryHandler))
	mux.HandleFunc("GET /v1/categories/{categoryID}/entries", requireScope(model.ScopeEntriesRead, handler.getCategoryEntriesHandler))
	mux.HandleFunc("GET /v1/categories/{categoryID}/entries/{entryID}", requireScope(model.ScopeEntriesRead, handler.getCategoryEntryHandler))
	mux.HandleFunc("POST /v1/discover", requireScope(model.ScopeFeedsRead, handler.discoverSubscriptionsHandler))
	mux.HandleFunc("POST /v1/feeds", requireScope(model.ScopeFeedsWrite, handler.createFeedHandler))
	mux.HandleFunc("GET /v1/feeds", requireScope(model.ScopeFeedsRead, handler.getFeedsHandler))
	mux.HandleFunc("GET /v1/feeds/counters", requireScope(model.ScopeFeedsRead, handler.fetchCountersHandler))
	mux.HandleFunc("PUT /v1/feeds/refresh", requireScope(model.ScopeFeedsWrite, handler.refreshAllFeedsHandler))
	mux.HandleFunc("PUT /v1/feeds/{feedID}/refresh", requireScope(model.ScopeFeedsWrite, handler.refreshFeedHandler))
	mux.HandleFunc("GET /v1/feeds/{feedID}", requireScope(model.ScopeFeedsRead, handler.getFeedHandler))
	mux.HandleFunc("PUT /v1/feeds/{feedID}", requireScope(model.ScopeFeedsWrite, handler.updateFeedHandler))
	mux.HandleFunc("DELETE /v1/feeds/{feedID}", requireScope(model.ScopeFeedsWrite, handler.removeFeedHandler))
	mux.HandleFunc("GET /v1/feeds/{feedID}/icon", requireScope(model.ScopeFeedsRead, handler.getIconByFeedIDHandler))
	mux.HandleFunc("PUT /v1/feeds/{feedID}/mark-all-as-read", requireScope(model.ScopeEntriesWrite, handler.markFeedAsReadHandler))
	mux.HandleFunc("GET /v1/export", requireScope(model.ScopeFeedsRead, handler.exportFeedsHandler))
	mux.HandleFunc("POST /v1/import", requireScope(model.ScopeFeedsWrite, handler.importFeedsHandler))
	mux.HandleFunc("GET /v1/feeds/{feedID}/entries", requireScope(model.ScopeEntriesRead, handler.getFeedEntriesHandler))
	mux.HandleFunc("POST /v1/feeds/{feedID}/entries/import", requireScope(model.ScopeEntriesWrite, handler.importFeedEntryHandler))
	mux.HandleFunc("GET /v1/feeds/{feedID}/entries/{entryID}", requireScope(model.ScopeEntriesRead, handler.getFeedEntryHandler))
	mux.HandleFunc("GET /v1/entries/ids", requireScope(model.ScopeEntriesRead, handler.getEntryIDsHandler))
	mux.HandleFunc("GET /v1/entries", requireScope(model.ScopeEntriesRead, handler.getEntriesHandler))
	mux.HandleFunc("PUT /v1/entries", requireScope(model.ScopeEntriesWrite, handler.setEntryStatusAndStarredHandler))
	mux.HandleFunc("GET /v1/entries/{entryID}", requireScope(model.ScopeEntriesRead, handler.getEntryHandler))
	mux.HandleFunc("PUT /v1/entries/{entryID}", requireScope(model.ScopeEntriesWrite, handler.updateEntryHandler))
	mux.HandleFunc("PUT /v1/entries/{entryID}/bookmark", requireScope(model.ScopeEntriesWrite, handler.toggleStarredHandler))
	mux.HandleFunc("PUT /v1/entries/{entryID}/star", requireScope(model.ScopeEntriesWrite, handler.toggleStarredHandler))
	mux.HandleFunc("POST /v1/entries/{entryID}/save", requireScope(model.ScopeEntriesWrite, handler.saveEntryHandler))
	mux.HandleFunc("PUT /v1/flush-history", requireScope(model.ScopeEntriesWrite, handler.flushHistoryHandler))
	mux.HandleFunc("DELETE /v1/flush-history", requireScope(model.ScopeEntriesWrite, handler.flushHistoryHandler))
	mux.HandleFunc("GET /v1/icons/{iconID}", requireScope(model.ScopeFeedsRead, handler.getIconByIconIDHandler))
	mux.HandleFunc("GET /v1/enclosures/{enclosureID}", requireScope(model.ScopeEntriesRead, handler.getEnclosureByIDHandler))
	mux.HandleFunc("PUT /v1/enclosures/{enclosureID}", requireScope(model.ScopeEntriesWrite, handler.updateEnclosureByIDHandler))
	mux.HandleFunc("GET /v1/integrations/status", requireScope(model.ScopeIntegrationsRead, handler.getIntegrationsStatusHandler))
	mux.HandleFunc("GET /v1/version", handler.versionHandler)
	mux.HandleFunc("POST /v1/api-keys", requireScope(model.ScopeAPIKeysWrite, handler.createAPIKeyHandler))
	mux.HandleFunc("GET /v1/api-keys", requireScope(model.ScopeAPIKeysRead, handler.getAPIKeysHandler))
	mux.HandleFunc("DELETE /v1/api-keys/{apiKeyID}", requireScope(model.ScopeAPIKeysWrite, handler.deleteAPIKeyHandler))

	return middleware.withCORSHeaders(middleware.validateAPIKeyAuth(middleware.validateBasicAuth(mux)))
}
