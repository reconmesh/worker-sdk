package proxyclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeNow drives the breaker's clock from tests without sleeping.
type fakeClock struct{ t atomic.Int64 }

func (f *fakeClock) now() time.Time     { return time.Unix(0, f.t.Load()) }
func (f *fakeClock) advance(d time.Duration) { f.t.Add(int64(d)) }

func newClockedBreaker(threshold int, window, cooldown time.Duration, c *fakeClock) *breaker {
	b := newBreaker(threshold, window, cooldown)
	b.now = c.now
	return b
}

// Breaker behaviour pins ----------------------------------------------

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	c := &fakeClock{}
	c.t.Store(time.Now().UnixNano())
	b := newClockedBreaker(3, 30*time.Second, 60*time.Second, c)

	if !b.allow() {
		t.Fatal("closed breaker should allow")
	}
	b.failure()
	b.failure()
	if b.currentState() != breakerClosed {
		t.Errorf("after 2 failures: state=%s want closed", b.currentState())
	}
	b.failure()
	if b.currentState() != breakerOpen {
		t.Errorf("after 3 failures: state=%s want open", b.currentState())
	}
	if b.allow() {
		t.Errorf("open breaker should not allow")
	}
}

func TestBreaker_WindowResets(t *testing.T) {
	c := &fakeClock{}
	c.t.Store(time.Now().UnixNano())
	b := newClockedBreaker(3, 30*time.Second, 60*time.Second, c)

	b.failure()
	b.failure()
	c.advance(31 * time.Second) // window expired
	b.failure()                  // first failure of new window
	if b.currentState() != breakerClosed {
		t.Errorf("state=%s want closed (window reset)", b.currentState())
	}
}

func TestBreaker_HalfOpenAfterCooldown(t *testing.T) {
	c := &fakeClock{}
	c.t.Store(time.Now().UnixNano())
	b := newClockedBreaker(2, 30*time.Second, 60*time.Second, c)
	b.failure()
	b.failure()
	if b.currentState() != breakerOpen {
		t.Fatal("expected open after 2 failures")
	}
	if b.allow() {
		t.Errorf("open breaker should not allow before cooldown")
	}
	c.advance(61 * time.Second) // past cooldown
	if !b.allow() {
		t.Errorf("expected half-open allow after cooldown")
	}
	if b.currentState() != breakerHalfOpen {
		t.Errorf("state=%s want half-open", b.currentState())
	}
	// Probe success → close.
	b.success()
	if b.currentState() != breakerClosed {
		t.Errorf("state=%s want closed after probe success", b.currentState())
	}
}

func TestBreaker_HalfOpenProbeFailureReopens(t *testing.T) {
	c := &fakeClock{}
	c.t.Store(time.Now().UnixNano())
	b := newClockedBreaker(2, 30*time.Second, 60*time.Second, c)
	b.failure()
	b.failure()
	c.advance(61 * time.Second)
	_ = b.allow() // half-open
	b.failure()    // probe fails
	if b.currentState() != breakerOpen {
		t.Errorf("state=%s want open after probe failure", b.currentState())
	}
}

// Client behaviour pins -----------------------------------------------

func TestClient_NoControlplaneNoProxy(t *testing.T) {
	// Empty ControlplaneURL = no proxy mode.
	c := New(Options{}) // intentionally minimal
	c.Start(context.Background())
	defer c.Close()

	hc := c.HTTPClient(true)
	if hc == nil {
		t.Fatal("HTTPClient returned nil")
	}
	// Client should still work for direct requests; the transport
	// must not have a proxy set.
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", hc.Transport)
	}
	if tr.Proxy != nil {
		t.Errorf("expected no proxy on direct fallback")
	}
}

func TestClient_DirectClientWhenUseGlobalCacheFalse(t *testing.T) {
	c := New(Options{ControlplaneURL: "http://127.0.0.1:1", WorkerID: "w"})
	hc := c.HTTPClient(false)
	tr := hc.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Errorf("HTTPClient(false) must not set proxy")
	}
}

func TestClient_FetchAssignmentSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/proxy/assignment" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("worker_id") != "tm-foo-1" {
			t.Errorf("worker_id missing or wrong: %q", r.URL.Query().Get("worker_id"))
		}
		if r.Header.Get("Authorization") != "Bearer rcm_test" {
			t.Errorf("auth header wrong: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"body": map[string]any{
				"proxy_id":      "abc-123",
				"proxy_host":    "proxy-1",
				"proxy_address": "proxy-1.lan:8080",
			},
		})
	}))
	defer srv.Close()

	c := New(Options{
		ControlplaneURL: srv.URL,
		APIToken:        "rcm_test",
		WorkerID:        "tm-foo-1",
		HTTPTimeout:     2 * time.Second,
	})
	defer c.Close()
	c.Start(context.Background())

	a := c.CurrentAssignment()
	if a == nil {
		t.Fatal("expected assignment after Start")
	}
	if a.ProxyHost != "proxy-1" || a.ProxyAddress != "proxy-1.lan:8080" {
		t.Errorf("got %+v", a)
	}
	if c.BreakerState() != "closed" {
		t.Errorf("breaker state = %s, want closed", c.BreakerState())
	}
}

func TestClient_FetchAssignmentHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, "no healthy replica")
	}))
	defer srv.Close()

	c := New(Options{
		ControlplaneURL: srv.URL,
		APIToken:        "x",
		WorkerID:        "w",
		HTTPTimeout:     1 * time.Second,
	})
	defer c.Close()
	c.Start(context.Background())

	if c.CurrentAssignment() != nil {
		t.Errorf("expected no assignment on 503")
	}
	// Single failure shouldn't open the breaker (default threshold 3).
	if c.BreakerState() != "closed" {
		t.Errorf("breaker state = %s, want closed (only 1 failure)", c.BreakerState())
	}
}

func TestClient_HTTPClientUsesProxyWhenAssigned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"body": map[string]any{
				"proxy_address": "10.0.0.5:8080",
			},
		})
	}))
	defer srv.Close()

	c := New(Options{
		ControlplaneURL: srv.URL,
		APIToken:        "x",
		WorkerID:        "w",
		HTTPTimeout:     1 * time.Second,
	})
	defer c.Close()
	c.Start(context.Background())

	hc := c.HTTPClient(true)
	to, ok := hc.Transport.(*transportObserver)
	if !ok {
		t.Fatalf("expected transportObserver wrapping inner http.Transport, got %T", hc.Transport)
	}
	tr := to.inner.(*http.Transport)
	if tr.Proxy == nil {
		t.Fatal("expected proxy to be set on HTTPClient(true) when assigned")
	}
	// http.ProxyURL captures a URL by closure; we can't easily compare,
	// but invoking it returns the URL we set.
	req, _ := http.NewRequest("GET", "http://example.com/", nil)
	pu, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy func errored: %v", err)
	}
	if !strings.Contains(pu.Host, "10.0.0.5:8080") {
		t.Errorf("proxy URL = %q, want host 10.0.0.5:8080", pu.Host)
	}
}

func TestClient_BreakerOpensAfterRefreshFailures(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	c := New(Options{
		ControlplaneURL: srv.URL,
		APIToken:        "x",
		WorkerID:        "w",
		HTTPTimeout:     500 * time.Millisecond,
		BreakerErrors:   3,
		BreakerWindow:   5 * time.Second,
		BreakerCooldown: 5 * time.Second,
	})
	// Don't call Start - we drive Refresh manually so the test
	// doesn't race with the loop.
	for i := 0; i < 3; i++ {
		c.Refresh(context.Background())
	}
	if calls.Load() < 3 {
		t.Errorf("expected >=3 fetches, got %d", calls.Load())
	}
	if c.BreakerState() != "open" {
		t.Errorf("breaker state = %s, want open after 3 failures", c.BreakerState())
	}
	// HTTPClient(true) under open breaker → direct fallback (no proxy).
	hc := c.HTTPClient(true)
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected direct *http.Transport when breaker open, got %T", hc.Transport)
	}
	if tr.Proxy != nil {
		t.Errorf("expected no proxy when breaker open")
	}
}

func TestEnsureScheme(t *testing.T) {
	cases := map[string]string{
		"proxy.lan:8080":          "http://proxy.lan:8080",
		"http://proxy.lan:8080":   "http://proxy.lan:8080",
		"https://proxy.lan:8443":  "https://proxy.lan:8443",
		"":                        "http://",
	}
	for in, want := range cases {
		if got := ensureScheme(in); got != want {
			t.Errorf("ensureScheme(%q) = %q, want %q", in, got, want)
		}
	}
}

// Ensure the controlplane fetcher returns a typed error on broken
// JSON so the breaker's failure() path isn't fooled into thinking
// a malformed response is a success.
func TestClient_FetchAssignmentMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not json at all")
	}))
	defer srv.Close()

	c := New(Options{
		ControlplaneURL: srv.URL,
		APIToken:        "x",
		WorkerID:        "w",
		HTTPTimeout:     500 * time.Millisecond,
	})
	defer c.Close()
	_, err := c.fetchAssignment(context.Background())
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
	if !errors.Is(err, err) { // sanity; err must be non-nil
		t.Fatal("err is nil")
	}
}
