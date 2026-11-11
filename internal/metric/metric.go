// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metric // import "miniflux.app/v2/internal/metric"

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"miniflux.app/v2/internal/storage"

	"github.com/prometheus/client_golang/prometheus"
)

// Status label values for histogram metrics.
const (
	StatusSuccess = "success"
	StatusError   = "error"
)

// Prometheus Metrics.
var (
	BackgroundFeedRefreshDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "miniflux",
			Name:      "background_feed_refresh_duration",
			Help:      "Processing time to refresh feeds from the background workers",
			Buckets:   prometheus.LinearBuckets(1, 2, 15),
		},
		[]string{"status"},
	)

	// HostThrottledGauge reports the hosts currently paced slower than their
	// baseline interval because of rate limiting responses.
	HostThrottledGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "host_throttled",
			Help:      "1 when the host is being throttled after rate limiting responses, 0 otherwise",
		},
		[]string{"host"},
	)

	// HostRequestIntervalGauge reports the effective minimum interval between
	// two outgoing requests to a host.
	HostRequestIntervalGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "host_request_interval_seconds",
			Help:      "Effective minimum interval in seconds between requests to a host",
		},
		[]string{"host"},
	)

	// HostInFlightRequestsGauge reports how many requests to a host are in
	// flight, useful to verify the per-host concurrency limit.
	HostInFlightRequestsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "host_inflight_requests",
			Help:      "Number of outgoing requests currently in flight to a host",
		},
		[]string{"host"},
	)

	// HostWaitingRequestsGauge reports how many jobs are queued waiting for
	// their host's pacing gate, i.e. the per-host backlog.
	HostWaitingRequestsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "host_waiting_requests",
			Help:      "Number of refresh jobs waiting for the host pacing gate",
		},
		[]string{"host"},
	)

	// HostRateLimitingTotal counts rate limiting responses (HTTP 429/503)
	// received from each host.
	HostRateLimitingTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "miniflux",
			Name:      "host_rate_limiting_responses_total",
			Help:      "Total number of rate limiting responses (HTTP 429 or 503) received from a host",
		},
		[]string{"host"},
	)

	ArchiveEntriesDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "miniflux",
			Name:      "archive_entries_duration",
			Help:      "Archive entries duration",
			Buckets:   prometheus.LinearBuckets(1, 2, 30),
		},
		[]string{"status"},
	)

	usersGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "users",
			Help:      "Number of users",
		},
	)

	feedsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "feeds",
			Help:      "Number of feeds by status",
		},
		[]string{"status"},
	)

	brokenFeedsGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "broken_feeds",
			Help:      "Number of broken feeds",
		},
	)

	entriesGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "entries",
			Help:      "Number of entries by status",
		},
		[]string{"status"},
	)

	dbOpenConnectionsGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "db_open_connections",
			Help:      "The number of established connections both in use and idle",
		},
	)

	dbConnectionsInUseGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "db_connections_in_use",
			Help:      "The number of connections currently in use",
		},
	)

	dbConnectionsIdleGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "db_connections_idle",
			Help:      "The number of idle connections",
		},
	)

	dbConnectionsWaitCountGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "db_connections_wait_count",
			Help:      "The total number of connections waited for",
		},
	)

	dbConnectionsMaxIdleClosedGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "db_connections_max_idle_closed",
			Help:      "The total number of connections closed due to SetMaxIdleConns",
		},
	)

	dbConnectionsMaxIdleTimeClosedGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "db_connections_max_idle_time_closed",
			Help:      "The total number of connections closed due to SetConnMaxIdleTime",
		},
	)

	dbConnectionsMaxLifetimeClosedGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "db_connections_max_lifetime_closed",
			Help:      "The total number of connections closed due to SetConnMaxLifetime",
		},
	)
)

// collector represents a metric collector.
type collector struct {
	store           *storage.Storage
	refreshInterval time.Duration
}

// NewCollector initializes a new metric collector.
func NewCollector(store *storage.Storage, refreshInterval time.Duration) *collector {
	prometheus.MustRegister(BackgroundFeedRefreshDuration)
	prometheus.MustRegister(HostThrottledGauge)
	prometheus.MustRegister(HostRequestIntervalGauge)
	prometheus.MustRegister(HostInFlightRequestsGauge)
	prometheus.MustRegister(HostWaitingRequestsGauge)
	prometheus.MustRegister(HostRateLimitingTotal)
	prometheus.MustRegister(ArchiveEntriesDuration)
	prometheus.MustRegister(usersGauge)
	prometheus.MustRegister(feedsGauge)
	prometheus.MustRegister(brokenFeedsGauge)
	prometheus.MustRegister(entriesGauge)
	prometheus.MustRegister(dbOpenConnectionsGauge)
	prometheus.MustRegister(dbConnectionsInUseGauge)
	prometheus.MustRegister(dbConnectionsIdleGauge)
	prometheus.MustRegister(dbConnectionsWaitCountGauge)
	prometheus.MustRegister(dbConnectionsMaxIdleClosedGauge)
	prometheus.MustRegister(dbConnectionsMaxIdleTimeClosedGauge)
	prometheus.MustRegister(dbConnectionsMaxLifetimeClosedGauge)

	MarkRegistered()

	return &collector{store, refreshInterval}
}

// GatherStorageMetrics polls the database to fetch metrics.
func (c *collector) GatherStorageMetrics(ctx context.Context) {
	ticker := time.NewTicker(c.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Debug("Stopping metric collector")
			return
		case <-ticker.C:
		}
		slog.Debug("Collecting metrics from the database")

		if usersCount, err := c.store.CountUsers(); err != nil {
			slog.Warn("Unable to collect users metric", slog.Any("error", err))
		} else {
			usersGauge.Set(float64(usersCount))
		}

		if brokenFeedsCount, err := c.store.CountAllFeedsWithErrors(); err != nil {
			slog.Warn("Unable to collect broken feeds metric", slog.Any("error", err))
		} else {
			brokenFeedsGauge.Set(float64(brokenFeedsCount))
		}

		if feedsCount, err := c.store.CountAllFeeds(); err != nil {
			slog.Warn("Unable to collect feeds metric", slog.Any("error", err))
		} else {
			for status, count := range feedsCount {
				feedsGauge.WithLabelValues(status).Set(float64(count))
			}
		}

		if entriesCount, err := c.store.CountAllEntries(); err != nil {
			slog.Warn("Unable to collect entries metric", slog.Any("error", err))
		} else {
			for status, count := range entriesCount {
				entriesGauge.WithLabelValues(status).Set(float64(count))
			}
		}

		dbStats := c.store.DBStats()
		dbOpenConnectionsGauge.Set(float64(dbStats.OpenConnections))
		dbConnectionsInUseGauge.Set(float64(dbStats.InUse))
		dbConnectionsIdleGauge.Set(float64(dbStats.Idle))
		dbConnectionsWaitCountGauge.Set(float64(dbStats.WaitCount))
		dbConnectionsMaxIdleClosedGauge.Set(float64(dbStats.MaxIdleClosed))
		dbConnectionsMaxIdleTimeClosedGauge.Set(float64(dbStats.MaxIdleTimeClosed))
		dbConnectionsMaxLifetimeClosedGauge.Set(float64(dbStats.MaxLifetimeClosed))
	}
}

// HostStatusSnapshot is the point-in-time pacing view of a host that
// UpdateHostThrottleMetrics consumes. The throttle package implements it
// without this package having to import it (which would create a cycle).
type HostStatusSnapshot struct {
	Host          string
	Interval      time.Duration
	InFlight      int
	Waiting       int
	Throttled     bool
	ThrottleUntil time.Time
}

// metricsRegistered guards the host gauges, which only exist once a collector
// has been created.
var metricsRegistered atomic.Bool

// MarkRegistered is called by NewCollector once every metric is registered.
func MarkRegistered() {
	metricsRegistered.Store(true)
}

// UpdateHostThrottleMetrics publishes the per-host pacing gauges. It is a
// no-op when the metrics collector is disabled.
func UpdateHostThrottleMetrics(statuses []HostStatusSnapshot) {
	if !metricsRegistered.Load() {
		return
	}

	seen := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		seen[status.Host] = struct{}{}

		if status.Throttled {
			HostThrottledGauge.WithLabelValues(status.Host).Set(1)
		} else {
			HostThrottledGauge.WithLabelValues(status.Host).Set(0)
		}

		HostRequestIntervalGauge.WithLabelValues(status.Host).Set(status.Interval.Seconds())
		HostInFlightRequestsGauge.WithLabelValues(status.Host).Set(float64(status.InFlight))
		HostWaitingRequestsGauge.WithLabelValues(status.Host).Set(float64(status.Waiting))
	}
}

// IncHostRateLimitingResponse increments the rate limiting response counter
// for a host. It is a no-op when the metrics collector is disabled.
func IncHostRateLimitingResponse(host string) {
	if !metricsRegistered.Load() {
		return
	}

	HostRateLimitingTotal.WithLabelValues(host).Inc()
}
