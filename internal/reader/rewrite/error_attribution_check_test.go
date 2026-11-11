// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rewrite

import "testing"

func TestUnquoteErrorRuleAttribution(t *testing.T) {
	scenarios := []struct {
		name  string
		input string
		rule  string
	}{
		{name: "SingleRuleBadEscape", input: `replace("\d+"|"x")`, rule: "replace"},
		{name: "SecondRuleBadEscape", input: `nl2br replace("\d+"|"x")`, rule: "replace"},
		{name: "BadEscapeAfterGoodArg", input: `replace("ok"|"\d+")`, rule: "replace"},
		{name: "TitleRuleBadEscape", input: `replace_title("\d+"|"x")`, rule: "replace_title"},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			_, errs := parseRules(sc.input)
			var found bool
			for _, e := range errs {
				if e.Kind == RuleErrUnquote && e.Rule == sc.rule {
					found = true
				}
			}
			if !found {
				t.Errorf("expected an unquote error attributed to %q for %q, got %+v", sc.rule, sc.input, errs)
			}
		})
	}
}
