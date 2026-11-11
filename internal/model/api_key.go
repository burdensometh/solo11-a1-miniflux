// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model // import "miniflux.app/v2/internal/model"

import (
	"slices"
	"time"
)

// API key scopes. A key with an empty scope list has full access (legacy
// behavior); otherwise every API route requires one of these scopes.
const (
	ScopeEntriesRead      = "entries:read"
	ScopeEntriesWrite     = "entries:write"
	ScopeFeedsRead        = "feeds:read"
	ScopeFeedsWrite       = "feeds:write"
	ScopeCategoriesRead   = "categories:read"
	ScopeCategoriesWrite  = "categories:write"
	ScopeUsersRead        = "users:read"
	ScopeUsersWrite       = "users:write"
	ScopeIntegrationsRead = "integrations:read"
	ScopeAPIKeysRead      = "apikeys:read"
	ScopeAPIKeysWrite     = "apikeys:write"
)

// ValidAPIKeyScopes returns the list of scopes that can be granted to an API key.
func ValidAPIKeyScopes() APIKeyScopes {
	return APIKeyScopes{
		ScopeEntriesRead,
		ScopeEntriesWrite,
		ScopeFeedsRead,
		ScopeFeedsWrite,
		ScopeCategoriesRead,
		ScopeCategoriesWrite,
		ScopeUsersRead,
		ScopeUsersWrite,
		ScopeIntegrationsRead,
		ScopeAPIKeysRead,
		ScopeAPIKeysWrite,
	}
}

// APIKeyScopes represents the scopes granted to an API key.
// An empty list means the key is unrestricted (full access).
type APIKeyScopes []string

// Allows reports whether the scopes grant the given scope. Unrestricted keys
// (empty list) allow everything.
func (s APIKeyScopes) Allows(scope string) bool {
	if len(s) == 0 {
		return true
	}
	return slices.Contains(s, scope)
}

// IsValid reports whether all scopes are known scopes.
func (s APIKeyScopes) IsValid() bool {
	valid := ValidAPIKeyScopes()
	for _, scope := range s {
		if !slices.Contains(valid, scope) {
			return false
		}
	}
	return true
}

// IsSubsetOf reports whether s grants no more permissions than other. An
// empty list means full access, so it is only a subset of another empty
// (unrestricted) list.
func (s APIKeyScopes) IsSubsetOf(other APIKeyScopes) bool {
	if len(s) == 0 {
		return len(other) == 0
	}
	if len(other) == 0 {
		return true
	}
	for _, scope := range s {
		if !slices.Contains(other, scope) {
			return false
		}
	}
	return true
}

// APIKey represents an application API key.
// We need to use a pointer for LastUsedAt,
// as the value obtained from the database might sometimes be nil.
type APIKey struct {
	ID          int64        `json:"id"`
	UserID      int64        `json:"user_id"`
	Token       string       `json:"token"`
	Description string       `json:"description"`
	Scopes      APIKeyScopes `json:"scopes"`
	LastUsedAt  *time.Time   `json:"last_used_at"`
	CreatedAt   time.Time    `json:"created_at"`
}

// APIKeys represents a collection of API Key.
type APIKeys []APIKey

// APIKeyCreationRequest represents the request to create a new API Key.
// An empty Scopes list creates a key with full access.
type APIKeyCreationRequest struct {
	Description string       `json:"description"`
	Scopes      APIKeyScopes `json:"scopes,omitempty"`
}
