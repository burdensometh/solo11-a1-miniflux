// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rewrite

import "testing"

// TestErrorPositionsPopulated locks in that parse errors carry the rule name,
// token and position context callers rely on for localized messages and logs.
func TestErrorPositionsPopulated(t *testing.T) {
	errs := ValidateRules(`"orphan" nl2br`)
	if len(errs) == 0 || errs[0].Pos.Column == 0 {
		t.Fatalf("expected orphan-arg error with a column, got %+v", errs)
	}
	if errs[0].Kind != RuleErrOrphanArg {
		t.Fatalf("expected RuleErrOrphanArg, got %v", errs[0].Kind)
	}

	// Invalid regexp error should carry the rule name and the offending token.
	errs = ValidateRules(`replace("["|"x")`)
	if len(errs) != 1 || errs[0].Kind != RuleErrInvalidRegexp || errs[0].Rule != "replace" || errs[0].Token != "[" {
		t.Fatalf("unexpected invalid-regexp error: %+v", errs)
	}

	// Unquote error should be attributed to the current rule name.
	errs = ValidateRules(`replace("\d+"|"x")`)
	var found bool
	for _, e := range errs {
		if e.Kind == RuleErrUnquote && e.Rule == "replace" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an unquote error attributed to replace, got %+v", errs)
	}
}
