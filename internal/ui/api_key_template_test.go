// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/template"
	"miniflux.app/v2/internal/ui/form"
	"miniflux.app/v2/internal/ui/view"
)

func renderAPIKeyTemplate(t *testing.T, templateName string, params map[string]any) string {
	t.Helper()

	parsedOptions, err := config.NewConfigParser().ParseEnvironmentVariables()
	if err != nil {
		t.Fatalf(`Unable to configure test options: %v`, err)
	}

	previousOptions := config.Opts
	config.Opts = parsedOptions
	t.Cleanup(func() {
		config.Opts = previousOptions
	})

	tpl := template.NewEngine("")
	tpl.ParseTemplates()

	session, _ := model.NewWebSession("test", "127.0.0.1")
	r := httptest.NewRequest(http.MethodGet, "/keys", nil)
	r = r.WithContext(context.WithValue(r.Context(), request.WebSessionContextKey, session))

	v := view.New(tpl, r)
	v.Set("menu", "settings")
	v.Set("user", &model.User{Username: "testuser", Language: "en_US", Timezone: "UTC"})
	v.Set("countUnread", 0)
	v.Set("countErrorFeeds", 0)
	for key, value := range params {
		v.Set(key, value)
	}

	return string(v.Render(templateName))
}

func TestCreateAPIKeyTemplateRendersScopeChoices(t *testing.T) {
	output := renderAPIKeyTemplate(t, "create_api_key", map[string]any{
		"form":   &form.APIKeyForm{},
		"scopes": model.ValidAPIKeyScopes(),
	})

	for _, scope := range model.ValidAPIKeyScopes() {
		if !strings.Contains(output, `value="`+scope+`"`) {
			t.Fatalf(`Expected the form to contain a checkbox for scope %q`, scope)
		}
	}

	if !strings.Contains(output, `name="access_type"`) {
		t.Fatal(`Expected the form to contain the access type selector`)
	}
}

func TestAPIKeysTemplateShowsScopes(t *testing.T) {
	createdAt := time.Now()
	output := renderAPIKeyTemplate(t, "api_keys", map[string]any{
		"apiKeys": model.APIKeys{
			model.APIKey{ID: 1, Token: "legacy-token", Description: "legacy", CreatedAt: createdAt},
			model.APIKey{ID: 2, Token: "scoped-token", Description: "scoped", Scopes: model.APIKeyScopes{model.ScopeEntriesRead}, CreatedAt: createdAt},
		},
	})

	if !strings.Contains(output, "Full access") {
		t.Fatal(`Expected the unrestricted key to be labeled as full access`)
	}

	if !strings.Contains(output, "Read entries") {
		t.Fatal(`Expected the scoped key to display its translated scope label`)
	}

	if strings.Contains(output, "page.api_keys.scope.") {
		t.Fatal(`Expected scope labels to be translated, got a raw translation key`)
	}
}
