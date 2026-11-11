// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package worker // import "miniflux.app/v2/internal/worker"

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/locale"
	"miniflux.app/v2/internal/metric"
	"miniflux.app/v2/internal/model"
	feedHandler "miniflux.app/v2/internal/reader/handler"
	"miniflux.app/v2/internal/storage"
	"miniflux.app/v2/internal/throttle"
)

// metricsRefreshInterval is how often the per-host pacing gauges are
// published while the pool is running.
const metricsRefreshInterval = 30 * time.Second

type worker struct {
	id        int
	store     *storage.Storage
	throttler *throttle.Throttler
}

// Run processes feed refresh jobs from the channel until the pool is shut down.
func (w *worker) Run(c <-chan model.Job, shutdown <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()

	slog.Debug("Worker started",
		slog.Int("worker_id", w.id),
	)

	// Only the first worker publishes the gauges; the throttler is shared.
	if w.id == 0 {
		go w.publishThrottleMetrics(shutdown)
	}

	for {
		var job model.Job
		select {
		case <-shutdown:
			return
		case job = <-c:
		}

		slog.Debug("Job received by worker",
			slog.Int("worker_id", w.id),
			slog.Int64("user_id", job.UserID),
			slog.Int64("feed_id", job.FeedID),
			slog.String("feed_url", job.FeedURL),
		)

		// Wait for the per-host pacing gate. On shutdown the release function
		// is nil and the job is left in the queue for the next scheduler run.
		release := w.throttler.Acquire(job.FeedURL, shutdown)
		if release == nil {
			return
		}

		startTime := time.Now()
		localizedError := feedHandler.RefreshFeed(w.store, job.UserID, job.FeedID, false)
		release()

		w.reportThrottlingOutcome(job.FeedURL, localizedError)

		if config.Opts != nil && config.Opts.HasMetricsCollector() {
			status := metric.StatusSuccess
			if localizedError != nil {
				status = metric.StatusError
			}
			metric.BackgroundFeedRefreshDuration.WithLabelValues(status).Observe(time.Since(startTime).Seconds())
		}
	}
}

// reportThrottlingOutcome feeds the outcome of a refresh back into the
// per-host throttler so rate limited hosts slow down automatically and
// recover once they answer normally again.
func (w *worker) reportThrottlingOutcome(feedURL string, localizedError *locale.LocalizedErrorWrapper) {
	if !w.throttler.Enabled() {
		return
	}

	host := throttle.HostOf(feedURL)

	if localizedError != nil && isRateLimitingStatus(localizedError.HTTPStatusCode()) {
		if config.Opts != nil && config.Opts.HasMetricsCollector() {
			metric.IncHostRateLimitingResponse(host)
		}
		// Retry-After is already honored by the feed's next_check_at; the
		// throttler applies an additional in-process backoff window.
		w.throttler.ReportResult(feedURL, throttle.OutcomeRateLimited, 0)
		return
	}

	w.throttler.ReportResult(feedURL, throttle.OutcomeSuccess, 0)
}

// publishThrottleMetrics periodically exports the per-host pacing gauges.
func (w *worker) publishThrottleMetrics(shutdown <-chan struct{}) {
	if config.Opts == nil || !config.Opts.HasMetricsCollector() || !w.throttler.Enabled() {
		return
	}

	ticker := time.NewTicker(metricsRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-shutdown:
			return
		case <-ticker.C:
			snapshot := w.throttler.Snapshot()
			statuses := make([]metric.HostStatusSnapshot, 0, len(snapshot))
			for _, status := range snapshot {
				statuses = append(statuses, metric.HostStatusSnapshot{
					Host:          status.Host,
					Interval:      status.Interval,
					InFlight:      status.InFlight,
					Waiting:       status.Waiting,
					Throttled:     status.Throttled,
					ThrottleUntil: status.ThrottleUntil,
				})
			}
			metric.UpdateHostThrottleMetrics(statuses)
		}
	}
}

// isRateLimitingStatus reports whether an HTTP status code is treated as a
// signal that the host is rate limiting us.
func isRateLimitingStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode == http.StatusServiceUnavailable
}
