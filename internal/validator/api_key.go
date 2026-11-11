// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package validator // import "miniflux.app/v2/internal/validator"

import (
	"miniflux.app/v2/internal/locale"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
)

// ValidateAPIKeyCreation ensures API key creation requests include a description, are unique per user,
// and contain only valid scopes.
func ValidateAPIKeyCreation(store *storage.Storage, userID int64, request *model.APIKeyCreationRequest) *locale.LocalizedError {
	if request.Description == "" {
		return locale.NewLocalizedError("error.fields_mandatory")
	}

	if store.APIKeyExists(userID, request.Description) {
		return locale.NewLocalizedError("error.api_key_already_exists")
	}

	if !request.Scopes.IsValid() {
		return locale.NewLocalizedError("error.api_key_invalid_scope")
	}

	return nil
}
