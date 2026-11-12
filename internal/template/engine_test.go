// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package template // import "miniflux.app/v2/internal/template"

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/reader/preview"
)

// TestRenderConcurrency renders the same template concurrently in different
// languages. Because Render binds per-request, language-specific functions
// ("t", "plural", "elapsed") onto the template, doing so on a shared template
// while other requests execute it corrupts the output: a request can be served
// another request's language. Each concurrent render must match the output of
// the equivalent sequential render for its language.
func TestRenderConcurrency(t *testing.T) {
	engine := NewEngine("")
	engine.ParseTemplates()

	languages := []string{"en_US", "fr_FR", "de_DE", "es_ES", "pt_BR", "ru_RU", "zh_CN", "it_IT"}

	newData := func(language string) map[string]any {
		return map[string]any{"language": language, "theme": "system_serif"}
	}

	// Establish the expected output for each language sequentially.
	expected := make(map[string][]byte, len(languages))
	for _, language := range languages {
		expected[language] = engine.Render("offline.html", newData(language))
	}

	const iterations = 300

	var wg sync.WaitGroup
	var mu sync.Mutex
	mismatches := make(map[string]int)

	for i := 0; i < iterations; i++ {
		language := languages[i%len(languages)]
		wg.Add(1)
		go func(language string) {
			defer wg.Done()
			got := engine.Render("offline.html", newData(language))
			if !bytes.Equal(got, expected[language]) {
				mu.Lock()
				mismatches[language]++
				mu.Unlock()
			}
		}(language)
	}
	wg.Wait()

	if len(mismatches) > 0 {
		total := 0
		for _, n := range mismatches {
			total += n
		}
		t.Fatalf("concurrent Render produced wrong output for %d/%d requests (wrong-language translations); per-language mismatches: %v", total, iterations, mismatches)
	}
}

// TestChooseSubscriptionTemplateRendersPreviews exercises the feed preview
// blocks (entries, cached marker, error and empty states) of the candidate
// selection page.
func TestChooseSubscriptionTemplateRendersPreviews(t *testing.T) {
	previousOptions := config.Opts
	config.Opts = config.NewConfigOptions()
	t.Cleanup(func() { config.Opts = previousOptions })

	engine := NewEngine("")
	engine.ParseTemplates()

	subscriptionWithPreview := map[string]any{
		"Title": "Main feed",
		"URL":   "https://example.com/feed.xml",
		"Type":  "rss",
		"Preview": &preview.Preview{
			FromCache: true,
			LastBuild: time.Now(),
			Entries: []*preview.PreviewEntry{
				{Title: "First entry", URL: "https://example.com/1", Published: time.Now()},
			},
		},
	}
	subscriptionWithError := map[string]any{
		"Title":   "Broken feed",
		"URL":     "https://example.com/broken.xml",
		"Type":    "atom",
		"Preview": &preview.Preview{Error: "feed not found"},
	}
	subscriptionEmpty := map[string]any{
		"Title":   "Empty feed",
		"URL":     "https://example.com/empty.xml",
		"Type":    "rss",
		"Preview": &preview.Preview{},
	}

	data := map[string]any{
		"language":            "en_US",
		"theme":               "system_serif",
		"theme_checksum":      "test",
		"app_js_checksum":     "test",
		"sw_js_checksum":      "test",
		"webAuthnEnabled":     false,
		"csrf":                "token",
		"menu":                "feeds",
		"subscriptions":       []map[string]any{subscriptionWithPreview, subscriptionWithError, subscriptionEmpty},
		"form":                map[string]any{},
		"user":                &model.User{Username: "alice", Timezone: "UTC", DefaultHomePage: "unread", Stylesheet: "system_serif"},
		"countUnread":         0,
		"countErrorFeeds":     0,
		"categories":          model.Categories{},
		"feeds":               model.Feeds{},
		"flashSuccessMessage": "",
		"flashErrorMessage":   "",
	}

	output := engine.Render("choose_subscription.html", data)
	html := string(output)

	for _, expected := range []string{
		"Main feed",
		"Broken feed",
		"Empty feed",
		"First entry",
		"https://example.com/feed.xml",
		"(cached preview)",
		"Preview unavailable",
		"does not contain any entry",
	} {
		if !strings.Contains(html, expected) {
			t.Fatalf("Rendered template should contain %q", expected)
		}
	}
}
