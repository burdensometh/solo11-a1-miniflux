// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rewrite // import "miniflux.app/v2/internal/reader/rewrite"

import (
	"log/slog"
	"regexp"
	"strings"
	"text/scanner"

	"miniflux.app/v2/internal/model"
)

// urlRewriteRuleName is the only supported URL rewrite rule name.
const urlRewriteRuleName = "rewrite"

// urlRewriteRuleShape encodes the exact shape of a well-formed URL rewrite
// rule, anchored to the whole string: rewrite("search-regex"|"replacement")
// with no whitespace tolerated anywhere and both quoted terms non-empty. It is
// the structural gate the parser checks before tokenizing, so the grammar
// accepted at save time stays identical to what was historically applied.
var urlRewriteRuleShape = regexp.MustCompile(`^rewrite\("([^"]+)"\|"([^"]+)"\)$`)

// URLRewriteRule is the typed representation of a URL rewrite rule, parsed
// once from the stored rewrite("search-regex"|"replacement") string. Both
// save-time validation (IsValidURLRewriteRules) and feed-time application
// (RewriteEntryURL) use the object produced by ParseURLRewriteRule, so the
// rule grammar and its arguments are interpreted in exactly one place.
type URLRewriteRule struct {
	Search      string
	Replacement string

	// regexp is the compiled Search expression; parse failures are reported as
	// errors instead of being stored here.
	regexp *regexp.Regexp
}

// ParseURLRewriteRule parses a URL rewrite rule string.
//
// An empty or whitespace-only string means "no rewriting" and returns a nil
// rule with no error. The only supported form is
// rewrite("search-regex"|"replacement") with no surrounding whitespace, the
// search term must be a compilable regular expression, the replacement term
// must be non-empty, and exactly one rule is allowed. Anything else returns
// errors describing the problems found.
func ParseURLRewriteRule(rulesText string) (*URLRewriteRule, []RuleError) {
	if strings.TrimSpace(rulesText) == "" {
		return nil, nil
	}

	parts := urlRewriteRuleShape.FindStringSubmatch(rulesText)
	if len(parts) != 3 {
		return nil, []RuleError{{Kind: RuleErrLexical, Rule: urlRewriteRuleName,
			Token:   rulesText,
			Message: `URL rewrite rules must have the form rewrite("search-regex"|"replacement"), with non-empty quoted terms and no extra whitespace`}}
	}

	// Re-tokenize the structurally valid string through the shared tokenizer so
	// the typed object's argument values come from the same lexical machinery
	// as content rewrite rules. The shape check above already guarantees the
	// tokens are one "rewrite" identifier followed by two raw string arguments.
	//
	// In raw mode the scanner may emit lexical errors for regex backslashes
	// such as "\."; those are legitimate inside a URL rule and are therefore
	// ignored here. The shape gate decides structure, and regexp.Compile below
	// decides whether the search term is a valid regular expression.
	tokens, _ := tokenizeRuleText(rulesText, true)
	if len(tokens) != 3 || tokens[0].kind != scanner.Ident || tokens[0].text != urlRewriteRuleName ||
		tokens[1].kind != scanner.String || tokens[2].kind != scanner.String {
		return nil, []RuleError{{Kind: RuleErrLexical, Rule: urlRewriteRuleName,
			Token: rulesText, Message: "malformed URL rewrite rule"}}
	}

	search, replacement := tokens[1].text, tokens[2].text
	compiled, err := regexp.Compile(search)
	if err != nil {
		return nil, []RuleError{{Kind: RuleErrInvalidRegexp, Rule: urlRewriteRuleName,
			Token: search, Message: err.Error()}}
	}

	return &URLRewriteRule{Search: search, Replacement: replacement, regexp: compiled}, nil
}

// IsValidURLRewriteRules reports whether the given URL rewrite rules string is
// something RewriteEntryURL can actually apply. An empty or whitespace-only
// string is valid and means "no rewriting".
//
// It uses the same parser as RewriteEntryURL, so a rule accepted here is
// guaranteed to be applicable later instead of being silently ignored during
// feed refresh.
func IsValidURLRewriteRules(rules string) bool {
	_, errs := ParseURLRewriteRule(rules)
	return len(errs) == 0
}

func RewriteEntryURL(feed *model.Feed, entry *model.Entry) string {
	if feed.UrlRewriteRules == "" {
		return entry.URL
	}

	parsedRule, errs := ParseURLRewriteRule(feed.UrlRewriteRules)
	if len(errs) > 0 {
		// Malformed rules are rejected at save time, but this path also runs
		// previously-stored rules, which are warn-only for backward
		// compatibility: keep the URL unchanged instead of failing the refresh.
		slog.Debug("Cannot find search and replace terms for replace rule",
			slog.String("original_entry_url", entry.URL),
			slog.String("rewritten_entry_url", entry.URL),
			slog.Int64("feed_id", feed.ID),
			slog.String("feed_url", feed.FeedURL),
			slog.String("url_rewrite_rules", feed.UrlRewriteRules),
		)
		return entry.URL
	}
	if parsedRule == nil {
		return entry.URL
	}

	rewrittenURL := parsedRule.regexp.ReplaceAllString(entry.URL, parsedRule.Replacement)
	slog.Debug("Rewriting entry URL",
		slog.String("original_entry_url", entry.URL),
		slog.String("rewritten_entry_url", rewrittenURL),
		slog.Int64("feed_id", feed.ID),
		slog.String("feed_url", feed.FeedURL),
	)

	return rewrittenURL
}
