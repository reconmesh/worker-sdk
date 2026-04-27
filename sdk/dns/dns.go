// Package dns is the worker-side client for the dns-service core service.
// Workers import this and call sdk.DNS().Resolve(ctx, host) without
// caring whether the resolution is served from the in-process LRU, a
// remote dns-service over HTTP, or the local system resolver as a
// last-resort fallback.
//
// The package is intentionally backend-agnostic: a Resolver is just an
// interface. Production deployments wire the HTTP backend (talking to
// dns-service); unit tests and the `--once` debug mode wire the local
// stdlib backend; one day a gRPC backend can be added without touching
// callers.
package dns

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// Records is the union of every record type a Resolver can return.
// Empty fields are valid: they mean "not asked" or "no answer".
//
// We don't split this into multiple types per RR-type because the
// majority of callers want a "give me everything you know about this
// host" view and would otherwise issue 5 calls.
type Records struct {
	Host        string    `json:"host"`
	A           []string  `json:"a,omitempty"`     // IPv4 strings
	AAAA        []string  `json:"aaaa,omitempty"`  // IPv6 strings
	CNAME       string    `json:"cname,omitempty"` // single, post-trim
	MX          []MX      `json:"mx,omitempty"`
	NS          []string  `json:"ns,omitempty"`
	TXT         []string  `json:"txt,omitempty"`
	NXDomain    bool      `json:"nxdomain,omitempty"`
	// Degraded is true when the answer comes from a fallback path
	// (system resolver because dns-service is down). Callers logging
	// this can help operators spot a service outage.
	Degraded    bool      `json:"degraded,omitempty"`
	// QueriedAt is when the upstream actually answered. For cache
	// hits in the worker-side LRU, this stays the upstream's
	// timestamp — not the moment we returned from cache.
	QueriedAt   time.Time `json:"queried_at"`
	// ValidUntil reflects the upstream TTL clipped by service caps.
	// Callers shouldn't trust answers past this; the lib enforces it
	// on its own LRU.
	ValidUntil  time.Time `json:"valid_until"`
}

// MX is a (priority, host) tuple. Smaller priority = preferred.
type MX struct {
	Priority int    `json:"priority"`
	Host     string `json:"host"`
}

// IPs returns A + AAAA flattened. Order is A first (IPv4) then AAAA
// (IPv6) — matches what most callers want when they do TCP dial with
// dual-stack fallback.
func (r *Records) IPs() []string {
	out := make([]string, 0, len(r.A)+len(r.AAAA))
	out = append(out, r.A...)
	out = append(out, r.AAAA...)
	return out
}

// Resolves reports whether the host has at least one A or AAAA. It's
// false on NXDOMAIN and on hosts that only have, say, an MX record.
// Used by callers that want "should this become a `host` asset?".
func (r *Records) Resolves() bool {
	return len(r.A) > 0 || len(r.AAAA) > 0
}

// Resolver is the contract every backend implements.
//
// Methods are split per RR-type for the common case (workers that
// only need IPs). ResolveAll is the union view; backends typically
// implement it as a single upstream multi-query.
type Resolver interface {
	// Resolve returns A + AAAA for host. Equivalent to net.LookupIP
	// in semantics (deduplicated, includes both v4 and v6).
	Resolve(ctx context.Context, host string) ([]net.IP, error)
	// ResolveAll returns every record type the backend knows. May
	// return a partial Records value with some fields empty when the
	// upstream answered for some but not all types.
	ResolveAll(ctx context.Context, host string) (*Records, error)
	// LookupCNAME follows a single CNAME hop. Empty string when
	// the host has no CNAME.
	LookupCNAME(ctx context.Context, host string) (string, error)
	// LookupMX returns MX records sorted by ascending priority.
	LookupMX(ctx context.Context, host string) ([]MX, error)
	// LookupTXT returns the raw TXT strings as the upstream sent them.
	LookupTXT(ctx context.Context, host string) ([]string, error)
	// LookupNS returns the authoritative nameservers for the zone.
	LookupNS(ctx context.Context, host string) ([]string, error)
}

// Errors that callers want to distinguish.
var (
	// ErrNXDomain mirrors the DNS NXDOMAIN response. Distinct from
	// ErrTimeout / generic transport errors so callers can decide
	// whether to retry. The default policy: don't retry NXDOMAIN
	// within the same scan run.
	ErrNXDomain = errors.New("dns: nxdomain")
	// ErrTimeout for every flavor of "the upstream didn't answer".
	// Retryable.
	ErrTimeout = errors.New("dns: timeout")
	// ErrServiceUnavailable marks a degraded mode: the service is
	// unreachable, fallback couldn't help (system resolver also
	// failed). Caller may want to skip the asset rather than
	// register a hard error.
	ErrServiceUnavailable = errors.New("dns: service unavailable")
)

// IsNXDomain wraps errors.Is for the most common conditional.
func IsNXDomain(err error) bool { return errors.Is(err, ErrNXDomain) }

// ----------------------------------------------------------------- defaults --

// DefaultLocalResolver is what the SDK serves when the worker hasn't
// configured anything explicit. It's the system resolver with a tiny
// LRU in front. Good for unit tests and for the `--once` debug mode;
// production workers should call New() and pass an HTTPBackend pointing
// at the dns-service.
var defaultOnce sync.Once
var defaultResolver Resolver

// Default returns the global Resolver. First call lazily initializes
// it from environment:
//
//   - DNS_SERVICE_URL set → HTTPBackend pointed at it
//   - unset             → LocalBackend (system resolver)
//
// Both wrap a 5-minute LRU.
func Default() Resolver {
	defaultOnce.Do(func() {
		defaultResolver = newFromEnv()
	})
	return defaultResolver
}

// New builds a Resolver with explicit config. Use this when the worker
// wants to control the backend (e.g. tests, custom cache TTL).
func New(opts Options) Resolver {
	return newWithOptions(opts)
}
