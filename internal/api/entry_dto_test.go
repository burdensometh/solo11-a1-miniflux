// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"miniflux.app/v2/internal/model"
)

func sampleEntry() *model.Entry {
	date := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	return &model.Entry{
		ID:          123,
		UserID:      7,
		FeedID:      9,
		Status:      model.EntryStatusUnread,
		Hash:        "deadbeef",
		Title:       "Hello world",
		URL:         "https://example.org/post",
		CommentsURL: "https://example.org/post/comments",
		Language:    "en",
		Date:        date,
		CreatedAt:   date.Add(time.Minute),
		ChangedAt:   date.Add(2 * time.Minute),
		Content:     `<p>Body <img src="https://example.org/a.png"> text</p>`,
		Author:      "Jane",
		ShareCode:   "share-1",
		Starred:     true,
		ReadingTime: 3,
		Enclosures: model.EnclosureList{
			{ID: 1, UserID: 7, EntryID: 123, URL: "https://example.org/audio.mp3", MimeType: "audio/mpeg", Size: 4096},
		},
		Feed: &model.Feed{ID: 9, Title: "Example feed", Icon: &model.FeedIcon{}},
		Tags: []string{"go", "api"},
	}
}

// TestV1EntryJSONMatchesLegacyModel is the regression guard for the v1
// contract: serializing through newV1Entry must produce exactly the same JSON
// as serializing model.Entry directly used to. If this test fails, a legacy
// client decoding the response into its rigid struct would break.
func TestV1EntryJSONMatchesLegacyModel(t *testing.T) {
	entry := sampleEntry()

	legacy, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal legacy model: %v", err)
	}

	frozen, err := json.Marshal(newV1Entry(entry))
	if err != nil {
		t.Fatalf("marshal v1 DTO: %v", err)
	}

	if !reflect.DeepEqual(legacy, frozen) {
		t.Fatalf("v1 DTO drift detected\nlegacy: %s\nfrozen: %s", legacy, frozen)
	}
}

// TestV1EnvelopeShapeLocksTheHistoricalContract locks the list envelope keys.
func TestV1EnvelopeShapeLocksTheHistoricalContract(t *testing.T) {
	payload, err := json.Marshal(&v1EntryResponse{Total: 1, Entries: []*v1Entry{newV1Entry(sampleEntry())}})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(payload, &keys); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	if len(keys) != 2 {
		t.Fatalf("v1 envelope must keep exactly the two historical keys, got: %v", mapKeys(keys))
	}
	if _, ok := keys["total"]; !ok {
		t.Fatal(`v1 envelope must keep the "total" key`)
	}
	if _, ok := keys["entries"]; !ok {
		t.Fatal(`v1 envelope must keep the "entries" key`)
	}
}

// TestV1EntryHasNoV2Fields proves the additive v2 fields never leak onto v1.
func TestV1EntryHasNoV2Fields(t *testing.T) {
	payload, err := json.Marshal(newV1Entry(sampleEntry()))
	if err != nil {
		t.Fatalf("marshal v1 DTO: %v", err)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(payload, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, forbidden := range []string{"entry_id", "external_url", "excerpt", "image_url", "has_media", "next_cursor", "has_more"} {
		if _, ok := keys[forbidden]; ok {
			t.Fatalf("v2-only field %q leaked into the v1 entry payload", forbidden)
		}
	}
}

func TestV2EntryAddsNewFieldsAndKeepsLegacyOnes(t *testing.T) {
	payload, err := json.Marshal(newV2Entry(sampleEntry()))
	if err != nil {
		t.Fatalf("marshal v2 DTO: %v", err)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(payload, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, required := range []string{
		// Historical fields kept verbatim.
		"id", "user_id", "feed_id", "status", "hash", "title", "url",
		"published_at", "created_at", "changed_at", "content", "author",
		"starred", "reading_time", "enclosures", "feed", "tags",
		// New v2 fields.
		"entry_id", "external_url", "excerpt", "has_media",
	} {
		if _, ok := keys[required]; !ok {
			t.Fatalf("v2 entry is missing field %q", required)
		}
	}
}

func TestV2ExcerptAndImageExtraction(t *testing.T) {
	v2 := newV2Entry(sampleEntry())

	if v2.ImageURL != "https://example.org/a.png" {
		t.Fatalf("unexpected image URL: %q", v2.ImageURL)
	}
	if v2.Excerpt == "" {
		t.Fatal("expected a plain-text excerpt derived from content")
	}
	if !v2.HasMedia {
		t.Fatal("expected the audio enclosure to set has_media")
	}
}

func TestExtractPreviewHandlesEmptyAndMalformedContent(t *testing.T) {
	if excerpt, image := extractPreview(""); excerpt != "" || image != "" {
		t.Fatalf("empty content should yield empty preview, got %q/%q", excerpt, image)
	}

	excerpt, _ := extractPreview("<p>one</p><script>x</script><p>two</p>")
	if excerpt != "onetwo" {
		t.Fatalf("unexpected excerpt: %q", excerpt)
	}
}

func TestV2EnvelopeOmitsTotalByDefaultAndCursorWhenNoMorePages(t *testing.T) {
	payload, err := json.Marshal(&v2EntryResponse{Entries: []*v2Entry{}, Limit: 100, HasMore: false})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(payload, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := keys["total"]; ok {
		t.Fatal(`"total" must be omitted unless with_total=true`)
	}
	if _, ok := keys["next_cursor"]; ok {
		t.Fatal(`"next_cursor" must be omitted when there is no next page`)
	}
	if _, ok := keys["has_more"]; !ok {
		t.Fatal(`"has_more" must always be present`)
	}
}

func mapKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
