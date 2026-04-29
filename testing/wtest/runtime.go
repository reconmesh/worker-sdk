package wtest

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Runtime bundles the in-memory fakes a tm-* worker typically needs:
//
//   - HTTP: a [MockUpstreamServer] for stubbed upstream APIs
//   - DNS: a [FakeDNS] returning canned A / AAAA per host
//   - Cache: an in-memory KV implementing the small subset of httpcache
//     that workers actually call (Lookup / Upsert)
//   - Ctx: a context with a default 5s deadline
//   - Cancel: explicit cancel for tests that want to simulate shutdown
//
// All cleanups are wired via t.Cleanup; a separate Close() call is
// optional but supported for tests that want to release resources
// early.
type Runtime struct {
	HTTP   *MockUpstreamServer
	DNS    *FakeDNS
	Cache  *FakeCache
	Ctx    context.Context
	Cancel context.CancelFunc
}

// NewRuntime returns a Runtime with sensible defaults. The HTTP server
// is started immediately so tests can grab .HTTP.URL() before adding
// any routes (handlers can be registered after Build via OnGET / etc.).
func NewRuntime(t testing.TB) *Runtime {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	rt := &Runtime{
		HTTP:   MockUpstream(t).Build(),
		DNS:    NewFakeDNS(),
		Cache:  NewFakeCache(),
		Ctx:    ctx,
		Cancel: cancel,
	}
	t.Cleanup(rt.Close)
	return rt
}

// Close releases the runtime's resources. Safe to call multiple times.
func (r *Runtime) Close() {
	if r.Cancel != nil {
		r.Cancel()
	}
	if r.HTTP != nil {
		r.HTTP.Close()
	}
}

// FakeDNS is a stub resolver returning canned A / AAAA / CNAME records
// per host. Records added via SetA / SetAAAA / SetCNAME; the Lookup
// methods return what was set (empty + nxdomain when nothing matches).
type FakeDNS struct {
	mu    sync.Mutex
	a     map[string][]string
	aaaa  map[string][]string
	cname map[string]string
	calls map[string]int
}

// NewFakeDNS returns an empty FakeDNS. Populate it with SetA / etc.
func NewFakeDNS() *FakeDNS {
	return &FakeDNS{
		a:     map[string][]string{},
		aaaa:  map[string][]string{},
		cname: map[string]string{},
		calls: map[string]int{},
	}
}

// SetA records canned A (IPv4) results for host.
func (f *FakeDNS) SetA(host string, ips ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.a[host] = ips
}

// SetAAAA records canned AAAA (IPv6) results for host.
func (f *FakeDNS) SetAAAA(host string, ips ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aaaa[host] = ips
}

// SetCNAME records a canonical-name pointer for host.
func (f *FakeDNS) SetCNAME(host, cname string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cname[host] = cname
}

// LookupA returns the canned A records (or nil if unset).
func (f *FakeDNS) LookupA(host string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[host]++
	out := f.a[host]
	if out == nil {
		return nil
	}
	dup := make([]string, len(out))
	copy(dup, out)
	return dup
}

// LookupAAAA returns the canned AAAA records (or nil if unset).
func (f *FakeDNS) LookupAAAA(host string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[host]++
	out := f.aaaa[host]
	if out == nil {
		return nil
	}
	dup := make([]string, len(out))
	copy(dup, out)
	return dup
}

// LookupCNAME returns the canned CNAME (or "" if unset).
func (f *FakeDNS) LookupCNAME(host string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[host]++
	return f.cname[host]
}

// CallCount returns the number of times any LookupX method was called
// for host.
func (f *FakeDNS) CallCount(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[host]
}

// FakeCache is a tiny in-memory KV approximating the common (Lookup,
// Upsert) pattern of httpcache.Cache. The keys are typically URLs;
// values are []byte payloads. Workers under test that depend on
// httpcache directly should still receive a real implementation -
// this fake is for tests of the *tool* logic, not the cache itself.
type FakeCache struct {
	mu    sync.Mutex
	store map[string][]byte
	hits  int
	miss  int
}

// NewFakeCache returns an empty FakeCache.
func NewFakeCache() *FakeCache {
	return &FakeCache{store: map[string][]byte{}}
}

// Lookup returns the cached bytes for key, or (nil, false) on miss.
func (f *FakeCache) Lookup(_ context.Context, key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.store[key]
	if ok {
		f.hits++
	} else {
		f.miss++
	}
	return v, ok
}

// Upsert writes value at key. Always succeeds.
func (f *FakeCache) Upsert(_ context.Context, key string, value []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dup := make([]byte, len(value))
	copy(dup, value)
	f.store[key] = dup
}

// Stats returns (hits, misses, size).
func (f *FakeCache) Stats() (hits, misses, size int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits, f.miss, len(f.store)
}
