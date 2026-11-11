// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model // import "miniflux.app/v2/internal/model"

import "testing"

func TestAPIKeyScopesAllowsEverythingWhenEmpty(t *testing.T) {
	var scopes APIKeyScopes

	for _, scope := range ValidAPIKeyScopes() {
		if !scopes.Allows(scope) {
			t.Fatalf(`Expected an empty scope list to allow %q`, scope)
		}
	}
}

func TestAPIKeyScopesAllowsGrantedScope(t *testing.T) {
	scopes := APIKeyScopes{ScopeEntriesRead, ScopeEntriesWrite}

	if !scopes.Allows(ScopeEntriesRead) {
		t.Fatalf(`Expected %q to be allowed`, ScopeEntriesRead)
	}

	if !scopes.Allows(ScopeEntriesWrite) {
		t.Fatalf(`Expected %q to be allowed`, ScopeEntriesWrite)
	}
}

func TestAPIKeyScopesDeniesScopeThatWasNotGranted(t *testing.T) {
	scopes := APIKeyScopes{ScopeEntriesRead}

	if scopes.Allows(ScopeUsersWrite) {
		t.Fatalf(`Expected %q to be denied`, ScopeUsersWrite)
	}

	if scopes.Allows(ScopeEntriesWrite) {
		t.Fatalf(`Expected %q to be denied`, ScopeEntriesWrite)
	}
}

func TestAPIKeyScopesIsValid(t *testing.T) {
	if !ValidAPIKeyScopes().IsValid() {
		t.Fatal(`Expected the list of valid scopes to be valid`)
	}

	if (APIKeyScopes{}).IsValid() != true {
		t.Fatal(`Expected an empty scope list to be valid`)
	}

	if (APIKeyScopes{"entries:read", "bogus"}).IsValid() {
		t.Fatal(`Expected an unknown scope to be invalid`)
	}
}

func TestAPIKeyScopesIsSubsetOf(t *testing.T) {
	scenarios := []struct {
		name     string
		scopes   APIKeyScopes
		other    APIKeyScopes
		expected bool
	}{
		{name: "unrestricted subset of unrestricted", scopes: nil, other: nil, expected: true},
		{name: "unrestricted not subset of restricted", scopes: nil, other: APIKeyScopes{ScopeEntriesRead}, expected: false},
		{name: "restricted subset of unrestricted", scopes: APIKeyScopes{ScopeEntriesRead}, other: nil, expected: true},
		{name: "same scopes", scopes: APIKeyScopes{ScopeEntriesRead}, other: APIKeyScopes{ScopeEntriesRead}, expected: true},
		{name: "strict subset", scopes: APIKeyScopes{ScopeEntriesRead}, other: APIKeyScopes{ScopeEntriesRead, ScopeEntriesWrite}, expected: true},
		{name: "superset is not a subset", scopes: APIKeyScopes{ScopeEntriesRead, ScopeEntriesWrite}, other: APIKeyScopes{ScopeEntriesRead}, expected: false},
		{name: "disjoint scopes", scopes: APIKeyScopes{ScopeUsersWrite}, other: APIKeyScopes{ScopeEntriesRead}, expected: false},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			if got := scenario.scopes.IsSubsetOf(scenario.other); got != scenario.expected {
				t.Fatalf(`Expected IsSubsetOf to return %v, got %v`, scenario.expected, got)
			}
		})
	}
}
