// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"database/sql"
	"sync"
	"time"

	"github.com/lib/pq"

	"miniflux.app/v2/internal/model"
)

const (
	iconCacheTTL        = 24 * time.Hour
	iconCacheMaxEntries = 4096
)

// cachedIcon is an immutable icon row keyed by its external identifier. The
// external identifier is generated once per icons row and never reused for
// different content, so the mapping stays valid until the row is deleted.
type cachedIcon struct {
	icon      *model.Icon
	expiresAt time.Time
}

// iconCache avoids repeating the icon lookup for every entry rendered on a
// list page. The same icon is referenced by every entry of a feed, and list
// pages request one icon per row.
type iconCache struct {
	mu      sync.Mutex
	entries map[string]cachedIcon
}

func newIconCache() *iconCache {
	return &iconCache{entries: make(map[string]cachedIcon)}
}

func cloneIcon(icon *model.Icon) *model.Icon {
	if icon == nil {
		return nil
	}
	cloned := *icon
	cloned.Content = append([]byte(nil), icon.Content...)
	return &cloned
}

func (c *iconCache) get(key string) (*model.Icon, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, exists := c.entries[key]
	if !exists || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return cloneIcon(entry.icon), true
}

func (c *iconCache) contains(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, exists := c.entries[key]
	return exists && !time.Now().After(entry.expiresAt)
}

func (c *iconCache) put(key string, icon *model.Icon) {
	if key == "" || icon == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[key]; !exists && len(c.entries) >= iconCacheMaxEntries {
		// The cache is only an optimization. Dropping an arbitrary entry
		// keeps memory bounded without affecting correctness: a miss falls
		// back to the database.
		for arbitraryKey := range c.entries {
			delete(c.entries, arbitraryKey)
			break
		}
	}

	c.entries[key] = cachedIcon{icon: cloneIcon(icon), expiresAt: time.Now().Add(iconCacheTTL)}
}

func (c *iconCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
}

// collectMissingIDs drops empty, duplicate, and already-cached identifiers
// while preserving the order of first appearance.
func (c *iconCache) collectMissingIDs(externalIconIDs []string) []string {
	missing := make([]string, 0, len(externalIconIDs))
	seen := make(map[string]struct{}, len(externalIconIDs))
	for _, externalIconID := range externalIconIDs {
		if externalIconID == "" {
			continue
		}
		if _, exists := seen[externalIconID]; exists {
			continue
		}
		seen[externalIconID] = struct{}{}
		if c.contains(externalIconID) {
			continue
		}
		missing = append(missing, externalIconID)
	}
	return missing
}

// load returns the icon for the given external identifier. On a cache miss it
// runs fetch once and caches the result; errors and missing icons are not
// cached, so transient failures keep their previous semantics.
func (c *iconCache) load(externalIconID string, fetch func() (*model.Icon, error)) (*model.Icon, error) {
	if icon, ok := c.get(externalIconID); ok {
		return icon, nil
	}

	icon, err := fetch()
	if err == nil && icon != nil {
		c.put(icon.ExternalID, icon)
	}
	return icon, err
}

// preload loads all the given icons in a single query. Identifiers that are
// already cached are skipped. It is safe to call preload concurrently with
// load: a miss simply falls back to the per-icon query.
func (c *iconCache) preload(db *sql.DB, externalIconIDs []string) error {
	if len(externalIconIDs) == 0 {
		return nil
	}

	missing := c.collectMissingIDs(externalIconIDs)
	if len(missing) == 0 {
		return nil
	}

	query := `
		SELECT
			id,
			hash,
			mime_type,
			content,
			external_id
		FROM
			icons
		WHERE
			external_id = ANY($1)
	`

	rows, err := db.Query(query, pq.Array(missing))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var icon model.Icon
		if err := rows.Scan(&icon.ID, &icon.Hash, &icon.MimeType, &icon.Content, &icon.ExternalID); err != nil {
			return err
		}
		c.put(icon.ExternalID, &icon)
	}

	return rows.Err()
}
