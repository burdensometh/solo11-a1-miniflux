// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"miniflux.app/v2/internal/config"
)

const previewTestFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Preview Test Feed</title>
    <link>https://example.com/</link>
    <description>A feed used by API tests</description>
    <item>
      <title>First entry</title>
      <link>https://example.com/1</link>
      <pubDate>Mon, 01 Jan 2024 00:00:00 GMT</pubDate>
    </item>
  </channel>
</rss>`

func newPreviewEndpointTestServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, previewTestFeed)
	})
	mux.HandleFunc("/broken.xml", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	return httptest.NewServer(mux)
}

func configurePreviewTestOptions(t *testing.T) {
	t.Helper()
	t.Setenv("FETCHER_ALLOW_PRIVATE_NETWORKS", "1")

	parsedOptions, err := config.NewConfigParser().ParseEnvironmentVariables()
	if err != nil {
		t.Fatalf("Unable to parse test options: %v", err)
	}

	previousOptions := config.Opts
	config.Opts = parsedOptions
	t.Cleanup(func() { config.Opts = previousOptions })
}

func TestFeedPreviewHandlerReturnsPreviewsAndIsolatesFailures(t *testing.T) {
	configurePreviewTestOptions(t)
	server := newPreviewEndpointTestServer()
	defer server.Close()

	requestBody, err := json.Marshal(map[string][]string{
		"feed_urls": {server.URL + "/broken.xml", server.URL + "/feed.xml"},
	})
	if err != nil {
		t.Fatal(err)
	}

	h := &handler{}
	r := httptest.NewRequest(http.MethodPost, "/v1/discover/previews", bytes.NewReader(requestBody))
	w := httptest.NewRecorder()

	h.feedPreviewHandler(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf(`Unexpected status code, got %d instead of %d`, w.Code, http.StatusOK)
	}

	var results []feedPreviewResult
	if err := json.NewDecoder(w.Body).Decode(&results); err != nil {
		t.Fatalf("Unable to decode response: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf(`Expected 2 results, got %d`, len(results))
	}

	// Request order must be preserved.
	if results[0].FeedURL != server.URL+"/broken.xml" || results[1].FeedURL != server.URL+"/feed.xml" {
		t.Fatalf("Result order does not match request order: %+v", results)
	}

	if results[0].Preview.Error == "" {
		t.Fatal("The broken feed should carry a preview error")
	}
	if results[1].Preview.Error != "" {
		t.Fatalf("The valid feed should not carry an error: %s", results[1].Preview.Error)
	}
	if results[1].Preview.FeedTitle != "Preview Test Feed" {
		t.Fatalf(`Unexpected feed title: %q`, results[1].Preview.FeedTitle)
	}
	if len(results[1].Preview.Entries) != 1 || results[1].Preview.Entries[0].Title != "First entry" {
		t.Fatalf("Unexpected preview entries: %+v", results[1].Preview.Entries)
	}
}

func TestFeedPreviewHandlerRejectsInvalidBody(t *testing.T) {
	h := &handler{}
	r := httptest.NewRequest(http.MethodPost, "/v1/discover/previews", bytes.NewReader([]byte("{")))
	w := httptest.NewRecorder()

	h.feedPreviewHandler(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf(`Unexpected status code, got %d instead of %d`, w.Code, http.StatusBadRequest)
	}
}

func TestFeedPreviewHandlerRejectsEmptyAndMalformedURLs(t *testing.T) {
	h := &handler{}

	for _, body := range []string{
		`{"feed_urls":[]}`,
		`{"feed_urls":["not-an-url"]}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/discover/previews", bytes.NewReader([]byte(body)))
		w := httptest.NewRecorder()

		h.feedPreviewHandler(w, r)

		if w.Code != http.StatusBadRequest {
			t.Fatalf(`Body %q: unexpected status code, got %d instead of %d`, body, w.Code, http.StatusBadRequest)
		}
	}
}
