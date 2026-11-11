// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package response // import "miniflux.app/v2/internal/http/response"

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"miniflux.app/v2/internal/http/request"
)

const jsonContentTypeHeader = `application/json`

// JSON creates a new JSON response with a 200 status code.
func JSON(w http.ResponseWriter, r *http.Request, body any) {
	responseBody, err := json.Marshal(body)
	if err != nil {
		JSONServerError(w, r, err)
		return
	}

	NewBuilder(w, r).
		WithHeader("Content-Type", jsonContentTypeHeader).
		WithBodyAsBytes(responseBody).
		Write()
}

// JSONWithETag behaves like JSON but also exposes the given ETag, which
// clients can echo back through If-Match to perform conditional writes.
func JSONWithETag(w http.ResponseWriter, r *http.Request, body any, etag string) {
	responseBody, err := json.Marshal(body)
	if err != nil {
		JSONServerError(w, r, err)
		return
	}

	NewBuilder(w, r).
		WithHeader("Content-Type", jsonContentTypeHeader).
		WithHeader("ETag", etag).
		WithHeader("Vary", "If-Match").
		WithBodyAsBytes(responseBody).
		Write()
}

// JSONCreated sends a created response to the client.
func JSONCreated(w http.ResponseWriter, r *http.Request, body any) {
	responseBody, err := json.Marshal(body)
	if err != nil {
		JSONServerError(w, r, err)
		return
	}

	NewBuilder(w, r).
		WithStatus(http.StatusCreated).
		WithHeader("Content-Type", jsonContentTypeHeader).
		WithBodyAsBytes(responseBody).
		Write()
}

// JSONCreatedWithETag behaves like JSONCreated but also exposes the given
// ETag of the updated resource.
func JSONCreatedWithETag(w http.ResponseWriter, r *http.Request, body any, etag string) {
	responseBody, err := json.Marshal(body)
	if err != nil {
		JSONServerError(w, r, err)
		return
	}

	NewBuilder(w, r).
		WithStatus(http.StatusCreated).
		WithHeader("Content-Type", jsonContentTypeHeader).
		WithHeader("ETag", etag).
		WithHeader("Vary", "If-Match").
		WithBodyAsBytes(responseBody).
		Write()
}

// JSONAccepted sends an accepted response to the client.
func JSONAccepted(w http.ResponseWriter, r *http.Request) {
	NewBuilder(w, r).
		WithStatus(http.StatusAccepted).
		WithHeader("Content-Type", jsonContentTypeHeader).
		Write()
}

// JSONServerError sends an internal error to the client.
func JSONServerError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error(http.StatusText(http.StatusInternalServerError),
		slog.Any("error", err),
		slog.String("client_ip", request.ClientIP(r)),
		slog.Group("request",
			slog.String("method", r.Method),
			slog.String("uri", r.RequestURI),
			slog.String("user_agent", r.UserAgent()),
		),
		slog.Group("response",
			slog.Int("status_code", http.StatusInternalServerError),
		),
	)

	NewBuilder(w, r).
		WithStatus(http.StatusInternalServerError).
		WithHeader("Content-Type", jsonContentTypeHeader).
		WithBodyAsBytes(generateJSONError(err)).
		Write()
}

// JSONBadRequest sends a bad request error to the client.
func JSONBadRequest(w http.ResponseWriter, r *http.Request, err error) {
	slog.Warn(http.StatusText(http.StatusBadRequest),
		slog.Any("error", err),
		slog.String("client_ip", request.ClientIP(r)),
		slog.Group("request",
			slog.String("method", r.Method),
			slog.String("uri", r.RequestURI),
			slog.String("user_agent", r.UserAgent()),
		),
		slog.Group("response",
			slog.Int("status_code", http.StatusBadRequest),
		),
	)

	NewBuilder(w, r).
		WithStatus(http.StatusBadRequest).
		WithHeader("Content-Type", jsonContentTypeHeader).
		WithBodyAsBytes(generateJSONError(err)).
		Write()
}

// JSONUnauthorized sends a not authorized error to the client.
func JSONUnauthorized(w http.ResponseWriter, r *http.Request) {
	slog.Warn(http.StatusText(http.StatusUnauthorized),
		slog.String("client_ip", request.ClientIP(r)),
		slog.Group("request",
			slog.String("method", r.Method),
			slog.String("uri", r.RequestURI),
			slog.String("user_agent", r.UserAgent()),
		),
		slog.Group("response",
			slog.Int("status_code", http.StatusUnauthorized),
		),
	)

	NewBuilder(w, r).
		WithStatus(http.StatusUnauthorized).
		WithHeader("Content-Type", jsonContentTypeHeader).
		WithBodyAsBytes(generateJSONError(errors.New("access unauthorized"))).
		Write()
}

// JSONForbidden sends a forbidden error to the client.
func JSONForbidden(w http.ResponseWriter, r *http.Request) {
	slog.Warn(http.StatusText(http.StatusForbidden),
		slog.String("client_ip", request.ClientIP(r)),
		slog.Group("request",
			slog.String("method", r.Method),
			slog.String("uri", r.RequestURI),
			slog.String("user_agent", r.UserAgent()),
		),
		slog.Group("response",
			slog.Int("status_code", http.StatusForbidden),
		),
	)

	NewBuilder(w, r).
		WithStatus(http.StatusForbidden).
		WithHeader("Content-Type", jsonContentTypeHeader).
		WithBodyAsBytes(generateJSONError(errors.New("access forbidden"))).
		Write()
}

// JSONNotFound sends a page not found error to the client.
func JSONNotFound(w http.ResponseWriter, r *http.Request) {
	slog.Warn(http.StatusText(http.StatusNotFound),
		slog.String("client_ip", request.ClientIP(r)),
		slog.Group("request",
			slog.String("method", r.Method),
			slog.String("uri", r.RequestURI),
			slog.String("user_agent", r.UserAgent()),
		),
		slog.Group("response",
			slog.Int("status_code", http.StatusNotFound),
		),
	)

	NewBuilder(w, r).
		WithStatus(http.StatusNotFound).
		WithHeader("Content-Type", jsonContentTypeHeader).
		WithBodyAsBytes(generateJSONError(errors.New("resource not found"))).
		Write()
}

// JSONConflict sends an optimistic-locking conflict to the client. The
// currentETag, when non-empty, is returned in the ETag header so the client
// can refresh its copy without an extra GET round-trip.
func JSONConflict(w http.ResponseWriter, r *http.Request, err error, currentETag string) {
	slog.Warn(http.StatusText(http.StatusConflict),
		slog.Any("error", err),
		slog.String("client_ip", request.ClientIP(r)),
		slog.Group("request",
			slog.String("method", r.Method),
			slog.String("uri", r.RequestURI),
			slog.String("user_agent", r.UserAgent()),
		),
		slog.Group("response",
			slog.Int("status_code", http.StatusConflict),
		),
	)

	builder := NewBuilder(w, r).
		WithStatus(http.StatusConflict).
		WithHeader("Content-Type", jsonContentTypeHeader).
		WithHeader("Vary", "If-Match")

	if currentETag != "" {
		builder.WithHeader("ETag", currentETag)
	}

	builder.WithBodyAsBytes(generateJSONError(err)).Write()
}

func generateJSONError(err error) []byte {
	type errorMsg struct {
		ErrorMessage string `json:"error_message"`
	}

	encodedBody, _ := json.Marshal(errorMsg{ErrorMessage: err.Error()})
	return encodedBody
}
