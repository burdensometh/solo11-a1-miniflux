// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package validator // import "miniflux.app/v2/internal/validator"

import (
	"errors"
	"fmt"

	"miniflux.app/v2/internal/model"
)

// ValidateEntriesStatusUpdateRequest validates a status update for a list of entries.
func ValidateEntriesStatusUpdateRequest(request *model.EntriesStatusUpdateRequest) error {
	if len(request.EntryIDs) == 0 {
		return errors.New(`the list of entries cannot be empty`)
	}

	return ValidateEntryStatus(request.Status)
}

// ValidateEntriesStatusAndStarredUpdateRequest validates a status and/or starred update
// for a list of entries. At least one of the status or starred fields must be specified.
// This is used by the API, which can update the read status, the starred state, or both.
func ValidateEntriesStatusAndStarredUpdateRequest(request *model.EntriesStatusUpdateRequest) error {
	if len(request.EntryIDs) == 0 {
		return errors.New(`the list of entries cannot be empty`)
	}

	if request.Status == "" && request.Starred == nil {
		return errors.New(`either the status or the starred field must be specified`)
	}

	if request.Status != "" {
		if err := ValidateEntryStatus(request.Status); err != nil {
			return err
		}
	}

	// Every optimistic-locking guard must reference an entry the request is
	// actually updating; otherwise the server would silently ignore a stale
	// version and the client could not reason about the outcome.
	if len(request.ExpectedVersions) > 0 {
		knownIDs := make(map[int64]struct{}, len(request.EntryIDs))
		for _, id := range request.EntryIDs {
			knownIDs[id] = struct{}{}
		}
		for entryID, version := range request.ExpectedVersions {
			if _, exists := knownIDs[entryID]; !exists {
				return fmt.Errorf(`expected_versions references entry #%d which is not in entry_ids`, entryID)
			}
			if version <= 0 {
				return fmt.Errorf(`expected_versions for entry #%d must be greater than zero`, entryID)
			}
		}
	}

	return nil
}

// ValidateEntryStatus makes sure the entry status is valid.
func ValidateEntryStatus(status string) error {
	switch status {
	case model.EntryStatusRead, model.EntryStatusUnread:
		return nil
	}

	return fmt.Errorf(`invalid entry status, valid status values are: %q and %q`, model.EntryStatusRead, model.EntryStatusUnread)
}

// ValidateEntryOrder makes sure the sorting order is valid.
func ValidateEntryOrder(order string) error {
	switch order {
	case "id", "status", "changed_at", "published_at", "created_at", "category_title", "category_id", "title", "author":
		return nil
	}

	return errors.New(`invalid entry order, valid order values are: "id", "status", "changed_at", "published_at", "created_at", "category_title", "category_id", "title", "author"`)
}

// ValidateEntryModification makes sure the entry modification is valid.
func ValidateEntryModification(request *model.EntryUpdateRequest) error {
	if request.Title != nil && *request.Title == "" {
		return errors.New(`the entry title cannot be empty`)
	}

	if request.Content != nil && *request.Content == "" {
		return errors.New(`the entry content cannot be empty`)
	}

	return nil
}
