// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package incremental

import (
	"strings"
	"time"

	"miniflux.app/v2/internal/reader/date"
)

// ParseCursorTime normalizes one or more raw timestamp candidates coming from
// a feed format into a single cursor timestamp. Candidates are tried in order,
// so callers pass the modification field before the publication field. Blank
// strings and unparseable values are skipped.
//
// Unlike the display-date logic in the format adapters, this function never
// falls back to time.Now(): a fetch-local wall clock is non-deterministic and
// would make the cursor non-monotonic across refreshes. When no candidate is
// usable the zero time is returned, and the entry is tracked by hash instead.
func ParseCursorTime(candidates ...string) time.Time {
	for _, raw := range candidates {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}

		parsed, err := date.Parse(raw)
		if err != nil || parsed.IsZero() {
			continue
		}

		return parsed.UTC()
	}

	return time.Time{}
}
