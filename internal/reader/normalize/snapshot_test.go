// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package normalize_test

import (
	"bytes"
	"embed"
	encjson "encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/reader/atom"
	jsonfeed "miniflux.app/v2/internal/reader/json"
	"miniflux.app/v2/internal/reader/rdf"
	"miniflux.app/v2/internal/reader/rss"
)

//go:embed samples/*
var sampleFS embed.FS

const baseURL = "https://example.org/blog/feed"

type enclosureSnapshot struct {
	URL      string `json:"url"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

type entrySnapshot struct {
	URL         string              `json:"url"`
	Title       string              `json:"title"`
	Author      string              `json:"author"`
	Content     string              `json:"content"`
	Hash        string              `json:"hash"`
	CommentsURL string              `json:"comments_url"`
	Language    string              `json:"language"`
	ReadingTime int                 `json:"reading_time"`
	Tags        []string            `json:"tags"`
	Date        string              `json:"date"`
	DateIsNow   bool                `json:"date_is_now"`
	Enclosures  []enclosureSnapshot `json:"enclosures"`
}

type feedSnapshot struct {
	Format      string          `json:"format"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	FeedURL     string          `json:"feed_url"`
	SiteURL     string          `json:"site_url"`
	Language    string          `json:"language"`
	IconURL     string          `json:"icon_url"`
	TTLMinutes  int64           `json:"ttl_minutes"`
	HasTTL      bool            `json:"has_ttl"`
	Entries     []entrySnapshot `json:"entries"`
}

// parseSample routes a sample file to the matching format parser.
func parseSample(t *testing.T, name string, data []byte) *model.Feed {
	t.Helper()
	reader := bytes.NewReader(data)
	switch {
	case strings.HasPrefix(name, "rss_"):
		feed, err := rss.Parse(baseURL, reader)
		if err != nil {
			t.Fatalf("rss parse error: %v", err)
		}
		return feed
	case strings.HasPrefix(name, "atom10_"):
		feed, err := atom.Parse(baseURL, reader, "1.0")
		if err != nil {
			t.Fatalf("atom 1.0 parse error: %v", err)
		}
		return feed
	case strings.HasPrefix(name, "atom03_"):
		feed, err := atom.Parse(baseURL, reader, "0.3")
		if err != nil {
			t.Fatalf("atom 0.3 parse error: %v", err)
		}
		return feed
	case strings.HasPrefix(name, "json_"):
		feed, err := jsonfeed.Parse(baseURL, reader)
		if err != nil {
			t.Fatalf("json parse error: %v", err)
		}
		return feed
	case strings.HasPrefix(name, "rdf_"):
		feed, err := rdf.Parse(baseURL, reader)
		if err != nil {
			t.Fatalf("rdf parse error: %v", err)
		}
		return feed
	default:
		t.Fatalf("unknown sample family: %s", name)
		return nil
	}
}

func snapshotFeed(feed *model.Feed, format string) feedSnapshot {
	snap := feedSnapshot{
		Format:      format,
		Title:       feed.Title,
		Description: feed.Description,
		FeedURL:     feed.FeedURL,
		SiteURL:     feed.SiteURL,
		Language:    feed.Language,
		IconURL:     feed.IconURL,
		TTLMinutes:  int64(feed.TTL / time.Minute),
		HasTTL:      feed.TTL != 0,
		Entries:     make([]entrySnapshot, 0, len(feed.Entries)),
	}

	for _, entry := range feed.Entries {
		enclosures := make([]enclosureSnapshot, 0, len(entry.Enclosures))
		for _, enclosure := range entry.Enclosures {
			enclosures = append(enclosures, enclosureSnapshot{
				URL:      enclosure.URL,
				MimeType: enclosure.MimeType,
				Size:     enclosure.Size,
			})
		}

		tags := entry.Tags
		if tags == nil {
			tags = []string{}
		}

		// Dates produced via the time.Now() fallback sit within a few minutes
		// of the test run; mark them explicitly instead of serializing a value
		// that changes on every run. Parsed feed dates are always far in the
		// past or future relative to the run date.
		dateIsNow := !entry.Date.IsZero() && time.Since(entry.Date).Abs() <= 5*time.Minute
		dateValue := entry.Date.UTC().Format(time.RFC3339Nano)
		if dateIsNow {
			dateValue = "<now>"
		}

		snap.Entries = append(snap.Entries, entrySnapshot{
			URL:         entry.URL,
			Title:       entry.Title,
			Author:      entry.Author,
			Content:     entry.Content,
			Hash:        entry.Hash,
			CommentsURL: entry.CommentsURL,
			Language:    entry.Language,
			ReadingTime: entry.ReadingTime,
			Tags:        tags,
			Date:        dateValue,
			DateIsNow:   dateIsNow,
			Enclosures:  enclosures,
		})
	}

	return snap
}

func listSamples(t *testing.T) []string {
	t.Helper()
	entries, err := sampleFS.ReadDir("samples")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

func formatOf(name string) string {
	switch {
	case strings.HasPrefix(name, "rss_"):
		return "rss"
	case strings.HasPrefix(name, "atom10_"):
		return "atom10"
	case strings.HasPrefix(name, "atom03_"):
		return "atom03"
	case strings.HasPrefix(name, "json_"):
		return "json"
	case strings.HasPrefix(name, "rdf_"):
		return "rdf"
	default:
		return "unknown"
	}
}

// TestGoldenSnapshots compares the full normalized output of every sample
// against the golden files under testdata/golden. Set UPDATE_GOLDEN=1 to
// regenerate them.
func TestGoldenSnapshots(t *testing.T) {
	for _, name := range listSamples(t) {
		t.Run(name, func(t *testing.T) {
			data, err := sampleFS.ReadFile(filepath.Join("samples", name))
			if err != nil {
				t.Fatal(err)
			}

			feed := parseSample(t, name, data)
			got := snapshotFeed(feed, formatOf(name))
			gotJSON, err := encjson.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}

			goldenPath := filepath.Join("testdata", "golden", name+".golden.json")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenPath, append(gotJSON, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			wantJSON, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("missing golden file %s (run with UPDATE_GOLDEN=1): %v", goldenPath, err)
			}

			if string(wantJSON) != string(gotJSON)+"\n" {
				t.Fatalf("snapshot mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", name, wantJSON, gotJSON)
			}
		})
	}
}
