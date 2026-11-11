// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"errors"
	"testing"
)

func TestOrderedVersionPairs(t *testing.T) {
	ids, versions := orderedVersionPairs(map[int64]int64{
		30: 3,
		10: 1,
		20: 2,
	})

	wantIDs := []int64{10, 20, 30}
	wantVersions := []int64{1, 2, 3}

	if len(ids) != len(wantIDs) {
		t.Fatalf(`got %d ids, want %d`, len(ids), len(wantIDs))
	}

	for i := range ids {
		if ids[i] != wantIDs[i] {
			t.Fatalf(`ids not sorted by key at index %d: got %d, want %d`, i, ids[i], wantIDs[i])
		}
		if versions[i] != wantVersions[i] {
			t.Fatalf(`version not paired with id at index %d: got %d, want %d`, i, versions[i], wantVersions[i])
		}
	}
}

func TestOrderedVersionPairsEmpty(t *testing.T) {
	ids, versions := orderedVersionPairs(nil)
	if len(ids) != 0 || len(versions) != 0 {
		t.Fatalf(`expected empty slices, got %v and %v`, ids, versions)
	}
}

func TestEntryConflictErrorIs(t *testing.T) {
	err := &EntryConflictError{EntryID: 42, CurrentVersion: 7}

	if !errors.Is(err, ErrEntryConflict) {
		t.Fatal(`EntryConflictError must satisfy errors.Is(err, ErrEntryConflict)`)
	}

	if err.Error() == "" {
		t.Fatal(`EntryConflictError must render a message`)
	}
}
