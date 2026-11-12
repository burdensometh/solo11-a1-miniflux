// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"testing"
	"time"
)

func TestWithinArchiveWindow(t *testing.T) {
	at := func(hour, minute int) time.Time {
		return time.Date(2026, time.November, 12, hour, minute, 0, 0, time.Local)
	}

	scenarios := []struct {
		name       string
		now        time.Time
		start, end int
		expected   bool
	}{
		{name: "zero length window is always open", now: at(13, 0), start: 0, end: 0, expected: true},
		{name: "inside window", now: at(3, 30), start: 2 * 60, end: 5 * 60, expected: true},
		{name: "window start is inclusive", now: at(2, 0), start: 2 * 60, end: 5 * 60, expected: true},
		{name: "window end is exclusive", now: at(5, 0), start: 2 * 60, end: 5 * 60, expected: false},
		{name: "before window", now: at(1, 59), start: 2 * 60, end: 5 * 60, expected: false},
		{name: "after window", now: at(5, 1), start: 2 * 60, end: 5 * 60, expected: false},
		{name: "wrapping window before midnight", now: at(23, 0), start: 22 * 60, end: 6 * 60, expected: true},
		{name: "wrapping window after midnight", now: at(5, 59), start: 22 * 60, end: 6 * 60, expected: true},
		{name: "wrapping window boundary at midnight", now: at(0, 0), start: 22 * 60, end: 6 * 60, expected: true},
		{name: "wrapping window closed during the day", now: at(12, 0), start: 22 * 60, end: 6 * 60, expected: false},
		{name: "wrapping window end is exclusive", now: at(6, 0), start: 22 * 60, end: 6 * 60, expected: false},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			if result := withinArchiveWindow(scenario.now, scenario.start, scenario.end); result != scenario.expected {
				t.Fatalf("withinArchiveWindow(%s, %d, %d) = %v, expected %v",
					scenario.now.Format("15:04"), scenario.start, scenario.end, result, scenario.expected)
			}
		})
	}
}

func TestSleepWithContext(t *testing.T) {
	if !sleepWithContext(context.Background(), 10*time.Millisecond) {
		t.Fatal("Expected sleep to complete when the context is not canceled")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if sleepWithContext(ctx, time.Hour) {
		t.Fatal("Expected sleep to be interrupted by the canceled context")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Expected the canceled context to interrupt the sleep immediately, took %v", elapsed)
	}

	if !sleepWithContext(context.Background(), 0) {
		t.Fatal("Expected a zero delay to be a no-op")
	}
}
