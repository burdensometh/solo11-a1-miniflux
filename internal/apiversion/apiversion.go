// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package apiversion implements API version negotiation.
//
// Versioning rules (stable for the whole v1 support window):
//
//   - A request without any version selector is always treated as v1. Old
//     clients therefore keep their exact historical behavior without any
//     code change.
//   - A client opts in to the new behavior by sending either the
//     "X-API-Version: 2" header or an Accept media type parameter
//     (e.g. "application/json; version=2").
//   - The server always echoes the negotiated version back in the
//     "X-API-Version" response header and lists the supported versions in
//     "X-API-Version-Supported", so clients can assert the contract they
//     got instead of guessing it.
package apiversion // import "miniflux.app/v2/internal/apiversion"

import (
	"context"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

const (
	// V1 is the legacy API version. It is the default when a request does
	// not carry any explicit version selector.
	V1 = 1

	// V2 is the new API version introducing additional response fields and
	// cursor-based pagination.
	V2 = 2

	// HeaderName is the request/response header used to select and to echo
	// the negotiated API version.
	HeaderName = "X-API-Version"

	// SupportedHeaderName advertises the versions the server understands.
	SupportedHeaderName = "X-API-Version-Supported"

	supportedHeaderValue = "1, 2"
)

type contextKey struct{}

// FromContext returns the API version stored in the request context, or V1
// when none was negotiated. V1 is intentionally the zero-ish default so a
// request that bypasses the negotiation middleware still gets legacy behavior.
func FromContext(ctx context.Context) int {
	if version, ok := ctx.Value(contextKey{}).(int); ok {
		return version
	}
	return V1
}

// WithContext stores the negotiated API version in the context.
func WithContext(ctx context.Context, version int) context.Context {
	return context.WithValue(ctx, contextKey{}, version)
}

// Negotiate resolves the API version requested by the client.
//
// The X-API-Version header takes precedence over the Accept media type
// parameter. Unknown, empty or malformed selectors fall back to v1 instead of
// producing an error, so a misbehaving proxy or a future client talking to an
// older server never breaks legacy traffic.
func Negotiate(r *http.Request) int {
	if value := strings.TrimSpace(r.Header.Get(HeaderName)); value != "" {
		if version, ok := parseVersion(value); ok {
			return version
		}
		return V1
	}

	if _, params, err := mime.ParseMediaType(r.Header.Get("Accept")); err == nil {
		if value := strings.TrimSpace(params["version"]); value != "" {
			if version, ok := parseVersion(value); ok {
				return version
			}
		}
	}

	return V1
}

// IsSupported reports whether the given version is served by this build.
func IsSupported(version int) bool {
	switch version {
	case V1, V2:
		return true
	}
	return false
}

// Middleware resolves the requested version once, stores it in the request
// context and advertises the negotiated and supported versions on the
// response.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := Negotiate(r)
		w.Header().Set(HeaderName, strconv.Itoa(version))
		w.Header().Set(SupportedHeaderName, supportedHeaderValue)
		next.ServeHTTP(w, r.WithContext(WithContext(r.Context(), version)))
	})
}

func parseVersion(value string) (int, bool) {
	// Be liberal about the accepted shape: "2", "v2" and "2.0" all resolve
	// to the same major version, because only major versions carry breaking
	// semantics.
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	value = strings.TrimPrefix(value, "V")
	if major, _, found := strings.Cut(value, "."); found {
		value = major
	}

	version, err := strconv.Atoi(value)
	if err != nil || !IsSupported(version) {
		return 0, false
	}
	return version, true
}
