// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rss

import (
	"html"
	"strings"
)

// htmlUnescapeTrim trims the value and unescapes HTML entities twice, which
// matches the historical RSS title handling for doubly-encoded feeds.
func htmlUnescapeTrim(value string) string {
	return html.UnescapeString(html.UnescapeString(strings.TrimSpace(value)))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
