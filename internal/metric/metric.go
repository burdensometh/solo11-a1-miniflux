// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metric // import "miniflux.app/v2/internal/metric"

import (
	"context"
	"log/slog"
	"time"

	"miniflux.app/v2/internal/health"
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

	// Refresh pipeline health gauges. These are fed by the periodic health
	// sampler, never scraped synchronously from the database or workers.
	refreshQueueDepthGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "refresh_queue_depth",
			Help:      "Number of refresh jobs waiting in the in-memory worker queue at the last sampling tick",
		},
	)

	refreshInFlightGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "refresh_jobs_in_flight",
			Help:      "Number of refresh jobs currently being processed by workers",
		},
	)

	refreshDueFeedsGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "refresh_due_feeds",
			Help:      "Number of enabled feeds with an overdue next_check_at at the last sampling tick",
		},
	)

	refreshWorkersGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "refresh_workers",
			Help:      "Configured size of the background refresh worker pool",
		},
	)

	refreshSamplerOKGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "miniflux",
			Name:      "refresh_health_sampler_up",
			Help:      "1 if the last health sampling tick queried the database successfully, 0 otherwise",
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
	prometheus.MustRegister(refreshQueueDepthGauge)
	prometheus.MustRegister(refreshInFlightGauge)
	prometheus.MustRegister(refreshDueFeedsGauge)
	prometheus.MustRegister(refreshWorkersGauge)
	prometheus.MustRegister(refreshSamplerOKGauge)

	return &collector{store, refreshInterval}
}

// ObserveRefreshHealth publishes one health sampler snapshot to the
// Prometheus registry. It is called by the sampler goroutine, never by an
// HTTP request path.
func ObserveRefreshHealth(snap health.Snapshot) {
	refreshQueueDepthGauge.Set(float64(snap.Queued))
	refreshInFlightGauge.Set(float64(snap.InFlight))
	refreshDueFeedsGauge.Set(float64(snap.DueFeeds))
	refreshWorkersGauge.Set(float64(snap.Workers))

	if snap.SampledOK {
		refreshSamplerOKGauge.Set(1)
	} else {
		refreshSamplerOKGauge.Set(0)
	}
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
