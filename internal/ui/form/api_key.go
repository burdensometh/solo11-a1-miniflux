// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package form // import "miniflux.app/v2/internal/ui/form"

import (
	"net/http"
	"slices"
	"strings"

	"miniflux.app/v2/internal/model"
)

// APIKeyForm represents the API Key form.
type APIKeyForm struct {
	Description string
	AccessType  string
	Scopes      model.APIKeyScopes
}

// IsScoped returns true when the key must be restricted to the selected scopes.
func (f *APIKeyForm) IsScoped() bool {
	return f.AccessType == "scoped"
}

// HasScope returns true when the given scope is selected in the form.
func (f *APIKeyForm) HasScope(scope string) bool {
	return slices.Contains(f.Scopes, scope)
}

// NewAPIKeyForm returns a new APIKeyForm.
func NewAPIKeyForm(r *http.Request) *APIKeyForm {
	return &APIKeyForm{
		Description: strings.TrimSpace(r.FormValue("description")),
		AccessType:  r.FormValue("access_type"),
		Scopes:      model.APIKeyScopes(r.Form["scopes"]),
	}
}
