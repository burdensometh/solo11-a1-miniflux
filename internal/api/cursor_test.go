// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"
	"time"

	"miniflux.app/v2/internal/storage"
)

func TestCursorRoundTrip(t *testing.T) {
	now := time.Now().UTC()
	original := storage.EntryCursor{
		SortColumn: "published_at",
		SortValue:  time.Date(2026, 11, 11, 10, 30, 0, 0, time.UTC),
		EntryID:    4242,
		Desc:       true,
	}

	token, err := encodeCursor(original, now)
	if err != nil {
		t.Fatalf("encodeCursor failed: %v", err)
	}

	// The token must be URL-safe and must not leak a raw JSON structure.
	if strings.ContainsAny(token, "+/=") {
		t.Fatalf("cursor token is not URL-safe: %q", token)
	}

	decoded, err := decodeCursor(token, now)
	if err != nil {
		t.Fatalf("decodeCursor failed: %v", err)
	}

	if decoded != original {
		t.Fatalf("round trip mismatch:\nwant %+v\ngot  %+v", original, decoded)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	if _, err := decodeCursor("not-a-real-token", time.Now()); err == nil {
		t.Fatal("expected an error for a malformed token")
	}
}

func TestDecodeCursorRejectsTamperedPayload(t *testing.T) {
	now := time.Now().UTC()
	token, err := encodeCursor(storage.EntryCursor{SortColumn: "id", EntryID: 7}, now)
	if err != nil {
		t.Fatalf("encodeCursor failed: %v", err)
	}

	// Flip a character in the middle: base64 decoding or JSON validation
	// must catch the tampering.
	tampered := token[:len(token)-2]
	switch token[len(token)-2] {
	case 'A':
		tampered += "B"
	default:
		tampered += "A"
	}
	tampered += token[len(token)-1:]

	if _, err := decodeCursor(tampered, now); err == nil {
		t.Fatal("expected an error for a tampered token")
	}
}

func TestDecodeCursorRejectsExpiredToken(t *testing.T) {
	issued := time.Now().Add(-cursorTTL - time.Hour)
	token, err := encodeCursor(storage.EntryCursor{SortColumn: "id", EntryID: 7}, issued)
	if err != nil {
		t.Fatalf("encodeCursor failed: %v", err)
	}

	if _, err := decodeCursor(token, time.Now()); err == nil {
		t.Fatal("expected an error for an expired token")
	}
}

func TestDecodeCursorRejectsFutureToken(t *testing.T) {
	issued := time.Now().Add(2 * time.Hour)
	token, err := encodeCursor(storage.EntryCursor{SortColumn: "id", EntryID: 7}, issued)
	if err != nil {
		t.Fatalf("encodeCursor failed: %v", err)
	}

	if _, err := decodeCursor(token, time.Now()); err == nil {
		t.Fatal("expected an error for a token issued in the future")
	}
}

func TestDecodeCursorRejectsUnsupportedColumn(t *testing.T) {
	now := time.Now().UTC()
	token, err := encodeCursor(storage.EntryCursor{SortColumn: "title", EntryID: 7}, now)
	if err != nil {
		t.Fatalf("encodeCursor failed: %v", err)
	}

	if _, err := decodeCursor(token, now); err == nil {
		t.Fatal("expected an error for an unsupported sort column")
	}
}

func TestDecodeCursorRejectsMissingPosition(t *testing.T) {
	now := time.Now().UTC()
	token, err := encodeCursor(storage.EntryCursor{SortColumn: "id"}, now)
	if err != nil {
		t.Fatalf("encodeCursor failed: %v", err)
	}

	if _, err := decodeCursor(token, now); err == nil {
		t.Fatal("expected an error for a cursor without entry position")
	}
}
