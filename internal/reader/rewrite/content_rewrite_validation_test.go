// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rewrite // import "miniflux.app/v2/internal/reader/rewrite"

import (
	"testing"
)

func TestParseRulesInvalidEscape(t *testing.T) {
	rulesText := `replace("\d+"|"x")`
	_, errs := parseRules(rulesText)

	if len(errs) == 0 {
		t.Fatalf(`Expected at least one error, got none`)
	}

	var found bool
	for _, e := range errs {
		if e.Kind == RuleErrUnquote || e.Kind == RuleErrLexical {
			found = true
		}
	}
	if !found {
		t.Errorf(`Expected an error with Kind RuleErrUnquote or RuleErrLexical, got %v`, errs)
	}
}

func TestParseRulesUnknownName(t *testing.T) {
	rulesText := `replce("a"|"b")`
	_, errs := parseRules(rulesText)

	var found bool
	for _, e := range errs {
		if e.Kind == RuleErrUnknownName && e.Rule == "replce" {
			found = true
		}
	}
	if !found {
		t.Errorf(`Expected an error with Kind RuleErrUnknownName and Rule "replce", got %v`, errs)
	}
}

func TestParseRulesMissingArgs(t *testing.T) {
	rulesText := `replace("a")`
	_, errs := parseRules(rulesText)

	var found bool
	for _, e := range errs {
		if e.Kind == RuleErrMissingArgs && e.Rule == "replace" {
			found = true
		}
	}
	if !found {
		t.Errorf(`Expected an error with Kind RuleErrMissingArgs and Rule "replace", got %v`, errs)
	}
}

func TestParseRulesInvalidRegexp(t *testing.T) {
	rulesText := `replace("["|"x")`
	_, errs := parseRules(rulesText)

	var found bool
	for _, e := range errs {
		if e.Kind == RuleErrInvalidRegexp && e.Rule == "replace" {
			found = true
		}
	}
	if !found {
		t.Errorf(`Expected an error with Kind RuleErrInvalidRegexp and Rule "replace", got %v`, errs)
	}
}

func TestParseRulesInvalidSelector(t *testing.T) {
	for _, rulesText := range []string{
		`remove("div:has(> p)")`,
		`remove(".a,,.b")`,
		`base64_decode("div[")`,
	} {
		_, errs := parseRules(rulesText)
		if len(errs) != 1 || errs[0].Kind != RuleErrInvalidSelector {
			t.Errorf(`Expected one RuleErrInvalidSelector error for %s, got %v`, rulesText, errs)
		}
	}
}

func TestParseRulesValidSelector(t *testing.T) {
	rulesText := `remove(".newsletter, div.share > a, p:contains(Subscribe)") base64_decode base64_decode("pre")`
	if _, errs := parseRules(rulesText); len(errs) != 0 {
		t.Errorf(`Expected zero errors, got %v`, errs)
	}
}

func TestPredefinedRulesAreValid(t *testing.T) {
	for domain, rulesText := range predefinedRules {
		if errs := ValidateRules(rulesText); len(errs) != 0 {
			t.Errorf(`Predefined rewrite rules for %s are invalid: %v`, domain, errs)
		}
	}
}

func TestParseRulesWellFormed(t *testing.T) {
	rulesText := `replace("a"|"b") nl2br`
	rules, errs := parseRules(rulesText)

	if len(errs) != 0 {
		t.Fatalf(`Expected zero errors, got %v`, errs)
	}

	if len(rules) != 2 {
		t.Fatalf(`Expected 2 rules, got %d: %v`, len(rules), rules)
	}

	if rules[0].name != "replace" || len(rules[0].args) != 2 || rules[0].args[0] != "a" || rules[0].args[1] != "b" {
		t.Errorf(`Unexpected first rule: %v`, rules[0])
	}

	if rules[1].name != "nl2br" || len(rules[1].args) != 0 {
		t.Errorf(`Unexpected second rule: %v`, rules[1])
	}
}

// TestTokenizerMatchesLegacyScanner proves the hand-written tokenizer accepts
// the same identifier/string token sequences as the previous text/scanner
// based parser (Go-escaping string mode), so the refactor changes no grammar.
func TestTokenizerMatchesLegacyScanner(t *testing.T) {
	corpus := []string{
		``,
		`nl2br`,
		`add_dynamic_image,replace("article/(.*).svg"|"article/$1.png"),remove(".spam, .ads:not(.keep)")`,
		`replace("\d+"|"x")`,
		`replace("["|"x")`,
		`remove("div[")`,
		`base64_decode(".base64")`,
		`remove("img[src^='/assets/categories/']")`,
		`remove("h1.title-detail, table[border=\"0\"]")`,
		`replace("(?i)^a\\s*ti"|"Ouch, a this")`,
		`"orphan" nl2br`,
		`nl2br "$1"`,
		`a-b`,
		`not_a_function("find"|"replace")`,
		`replace("a"|"b") replace_title("c"|"d")`,
		`  nl2br  ,  add_image_title  `,
		"nl2br\nadd_image_title",
		`replace("unterminated|"x")`,
		`rule$with$dollars`,
		`unicodé_rulé`,
	}
	// Include every predefined rule: they are all known-good historical inputs.
	for _, rulesText := range predefinedRules {
		corpus = append(corpus, rulesText)
	}

	for _, input := range corpus {
		newTokens, newErrs := tokenizeRuleText(input, false)
		legacyTokens, legacyLexicalErrs := legacyScanTokens(input)

		if len(newTokens) != len(legacyTokens) {
			t.Fatalf("token count mismatch for %q: new=%d (%v) legacy=%d (%v)",
				input, len(newTokens), newTokens, len(legacyTokens), legacyTokens)
		}
		for i := range newTokens {
			if newTokens[i].kind != legacyTokens[i].kind || newTokens[i].text != legacyTokens[i].text {
				t.Errorf("token %d mismatch for %q: new=%+v legacy=%+v",
					i, input, newTokens[i], legacyTokens[i])
			}
		}

		// Both implementations report lexical and unquote problems out of band;
		// they must agree on whether the input has any token-level problem and
		// on the kinds of those problems (ordering is deterministic as well).
		if len(newErrs) != len(legacyLexicalErrs) {
			t.Errorf("error count mismatch for %q: new=%v legacy=%v",
				input, newErrs, legacyLexicalErrs)
		} else {
			for i := range newErrs {
				if newErrs[i].Kind != legacyLexicalErrs[i].Kind {
					t.Errorf("error %d kind mismatch for %q: new=%v legacy=%v",
						i, input, newErrs[i].Kind, legacyLexicalErrs[i].Kind)
				}
			}
		}
	}
}
