// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package worker // import "miniflux.app/v2/internal/worker"

import (
	"log/slog"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/throttle"
)

// newThrottlerFromConfig builds the shared per-host throttler used by the
// background workers. It returns an inert throttler when per-host pacing is
// disabled or the configuration has not been loaded yet, in which case
// Acquire never blocks.
func newThrottlerFromConfig() *throttle.Throttler {
	if config.Opts == nil || !config.Opts.PollingThrottleEnabled() {
		slog.Debug("Per-host request throttling is disabled")
		return throttle.New(throttle.Config{})
	}

	minInterval := config.Opts.PollingThrottleMinInterval()
	maxInterval := config.Opts.PollingThrottleMaxInterval()
	if maxInterval < minInterval {
		maxInterval = minInterval
	}

	slog.Info("Per-host request throttling is enabled",
		slog.String("min_interval", minInterval.String()),
		slog.String("max_interval", maxInterval.String()),
		slog.Int("max_concurrent_per_host", config.Opts.PollingThrottleMaxConcurrent()),
	)

	return throttle.New(throttle.Config{
		MinInterval:   minInterval,
		MaxInterval:   maxInterval,
		MaxConcurrent: config.Opts.PollingThrottleMaxConcurrent(),
		BackoffFactor: 2,
		BackoffMax:    maxInterval,
	})
}
