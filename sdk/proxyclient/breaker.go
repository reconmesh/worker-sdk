package proxyclient

import (
	"sync"
	"time"
)

// breakerState is exported via Client.BreakerState() so admin tooling
// can show the worker-side state alongside the proxy-side metrics.
type breakerState int

const (
	breakerClosed breakerState = iota // proxy in use, errors below threshold
	breakerOpen                        // proxy disabled, fallback direct
	breakerHalfOpen                    // probe attempt allowed
)

func (s breakerState) String() string {
	switch s {
	case breakerClosed:
		return "closed"
	case breakerOpen:
		return "open"
	case breakerHalfOpen:
		return "half-open"
	}
	return "unknown"
}

// breaker is a tiny consecutive-error counter with a sliding window
// + cooldown. Goroutine-safe. Zero allocations on the success path.
type breaker struct {
	threshold int
	window    time.Duration
	cooldown  time.Duration
	now       func() time.Time // injection seam for tests

	mu        sync.Mutex
	state     breakerState
	failures  int
	firstFail time.Time
	openedAt  time.Time
}

func newBreaker(threshold int, window, cooldown time.Duration) *breaker {
	return &breaker{
		threshold: threshold,
		window:    window,
		cooldown:  cooldown,
		now:       time.Now,
	}
}

// allow reports whether a proxy attempt is allowed right now. Closed
// always allows; open allows once after cooldown has elapsed (ie
// half-open probe); half-open allows nothing more until the previous
// probe resolves.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerClosed:
		return true
	case breakerOpen:
		if b.now().Sub(b.openedAt) >= b.cooldown {
			b.state = breakerHalfOpen
			return true
		}
		return false
	case breakerHalfOpen:
		// One probe in flight already; further callers must wait.
		return false
	}
	return false
}

// success notifies the breaker that an attempt succeeded. Resets the
// failure counter; closes the breaker if it was half-open.
func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.firstFail = time.Time{}
	if b.state != breakerClosed {
		b.state = breakerClosed
	}
}

// failure records one error. Opens the breaker when threshold is hit
// inside the rolling window, or when a half-open probe failed.
func (b *breaker) failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	switch b.state {
	case breakerHalfOpen:
		// Probe failed - reopen immediately.
		b.state = breakerOpen
		b.openedAt = now
		return
	case breakerOpen:
		return // already open, nothing to do
	}
	if b.failures == 0 || now.Sub(b.firstFail) > b.window {
		b.firstFail = now
		b.failures = 1
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.state = breakerOpen
		b.openedAt = now
	}
}

// state returns the current breaker state for observability.
func (b *breaker) currentState() breakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
