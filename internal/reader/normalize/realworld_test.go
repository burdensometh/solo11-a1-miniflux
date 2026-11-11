// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package normalize_test

import (
	encjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"miniflux.app/v2/internal/reader/parser"
)

// realWorldSample pairs a parser testdata file with the base URL used when it
// was first snapshotted.
var realWorldSamples = map[string]string{
	"large_atom.xml":             "https://dustri.org/b",
	"large_rss.xml":              "https://dustri.org/b",
	"small_atom.xml":             "https://github.com/miniflux/v2/commits/main",
	"rdf_UTF8.xml":               "https://example.org/feed",
	"urdu_UTF8.xml":              "https://example.org/feed",
	"encoding_ISO-8859-1.xml":    "https://example.org/feed",
	"encoding_WINDOWS-1251.xml":  "https://example.org/feed",
	"no_encoding_ISO-8859-1.xml": "https://example.org/feed",
}

// TestRealWorldSnapshots drives the public ParseFeed entry point (format
// detection + decoding + the unified pipeline) with the parser package's
// real-world samples and compares the complete normalized output field by
// field. Set UPDATE_GOLDEN=1 to regenerate the golden files.
func TestRealWorldSnapshots(t *testing.T) {
	parserTestdata := filepath.Join("..", "parser", "testdata")

	for name, baseURL := range realWorldSamples {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(parserTestdata, name))
			if err != nil {
				t.Skipf("sample not available: %v", err)
			}

			feed, err := parser.ParseFeed(baseURL, strings.NewReader(string(data)))
			if err != nil {
				t.Fatalf("ParseFeed error: %v", err)
			}

			got := snapshotFeed(feed, "parser_testdata")
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
