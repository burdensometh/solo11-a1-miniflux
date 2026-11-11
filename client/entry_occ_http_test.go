// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
)

func TestUpdateEntryWithETagSendsIfMatch(t *testing.T) {
	updated := &Entry{ID: 1, Title: "New title", Version: 2}
	etag := EntryVersionETag(1)

	c := NewClientWithOptions(
		"http://mf",
		WithHTTPClient(
			newFakeHTTPClient(t, func(t *testing.T, req *http.Request) *http.Response {
				expectRequest(t, http.MethodPut, "http://mf/v1/entries/1", nil, req)

				if got := req.Header.Get("If-Match"); got != etag {
					t.Fatalf(`Unexpected If-Match header, got %q instead of %q`, got, etag)
				}

				return jsonResponseFrom(t, http.StatusCreated, http.Header{"ETag": []string{EntryVersionETag(2)}}, updated)
			})))

	entry, err := c.UpdateEntryWithETag(t.Context(), 1, &EntryModificationRequest{Title: &updated.Title}, etag)
	if err != nil {
		t.Fatalf(`Expected no error, got %v`, err)
	}
	if entry.Version != 2 {
		t.Fatalf(`Unexpected version, got %d`, entry.Version)
	}
}

func TestUpdateEntryWithETagConflict(t *testing.T) {
	currentETag := EntryVersionETag(5)
	c := NewClientWithOptions(
		"http://mf",
		WithHTTPClient(
			newFakeHTTPClient(t, func(t *testing.T, req *http.Request) *http.Response {
				return jsonResponseFrom(t, http.StatusConflict, http.Header{"ETag": []string{currentETag}}, map[string]string{
					"error_message": "entry has been modified",
				})
			})))

	_, err := c.UpdateEntryWithETag(t.Context(), 1, &EntryModificationRequest{}, EntryVersionETag(4))
	if err == nil {
		t.Fatal(`Expected a conflict error`)
	}

	var conflict *EntryConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf(`Expected *EntryConflictError, got %T: %v`, err, err)
	}
	if conflict.CurrentETag != currentETag {
		t.Fatalf(`Unexpected current ETag, got %q`, conflict.CurrentETag)
	}
	if conflict.CurrentVersion != 5 {
		t.Fatalf(`Unexpected current version, got %d`, conflict.CurrentVersion)
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatal(`The conflict error must unwrap to ErrConflict`)
	}
}

func TestToggleStarredWithETagReturnsNewVersion(t *testing.T) {
	c := NewClientWithOptions(
		"http://mf",
		WithHTTPClient(
			newFakeHTTPClient(t, func(t *testing.T, req *http.Request) *http.Response {
				expectRequest(t, http.MethodPut, "http://mf/v1/entries/1/star", nil, req)
				if got := req.Header.Get("If-Match"); got != EntryVersionETag(3) {
					t.Fatalf(`Unexpected If-Match header, got %q`, got)
				}
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Body:       http.NoBody,
					Header:     http.Header{"ETag": []string{EntryVersionETag(4)}},
				}
			})))

	version, err := c.ToggleStarredWithETag(t.Context(), 1, EntryVersionETag(3))
	if err != nil {
		t.Fatalf(`Expected no error, got %v`, err)
	}
	if version != 4 {
		t.Fatalf(`Expected new version 4, got %d`, version)
	}
}

func TestUpdateEntriesWithExpectedVersionsPayload(t *testing.T) {
	starred := true
	payload := &EntriesStatusUpdateRequest{
		EntryIDs:         []int64{1, 2},
		Starred:          &starred,
		ExpectedVersions: map[int64]int64{1: 3, 2: 7},
	}

	c := NewClientWithOptions(
		"http://mf",
		WithHTTPClient(
			newFakeHTTPClient(t, func(t *testing.T, req *http.Request) *http.Response {
				expectRequest(t, http.MethodPut, "http://mf/v1/entries", func(r io.Reader) {
					body, err := io.ReadAll(r)
					if err != nil {
						t.Fatal(err)
					}
					decoded := &EntriesStatusUpdateRequest{}
					if err := json.Unmarshal(body, decoded); err != nil {
						t.Fatal(err)
					}
					if decoded.ExpectedVersions[1] != 3 || decoded.ExpectedVersions[2] != 7 {
						t.Fatalf(`Unexpected expected versions: %v`, decoded.ExpectedVersions)
					}
				}, req)
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Body:       http.NoBody,
					Header:     http.Header{},
				}
			})))

	if err := c.UpdateEntriesWithExpectedVersions(t.Context(), payload); err != nil {
		t.Fatalf(`Expected no error, got %v`, err)
	}
}
