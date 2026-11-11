// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rewrite // import "miniflux.app/v2/internal/reader/rewrite"

import (
	"strconv"
	"strings"
	"text/scanner"
)

// ruleToken is one lexical token produced by tokenizeRuleText: either a rule
// name (scanner.Ident) or a quoted argument string (scanner.String). Content
// and URL rewrite rules share this tokenizer, built on the same text/scanner
// engine the historical parser used, so the rule grammar is scanned in exactly
// one place with identical token semantics.
type ruleToken struct {
	kind rune // scanner.Ident or scanner.String
	text string
	pos  scanner.Position
}

// tokenizeRuleText scans a rewrite rule string into identifiers and quoted
// strings, using text/scanner in Idents|Strings mode (parentheses, the "|"
// separator and commas are separators and are not emitted).
//
// rawStrings selects how quoted strings are interpreted:
//
//   - false (content rewrite rules): quoted strings follow Go string escaping
//     rules and are unquoted with strconv.Unquote; an invalid escape is a
//     RuleErrUnquote error, exactly as the previous content parser did.
//   - true (URL rewrite rules): the quoted text is taken verbatim, including
//     surrounding quotes removed by hand but backslashes preserved, matching
//     the historical regex-based grammar where patterns such as
//     news\.ycombinator\.com are written with single backslashes. Callers
//     validate the overall shape separately, so the scanner's lexical error
//     for an escape such as "\." is irrelevant for well-formed URL rules.
//
// Scanner-internal problems (e.g. an unterminated literal) are reported as
// RuleErrLexical errors, matching the previous parser.
func tokenizeRuleText(rulesText string, rawStrings bool) (tokens []ruleToken, errs []RuleError) {
	var scan scanner.Scanner
	scan.Mode = scanner.ScanIdents | scanner.ScanStrings
	scan.Init(strings.NewReader(rulesText))
	scan.Error = func(s *scanner.Scanner, msg string) {
		errs = append(errs, RuleError{Pos: s.Pos(), Kind: RuleErrLexical, Message: msg})
	}

	for {
		switch tok := scan.Scan(); tok {
		case scanner.Ident:
			tokens = append(tokens, ruleToken{kind: tok, text: scan.TokenText(), pos: scan.Position})
		case scanner.String:
			if rawStrings {
				// Keep the literal body verbatim: strip only the surrounding
				// quotes, without interpreting backslash escapes.
				raw := scan.TokenText()
				if len(raw) >= 2 {
					raw = raw[1 : len(raw)-1]
				}
				tokens = append(tokens, ruleToken{kind: tok, text: raw, pos: scan.Position})
			} else {
				text, err := strconv.Unquote(scan.TokenText())
				if err != nil {
					errs = append(errs, RuleError{
						Pos:     scan.Position,
						Kind:    RuleErrUnquote,
						Token:   scan.TokenText(),
						Message: err.Error(),
					})
					continue
				}
				tokens = append(tokens, ruleToken{kind: tok, text: text, pos: scan.Position})
			}
		case scanner.EOF:
			return tokens, errs
		}
	}
}

// parseRules parses the content rewrite rule text into typed rule objects. It
// is the single parsing entry point shared by validation (ValidateRules) and
// application (ApplyContentRewriteRules).
func parseRules(rulesText string) (rules []rule, errs []RuleError) {
	tokens, tokenErrs := tokenizeRuleText(rulesText, false)
	errs = append(errs, tokenErrs...)

	for _, tok := range tokens {
		switch tok.kind {
		case scanner.Ident:
			rules = append(rules, rule{name: tok.text})
		case scanner.String:
			if l := len(rules) - 1; l >= 0 {
				// Attribute the most recent unquote error to the current rule
				// name, matching the previous parser's error context.
				if n := len(errs); n > 0 && errs[n-1].Kind == RuleErrUnquote && errs[n-1].Rule == "" {
					errs[n-1].Rule = rules[l].name
				}
				rules[l].args = append(rules[l].args, tok.text)
			} else {
				errs = append(errs, RuleError{
					Pos:     tok.pos,
					Kind:    RuleErrOrphanArg,
					Token:   tok.text,
					Message: "quoted string before any rule name",
				})
			}
		}
	}

	errs = append(errs, validateParsedRules(rules)...)
	return rules, errs
}
