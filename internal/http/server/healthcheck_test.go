// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"miniflux.app/v2/internal/health"
)

type stubBacklog struct {
	count int
	err   error
}

func (s stubBacklog) CountDueFeeds(int) (int, error) { return s.count, s.err }

func startTestSampler(t *testing.T, state *health.State, store health.BacklogReader, interval time.Duration) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go state.RunSampler(ctx, store, func() int { return 0 }, interval, 0, nil)

	// Wait for the immediate sample so SamplerRunning is true.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if state.Snapshot().SamplerRunning {
			return cancel
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	t.Fatal("sampler never published a snapshot")
	return cancel
}

func TestWorkerHealthProbeHealthyWhenIdle(t *testing.T) {
	state := health.NewState(4)
	cancel := startTestSampler(t, state, stubBacklog{count: 0}, 10*time.Millisecond)
	defer cancel()

	handler := newWorkerHealthProbe(state, time.Minute, 10*time.Millisecond)
	req := httptest.NewRequest(http.MethodGet, "/workerz", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for idle healthy pool, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp workerHealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if resp.Status != "ok" || resp.Workers != 4 || resp.Stalled {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestWorkerHealthProbeHealthyWithBacklogButRecentActivity(t *testing.T) {
	state := health.NewState(2)
	cancel := startTestSampler(t, state, stubBacklog{count: 10}, 10*time.Millisecond)
	defer cancel()

	// Simulate jobs flowing through; the last activity is "now".
	state.RecordPushed(5)
	state.RecordJobStarted()
	state.RecordJobCompleted()

	handler := newWorkerHealthProbe(state, time.Minute, 10*time.Millisecond)
	req := httptest.NewRequest(http.MethodGet, "/workerz", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with recent activity, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWorkerHealthProbeReportsStallWhenWorkPendingAndIdleTooLong(t *testing.T) {
	state := health.NewState(2)
	cancel := startTestSampler(t, state, stubBacklog{count: 10}, 10*time.Millisecond)
	defer cancel()

	// Jobs were pushed but never picked up; no activity, backlog present.
	state.RecordPushed(3)

	handler := newWorkerHealthProbe(state, 20*time.Millisecond, 10*time.Millisecond)

	// First call right after creation is below the threshold.
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/workerz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 before threshold elapses, got %d", rec.Code)
	}

	time.Sleep(40 * time.Millisecond)

	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/workerz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 after stall threshold, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp workerHealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if !resp.Stalled || resp.Reason == "" || resp.DueFeeds != 10 {
		t.Fatalf("expected a detailed stall response, got %+v", resp)
	}
}

func TestWorkerHealthProbeNoFalseAlarmWithoutPendingWork(t *testing.T) {
	state := health.NewState(2)
	cancel := startTestSampler(t, state, stubBacklog{count: 0}, 10*time.Millisecond)
	defer cancel()

	// Even though nothing has happened for longer than the threshold,
	// there is no pending work, so an idle system must stay healthy.
	handler := newWorkerHealthProbe(state, 20*time.Millisecond, 10*time.Millisecond)
	time.Sleep(40 * time.Millisecond)

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/workerz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("idle system with no backlog must stay healthy, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWorkerHealthProbeDetectsStaleSampler(t *testing.T) {
	state := health.NewState(2)
	// No sampler started at all: snapshot exists from NewState but no heartbeat.
	handler := newWorkerHealthProbe(state, time.Minute, 10*time.Millisecond)

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/workerz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when sampler never ran, got %d", rec.Code)
	}
}

func TestWorkerHealthProbeResponseTimeIsBounded(t *testing.T) {
	state := health.NewState(8)
	cancel := startTestSampler(t, state, stubBacklog{count: 42}, 10*time.Millisecond)
	defer cancel()

	handler := newWorkerHealthProbe(state, time.Minute, 10*time.Millisecond)

	// The probe must stay in the sub-millisecond range: it performs no I/O.
	// Use a generous upper bound to keep the test stable on shared CI runners.
	for range 100 {
		rec := httptest.NewRecorder()
		start := time.Now()
		handler(rec, httptest.NewRequest(http.MethodGet, "/workerz", nil))
		if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
			t.Fatalf("probe took %s, it must not perform blocking work", elapsed)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("unexpected status %d", rec.Code)
		}
	}
}
