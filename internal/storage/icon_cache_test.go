// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"strconv"
	"testing"
	"time"

	"miniflux.app/v2/internal/model"
)

func TestIconCacheInMemoryBehavior(t *testing.T) {
	cache := newIconCache()
	cache.put("dup", &model.Icon{ID: 1, ExternalID: "dup", Content: []byte("v1")})

	if !cache.contains("dup") {
		t.Fatal("expected cache to contain the inserted icon")
	}
	if cache.contains("missing") {
		t.Fatal("unexpected cache hit")
	}

	got, ok := cache.get("dup")
	if !ok || string(got.Content) != "v1" {
		t.Fatalf("unexpected cached value: %+v %v", got, ok)
	}

	// Callers receive independent copies, so mutating a returned icon must
	// not poison subsequent lookups.
	got.Content[0] = 'x'
	again, ok := cache.get("dup")
	if !ok || string(again.Content) != "v1" {
		t.Fatal("expected callers to receive independent copies")
	}

	// Empty keys and nil icons are never cached.
	cache.put("", &model.Icon{ExternalID: ""})
	cache.put("nil", nil)
	if cache.contains("") || cache.contains("nil") {
		t.Fatal("expected empty keys and nil icons to be ignored")
	}

	cache.clear()
	if cache.contains("dup") {
		t.Fatal("expected cache clear to remove entries")
	}
}

func TestIconCacheBoundedSize(t *testing.T) {
	cache := newIconCache()

	for i := 0; i < iconCacheMaxEntries+10; i++ {
		cache.put(externalID(i), &model.Icon{ID: int64(i), ExternalID: externalID(i)})
	}

	if len(cache.entries) > iconCacheMaxEntries {
		t.Fatalf("cache size %d exceeds cap %d", len(cache.entries), iconCacheMaxEntries)
	}
}

func externalID(i int) string {
	return "icon-" + strconv.Itoa(i)
}

func TestIconCachePreloadSkipsEmptyAndDuplicates(t *testing.T) {
	cache := newIconCache()

	missing := cache.collectMissingIDs([]string{"a", "a", "", "b", "a"})
	if len(missing) != 2 || missing[0] != "a" || missing[1] != "b" {
		t.Fatalf("unexpected missing ids: %#v", missing)
	}

	cache.put("a", &model.Icon{ID: 1, ExternalID: "a"})
	missing = cache.collectMissingIDs([]string{"a", "b"})
	if len(missing) != 1 || missing[0] != "b" {
		t.Fatalf("expected cached id to be skipped, got %#v", missing)
	}
}

func TestIconCacheExpiry(t *testing.T) {
	cache := newIconCache()
	cache.entries["expired"] = cachedIcon{
		icon:      &model.Icon{ExternalID: "expired"},
		expiresAt: time.Now().Add(-time.Minute),
	}
	cache.entries["fresh"] = cachedIcon{
		icon:      &model.Icon{ExternalID: "fresh"},
		expiresAt: time.Now().Add(time.Minute),
	}

	if cache.contains("expired") {
		t.Fatal("expired entry must report as absent")
	}
	if _, ok := cache.get("expired"); ok {
		t.Fatal("expired entry must not be returned")
	}
	if !cache.contains("fresh") {
		t.Fatal("fresh entry must report as present")
	}
	if missing := cache.collectMissingIDs([]string{"expired", "fresh"}); len(missing) != 1 || missing[0] != "expired" {
		t.Fatalf("expired id should be treated as missing, got %#v", missing)
	}
}
