// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client // import "miniflux.app/v2/client"

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	userAgent      = "Miniflux Client Library"
	defaultTimeout = 80 * time.Second
)

// List of exposed errors.
var (
	ErrNotAuthorized = errors.New("miniflux: unauthorized (bad credentials)")
	ErrForbidden     = errors.New("miniflux: access forbidden")
	ErrServerError   = errors.New("miniflux: internal server error")
	ErrNotFound      = errors.New("miniflux: resource not found")
	ErrBadRequest    = errors.New("miniflux: bad request")
	ErrConflict      = errors.New("miniflux: conflict (the entry was modified by another request)")
	ErrEmptyEndpoint = errors.New("miniflux: empty endpoint provided")
)

// EntryConflictError wraps ErrConflict with the ETag of the current entry
// version returned by the server. Callers can refresh the entry, apply the
// pending change again, and retry the conditional write.
type EntryConflictError struct {
	// CurrentETag is the ETag header returned by the server, possibly empty.
	CurrentETag string
	// CurrentVersion is the version parsed from CurrentETag, zero when the
	// header is missing or malformed.
	CurrentVersion int64
}

func (e *EntryConflictError) Error() string {
	if e.CurrentETag == "" {
		return ErrConflict.Error()
	}
	return ErrConflict.Error() + ": " + e.CurrentETag
}

func (e *EntryConflictError) Unwrap() error {
	return ErrConflict
}

type errorResponse struct {
	ErrorMessage string `json:"error_message"`
}

type request struct {
	endpoint string
	username string
	password string
	apiKey   string
	client   *http.Client
}

func (r *request) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	return r.execute(ctx, http.MethodGet, path, nil, nil)
}

func (r *request) Post(ctx context.Context, path string, data any) (io.ReadCloser, error) {
	return r.execute(ctx, http.MethodPost, path, data, nil)
}

func (r *request) PostFile(ctx context.Context, path string, f io.ReadCloser) (io.ReadCloser, error) {
	return r.execute(ctx, http.MethodPost, path, f, nil)
}

func (r *request) Put(ctx context.Context, path string, data any) (io.ReadCloser, error) {
	return r.execute(ctx, http.MethodPut, path, data, nil)
}

// PutWithHeaders behaves like Put but lets the caller set extra request
// headers, e.g. If-Match for conditional writes.
func (r *request) PutWithHeaders(ctx context.Context, path string, data any, extraHeaders http.Header) (io.ReadCloser, error) {
	return r.execute(ctx, http.MethodPut, path, data, extraHeaders)
}

// PutReturningHeaders behaves like PutWithHeaders but also returns the
// response headers, which conditional endpoints use to expose the new ETag
// alongside a 204 response.
func (r *request) PutReturningHeaders(ctx context.Context, path string, data any, extraHeaders http.Header) (http.Header, error) {
	body, headers, err := r.executeWithHeaders(ctx, http.MethodPut, path, data, extraHeaders)
	if body != nil {
		body.Close()
	}
	return headers, err
}

func (r *request) Delete(ctx context.Context, path string) error {
	_, err := r.execute(ctx, http.MethodDelete, path, nil, nil)
	return err
}

func (r *request) execute(
	ctx context.Context,
	method string,
	path string,
	data any,
	extraHeaders http.Header,
) (io.ReadCloser, error) {
	body, _, err := r.executeWithHeaders(ctx, method, path, data, extraHeaders)
	return body, err
}

func (r *request) executeWithHeaders(
	ctx context.Context,
	method string,
	path string,
	data any,
	extraHeaders http.Header,
) (io.ReadCloser, http.Header, error) {
	if r.endpoint == "" {
		return nil, nil, ErrEmptyEndpoint
	}
	if r.endpoint[len(r.endpoint)-1:] == "/" {
		r.endpoint = r.endpoint[:len(r.endpoint)-1]
	}

	u, err := url.Parse(r.endpoint + path)
	if err != nil {
		return nil, nil, err
	}

	request, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, nil, err
	}

	request.Header = r.buildHeaders()
	for key, values := range extraHeaders {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}

	if r.username != "" && r.password != "" {
		request.SetBasicAuth(r.username, r.password)
	}

	if data != nil {
		switch data := data.(type) {
		case io.ReadCloser:
			request.Body = data
		default:
			request.Body = io.NopCloser(bytes.NewBuffer(r.toJSON(data)))
		}
	}

	client := r.client
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, err
	}

	switch response.StatusCode {
	case http.StatusUnauthorized:
		response.Body.Close()
		return nil, response.Header, ErrNotAuthorized
	case http.StatusForbidden:
		response.Body.Close()
		return nil, response.Header, ErrForbidden
	case http.StatusInternalServerError:
		defer response.Body.Close()

		var resp errorResponse
		decoder := json.NewDecoder(response.Body)
		// If we failed to decode, just return a generic ErrServerError
		if err := decoder.Decode(&resp); err != nil {
			return nil, response.Header, ErrServerError
		}
		return nil, response.Header, errors.New("miniflux: internal server error: " + resp.ErrorMessage)
	case http.StatusNotFound:
		response.Body.Close()
		return nil, response.Header, ErrNotFound
	case http.StatusNoContent:
		response.Body.Close()
		return nil, response.Header, nil
	case http.StatusBadRequest:
		defer response.Body.Close()

		var resp errorResponse
		decoder := json.NewDecoder(response.Body)
		if err := decoder.Decode(&resp); err != nil {
			return nil, response.Header, fmt.Errorf("%w (%v)", ErrBadRequest, err)
		}

		return nil, response.Header, fmt.Errorf("%w (%s)", ErrBadRequest, resp.ErrorMessage)
	case http.StatusConflict:
		defer response.Body.Close()

		var resp errorResponse
		decoder := json.NewDecoder(response.Body)
		conflictErr := &EntryConflictError{CurrentETag: response.Header.Get("ETag")}
		conflictErr.CurrentVersion = parseEntryVersionETag(conflictErr.CurrentETag)
		if err := decoder.Decode(&resp); err == nil && resp.ErrorMessage != "" {
			// Keep the server message reachable while preserving the typed
			// conflict for errors.As type assertions.
			return nil, response.Header, fmt.Errorf("%w: %s", conflictErr, resp.ErrorMessage)
		}
		return nil, response.Header, conflictErr
	}

	if response.StatusCode > 400 {
		response.Body.Close()
		return nil, response.Header, fmt.Errorf("miniflux: status code=%d", response.StatusCode)
	}

	return response.Body, response.Header, nil
}

// parseEntryVersionETag extracts the numeric version from an ETag shaped like
// W/"entry-version-12". It returns zero for anything unparseable.
func parseEntryVersionETag(etag string) int64 {
	etag = strings.TrimSpace(etag)
	etag = strings.TrimPrefix(etag, "W/")
	etag = strings.TrimSpace(etag)
	if len(etag) < 2 || etag[0] != '"' || etag[len(etag)-1] != '"' {
		return 0
	}

	const prefix = "entry-version-"
	opaque := etag[1 : len(etag)-1]
	if !strings.HasPrefix(opaque, prefix) {
		return 0
	}

	version, err := strconv.ParseInt(opaque[len(prefix):], 10, 64)
	if err != nil {
		return 0
	}
	return version
}

func (r *request) buildHeaders() http.Header {
	headers := make(http.Header)
	headers.Add("User-Agent", userAgent)
	headers.Add("Content-Type", "application/json")
	headers.Add("Accept", "application/json")
	if r.apiKey != "" {
		headers.Add("X-Auth-Token", r.apiKey)
	}
	return headers
}

func (r *request) toJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		log.Println("Unable to convert interface to JSON:", err)
		return []byte("")
	}

	return b
}
