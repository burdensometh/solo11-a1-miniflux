// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestDefaultClientDoesNotSendVersionHeader(t *testing.T) {
	var seenHeader string
	httpClient := newFakeHTTPClient(t, func(t *testing.T, req *http.Request) *http.Response {
		seenHeader = req.Header.Get("X-API-Version")
		return jsonResponseFrom(t, http.StatusOK, nil, EntryResultSet{Total: 0, Entries: Entries{}})
	})

	c := NewClientWithOptions("https://miniflux.example", WithHTTPClient(httpClient), WithAPIKey("secret"))
	if _, err := c.Entries(nil); err != nil {
		t.Fatalf("Entries failed: %v", err)
	}

	if seenHeader != "" {
		t.Fatalf("legacy client must not send a version header, got %q", seenHeader)
	}
}

func TestV2ClientSendsVersionHeaderAndDecodesPage(t *testing.T) {
	var seenHeader string
	httpClient := newFakeHTTPClient(t, func(t *testing.T, req *http.Request) *http.Response {
		seenHeader = req.Header.Get("X-API-Version")
		if !strings.Contains(req.URL.RawQuery, "cursor=abc") {
			t.Fatalf("cursor query parameter missing from %s", req.URL.RawQuery)
		}
		if strings.Contains(req.URL.RawQuery, "offset") {
			t.Fatalf("v2 request must never carry an offset parameter: %s", req.URL.RawQuery)
		}
		total := 5
		return jsonResponseFrom(t, http.StatusOK, nil, EntryPageV2{
			Entries:    EntriesV2{{Entry: Entry{ID: 1}, EntryID: 1, Excerpt: "preview"}},
			Total:      &total,
			Limit:      100,
			NextCursor: "next-token",
			HasMore:    true,
		})
	})

	c := NewClientWithOptions("https://miniflux.example",
		WithHTTPClient(httpClient),
		WithAPIKey("secret"),
		WithAPIVersion(APIVersion2),
	)

	page, err := c.EntriesV2(&EntryPageFilterV2{Limit: 100, Cursor: "abc", WithTotal: true})
	if err != nil {
		t.Fatalf("EntriesV2 failed: %v", err)
	}

	if seenHeader != "2" {
		t.Fatalf(`expected X-API-Version "2", got %q`, seenHeader)
	}
	if page.NextCursor != "next-token" || !page.HasMore {
		t.Fatalf("page metadata not decoded: %+v", page)
	}
	if page.Total == nil || *page.Total != 5 {
		t.Fatalf("optional total not decoded: %+v", page.Total)
	}
	if page.Entries[0].Excerpt != "preview" || page.Entries[0].EntryID != 1 {
		t.Fatalf("v2 entry fields not decoded: %+v", page.Entries[0])
	}
}

func TestV2MethodRejectsV1Client(t *testing.T) {
	c := NewClientWithOptions("https://miniflux.example")
	_, err := c.EntriesV2(nil)
	if !errors.Is(err, ErrV2Required) {
		t.Fatalf("expected ErrV2Required, got %v", err)
	}
}

func TestWithAPIVersionIgnoresUnsupportedValues(t *testing.T) {
	c := NewClientWithOptions("https://miniflux.example", WithAPIVersion(99))
	if c.request.apiVersion != 0 {
		t.Fatalf("unsupported version must be ignored, got %d", c.request.apiVersion)
	}
}

func TestBuildV2FilterQueryString(t *testing.T) {
	path := buildV2FilterQueryString("/v1/entries", &EntryPageFilterV2{
		Limit:     50,
		Order:     "published_at",
		Direction: "desc",
		Cursor:    "tok/with+special",
		Statuses:  []string{"unread"},
		Tags:      []string{"go"},
		WithTotal: true,
	})

	for _, expected := range []string{"limit=50", "order=published_at", "direction=desc", "with_total=true", "status=unread", "tags=go"} {
		if !strings.Contains(path, expected) {
			t.Fatalf("query %q is missing %q", path, expected)
		}
	}
	if strings.Contains(path, "offset") {
		t.Fatalf("v2 query must not contain an offset: %q", path)
	}
}
