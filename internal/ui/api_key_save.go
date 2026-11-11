// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/locale"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/ui/form"
	"miniflux.app/v2/internal/ui/view"
	"miniflux.app/v2/internal/validator"
)

func (h *handler) saveAPIKey(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	apiKeyForm := form.NewAPIKeyForm(r)
	apiKeyCreationRequest := &model.APIKeyCreationRequest{
		Description: apiKeyForm.Description,
	}

	// An empty scope list means full access: it must be an explicit choice,
	// never the result of selecting no scope in restricted mode.
	var accessTypeErr *locale.LocalizedError
	if apiKeyForm.IsScoped() {
		if len(apiKeyForm.Scopes) == 0 {
			accessTypeErr = locale.NewLocalizedError("error.api_key_no_scope_selected")
		} else {
			apiKeyCreationRequest.Scopes = apiKeyForm.Scopes
		}
	}

	validationErr := accessTypeErr
	if validationErr == nil {
		validationErr = validator.ValidateAPIKeyCreation(h.store, user.ID, apiKeyCreationRequest)
	}

	if validationErr != nil {
		view := view.New(h.tpl, r)
		view.Set("form", apiKeyForm)
		view.Set("scopes", model.ValidAPIKeyScopes())
		view.Set("menu", "settings")
		view.Set("user", user)
		navMetadata, _ := h.store.GetNavMetadata(user.ID)
		view.Set("countUnread", navMetadata.CountUnread)
		view.Set("countErrorFeeds", navMetadata.CountErrorFeeds)
		view.Set("errorMessage", validationErr.Translate(user.Language))
		response.HTML(w, r, view.Render("create_api_key"))
		return
	}

	if _, err = h.store.CreateAPIKey(user.ID, apiKeyCreationRequest.Description, apiKeyCreationRequest.Scopes); err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	response.HTMLRedirect(w, r, h.routePath("/keys"))
}
