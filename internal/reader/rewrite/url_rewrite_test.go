// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rewrite // import "miniflux.app/v2/internal/reader/rewrite"

import (
	"regexp"
	"strings"
	"testing"

	"miniflux.app/v2/internal/model"
)

func TestRewriteEntryURL(t *testing.T) {
	scenarios := []struct {
		name        string
		feed        *model.Feed
		entry       *model.Entry
		expectedURL string
		description string
	}{
		{
			name: "NoRewriteRules",
			feed: &model.Feed{
				ID:              1,
				FeedURL:         "https://example.com/feed.xml",
				UrlRewriteRules: "",
			},
			entry: &model.Entry{
				URL: "https://example.com/article/123",
			},
			expectedURL: "https://example.com/article/123",
			description: "Should return original URL when no rewrite rules are specified",
		},
		{
			name: "EmptyRewriteRules",
			feed: &model.Feed{
				ID:              1,
				FeedURL:         "https://example.com/feed.xml",
				UrlRewriteRules: "   ",
			},
			entry: &model.Entry{
				URL: "https://example.com/article/123",
			},
			expectedURL: "https://example.com/article/123",
			description: "Should return original URL when rewrite rules are empty/whitespace",
		},
		{
			name: "ValidRewriteRule",
			feed: &model.Feed{
				ID:              1,
				FeedURL:         "https://example.com/feed.xml",
				UrlRewriteRules: `rewrite("^https://example.com/article/(.+)"|"https://example.com/full-article/$1")`,
			},
			entry: &model.Entry{
				URL: "https://example.com/article/123",
			},
			expectedURL: "https://example.com/full-article/123",
			description: "Should rewrite URL according to the regex pattern",
		},
		{
			name: "ComplexRegexRewrite",
			feed: &model.Feed{
				ID:              1,
				FeedURL:         "https://news.ycombinator.com/rss",
				UrlRewriteRules: `rewrite("^https://news\.ycombinator\.com/item\?id=(.+)"|"https://hn.algolia.com/api/v1/items/$1")`,
			},
			entry: &model.Entry{
				URL: "https://news.ycombinator.com/item?id=12345",
			},
			expectedURL: "https://hn.algolia.com/api/v1/items/12345",
			description: "Should handle complex regex patterns with escaped characters",
		},
		{
			name: "NoMatchingPattern",
			feed: &model.Feed{
				ID:              1,
				FeedURL:         "https://example.com/feed.xml",
				UrlRewriteRules: `rewrite("^https://different.com/(.+)"|"https://rewritten.com/$1")`,
			},
			entry: &model.Entry{
				URL: "https://example.com/article/123",
			},
			expectedURL: "https://example.com/article/123",
			description: "Should return original URL when regex pattern doesn't match",
		},
		{
			name: "InvalidRegexPattern",
			feed: &model.Feed{
				ID:              1,
				FeedURL:         "https://example.com/feed.xml",
				UrlRewriteRules: `rewrite("^https://example.com/[invalid"|"https://rewritten.com/$1")`,
			},
			entry: &model.Entry{
				URL: "https://example.com/article/123",
			},
			expectedURL: "https://example.com/article/123",
			description: "Should return original URL when regex pattern is invalid",
		},
		{
			name: "MalformedRewriteRule",
			feed: &model.Feed{
				ID:              1,
				FeedURL:         "https://example.com/feed.xml",
				UrlRewriteRules: `rewrite("invalid format")`,
			},
			entry: &model.Entry{
				URL: "https://example.com/article/123",
			},
			expectedURL: "https://example.com/article/123",
			description: "Should return original URL when rewrite rule format is malformed",
		},
		{
			name: "MultipleGroups",
			feed: &model.Feed{
				ID:              1,
				FeedURL:         "https://example.com/feed.xml",
				UrlRewriteRules: `rewrite("^https://example.com/([^/]+)/article/(.+)"|"https://example.com/full/$1/story/$2")`,
			},
			entry: &model.Entry{
				URL: "https://example.com/tech/article/ai-news",
			},
			expectedURL: "https://example.com/full/tech/story/ai-news",
			description: "Should handle multiple capture groups in regex",
		},
		{
			name: "URLWithSpecialCharacters",
			feed: &model.Feed{
				ID:              1,
				FeedURL:         "https://example.com/feed.xml",
				UrlRewriteRules: `rewrite("^https://example.com/(.+)"|"https://proxy.example.com/$1")`,
			},
			entry: &model.Entry{
				URL: "https://example.com/article/test?param=value&other=123#section",
			},
			expectedURL: "https://proxy.example.com/article/test?param=value&other=123#section",
			description: "Should handle URLs with query parameters and fragments",
		},
		{
			name: "ReplaceWithStaticURL",
			feed: &model.Feed{
				ID:              1,
				FeedURL:         "https://example.com/feed.xml",
				UrlRewriteRules: `rewrite("^https://example.com/(.+)"|"https://static.example.com/reader")`,
			},
			entry: &model.Entry{
				URL: "https://example.com/article/123",
			},
			expectedURL: "https://static.example.com/reader",
			description: "Should replace with static URL when no capture groups are used in replacement",
		},
		{
			name: "EmptyReplacementString",
			feed: &model.Feed{
				ID:              1,
				FeedURL:         "https://example.com/feed.xml",
				UrlRewriteRules: `rewrite("^https://example.com/(.+)"|"x")`,
			},
			entry: &model.Entry{
				URL: "https://example.com/article/123",
			},
			expectedURL: "x",
			description: "Should replace with specified string",
		},
		{
			name: "EmptyReplacementNotSupported",
			feed: &model.Feed{
				ID:              1,
				FeedURL:         "https://example.com/feed.xml",
				UrlRewriteRules: `rewrite("^https://example.com/(.+)"|"")`,
			},
			entry: &model.Entry{
				URL: "https://example.com/article/123",
			},
			expectedURL: "https://example.com/article/123",
			description: "Should return original URL when replacement is empty string (not supported by regex pattern)",
		},
		{
			name: "InvalidRewriteRuleFormat",
			feed: &model.Feed{
				ID:              1,
				FeedURL:         "https://example.com/feed.xml",
				UrlRewriteRules: `not-a-rewrite-rule`,
			},
			entry: &model.Entry{
				URL: "https://example.com/article/123",
			},
			expectedURL: "https://example.com/article/123",
			description: "Should return original URL when rewrite rule doesn't match expected format",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			result := RewriteEntryURL(scenario.feed, scenario.entry)
			if result != scenario.expectedURL {
				t.Errorf("Expected URL %q, got %q. Description: %s", scenario.expectedURL, result, scenario.description)
			}
		})
	}
}

func TestRewriteEntryURLWithNilValues(t *testing.T) {
	t.Run("NilFeed", func(t *testing.T) {
		entry := &model.Entry{URL: "https://example.com/article/123"}

		// This should panic or handle gracefully - let's see what happens
		defer func() {
			if r := recover(); r == nil {
				t.Error("Expected panic when feed is nil, but function completed normally")
			}
		}()

		RewriteEntryURL(nil, entry)
	})

	t.Run("NilEntry", func(t *testing.T) {
		feed := &model.Feed{
			ID:              1,
			FeedURL:         "https://example.com/feed.xml",
			UrlRewriteRules: `rewrite("^https://example.com/(.+)"|"https://rewritten.com/$1")`,
		}

		// This should panic or handle gracefully - let's see what happens
		defer func() {
			if r := recover(); r == nil {
				t.Error("Expected panic when entry is nil, but function completed normally")
			}
		}()

		RewriteEntryURL(feed, nil)
	})
}

func TestParseURLRewriteRule(t *testing.T) {
	scenarios := []struct {
		name        string
		input       string
		expectError bool
		want        *URLRewriteRule
	}{
		{
			name:  "ValidRule",
			input: `rewrite("^https://example.com/(.+)"|"https://rewritten.com/$1")`,
			want: &URLRewriteRule{
				Search:      `^https://example.com/(.+)`,
				Replacement: `https://rewritten.com/$1`,
			},
		},
		{
			// The historical regex-based grammar captured the search term
			// verbatim without interpreting backslash escapes, so double
			// backslashes in the input are preserved in the typed rule.
			name:  "ValidRuleWithEscapedCharacters",
			input: `rewrite("^https://news\\.ycombinator\\.com/item\\?id=(.+)"|"https://hn.algolia.com/api/v1/items/$1")`,
			want: &URLRewriteRule{
				Search:      `^https://news\\.ycombinator\\.com/item\\?id=(.+)`,
				Replacement: `https://hn.algolia.com/api/v1/items/$1`,
			},
		},
		{
			name:        "EmptyString",
			input:       ``,
			expectError: false,
			want:        nil,
		},
		{
			name:        "WhitespaceOnly",
			input:       `   `,
			expectError: false,
			want:        nil,
		},
		{
			name:        "InvalidFormat",
			input:       `rewrite("invalid")`,
			expectError: true,
		},
		{
			name:        "RandomText",
			input:       `some random text`,
			expectError: true,
		},
		{
			name:        "UnknownRuleName",
			input:       `not_a_function("find"|"replace")`,
			expectError: true,
		},
		{
			name:        "BareRegex",
			input:       `^https://example.com/(.+)$`,
			expectError: true,
		},
		{
			name:        "InvalidSearchRegex",
			input:       `rewrite("^https://example.com/[invalid"|"https://rewritten.com/$1")`,
			expectError: true,
		},
		{
			name:        "EmptyReplacementRejected",
			input:       `rewrite("^https://example.com/(.+)"|"")`,
			expectError: true,
		},
		{
			name:        "MoreThanOneRuleRejected",
			input:       `rewrite("a"|"b") rewrite("c"|"d")`,
			expectError: true,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			got, errs := ParseURLRewriteRule(scenario.input)
			if scenario.expectError {
				if len(errs) == 0 {
					t.Fatalf("Expected a parse error for %q, got rule %+v", scenario.input, got)
				}
				return
			}
			if len(errs) != 0 {
				t.Fatalf("Expected no parse errors for %q, got %v", scenario.input, errs)
			}
			if scenario.want == nil {
				if got != nil {
					t.Fatalf("Expected nil rule for %q, got %+v", scenario.input, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("Expected a parsed rule for %q, got nil", scenario.input)
			}
			if got.Search != scenario.want.Search || got.Replacement != scenario.want.Replacement {
				t.Errorf("Parsed rule mismatch for %q: got {Search: %q, Replacement: %q}, want {Search: %q, Replacement: %q}",
					scenario.input, got.Search, got.Replacement, scenario.want.Search, scenario.want.Replacement)
			}
			if got.regexp == nil {
				t.Errorf("Expected the parsed rule to carry a compiled regexp, got nil for %q", scenario.input)
			}
		})
	}
}

func TestURLRewriteValidationAndApplicationShareTheSameParser(t *testing.T) {
	// Every input must be classified identically by save-time validation and
	// by the refresh-time application path: a rule accepted by
	// IsValidURLRewriteRules must parse into a reusable object, and a rejected
	// one must leave the entry URL untouched.
	inputs := []string{
		``,
		`   `,
		`rewrite("^https://example.com/(.+)"|"https://rewritten.com/$1")`,
		`rewrite("^https://news\\.ycombinator\\.com/item\\?id=(.+)"|"https://hn.algolia.com/api/v1/items/$1")`,
		`rewrite("invalid")`,
		`some random text`,
		`not_a_function("find"|"replace")`,
		`^https://example.com/(.+)$`,
		`rewrite("^https://example.com/[invalid"|"https://rewritten.com/$1")`,
		`rewrite("^https://example.com/(.+)"|"")`,
		` rewrite("a"|"b")`,
		`rewrite("a"|"b") `,
	}

	for _, rulesText := range inputs {
		valid := IsValidURLRewriteRules(rulesText)
		parsed, errs := ParseURLRewriteRule(rulesText)

		if valid != (len(errs) == 0) {
			t.Errorf("Validation/parse divergence for %q: IsValidURLRewriteRules=%v but parser errors=%v", rulesText, valid, errs)
		}

		feed := &model.Feed{ID: 1, FeedURL: "https://example.com/feed.xml", UrlRewriteRules: rulesText}
		entry := &model.Entry{URL: "https://example.com/article/123"}
		result := RewriteEntryURL(feed, entry)

		if !valid && result != entry.URL {
			t.Errorf("Invalid rule %q was applied: URL changed to %q", rulesText, result)
		}
		if valid && parsed != nil {
			want := parsed.regexp.ReplaceAllString(entry.URL, parsed.Replacement)
			if result != want {
				t.Errorf("Applied URL %q does not match the parsed rule result %q for input %q", result, want, rulesText)
			}
		}
	}
}

func TestIsValidURLRewriteRules(t *testing.T) {
	scenarios := []struct {
		name  string
		rules string
		valid bool
	}{
		{name: "Empty", rules: ``, valid: true},
		{name: "WhitespaceOnly", rules: `   `, valid: true},
		{name: "WellFormed", rules: `rewrite("^https://example.com/(.+)"|"https://rewritten.com/$1")`, valid: true},
		{name: "WellFormedEmptyReplacement", rules: `rewrite("^https://example.com/(.+)"|"x")`, valid: true},
		// The reported bug: a valid regex that is not a URL rewrite rule.
		{name: "ValidRegexButNotARule", rules: `not_a_function("find"|"replace")`, valid: false},
		{name: "BareRegex", rules: `^https://example.com/(.+)$`, valid: false},
		{name: "PlainText", rules: `not-a-rewrite-rule`, valid: false},
		{name: "MissingReplacement", rules: `rewrite("invalid format")`, valid: false},
		{name: "LeadingWhitespaceBreaksRule", rules: ` rewrite("a"|"b")`, valid: false},
		{name: "TrailingWhitespaceBreaksRule", rules: `rewrite("a"|"b") `, valid: false},
		// Matches the rewrite() form but the search term is not a compilable regex.
		{name: "InvalidSearchRegex", rules: `rewrite("^https://example.com/[invalid"|"https://rewritten.com/$1")`, valid: false},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			if got := IsValidURLRewriteRules(scenario.rules); got != scenario.valid {
				t.Errorf("IsValidURLRewriteRules(%q) = %v, want %v", scenario.rules, got, scenario.valid)
			}
		})
	}
}

// legacyURLRewriteRuleRegex is the pre-refactor parser, kept in this test as an
// oracle to prove the typed parser preserves the exact historical grammar.
var legacyURLRewriteRuleRegex = regexp.MustCompile(`^rewrite\("([^"]+)"\|"([^"]+)"\)$`)

func legacyIsValid(rules string) bool {
	if strings.TrimSpace(rules) == "" {
		return true
	}
	parts := legacyURLRewriteRuleRegex.FindStringSubmatch(rules)
	if len(parts) != 3 {
		return false
	}
	_, err := regexp.Compile(parts[1])
	return err == nil
}

func legacyRewrite(rules, entryURL string) string {
	parts := legacyURLRewriteRuleRegex.FindStringSubmatch(rules)
	if len(parts) != 3 {
		return entryURL
	}
	re, err := regexp.Compile(parts[1])
	if err != nil {
		return entryURL
	}
	return re.ReplaceAllString(entryURL, parts[2])
}

func TestURLRewriteParserMatchesLegacyGrammar(t *testing.T) {
	corpus := []string{
		"",
		"   ",
		"\t\n",
		`rewrite("a"|"b")`,
		` rewrite("a"|"b")`,
		`rewrite("a"|"b") `,
		`rewrite("a"|"b")rewrite("c"|"d")`,
		`rewrite("a"|"b") rewrite("c"|"d")`,
		`rewrite("a")`,
		`rewrite()`,
		`rewrite("a"|"")`,
		`rewrite(""|"b")`,
		`rewrite("^https://news\.ycombinator\.com/item\?id=(.+)"|"https://hn/$1")`,
		`rewrite("^https://news\\.ycombinator\\.com/item\\?id=(.+)"|"https://hn/$1")`,
		`rewrite("["|"x")`,
		`rewrite("a"|"b"|"c")`,
		`not_a_function("a"|"b")`,
		`^https://example.com/(.+)$`,
		`not-a-rewrite-rule`,
		`rewrite("invalid format")`,
		`rewrite("a|b"|"c")`,
		`rewrite("a\"b"|"c")`,
		`rewrite("a"b"|"c")`,
		`rewrite("unclosed|"b")`,
		`REWRITE("a"|"b")`,
		`rewrite ("a"|"b")`,
		`rewrite("http://(.*)" | "https://$1")`,
		`rewrite("(.*)"|"$1?utm_source=x")`,
		`rewrite("x"|"y"),rewrite("z"|"w")`,
	}
	urls := []string{
		"https://example.com/article/123",
		"https://news.ycombinator.com/item?id=12345",
		"a",
		"",
	}

	for _, rulesText := range corpus {
		gotValid := IsValidURLRewriteRules(rulesText)
		wantValid := legacyIsValid(rulesText)
		if gotValid != wantValid {
			t.Errorf("validation mismatch for %q: new=%v legacy=%v", rulesText, gotValid, wantValid)
		}

		parsed, parseErrs := ParseURLRewriteRule(rulesText)
		if (len(parseErrs) == 0) != wantValid {
			t.Errorf("parser error mismatch for %q: errs=%v legacy validity=%v", rulesText, parseErrs, wantValid)
		}

		for _, u := range urls {
			feed := &model.Feed{ID: 1, FeedURL: "https://example.com/feed.xml", UrlRewriteRules: rulesText}
			entry := &model.Entry{URL: u}
			got := RewriteEntryURL(feed, entry)
			want := legacyRewrite(rulesText, u)
			if got != want {
				t.Errorf("rewrite mismatch for rules=%q url=%q: new=%q legacy=%q", rulesText, u, got, want)
			}
		}

		// When both accept the rule, the typed object's fields must equal the
		// legacy capture groups verbatim.
		if wantValid && parsed != nil {
			parts := legacyURLRewriteRuleRegex.FindStringSubmatch(rulesText)
			if parsed.Search != parts[1] || parsed.Replacement != parts[2] {
				t.Errorf("typed fields mismatch for %q: got={%q,%q} legacy={%q,%q}",
					rulesText, parsed.Search, parsed.Replacement, parts[1], parts[2])
			}
		}
	}
}
