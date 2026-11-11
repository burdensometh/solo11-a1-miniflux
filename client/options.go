// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client // import "miniflux.app/v2/client"

import "net/http"

// API version selectors understood by WithAPIVersion.
const (
	// APIVersion1 selects the legacy contract. It is also the default when
	// WithAPIVersion is not used, so existing client code keeps working
	// unchanged after a library upgrade.
	APIVersion1 = 1
	// APIVersion2 selects the new contract: additive entry fields and
	// cursor-based list pagination.
	APIVersion2 = 2
)

type Option func(*request)

// WithAPIKey sets the API key for the client.
func WithAPIKey(apiKey string) Option {
	return func(r *request) {
		r.apiKey = apiKey
	}
}

// WithCredentials sets the username and password for the client.
func WithCredentials(username, password string) Option {
	return func(r *request) {
		r.username = username
		r.password = password
	}
}

// WithHTTPClient sets the HTTP client for the client.
func WithHTTPClient(client *http.Client) Option {
	return func(r *request) {
		r.client = client
	}
}

// WithAPIVersion pins every request to the given API version. It is sent
// through the X-API-Version header, which the server echoes back on the
// response so callers can verify the negotiated contract.
//
// Passing 0 or an unsupported version leaves the default (v1) untouched, so a
// bad value can never silently change the behavior of legacy callers.
func WithAPIVersion(version int) Option {
	return func(r *request) {
		switch version {
		case APIVersion1, APIVersion2:
			r.apiVersion = version
		}
	}
}
