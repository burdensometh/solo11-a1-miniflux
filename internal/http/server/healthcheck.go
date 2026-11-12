// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server // import "miniflux.app/v2/internal/http/server

import (
	"encoding/json"
	"net/http"
	"time"

	"miniflux.app/v2/internal/health"
	"miniflux.app/v2/internal/storage"
)

func livenessProbe(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func newReadinessProbe(store *storage.Storage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(); err != nil {
			http.Error(w, `Database Connection Error`, http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}
}

// workerHealthResponse is the JSON body returned by the worker health probe.
type workerHealthResponse struct {
	Status            string    `json:"status"`
	Workers           int       `json:"workers"`
	InFlight          int64     `json:"in_flight"`
	Queued            int       `json:"queued"`
	DueFeeds          int       `json:"due_feeds"`
	JobsPushed        uint64    `json:"jobs_pushed"`
	JobsCompleted     uint64    `json:"jobs_completed"`
	LastJobAt         time.Time `json:"last_job_at,omitempty"`
	SampledAt         time.Time `json:"sampled_at"`
	SampledOK         bool      `json:"sampled_ok"`
	SamplerRunning    bool      `json:"sampler_running"`
	Stalled           bool      `json:"stalled"`
	StallThresholdSec int       `json:"stall_threshold_seconds"`
	Reason            string    `json:"reason,omitempty"`
}

// newWorkerHealthProbe exposes the state of the background refresh pipeline.
//
// The handler is intentionally I/O-free: it only reads atomic counters and
// the last snapshot published by the periodic sampler. It therefore has a
// near-constant response time and cannot be slowed down by a busy or slow
// database, nor can it add load to the worker processes.
func newWorkerHealthProbe(state *health.State, stallThreshold, sampleInterval time.Duration) http.HandlerFunc {
	startTime := time.Now()

	return func(w http.ResponseWriter, r *http.Request) {
		snap := state.Snapshot()
		now := time.Now()

		resp := workerHealthResponse{
			Workers:           snap.Workers,
			InFlight:          snap.InFlight,
			Queued:            snap.Queued,
			DueFeeds:          snap.DueFeeds,
			JobsPushed:        snap.Pushed,
			JobsCompleted:     snap.Completed,
			LastJobAt:         snap.LastJobAt,
			SampledAt:         snap.SampledAt,
			SampledOK:         snap.SampledOK,
			SamplerRunning:    snap.SamplerRunning,
			StallThresholdSec: int(stallThreshold.Seconds()),
		}

		// Stall detection: no worker has picked up a job within the
		// threshold while work exists (a backlog or a pushed/completed gap).
		// Before the first job ever runs, the process start time anchors the
		// calculation so an instance that never started processing still trips.
		idleSince := startTime
		if !snap.LastJobAt.IsZero() {
			idleSince = snap.LastJobAt
		}
		workPending := snap.Queued > 0 || snap.DueFeeds > 0 || snap.Pushed > snap.Completed
		idleFor := now.Sub(idleSince)

		switch {
		case !snap.SamplerRunning:
			resp.Stalled = true
			resp.Reason = "health sampler has never published a snapshot"
		case now.Sub(snap.SampledAt) > 3*sampleInterval:
			resp.Stalled = true
			resp.Reason = "health sampler appears stuck: snapshot is stale"
		case workPending && idleFor > stallThreshold:
			resp.Stalled = true
			resp.Reason = "no worker picked up a job within the stall threshold while work is pending"
		}

		w.Header().Set("Content-Type", "application/json")

		if resp.Stalled {
			resp.Status = "unhealthy"
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			resp.Status = "ok"
			w.WriteHeader(http.StatusOK)
		}

		// The body is a small, fixed-shape document; an encode error here
		// (e.g. client disconnect) is not actionable.
		_ = json.NewEncoder(w).Encode(resp)
	}
}
