// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package locale // import "miniflux.app/v2/internal/locale"

import "errors"

type LocalizedErrorWrapper struct {
	originalErr     error
	translationKey  string
	translationArgs []any
	httpStatusCode  int
}

func NewLocalizedErrorWrapper(originalErr error, translationKey string, translationArgs ...any) *LocalizedErrorWrapper {
	return &LocalizedErrorWrapper{
		originalErr:     originalErr,
		translationKey:  translationKey,
		translationArgs: translationArgs,
	}
}

// NewLocalizedHTTPErrorWrapper creates an error wrapper that also carries the
// HTTP status code returned by the remote server. A zero status code means the
// failure happened before any response was received.
func NewLocalizedHTTPErrorWrapper(originalErr error, httpStatusCode int, translationKey string, translationArgs ...any) *LocalizedErrorWrapper {
	wrapper := NewLocalizedErrorWrapper(originalErr, translationKey, translationArgs...)
	wrapper.httpStatusCode = httpStatusCode
	return wrapper
}

func (l *LocalizedErrorWrapper) Error() error {
	return l.originalErr
}

// HTTPStatusCode returns the HTTP status code associated with the error, or 0
// when the failure is not tied to a response.
func (l *LocalizedErrorWrapper) HTTPStatusCode() int {
	return l.httpStatusCode
}

func (l *LocalizedErrorWrapper) Translate(language string) string {
	if l.translationKey == "" {
		if l.originalErr == nil {
			return ""
		}
		return l.originalErr.Error()
	}
	return NewPrinter(language).Printf(l.translationKey, l.translationArgs...)
}

type LocalizedError struct {
	translationKey  string
	translationArgs []any
}

func NewLocalizedError(translationKey string, translationArgs ...any) *LocalizedError {
	return &LocalizedError{translationKey: translationKey, translationArgs: translationArgs}
}

func (v *LocalizedError) String() string {
	return NewPrinter("en_US").Printf(v.translationKey, v.translationArgs...)
}

func (v *LocalizedError) Error() error {
	return errors.New(v.String())
}

func (v *LocalizedError) Translate(language string) string {
	return NewPrinter(language).Printf(v.translationKey, v.translationArgs...)
}
