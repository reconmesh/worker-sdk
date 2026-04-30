package proxyclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Assignment mirrors the controlplane's proxy.Assignment shape. We
// duplicate the fields here rather than depend on the controlplane
// module to keep the SDK importable by anyone (no PG / huma drag).
type Assignment struct {
	ProxyID      string `json:"proxy_id"`
	ProxyHost    string `json:"proxy_host"`
	ProxyAddress string `json:"proxy_address"`
}

// Client owns the worker-side proxy assignment state machine. Safe
// for concurrent use; Start kicks off a single refresher goroutine
// that updates `current` atomically.
type Client struct {
	opts Options

	current atomic.Pointer[Assignment]
	br      *breaker

	started atomic.Bool
	stop    chan struct{}
	done    chan struct{}
}

// New builds a Client. Call Start to enable the refresher.
func New(opts Options) *Client {
	opts.applyDefaults()
	return &Client{
		opts: opts,
		br:   newBreaker(opts.BreakerErrors, opts.BreakerWindow, opts.BreakerCooldown),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// Start kicks off the refresher goroutine. Idempotent if called once;
// calling twice spawns two goroutines (don't). The first refresh
// runs synchronously inside Start so the worker's Run() that calls
// HTTPClient(true) right after Start gets a populated assignment.
func (c *Client) Start(ctx context.Context) {
	if !c.started.CompareAndSwap(false, true) {
		return // already started; idempotent
	}
	if c.opts.ControlplaneURL == "" || c.opts.WorkerID == "" {
		// No controlplane or no worker id → no proxy mode. HTTPClient
		// silently falls back to direct. Useful for local dev or
		// validate-only runs.
		close(c.done)
		return
	}
	// One synchronous fetch so the first HTTPClient(true) call sees
	// a real assignment. Failures here just open the breaker and
	// move on; the loop retries on its tick.
	c.refreshOnce(ctx)
	go c.refreshLoop(ctx)
}

// Close stops the refresher. Idempotent. Safe to call without Start
// (no-op when the loop never ran).
func (c *Client) Close() {
	select {
	case <-c.stop:
		// already closed
		return
	default:
		close(c.stop)
	}
	if c.started.Load() {
		<-c.done
	}
}

// HTTPClient returns an *http.Client. When useGlobalCache is true and
// the breaker is closed (or half-open), the returned client routes
// through the assigned parabellum-proxy. Otherwise it goes direct.
//
// The returned client is cheap to construct - we don't cache it
// because the proxy assignment can change on each call (after a
// reroll the next HTTPClient call should pick up the new address).
func (c *Client) HTTPClient(useGlobalCache bool) *http.Client {
	if !useGlobalCache {
		return c.directClient()
	}
	if !c.br.allow() {
		return c.directClient()
	}
	a := c.current.Load()
	if a == nil || a.ProxyAddress == "" {
		return c.directClient()
	}
	proxyURL, err := url.Parse(ensureScheme(a.ProxyAddress))
	if err != nil {
		c.br.failure()
		return c.directClient()
	}
	tlsConf := &tls.Config{}
	if c.opts.ProxyCAPool != nil {
		tlsConf.RootCAs = c.opts.ProxyCAPool
	}
	return &http.Client{
		Timeout: c.opts.HTTPTimeout,
		Transport: &transportObserver{
			inner: &http.Transport{
				Proxy:                 http.ProxyURL(proxyURL),
				TLSClientConfig:       tlsConf,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          16,
				IdleConnTimeout:       60 * time.Second,
				ResponseHeaderTimeout: c.opts.HTTPTimeout,
			},
			breaker: c.br,
		},
	}
}

// directClient is the no-proxy fallback. We still set sane keep-alive
// timeouts so a worker's RunBulk doesn't open a fresh socket per
// request.
func (c *Client) directClient() *http.Client {
	return &http.Client{
		Timeout: c.opts.HTTPTimeout,
		Transport: &http.Transport{
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          16,
			IdleConnTimeout:       60 * time.Second,
			ResponseHeaderTimeout: c.opts.HTTPTimeout,
		},
	}
}

// CurrentAssignment exposes the live assignment for tests + admin
// CLI tools. Returns nil when the refresher hasn't yet succeeded.
func (c *Client) CurrentAssignment() *Assignment {
	a := c.current.Load()
	if a == nil {
		return nil
	}
	cp := *a
	return &cp
}

// BreakerState returns "closed" / "open" / "half-open" for tests and
// for the worker's /healthz endpoint to surface to operators.
func (c *Client) BreakerState() string {
	return c.br.currentState().String()
}

// Refresh performs an immediate assignment fetch. Useful for the
// admin "force reroll" path - the operator can hint a worker to
// re-evaluate its sticky assignment without restarting it.
func (c *Client) Refresh(ctx context.Context) {
	c.refreshOnce(ctx)
}

// --- internals ------------------------------------------------------

func (c *Client) refreshLoop(ctx context.Context) {
	defer close(c.done)
	t := time.NewTicker(c.opts.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stop:
			return
		case <-t.C:
			c.refreshOnce(ctx)
		}
	}
}

func (c *Client) refreshOnce(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, c.opts.HTTPTimeout)
	defer cancel()
	a, err := c.fetchAssignment(rctx)
	if err != nil {
		c.opts.Logger.Warn("proxy assignment fetch failed",
			"worker_id", c.opts.WorkerID, "error", err)
		c.br.failure()
		return
	}
	c.current.Store(a)
	c.br.success()
}

// fetchAssignment hits GET /api/proxy/assignment?worker_id=<id>. The
// controlplane returns 503 when no healthy replica exists - we treat
// that as breaker-failure (worker stays direct) but log differently.
func (c *Client) fetchAssignment(ctx context.Context) (*Assignment, error) {
	u := strings.TrimRight(c.opts.ControlplaneURL, "/") +
		"/api/proxy/assignment?worker_id=" + url.QueryEscape(c.opts.WorkerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.opts.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.opts.APIToken)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("proxy assignment: http %d: %s", resp.StatusCode, body)
	}
	var env struct {
		Body Assignment `json:"body"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		// Some huma versions return the body unwrapped; tolerate both.
		var flat Assignment
		if jerr := json.Unmarshal(body, &flat); jerr == nil && flat.ProxyAddress != "" {
			return &flat, nil
		}
		return nil, fmt.Errorf("decode assignment: %w", err)
	}
	if env.Body.ProxyAddress == "" {
		return nil, errors.New("proxy assignment: empty proxy_address")
	}
	return &env.Body, nil
}

// ensureScheme prepends http:// when the proxy_address is bare host:port.
// The controlplane stores the bare form so this is the common case.
func ensureScheme(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "http://" + addr
}

// transportObserver wraps the proxy transport so we can flip the
// breaker on every observed HTTP error. Without this, a flaky proxy
// would never trip the breaker - the SDK consumer's request would
// just bubble up as a regular error, which is right for the caller
// but invisible to the breaker.
type transportObserver struct {
	inner   http.RoundTripper
	breaker *breaker
}

func (t *transportObserver) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		t.breaker.failure()
		return nil, err
	}
	// 5xx from the proxy itself (not the upstream) signal proxy
	// trouble. The proxy should pass-through upstream 5xx without
	// branding them as its own; this is a heuristic and may wrap a
	// few legitimate upstream 502/503s. Acceptable trade-off: a
	// few extra direct fallbacks beat a flapping breaker.
	if resp.StatusCode >= 500 && resp.Header.Get("X-Parabellum-Proxy") != "" {
		t.breaker.failure()
		return resp, nil
	}
	t.breaker.success()
	return resp, nil
}
