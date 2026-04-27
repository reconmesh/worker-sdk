package dns

import (
	"container/list"
	"context"
	"net"
	"sync"
	"time"
)

// cachingResolver wraps an underlying Resolver with an in-process LRU
// keyed on the lookup parameters. Used by both Local and HTTP backends
// to cut RTT on hot keys (the same host is asked dozens of times per
// scope run when many URLs share a domain).
//
// Cache shape: one map per call signature (Resolve, ResolveAll,
// LookupCNAME, ...). It would be neater to key on (method, host) in a
// single map but that requires reflection/interface{} boxing on the
// hot path; per-method maps are typed and allocation-free for hits.
//
// LRU eviction: doubly-linked list + map. Standard textbook impl;
// sync.Mutex for both. Concurrency is fine for the workload (1-10 k
// requests/s peak per worker process).
type cachingResolver struct {
	inner Resolver
	ttl   time.Duration
	cap   int

	mu   sync.Mutex
	resolveCache    *lru[[]net.IP]
	resolveAllCache *lru[*Records]
	cnameCache      *lru[string]
	mxCache         *lru[[]MX]
	txtCache        *lru[[]string]
	nsCache         *lru[[]string]
}

func newCachingResolver(inner Resolver, ttl time.Duration, capacity int) *cachingResolver {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if capacity <= 0 {
		capacity = 4096
	}
	return &cachingResolver{
		inner:           inner,
		ttl:             ttl,
		cap:             capacity,
		resolveCache:    newLRU[[]net.IP](capacity),
		resolveAllCache: newLRU[*Records](capacity),
		cnameCache:      newLRU[string](capacity),
		mxCache:         newLRU[[]MX](capacity),
		txtCache:        newLRU[[]string](capacity),
		nsCache:         newLRU[[]string](capacity),
	}
}

func (r *cachingResolver) Resolve(ctx context.Context, host string) ([]net.IP, error) {
	r.mu.Lock()
	if v, ok := r.resolveCache.get(host); ok {
		r.mu.Unlock()
		return v, nil
	}
	r.mu.Unlock()
	v, err := r.inner.Resolve(ctx, host)
	if err == nil {
		r.mu.Lock()
		r.resolveCache.put(host, v, r.ttl)
		r.mu.Unlock()
	}
	return v, err
}

func (r *cachingResolver) ResolveAll(ctx context.Context, host string) (*Records, error) {
	r.mu.Lock()
	if v, ok := r.resolveAllCache.get(host); ok {
		r.mu.Unlock()
		return v, nil
	}
	r.mu.Unlock()
	v, err := r.inner.ResolveAll(ctx, host)
	if err == nil {
		// Honor upstream's ValidUntil when set — it carries TTL info
		// from dns-service. Default to our own TTL otherwise.
		ttl := r.ttl
		if !v.ValidUntil.IsZero() {
			if d := time.Until(v.ValidUntil); d > 0 && d < ttl {
				ttl = d
			}
		}
		r.mu.Lock()
		r.resolveAllCache.put(host, v, ttl)
		r.mu.Unlock()
	}
	return v, err
}

func (r *cachingResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	r.mu.Lock()
	if v, ok := r.cnameCache.get(host); ok {
		r.mu.Unlock()
		return v, nil
	}
	r.mu.Unlock()
	v, err := r.inner.LookupCNAME(ctx, host)
	if err == nil {
		r.mu.Lock()
		r.cnameCache.put(host, v, r.ttl)
		r.mu.Unlock()
	}
	return v, err
}

func (r *cachingResolver) LookupMX(ctx context.Context, host string) ([]MX, error) {
	r.mu.Lock()
	if v, ok := r.mxCache.get(host); ok {
		r.mu.Unlock()
		return v, nil
	}
	r.mu.Unlock()
	v, err := r.inner.LookupMX(ctx, host)
	if err == nil {
		r.mu.Lock()
		r.mxCache.put(host, v, r.ttl)
		r.mu.Unlock()
	}
	return v, err
}

func (r *cachingResolver) LookupTXT(ctx context.Context, host string) ([]string, error) {
	r.mu.Lock()
	if v, ok := r.txtCache.get(host); ok {
		r.mu.Unlock()
		return v, nil
	}
	r.mu.Unlock()
	v, err := r.inner.LookupTXT(ctx, host)
	if err == nil {
		r.mu.Lock()
		r.txtCache.put(host, v, r.ttl)
		r.mu.Unlock()
	}
	return v, err
}

func (r *cachingResolver) LookupNS(ctx context.Context, host string) ([]string, error) {
	r.mu.Lock()
	if v, ok := r.nsCache.get(host); ok {
		r.mu.Unlock()
		return v, nil
	}
	r.mu.Unlock()
	v, err := r.inner.LookupNS(ctx, host)
	if err == nil {
		r.mu.Lock()
		r.nsCache.put(host, v, r.ttl)
		r.mu.Unlock()
	}
	return v, err
}

// ----------------------------------------------------------- generic LRU --
//
// Tiny generic LRU. Doubly-linked list with values pinned in a map.
// Not thread-safe on its own — the cachingResolver holds the mutex
// for the per-method maps to avoid map-key boxing across types.
//
// We don't pull in an external LRU library because we need TTL
// per-entry (most popular Go LRU libs cap on count only) and we want
// zero allocations on cache hits.

type lruEntry[V any] struct {
	key       string
	value     V
	expiresAt time.Time
}

type lru[V any] struct {
	cap  int
	ll   *list.List
	idx  map[string]*list.Element
}

func newLRU[V any](capacity int) *lru[V] {
	return &lru[V]{
		cap: capacity,
		ll:  list.New(),
		idx: make(map[string]*list.Element, capacity),
	}
}

func (c *lru[V]) get(key string) (V, bool) {
	var zero V
	el, ok := c.idx[key]
	if !ok {
		return zero, false
	}
	e := el.Value.(*lruEntry[V])
	if time.Now().After(e.expiresAt) {
		c.ll.Remove(el)
		delete(c.idx, key)
		return zero, false
	}
	c.ll.MoveToFront(el)
	return e.value, true
}

func (c *lru[V]) put(key string, value V, ttl time.Duration) {
	if el, ok := c.idx[key]; ok {
		e := el.Value.(*lruEntry[V])
		e.value = value
		e.expiresAt = time.Now().Add(ttl)
		c.ll.MoveToFront(el)
		return
	}
	e := &lruEntry[V]{key: key, value: value, expiresAt: time.Now().Add(ttl)}
	el := c.ll.PushFront(e)
	c.idx[key] = el
	for c.ll.Len() > c.cap {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.idx, oldest.Value.(*lruEntry[V]).key)
	}
}
