// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api // import "miniflux.app/v2/internal/api"

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/model"
)

func serveRequireScope(scope string, scopes model.APIKeyScopes, setScopes bool) *httptest.ResponseRecorder {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodPut, "/v1/entries", nil)
	if setScopes {
		ctx := context.WithValue(r.Context(), request.APIKeyScopesContextKey, scopes)
		r = r.WithContext(ctx)
	}

	w := httptest.NewRecorder()
	requireScope(scope, next).ServeHTTP(w, r)
	return w
}

func TestRequireScopeAllowsRequestWithoutAPIKeyScopes(t *testing.T) {
	// Requests authenticated with Basic auth have no scopes in the request
	// context and must not be restricted.
	w := serveRequireScope(model.ScopeEntriesWrite, nil, false)

	if got := w.Code; got != http.StatusOK {
		t.Fatalf(`Unexpected status code, got %d instead of %d`, got, http.StatusOK)
	}
}

func TestRequireScopeAllowsLegacyUnrestrictedKey(t *testing.T) {
	// Keys created before the introduction of scopes have an empty scope
	// list and keep their full access.
	w := serveRequireScope(model.ScopeEntriesWrite, model.APIKeyScopes{}, true)

	if got := w.Code; got != http.StatusOK {
		t.Fatalf(`Unexpected status code, got %d instead of %d`, got, http.StatusOK)
	}
}

func TestRequireScopeAllowsKeyWithMatchingScope(t *testing.T) {
	w := serveRequireScope(model.ScopeEntriesWrite, model.APIKeyScopes{model.ScopeEntriesRead, model.ScopeEntriesWrite}, true)

	if got := w.Code; got != http.StatusOK {
		t.Fatalf(`Unexpected status code, got %d instead of %d`, got, http.StatusOK)
	}
}

func TestRequireScopeDeniesKeyMissingScope(t *testing.T) {
	// Audit scenario: a read-only key must not be able to change settings.
	w := serveRequireScope(model.ScopeUsersWrite, model.APIKeyScopes{model.ScopeEntriesRead, model.ScopeFeedsRead}, true)

	if got := w.Code; got != http.StatusForbidden {
		t.Fatalf(`Unexpected status code, got %d instead of %d`, got, http.StatusForbidden)
	}

	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf(`Unexpected Content-Type header, got %q`, got)
	}

	var responseBody map[string]any
	if err := json.NewDecoder(w.Body).Decode(&responseBody); err != nil {
		t.Fatalf("Unexpected JSON decoding error: %v", err)
	}

	errorMessage, ok := responseBody["error_message"].(string)
	if !ok || !strings.Contains(errorMessage, model.ScopeUsersWrite) {
		t.Fatalf(`Expected the error message to mention the missing scope %q, got %q`, model.ScopeUsersWrite, errorMessage)
	}
}

func TestRequireScopeDeniesWriteScopeOnReadOnlyKey(t *testing.T) {
	w := serveRequireScope(model.ScopeEntriesWrite, model.APIKeyScopes{model.ScopeEntriesRead}, true)

	if got := w.Code; got != http.StatusForbidden {
		t.Fatalf(`Unexpected status code, got %d instead of %d`, got, http.StatusForbidden)
	}
}
