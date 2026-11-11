// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rewrite

import (
	"strconv"
	"strings"
	"text/scanner"
)

// legacyScanTokens reproduces the pre-refactor text/scanner based lexer used by
// parseRules, using the same ruleToken/RuleError structures as the new
// tokenizer. It exists only as an oracle for TestTokenizerMatchesLegacyScanner
// to prove the shared tokenizer preserves the identical token grammar and
// error behavior of the historical content-rule parser.
func legacyScanTokens(rulesText string) (tokens []ruleToken, errs []RuleError) {
	scan := scanner.Scanner{Mode: scanner.ScanIdents | scanner.ScanStrings}
	scan.Init(strings.NewReader(rulesText))
	scan.Error = func(s *scanner.Scanner, msg string) {
		errs = append(errs, RuleError{Pos: s.Pos(), Kind: RuleErrLexical, Message: msg})
	}

	for {
		switch tok := scan.Scan(); tok {
		case scanner.Ident:
			tokens = append(tokens, ruleToken{kind: tok, text: scan.TokenText(), pos: scan.Position})
		case scanner.String:
			text, err := strconv.Unquote(scan.TokenText())
			if err != nil {
				errs = append(errs, RuleError{Pos: scan.Position, Kind: RuleErrUnquote,
					Token: scan.TokenText(), Message: err.Error()})
				continue
			}
			tokens = append(tokens, ruleToken{kind: tok, text: text, pos: scan.Position})
		case scanner.EOF:
			return tokens, errs
		}
	}
}
