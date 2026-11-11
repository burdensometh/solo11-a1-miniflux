// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
)

func TestWriteEntryMutationErrorConflict(t *testing.T) {
	h := &handler{}
	r := httptest.NewRequest(http.MethodPut, "/v1/entries/1", nil)
	w := httptest.NewRecorder()

	err := &storage.EntryConflictError{EntryID: 1, CurrentVersion: 6}
	h.writeEntryMutationError(w, r, err)

	resp := w.Result()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf(`Unexpected status code, got %d instead of %d`, resp.StatusCode, http.StatusConflict)
	}

	wantETag := model.FormatEntryETag(6)
	if got := resp.Header.Get("ETag"); got != wantETag {
		t.Fatalf(`Unexpected ETag, got %q instead of %q`, got, wantETag)
	}

	if got := w.Body.String(); got != `{"error_message":"`+err.Error()+`"}` {
		t.Fatalf(`Unexpected body: %s`, got)
	}
}

func TestWriteEntryMutationErrorOtherErrorsStayServerErrors(t *testing.T) {
	h := &handler{}
	r := httptest.NewRequest(http.MethodPut, "/v1/entries/1", nil)
	w := httptest.NewRecorder()

	h.writeEntryMutationError(w, r, errors.New("boom"))

	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf(`Unexpected status code, got %d instead of %d`, w.Result().StatusCode, http.StatusInternalServerError)
	}
}
