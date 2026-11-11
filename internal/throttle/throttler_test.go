// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package throttle

import (
	"testing"
	"time"
)

func TestDisabledThrottlerNeverBlocks(t *testing.T) {
	throttler := New(Config{})

	if throttler.Enabled() {
		t.Fatal("Throttler with zero interval must be disabled")
	}

	stop := make(chan struct{})
	for range 5 {
		release := throttler.Acquire("https://example.com/feed.xml", stop)
		if release == nil {
			t.Fatal("Acquire on a disabled throttler must return a release function")
		}
		release()
	}

	// Reporting on a disabled throttler must be harmless.
	throttler.ReportResult("https://example.com/feed.xml", OutcomeRateLimited, time.Minute)
}

func TestIntervalPacingAppliesPerHost(t *testing.T) {
	throttler := New(Config{
		MinInterval:   200 * time.Millisecond,
		MaxInterval:   10 * time.Second,
		BackoffFactor: 2,
		BackoffMax:    10 * time.Second,
	})

	stop := make(chan struct{})

	// First acquire is immediate.
	release := throttler.Acquire("https://a.example/feed", stop)
	if release == nil {
		t.Fatal("First acquire should not be blocked")
	}
	release()

	// The second acquire to the same host must wait, but another host is free.
	waited := make(chan struct{})
	go func() {
		release := throttler.Acquire("https://a.example/feed", stop)
		if release != nil {
			release()
		}
		close(waited)
	}()

	select {
	case <-waited:
		t.Fatal("Second request to the same host was not paced")
	case <-time.After(50 * time.Millisecond):
	}

	// A different host is not affected.
	other := throttler.Acquire("https://b.example/feed", stop)
	if other == nil {
		t.Fatal("Request to another host was blocked by the first host")
	}
	other()

	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("Paced request did not proceed after the interval elapsed")
	}
}

func TestConcurrencyLimitIsPerHost(t *testing.T) {
	throttler := New(Config{
		MinInterval:   time.Millisecond,
		MaxInterval:   time.Second,
		MaxConcurrent: 1,
		BackoffFactor: 2,
		BackoffMax:    time.Second,
	})

	stop := make(chan struct{})

	first := throttler.Acquire("https://a.example/feed", stop)
	if first == nil {
		t.Fatal("First slot should be available")
	}

	acquired := make(chan struct{})
	go func() {
		release := throttler.Acquire("https://a.example/feed", stop)
		if release != nil {
			release()
		}
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("Concurrency limit was not enforced")
	case <-time.After(100 * time.Millisecond):
	}

	// Another host still has its own slot.
	other := throttler.Acquire("https://b.example/feed", stop)
	if other == nil {
		t.Fatal("Concurrency limit leaked across hosts")
	}
	other()

	first()

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("Queued request did not proceed after release")
	}
}

func TestRateLimitedResponseTriggersAdaptiveBackoff(t *testing.T) {
	throttler := New(Config{
		MinInterval:   20 * time.Millisecond,
		MaxInterval:   time.Second,
		BackoffFactor: 2,
		BackoffMax:    time.Second,
	})

	url := "https://slow.example/feed"

	throttler.ReportResult(url, OutcomeRateLimited, 0)
	state := throttler.hosts["slow.example"]
	if state.interval != 40*time.Millisecond {
		t.Fatalf("Expected interval to double to 40ms, got %s", state.interval)
	}
	if !state.throttleUntil.After(time.Now()) {
		t.Fatal("Expected an active backoff window after rate limiting")
	}

	// Further rate limiting keeps backing off up to the cap.
	throttler.ReportResult(url, OutcomeRateLimited, 0) // 80ms
	throttler.ReportResult(url, OutcomeRateLimited, 0) // 160ms
	throttler.ReportResult(url, OutcomeRateLimited, 0) // 320ms
	throttler.ReportResult(url, OutcomeRateLimited, 0) // 640ms
	throttler.ReportResult(url, OutcomeRateLimited, 0) // capped at 1s
	if state.interval != time.Second {
		t.Fatalf("Expected interval capped at 1s, got %s", state.interval)
	}
}

func TestBackoffWindowBlocksAcquire(t *testing.T) {
	throttler := New(Config{
		MinInterval:   10 * time.Millisecond,
		MaxInterval:   10 * time.Second,
		BackoffFactor: 2,
		BackoffMax:    10 * time.Second,
	})

	stop := make(chan struct{})
	url := "https://slow.example/feed"

	// A Retry-After of 400ms creates a backoff window that Acquire must wait for.
	throttler.ReportResult(url, OutcomeRateLimited, 400*time.Millisecond)

	start := time.Now()
	release := throttler.Acquire(url, stop)
	if release == nil {
		t.Fatal("Acquire returned without a release")
	}
	release()

	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("Acquire ignored the backoff window, returned after %s", elapsed)
	}
}

func TestRetryAfterExtendsBackoffWindow(t *testing.T) {
	throttler := New(Config{
		MinInterval:   time.Second,
		MaxInterval:   10 * time.Second,
		BackoffFactor: 2,
		BackoffMax:    10 * time.Second,
	})

	url := "https://slow.example/feed"
	before := time.Now()
	throttler.ReportResult(url, OutcomeRateLimited, 5*time.Minute)

	state := throttler.hosts["slow.example"]
	if state.throttleUntil.Before(before.Add(5*time.Minute - time.Second)) {
		t.Fatalf("Expected throttle window to follow Retry-After (+5m), got %s", state.throttleUntil)
	}
}

func TestSuccessRelaxesBackoff(t *testing.T) {
	throttler := New(Config{
		MinInterval:   time.Second,
		MaxInterval:   30 * time.Second,
		BackoffFactor: 2,
		BackoffMax:    30 * time.Second,
	})

	url := "https://slow.example/feed"
	throttler.ReportResult(url, OutcomeRateLimited, 0) // 2s
	throttler.ReportResult(url, OutcomeRateLimited, 0) // 4s

	throttler.ReportResult(url, OutcomeSuccess, 0) // 2s
	state := throttler.hosts["slow.example"]
	if state.interval != 2*time.Second {
		t.Fatalf("Expected interval to relax to 2s, got %s", state.interval)
	}
	if !state.throttleUntil.IsZero() {
		t.Fatal("Expected backoff window to be cleared on success")
	}

	throttler.ReportResult(url, OutcomeSuccess, 0) // 1s baseline
	if state.interval != time.Second {
		t.Fatalf("Expected interval to return to 1s baseline, got %s", state.interval)
	}

	// Stays at the baseline.
	throttler.ReportResult(url, OutcomeSuccess, 0)
	if state.interval != time.Second {
		t.Fatalf("Interval must not drop below the baseline, got %s", state.interval)
	}
}

func TestSnapshotExposesThrottledHosts(t *testing.T) {
	throttler := New(Config{
		MinInterval:   time.Second,
		MaxInterval:   10 * time.Second,
		BackoffFactor: 2,
		BackoffMax:    10 * time.Second,
	})

	throttler.ReportResult("https://slow.example/a", OutcomeRateLimited, time.Minute)
	throttler.ReportResult("https://healthy.example/b", OutcomeSuccess, 0)

	statuses := throttler.Snapshot()
	byHost := make(map[string]HostStatus, len(statuses))
	for _, status := range statuses {
		byHost[status.Host] = status
	}

	slow, ok := byHost["slow.example"]
	if !ok || !slow.Throttled {
		t.Fatal("Expected slow.example to be reported as throttled")
	}

	healthy, ok := byHost["healthy.example"]
	if !ok || healthy.Throttled {
		t.Fatal("Expected healthy.example not to be reported as throttled")
	}
}

func TestStopChannelUnblocksAcquire(t *testing.T) {
	throttler := New(Config{
		MinInterval:   time.Minute,
		MaxInterval:   time.Minute,
		BackoffFactor: 2,
		BackoffMax:    time.Minute,
	})

	stop := make(chan struct{})
	release := throttler.Acquire("https://a.example/feed", stop)
	release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if r := throttler.Acquire("https://a.example/feed", stop); r != nil {
			r()
			t.Error("Acquire returned a release after stop was closed")
		}
	}()

	time.Sleep(20 * time.Millisecond)
	close(stop)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Acquire did not return after stop was closed")
	}
}

func TestBacklogStretchesIntervalForTheBusyHostOnly(t *testing.T) {
	throttler := New(Config{
		MinInterval:   100 * time.Millisecond,
		MaxInterval:   time.Second,
		MaxConcurrent: 1,
		BackoffFactor: 2,
		BackoffMax:    time.Second,
	})

	stop := make(chan struct{})
	url := "https://busy.example/feed"

	// First request occupies the single slot; subsequent ones queue up and
	// count as a backlog, so the interval must be stretched beyond the
	// baseline.
	first := throttler.Acquire(url, stop)
	if first == nil {
		t.Fatal("First slot should be available")
	}

	secondStarted := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		release := throttler.Acquire(url, stop)
		if release != nil {
			release()
		}
		secondStarted <- time.Since(start)
	}()

	// Wait until the second job is registered as waiting.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		throttler.mu.Lock()
		waiting := throttler.hosts["busy.example"].waiting
		throttler.mu.Unlock()
		if waiting >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	first()

	select {
	case elapsed := <-secondStarted:
		// Baseline interval is 100ms; with at least one queued job the wait
		// must be noticeably longer (100ms baseline + one extra step).
		if elapsed < 150*time.Millisecond {
			t.Fatalf("Backlog did not stretch the interval, waited only %s", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Queued request never started")
	}

	// The other host keeps its baseline pace.
	otherStart := time.Now()
	other := throttler.Acquire("https://calm.example/feed", stop)
	if other == nil {
		t.Fatal("Other host slot should be available")
	}
	other()
	if elapsed := time.Since(otherStart); elapsed > 50*time.Millisecond {
		t.Fatalf("Backlog of busy.example slowed down another host: %s", elapsed)
	}
}

func TestIdleHostsAreEvicted(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	throttler := New(Config{
		MinInterval:   time.Second,
		MaxInterval:   time.Second,
		BackoffFactor: 2,
		BackoffMax:    time.Second,
	})
	throttler.now = func() time.Time { return clock }
	throttler.idleHostTTL = time.Minute
	throttler.maintenanceEvery = time.Second

	throttler.ReportResult("https://gone.example/feed", OutcomeSuccess, 0)
	clock = clock.Add(2 * time.Minute)
	throttler.ReportResult("https://active.example/feed", OutcomeSuccess, 0)

	if _, exists := throttler.hosts["gone.example"]; exists {
		t.Fatal("Idle host should have been evicted")
	}
	if _, exists := throttler.hosts["active.example"]; !exists {
		t.Fatal("Active host must be retained")
	}
}
