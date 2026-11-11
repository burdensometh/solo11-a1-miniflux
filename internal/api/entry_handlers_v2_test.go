// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"miniflux.app/v2/internal/apiversion"
	"miniflux.app/v2/internal/storage"
)

func encodeValidTestCursor(column string, desc bool) (string, error) {
	return encodeCursor(storage.EntryCursor{SortColumn: column, EntryID: 1, Desc: desc}, time.Now())
}

// v2Request builds a request whose context is negotiated as API v2, the same
// way the apiversion middleware would negotiate an X-API-Version: 2 request.
func v2Request(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	return r.WithContext(apiversion.WithContext(r.Context(), apiversion.V2))
}

func TestFindEntriesV2RejectsOffset(t *testing.T) {
	h := &handler{}
	r := v2Request("/v1/entries?limit=10&offset=20")
	w := httptest.NewRecorder()

	h.findEntries(w, r, 0, 0)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFindEntriesV2RejectsPositionalPaginationParams(t *testing.T) {
	h := &handler{}

	for _, param := range []string{"before_entry_id", "after_entry_id"} {
		r := v2Request("/v1/entries?" + param + "=123")
		w := httptest.NewRecorder()

		h.findEntries(w, r, 0, 0)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d", param, w.Code)
		}
	}
}

func TestFindEntriesV2KeepsDateFilterParameters(t *testing.T) {
	// "before"/"after" are date filters, not pagination: they must survive
	// the v2 validation layer instead of being mistaken for offset params.
	// The nil store panics later, which we swallow; anything but 400 proves
	// validation let the request through.
	h := &handler{}

	for _, param := range []string{"before", "after", "published_before", "published_after"} {
		r := v2Request("/v1/entries?" + param + "=1700000000")
		w := httptest.NewRecorder()

		func() {
			defer func() { _ = recover() }()
			h.findEntries(w, r, 0, 0)
		}()

		if w.Code == http.StatusBadRequest {
			t.Fatalf("%s must remain a valid v2 filter, got 400: %s", param, w.Body.String())
		}
	}
}

func TestFindEntriesV2RejectsUnsupportedSortOrder(t *testing.T) {
	h := &handler{}
	r := v2Request("/v1/entries?order=title")
	w := httptest.NewRecorder()

	h.findEntries(w, r, 0, 0)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-keyset order, got %d", w.Code)
	}
}

func TestFindEntriesV2RejectsInvalidDirection(t *testing.T) {
	h := &handler{}
	r := v2Request("/v1/entries?direction=sideways")
	w := httptest.NewRecorder()

	h.findEntries(w, r, 0, 0)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid direction, got %d", w.Code)
	}
}

func TestFindEntriesV2RejectsMalformedCursor(t *testing.T) {
	h := &handler{}
	// "!!!" is not in the URL-safe base64 alphabet, so decoding fails before
	// any store access.
	r := v2Request("/v1/entries?cursor=%21%21%21")
	w := httptest.NewRecorder()

	h.findEntries(w, r, 0, 0)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed cursor, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFindEntriesV2RejectsCursorOrderMismatch(t *testing.T) {
	h := &handler{}

	token, err := encodeValidTestCursor("id", false)
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}

	// The token was minted for order=id, but the request asks for
	// published_at: mixing them must be rejected rather than silently
	// reordering pages.
	r := v2Request("/v1/entries?order=published_at&direction=asc&cursor=" + token)
	w := httptest.NewRecorder()

	h.findEntries(w, r, 0, 0)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for cursor/order mismatch, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFindEntriesV1AcceptsOffsetAndLegacyOrder(t *testing.T) {
	// A request without the v2 context must stay on the v1 code path: v1
	// permits offset and the full historical set of order values. All v1
	// validation steps run before any store access, so a nil store is enough
	// to prove the v1 validation rules were the ones applied. Any outcome
	// other than 400 Bad Request means v2-style validation did not fire.
	h := &handler{}
	r := httptest.NewRequest(http.MethodGet, "/v1/entries?limit=10&offset=20&order=title", nil)
	w := httptest.NewRecorder()

	func() {
		defer func() { _ = recover() }()
		h.findEntries(w, r, 0, 0)
	}()

	if w.Code == http.StatusBadRequest {
		t.Fatalf("v1 request must not be rejected by v2 validation rules: %s", w.Body.String())
	}
}
