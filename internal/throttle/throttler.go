// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package throttle provides per-host pacing of outgoing feed requests.
//
// A single [Throttler] instance is shared by all background workers. Each
// hostname gets its own state: a minimum interval between requests, a
// concurrency limit and an adaptive backoff window triggered by rate limiting
// responses. Slow hosts therefore only throttle themselves; other hosts keep
// running at full pace.
package throttle // import "miniflux.app/v2/internal/throttle"

import (
	"log/slog"
	"sync"
	"time"

	"miniflux.app/v2/internal/urllib"
)

// Throttler enforces request pacing independently for every hostname.
type Throttler struct {
	mu    sync.Mutex
	hosts map[string]*hostState

	minInterval      time.Duration
	maxInterval      time.Duration
	maxConcurrent    int
	backoffFactor    float64
	backoffMax       time.Duration
	idleHostTTL      time.Duration
	now              func() time.Time
	lastMaintenance  time.Time
	maintenanceEvery time.Duration
}

// Config holds the tunables of a [Throttler].
type Config struct {
	// MinInterval is the baseline minimum delay between two requests to the
	// same hostname. Zero disables pacing.
	MinInterval time.Duration

	// MaxInterval caps the effective interval, including adaptive backoff.
	MaxInterval time.Duration

	// MaxConcurrent is the maximum number of simultaneous requests to the
	// same hostname. Zero or a negative value means unlimited.
	MaxConcurrent int

	// BackoffFactor multiplies the current interval when a host answers with
	// a rate limiting response. Values below 1 are treated as 2.
	BackoffFactor float64

	// BackoffMax caps the adaptive backoff interval.
	BackoffMax time.Duration
}

// New creates a Throttler. When MinInterval is zero the throttler is inert:
// Acquire returns immediately and ReportResult is a no-op.
func New(cfg Config) *Throttler {
	if cfg.BackoffFactor < 1 {
		cfg.BackoffFactor = 2
	}

	if cfg.MaxInterval <= 0 {
		cfg.MaxInterval = cfg.MinInterval
	}

	if cfg.BackoffMax <= 0 || cfg.BackoffMax < cfg.MaxInterval {
		cfg.BackoffMax = cfg.MaxInterval
	}

	return &Throttler{
		hosts:            make(map[string]*hostState),
		minInterval:      cfg.MinInterval,
		maxInterval:      cfg.MaxInterval,
		maxConcurrent:    cfg.MaxConcurrent,
		backoffFactor:    cfg.BackoffFactor,
		backoffMax:       cfg.BackoffMax,
		idleHostTTL:      30 * time.Minute,
		maintenanceEvery: 5 * time.Minute,
		now:              time.Now,
	}
}

// Enabled reports whether the throttler enforces any pacing.
func (t *Throttler) Enabled() bool {
	return t != nil && t.minInterval > 0
}

// HostOf extracts the hostname used as throttling key from a feed URL.
func HostOf(feedURL string) string {
	return urllib.Domain(feedURL)
}

// hostState is the pacing state of a single hostname.
type hostState struct {
	interval       time.Duration
	nextAllowedAt  time.Time
	inFlight       int
	waiting        int
	lastActivityAt time.Time

	// throttleUntil, when in the future, is an adaptive backoff window: all
	// requests to the host wait until it elapses.
	throttleUntil time.Time
}

// Acquire blocks until a request to the host of feedURL is allowed to start,
// honoring the per-host interval, concurrency limit, any active backoff
// window and the backlog of jobs already waiting for the host.
//
// If stop is closed while waiting, Acquire gives up and returns a nil release
// function so callers can shut down without issuing the request.
func (t *Throttler) Acquire(feedURL string, stop <-chan struct{}) (release func()) {
	if !t.Enabled() {
		return func() {}
	}

	host := HostOf(feedURL)

	t.mu.Lock()
	state := t.stateLocked(host)
	state.waiting++
	t.mu.Unlock()

	cleanupWaiting := func() {
		t.mu.Lock()
		state.waiting--
		if state.waiting < 0 {
			state.waiting = 0
		}
		t.mu.Unlock()
	}

	for {
		t.mu.Lock()
		state.lastActivityAt = t.now()

		now := t.now()
		wait, ok := delayLocked(state, now, t.maxConcurrent)
		if ok {
			t.mu.Unlock()

			select {
			case <-stop:
				cleanupWaiting()
				return nil
			case <-time.After(wait):
				continue
			}
		}

		state.inFlight++
		state.waiting--
		// The next request to this host must respect the interval, even when
		// a concurrency slot is currently free. When a backlog of jobs for the
		// host is queued up, stretch the interval so the host gets breathing
		// room instead of another burst as soon as the previous request ends.
		effectiveInterval := state.interval
		if state.waiting > 0 {
			effectiveInterval = t.backlogIntervalLocked(state)
		}
		state.nextAllowedAt = now.Add(effectiveInterval)
		t.mu.Unlock()

		return func() {
			t.mu.Lock()
			state.inFlight--
			if state.inFlight < 0 {
				state.inFlight = 0
			}
			state.lastActivityAt = t.now()
			t.mu.Unlock()
		}
	}
}

// backlogIntervalLocked returns the stretched interval applied while the host
// has queued jobs. Each additional waiting job adds one baseline step, capped
// by the configured maximum interval. Caller must hold t.mu.
func (t *Throttler) backlogIntervalLocked(state *hostState) time.Duration {
	stretched := state.interval + t.minInterval*time.Duration(state.waiting)
	if stretched > t.maxInterval {
		stretched = t.maxInterval
	}
	return stretched
}

// delayLocked returns how long to wait before the next request may start and
// false when it may start immediately. Caller must hold t.mu.
func delayLocked(state *hostState, now time.Time, maxConcurrent int) (time.Duration, bool) {
	var wait time.Duration
	blocked := false

	if maxConcurrent > 0 && state.inFlight >= maxConcurrent {
		// No slot is known to become free at a precise time; re-check
		// frequently enough to react as soon as a release happens.
		wait = 250 * time.Millisecond
		blocked = true
	}

	if !state.nextAllowedAt.IsZero() && state.nextAllowedAt.After(now) {
		if d := state.nextAllowedAt.Sub(now); !blocked || d > wait {
			wait = d
		}
		blocked = true
	}

	if state.throttleUntil.After(now) {
		if d := state.throttleUntil.Sub(now); !blocked || d > wait {
			wait = d
		}
		blocked = true
	}

	return wait, blocked
}

// Outcome describes how a finished request ended, so the throttler can adapt
// the pacing of the host.
type Outcome int

const (
	// OutcomeSuccess is any non-rate-limited response, including ordinary
	// HTTP errors unrelated to throttling.
	OutcomeSuccess Outcome = iota
	// OutcomeRateLimited means the host answered with HTTP 429 or 503.
	OutcomeRateLimited
)

// ReportResult adapts the pacing of the host of feedURL based on the request
// outcome. retryAfter is the value of the Retry-After header, if any.
func (t *Throttler) ReportResult(feedURL string, outcome Outcome, retryAfter time.Duration) {
	if !t.Enabled() {
		return
	}

	host := HostOf(feedURL)

	t.mu.Lock()
	defer t.mu.Unlock()

	state := t.stateLocked(host)
	state.lastActivityAt = t.now()

	switch outcome {
	case OutcomeRateLimited:
		previous := state.interval
		next := time.Duration(float64(previous) * t.backoffFactor)
		if next <= previous {
			next = previous * 2
		}
		if next > t.backoffMax {
			next = t.backoffMax
		}
		state.interval = next

		until := t.now().Add(next)
		if retryAfter > 0 {
			ra := t.now().Add(retryAfter)
			if ra.After(until) {
				until = ra
			}
		}
		state.throttleUntil = until

		slog.Warn("Host is being throttled after a rate limiting response",
			slog.String("host", host),
			slog.String("previous_interval", previous.String()),
			slog.String("new_interval", next.String()),
			slog.String("retry_after", retryAfter.String()),
			slog.Time("throttle_until", until),
		)

	default:
		// A successful response means the current pace is tolerated: relax
		// one step towards the baseline interval and clear any backoff.
		if state.interval > t.minInterval || state.throttleUntil.After(t.now()) {
			relaxed := time.Duration(float64(state.interval) / t.backoffFactor)
			if relaxed < t.minInterval {
				relaxed = t.minInterval
			}
			wasThrottled := state.interval > t.minInterval
			state.interval = relaxed
			state.throttleUntil = time.Time{}

			if wasThrottled {
				slog.Info("Host recovered from throttling",
					slog.String("host", host),
					slog.String("new_interval", relaxed.String()),
				)
			}
		}
	}

	t.maintenanceLocked()
}

// stateLocked returns the state for host, creating it on first use.
// Caller must hold t.mu.
func (t *Throttler) stateLocked(host string) *hostState {
	state, exists := t.hosts[host]
	if !exists {
		state = &hostState{interval: t.minInterval}
		t.hosts[host] = state
	}
	return state
}

// maintenanceLocked drops hosts that have been idle for a while so the map
// does not grow forever. It runs at most once per maintenanceEvery interval.
// Caller must hold t.mu.
func (t *Throttler) maintenanceLocked() {
	now := t.now()
	if now.Sub(t.lastMaintenance) < t.maintenanceEvery {
		return
	}
	t.lastMaintenance = now

	for host, state := range t.hosts {
		if state.inFlight > 0 || state.waiting > 0 || state.throttleUntil.After(now) {
			continue
		}
		if now.Sub(state.lastActivityAt) > t.idleHostTTL {
			delete(t.hosts, host)
		}
	}
}

// HostStatus is a point-in-time view of a host's pacing state, used for
// observability.
type HostStatus struct {
	Host          string
	Interval      time.Duration
	InFlight      int
	Waiting       int
	Throttled     bool
	ThrottleUntil time.Time
}

// Snapshot returns the current status of every tracked host.
func (t *Throttler) Snapshot() []HostStatus {
	if !t.Enabled() {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	statuses := make([]HostStatus, 0, len(t.hosts))
	for host, state := range t.hosts {
		statuses = append(statuses, HostStatus{
			Host:          host,
			Interval:      state.interval,
			InFlight:      state.inFlight,
			Waiting:       state.waiting,
			Throttled:     state.throttleUntil.After(now) || state.interval > t.minInterval,
			ThrottleUntil: state.throttleUntil,
		})
	}
	return statuses
}
