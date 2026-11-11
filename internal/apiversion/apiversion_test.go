// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package apiversion

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNegotiateDefaultsToV1WithoutAnySelector(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/entries", nil)

	if got := Negotiate(r); got != V1 {
		t.Fatalf(`Expected v1 by default, got %d`, got)
	}
}

func TestNegotiateReadsHeader(t *testing.T) {
	for _, value := range []string{"2", "v2", "V2", "2.0"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/entries", nil)
		r.Header.Set(HeaderName, value)

		if got := Negotiate(r); got != V2 {
			t.Fatalf(`Expected v2 for header %q, got %d`, value, got)
		}
	}
}

func TestNegotiateReadsAcceptMediaTypeParameter(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/entries", nil)
	r.Header.Set("Accept", "application/json; version=2")

	if got := Negotiate(r); got != V2 {
		t.Fatalf(`Expected v2 through Accept parameter, got %d`, got)
	}
}

func TestNegotiateHeaderTakesPrecedenceOverAccept(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/entries", nil)
	r.Header.Set(HeaderName, "1")
	r.Header.Set("Accept", "application/json; version=2")

	if got := Negotiate(r); got != V1 {
		t.Fatalf(`Expected header to win over Accept, got %d`, got)
	}
}

func TestNegotiateFallsBackToV1OnUnknownVersion(t *testing.T) {
	cases := map[string]string{
		"unknown header": "99",
		"garbage header": "banana",
		"unknown accept": "",
	}

	for name, headerValue := range cases {
		r := httptest.NewRequest(http.MethodGet, "/v1/entries", nil)
		if headerValue != "" {
			r.Header.Set(HeaderName, headerValue)
		}
		if name == "unknown accept" {
			r.Header.Set("Accept", "application/json; version=42")
		}

		if got := Negotiate(r); got != V1 {
			t.Fatalf(`%s: expected v1 fallback, got %d`, name, got)
		}
	}
}

func TestFromContextDefaultsToV1(t *testing.T) {
	if got := FromContext(context.Background()); got != V1 {
		t.Fatalf(`Expected v1 zero value, got %d`, got)
	}
}

func TestMiddlewareEchoesNegotiatedAndSupportedVersions(t *testing.T) {
	for _, tc := range []struct {
		header   string
		expected string
	}{
		{"", "1"},
		{"1", "1"},
		{"2", "2"},
		{"99", "1"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/v1/entries", nil)
		if tc.header != "" {
			r.Header.Set(HeaderName, tc.header)
		}
		w := httptest.NewRecorder()

		var seenVersion int
		Middleware(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
			seenVersion = FromContext(req.Context())
		})).ServeHTTP(w, r)

		if got := w.Header().Get(HeaderName); got != tc.expected {
			t.Fatalf(`header=%q: expected echoed %q, got %q`, tc.header, tc.expected, got)
		}
		if got := w.Header().Get(SupportedHeaderName); got != supportedHeaderValue {
			t.Fatalf(`header=%q: unexpected supported header %q`, tc.header, got)
		}
		if got := seenVersion; got != int(atol(tc.expected)) {
			t.Fatalf(`header=%q: context version = %d`, tc.header, got)
		}
	}
}

// atol avoids pulling strconv into the table readability above.
func atol(s string) int64 {
	var n int64
	for _, c := range s {
		n = n*10 + int64(c-'0')
	}
	return n
}
