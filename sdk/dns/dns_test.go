package dns

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecords_IPs_Order(t *testing.T) {
	r := &Records{
		A:    []string{"1.2.3.4"},
		AAAA: []string{"::1"},
	}
	got := r.IPs()
	if len(got) != 2 || got[0] != "1.2.3.4" || got[1] != "::1" {
		t.Fatalf("expected A then AAAA, got %v", got)
	}
}

func TestRecords_Resolves(t *testing.T) {
	cases := []struct {
		name string
		r    *Records
		want bool
	}{
		{"empty", &Records{}, false},
		{"a only", &Records{A: []string{"1.2.3.4"}}, true},
		{"aaaa only", &Records{AAAA: []string{"::1"}}, true},
		{"mx only", &Records{MX: []MX{{10, "x"}}}, false},
		{"nxdomain", &Records{NXDomain: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.r.Resolves() != c.want {
				t.Fatalf("Resolves()=%v want %v", c.r.Resolves(), c.want)
			}
		})
	}
}

// stubBackend lets us inject answers and count calls. Used to verify
// the LRU is actually short-circuiting the inner backend on hits.
type stubBackend struct {
	resolveCalls atomic.Int32
	cnameCalls   atomic.Int32
	ips          []net.IP
	cname        string
	err          error
}

func (s *stubBackend) Resolve(_ context.Context, _ string) ([]net.IP, error) {
	s.resolveCalls.Add(1)
	return s.ips, s.err
}
func (s *stubBackend) ResolveAll(_ context.Context, host string) (*Records, error) {
	return &Records{Host: host, A: ipStrings(s.ips), QueriedAt: time.Now()}, s.err
}
func (s *stubBackend) LookupCNAME(_ context.Context, _ string) (string, error) {
	s.cnameCalls.Add(1)
	return s.cname, s.err
}
func (s *stubBackend) LookupMX(_ context.Context, _ string) ([]MX, error)      { return nil, s.err }
func (s *stubBackend) LookupTXT(_ context.Context, _ string) ([]string, error) { return nil, s.err }
func (s *stubBackend) LookupNS(_ context.Context, _ string) ([]string, error)  { return nil, s.err }

func ipStrings(ips []net.IP) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}

func TestCachingResolver_HitSecondTime(t *testing.T) {
	stub := &stubBackend{ips: []net.IP{net.ParseIP("1.2.3.4")}}
	r := newCachingResolver(stub, time.Minute, 16)

	if _, err := r.Resolve(context.Background(), "host.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), "host.example"); err != nil {
		t.Fatal(err)
	}
	if got := stub.resolveCalls.Load(); got != 1 {
		t.Fatalf("expected one inner call (cache hit second time), got %d", got)
	}
}

func TestCachingResolver_PerKeyTTL(t *testing.T) {
	stub := &stubBackend{ips: []net.IP{net.ParseIP("1.1.1.1")}}
	r := newCachingResolver(stub, 50*time.Millisecond, 16)

	_, _ = r.Resolve(context.Background(), "x")
	_, _ = r.Resolve(context.Background(), "x")
	time.Sleep(80 * time.Millisecond) // expire
	_, _ = r.Resolve(context.Background(), "x")

	if got := stub.resolveCalls.Load(); got != 2 {
		t.Fatalf("expected 2 inner calls (one before expiry, one after), got %d", got)
	}
}

func TestHTTPBackend_NXDomainMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "nxdomain"})
	}))
	defer srv.Close()

	b := &HTTPBackend{BaseURL: srv.URL}
	_, err := b.Resolve(context.Background(), "doesnotexist.example.test")
	if !IsNXDomain(err) {
		t.Fatalf("expected ErrNXDomain, got %v", err)
	}
}

func TestHTTPBackend_FallbackOnUnreachable(t *testing.T) {
	// Point at a closed port so connect fails.
	stub := &stubBackend{ips: []net.IP{net.ParseIP("9.9.9.9")}}
	b := &HTTPBackend{BaseURL: "http://127.0.0.1:1", Fallback: stub}
	ips, err := b.Resolve(context.Background(), "x.example")
	if err != nil {
		t.Fatalf("expected fallback success, got %v", err)
	}
	if len(ips) != 1 || ips[0].String() != "9.9.9.9" {
		t.Fatalf("expected fallback ips, got %v", ips)
	}
	if stub.resolveCalls.Load() != 1 {
		t.Fatal("fallback not invoked")
	}
}

func TestNew_NoServiceFallsBackToLocal(t *testing.T) {
	r := New(Options{})
	// Just call ResolveAll on a name that ought to fail (RFC 6761
	// reserved invalid TLD); we don't care about the answer, only
	// that the chain works without panic.
	_, _ = r.ResolveAll(context.Background(), "example.invalid")
}
