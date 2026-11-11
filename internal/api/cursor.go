// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api // import "miniflux.app/v2/internal/api"

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"miniflux.app/v2/internal/storage"
)

// cursorTokenVersion tags the on-the-wire cursor format so a future,
// incompatible encoding can be introduced without silently misinterpreting
// existing tokens.
const cursorTokenVersion = 1

// cursorTTL bounds how long a pagination token stays usable. Clients paging
// through a feed for days would get a clean 400 with a retry hint instead of
// silently skipping or duplicating rows.
const cursorTTL = 24 * time.Hour

// cursorColumns is the whitelist of columns a cursor may order by. It is a
// subset of the v1 order values: cursor pagination needs a total, indexable
// ordering, so title/author/category/status sorts are not supported.
var cursorColumns = map[string]bool{
	"id":           true,
	"published_at": true,
	"changed_at":   true,
	"created_at":   true,
}

// cursorPayload is the JSON document hidden inside the opaque cursor token.
type cursorPayload struct {
	Version  int       `json:"v"`
	Column   string    `json:"c"`
	Time     time.Time `json:"t,omitempty"`
	EntryID  int64     `json:"i"`
	Desc     bool      `json:"d"`
	IssuedAt time.Time `json:"iat"`
}

// encodeCursor serializes a storage cursor into an opaque, URL-safe token.
// Tokens are not encrypted (they only expose an entry id and a timestamp the
// caller is already authorized to see), but the format is an implementation
// detail: clients must treat them as opaque strings.
func encodeCursor(cursor storage.EntryCursor, now time.Time) (string, error) {
	payload := cursorPayload{
		Version:  cursorTokenVersion,
		Column:   cursor.SortColumn,
		Time:     cursor.SortValue,
		EntryID:  cursor.EntryID,
		Desc:     cursor.Desc,
		IssuedAt: now.UTC(),
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// decodeCursor parses an opaque token back into a storage cursor. It rejects
// unknown formats, unsupported sort columns, malformed payloads and expired
// tokens with explicit errors so the handler can answer with a 400.
func decodeCursor(token string, now time.Time) (storage.EntryCursor, error) {
	var payload cursorPayload

	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return storage.EntryCursor{}, errors.New(`invalid "cursor" parameter: malformed token`)
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return storage.EntryCursor{}, errors.New(`invalid "cursor" parameter: malformed token`)
	}

	if payload.Version != cursorTokenVersion {
		return storage.EntryCursor{}, errors.New(`invalid "cursor" parameter: unsupported token version`)
	}

	if !cursorColumns[payload.Column] {
		return storage.EntryCursor{}, errors.New(`invalid "cursor" parameter: unknown sort column`)
	}

	if payload.EntryID <= 0 {
		return storage.EntryCursor{}, errors.New(`invalid "cursor" parameter: missing entry position`)
	}

	if payload.IssuedAt.IsZero() || now.Sub(payload.IssuedAt) > cursorTTL || payload.IssuedAt.After(now.Add(time.Minute)) {
		return storage.EntryCursor{}, errors.New(`invalid "cursor" parameter: token expired, restart pagination without a cursor`)
	}

	return storage.EntryCursor{
		SortColumn: payload.Column,
		SortValue:  payload.Time,
		EntryID:    payload.EntryID,
		Desc:       payload.Desc,
	}, nil
}
