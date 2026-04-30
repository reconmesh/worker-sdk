// Package proxyclient is the worker-side glue for parabellum-proxy:
// it asks the controlplane for a sticky assignment, refreshes it
// periodically, and produces *http.Client instances that route
// through that proxy (or direct, with a circuit breaker fallback).
//
// Lifecycle
//
//   pc := proxyclient.New(proxyclient.Options{...})
//   pc.Start(ctx)        // kicks off the 5min refresh ticker
//   defer pc.Close()
//
//   client := pc.HTTPClient(true)   // proxy when healthy, direct fallback
//   resp, err := client.Get("https://shodan.io/...")
//
// HTTPClient(false) always returns a direct client - useful for the
// rare flows that must bypass the cache (e.g. probing whether a
// scope still resolves, never cache-able).
//
// Circuit breaker
//
//   - 3 consecutive HTTPClient errors within 30s → open
//   - 60s cooldown → half-open (next request retries the proxy)
//   - 1 success in half-open → close
//
// While open, HTTPClient(true) silently returns a direct client. The
// worker keeps making progress; the operator sees the breaker state
// in the proxy admin UI via the heartbeat-side metrics.
package proxyclient

import (
	"crypto/x509"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// Options configures New(). Defaults are chosen so a worker calls
// New(NewFromEnv()) and gets a sensible client. Empty
// ControlplaneURL disables proxy routing entirely (HTTPClient(true)
// behaves identically to HTTPClient(false)) - useful for local dev
// without a controlplane.
type Options struct {
	// ControlplaneURL is the base URL of the controlplane HTTP API.
	// Empty disables proxy routing. Read from CONTROLPLANE_URL env
	// when using NewFromEnv.
	ControlplaneURL string
	// APIToken is the bearer the SDK presents on
	// /api/proxy/assignment. Operator-tier token suffices. Read from
	// WORKER_API_TOKEN env when using NewFromEnv.
	APIToken string
	// WorkerID is the unique identity the controlplane stores in
	// proxy_assignments. Pass the runtime's instance ID.
	WorkerID string
	// RefreshInterval is the cadence at which the SDK re-fetches the
	// assignment to pick up operator-side reroll commands. Default
	// 5 min.
	RefreshInterval time.Duration
	// BreakerErrors is the consecutive-error threshold within
	// BreakerWindow that flips the breaker to open. Default 3.
	BreakerErrors int
	// BreakerWindow scopes the consecutive-error count. A success
	// resets the count. Default 30s.
	BreakerWindow time.Duration
	// BreakerCooldown is how long the breaker stays open before
	// half-opening (one probe attempt allowed). Default 60s.
	BreakerCooldown time.Duration
	// HTTPTimeout caps the per-request total time on the
	// HTTPClient(true) result. Default 30s.
	HTTPTimeout time.Duration
	// HTTPClient overrides the bootstrap HTTP client used to call
	// /api/proxy/assignment. Mostly useful in tests. Production
	// uses an internal client with HTTPTimeout.
	HTTPClient *http.Client
	// ProxyCAPool overrides the system root pool for HTTPClient(true)
	// TLS verification. Production setups inject the proxy CA via
	// Dockerfile (see C7) so this is only needed for dev scenarios
	// where the operator hasn't run update-ca-certificates.
	ProxyCAPool *x509.CertPool
	// Logger receives breaker state + refresh log lines. nil → slog.Default().
	Logger *slog.Logger
}

// NewFromEnv assembles Options from the documented env vars. Used by
// SDK consumers that don't want to thread their own config.
func NewFromEnv() Options {
	return Options{
		ControlplaneURL: os.Getenv("CONTROLPLANE_URL"),
		APIToken:        os.Getenv("WORKER_API_TOKEN"),
	}
}

// applyDefaults fills zero-valued fields. Called by New().
func (o *Options) applyDefaults() {
	if o.RefreshInterval <= 0 {
		o.RefreshInterval = 5 * time.Minute
	}
	if o.BreakerErrors <= 0 {
		o.BreakerErrors = 3
	}
	if o.BreakerWindow <= 0 {
		o.BreakerWindow = 30 * time.Second
	}
	if o.BreakerCooldown <= 0 {
		o.BreakerCooldown = 60 * time.Second
	}
	if o.HTTPTimeout <= 0 {
		o.HTTPTimeout = 30 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: o.HTTPTimeout}
	}
}
